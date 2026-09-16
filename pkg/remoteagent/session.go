/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remoteagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Masterminds/semver"
	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// Session directory layout. Mirrors library/src/session.c (SPECS/SUBDIRS):
//
//	<base>/<session>/config/vgpu.config   quota region (this file is what
//	                                      provider restore() reads)
//	<base>/<session>/pids.config          SESSION-mode accounting PID list
//	<base>/<session>/.vgpu_lock/          per-device lock files
//	<base>/<session>/.vmem_node/          shared virtual-memory region
//	<base>/<session>/.sm_node/            shared SM token bucket region
//	<base>/watcher/sm_util.config         node-wide external SM watcher cache
//
// The library owns everything except vgpu.config and the directories; the
// agent only creates the skeleton and writes the quota.
//
// SM watcher bridge: the library resolves the shared cache to
// <base>/watcher/sm_util.config (session.c, from_base), but the file is
// written by the dra-server plugin at <manager-dir>/watcher/. Prepare()
// therefore makes <base>/watcher a symlink to the sibling watcher directory
// (<base>/../watcher), so both sides see one file. This requires the session
// base to live directly under the manager dir (the deployment default,
// /etc/vgpu-manager/remote-sessions).
const (
	sessionLockDir = "." + vgpu.VGPULockDirName
	sessionVMemDir = "." + util.VMemNode
	sessionSMDir   = "." + util.SMNode
	// sessionOwnerMarker is agent-private: token -> owner, written last. The
	// name predates pod-owned sessions and is kept so an agent upgrade finds
	// the sessions it left behind.
	sessionOwnerMarker = ".claim-uid"

	pidsFileMode = 0o644
)

// linkWatcherDir points <base>/watcher at the manager dir's watcher
// directory (see the SM watcher bridge note above). An empty leftover
// directory from an older agent is replaced; a non-empty one is kept with a
// warning (sessions then miss the shared cache and fall back to per-process
// NVML sampling — wrong data is never read).
func (s *SessionStore) linkWatcherDir() error {
	base := strings.TrimRight(s.cfg.SessionBase, "/")
	// Absolute target: every container mounts the manager dir at the same
	// path. An unset ContainerManagerDir (library callers) falls back to the
	// session base's parent — the deployment default layout.
	managerDir := s.cfg.ContainerManagerDir
	if managerDir == "" {
		managerDir = filepath.Dir(base)
	}
	target := filepath.Join(managerDir, util.Watcher)
	link := filepath.Join(base, util.Watcher)

	if current, err := os.Readlink(link); err == nil {
		if current == target {
			return nil
		}
		_ = os.Remove(link) // symlink to somewhere else: replace
	} else if info, err := os.Lstat(link); err == nil && info.IsDir() {
		if entries, _ := os.ReadDir(link); len(entries) > 0 {
			klog.Warningf("%s is a non-empty directory, not replacing it with a symlink; sessions will not see the shared SM watcher cache", link)
			return nil
		}
		_ = os.Remove(link)
	}
	// Make sure the real watcher dir exists so the link never dangles.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(base), util.Watcher), 0o755); err != nil {
		return fmt.Errorf("mkdir watcher dir: %w", err)
	}
	if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
		return fmt.Errorf("symlink %s -> %s: %w", link, target, err)
	}
	return nil
}

// tokenPattern bounds what we accept as a session directory name. The token
// travels as an HTTP/2 header on the lupine side and is a path component
// here, so it must be neither traversable nor exotic.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateToken(token string) error {
	if !tokenPattern.MatchString(token) || token == "." || token == ".." {
		return fmt.Errorf("invalid session token %q", token)
	}
	return nil
}

// NodeDevice is the agent's view of one device this node publishes, read back
// from the node's own ResourceSlice (DRA path) or device registry annotation
// (device-plugin path) so that the agent needs no NVML.
type NodeDevice struct {
	Name        string
	Minor       int64
	UUID        string
	MemoryMiB   int64 // published capacity (= physical * memory ratio)
	Cores       int64 // published capacity (= cores ratio)
	MemoryRatio int64
}

// NodeDevices is the node-level snapshot used to materialize sessions.
type NodeDevices struct {
	CudaVersion   *semver.Version
	DriverVersion *semver.Version
	Devices       map[string]NodeDevice // by device name
}

// CudaVersionString returns the CUDA driver version as published, or "" when
// the snapshot has none.
func (nd *NodeDevices) CudaVersionString() string {
	if nd == nil || nd.CudaVersion == nil {
		return ""
	}
	return nd.CudaVersion.Original()
}

// OwnerKind is what a session belongs to.
type OwnerKind string

const (
	// OwnerClaim is a session of a ResourceClaim (DRA path).
	OwnerClaim OwnerKind = "claim"
	// OwnerPod is a session of one container of a Pod (device-plugin path).
	OwnerPod OwnerKind = "pod"
	// OwnerAuto is not an owner a session can have: it is the configuration
	// value for an agent that serves both kinds (see Config.SessionOwnerKind).
	OwnerAuto OwnerKind = "auto"
)

// SessionOwner identifies the object a session belongs to. Version is the
// object's resourceVersion at materialization: it lets an event or sweep tell
// "this session predates what I am looking at" from "this session is newer
// than my view".
type SessionOwner struct {
	Kind      OwnerKind
	UID       string
	Namespace string
	Name      string
	Version   int64
}

func (o SessionOwner) String() string {
	if o.Name == "" {
		return fmt.Sprintf("%s %s", o.Kind, o.UID)
	}
	return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name)
}

// SessionSpec is what one session is made of: its owner and the per-device
// quota the owner may use on this node.
type SessionSpec struct {
	Owner SessionOwner
	// Infos is the full capacity of each device, Claims the share this
	// session gets, both in slot (host device index) order.
	Infos       []device.DeviceClaim
	Claims      []device.DeviceClaim
	MemoryRatio float64
}

// sessionRef is what the index remembers about a materialized session.
type sessionRef struct {
	ownerUID string
	version  int64
}

// SessionStore materializes and removes session directories under base and
// keeps an in-memory index (token <-> owner) so owner events never need a
// directory scan; the periodic sweep still walks the disk to catch orphans.
type SessionStore struct {
	cfg     Config
	mu      sync.Mutex
	refOf   map[string]sessionRef       // token -> owner
	byOwner map[string]sets.Set[string] // owner UID -> tokens
}

func NewSessionStore(cfg Config) *SessionStore {
	return &SessionStore{cfg: cfg, refOf: map[string]sessionRef{}, byOwner: map[string]sets.Set[string]{}}
}

// Prepare creates the base skeleton the server needs before it starts and
// rebuilds the index from whatever sessions survived a restart.
func (s *SessionStore) Prepare() error {
	if err := os.MkdirAll(s.cfg.SessionBase, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.cfg.SessionBase, err)
	}
	if err := s.linkWatcherDir(); err != nil {
		return err
	}
	entries, err := s.List()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Owner.UID != "" {
			s.indexLocked(e.Token, sessionRef{ownerUID: e.Owner.UID, version: e.Owner.Version})
		}
	}
	return nil
}

// TokensOfOwner returns the sessions currently materialized for an owner.
func (s *SessionStore) TokensOfOwner(ownerUID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sets.List(s.byOwner[ownerUID])
}

// Release removes the given sessions of an owner -- all of them when tokens
// is empty -- and returns how many were removed. A token that belongs to
// another owner (or to none) is skipped: the caller only ever knows the
// owner UID, so this is the check that keeps one owner from releasing
// another's sessions.
func (s *SessionStore) Release(ownerUID string, tokens []string) (int, error) {
	if len(tokens) == 0 {
		tokens = s.TokensOfOwner(ownerUID)
	}
	released := 0
	for _, token := range tokens {
		if err := validateToken(token); err != nil {
			return released, err
		}
		s.mu.Lock()
		ref, ok := s.refOf[token]
		s.mu.Unlock()
		if !ok || ref.ownerUID != ownerUID {
			klog.V(2).Infof("Release: session %s is not a session of owner %s; skipped", token, ownerUID)
			continue
		}
		if err := s.Remove(token); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// Sweep removes the sessions of an owner that a view of the owner shows to be
// stale: every session not in `keep` whose materialization is not newer than
// the view. `keep` is the owner's current session set, empty when the owner no
// longer uses any; `viewRV` is the resourceVersion of the object the view was
// taken from, MaxInt64 for "the owner no longer exists". A session
// materialized from a newer version than the view is left alone -- that is the
// informer lagging behind, not a stale session -- and the next, newer view
// settles it. Returns how many sessions were removed.
func (s *SessionStore) Sweep(ownerUID string, keep sets.Set[string], viewRV int64) int {
	s.mu.Lock()
	var stale []string
	for token := range s.byOwner[ownerUID] {
		if ref := s.refOf[token]; !keep.Has(token) && ref.version <= viewRV {
			stale = append(stale, token)
		}
	}
	s.mu.Unlock()

	removed := 0
	for _, token := range stale {
		if err := s.Remove(token); err != nil {
			klog.Warningf("sweep session %s of owner %s: %v", token, ownerUID, err)
			continue
		}
		removed++
	}
	return removed
}

func (s *SessionStore) indexLocked(token string, ref sessionRef) {
	s.refOf[token] = ref
	if s.byOwner[ref.ownerUID] == nil {
		s.byOwner[ref.ownerUID] = sets.New[string]()
	}
	s.byOwner[ref.ownerUID].Insert(token)
}

func (s *SessionStore) unindexLocked(token string) {
	if ref, ok := s.refOf[token]; ok {
		delete(s.refOf, token)
		if set := s.byOwner[ref.ownerUID]; set != nil {
			set.Delete(token)
			if set.Len() == 0 {
				delete(s.byOwner, ref.ownerUID)
			}
		}
	}
}

// objectRV parses a resourceVersion as the integer etcd revision it is in
// every supported apiserver (the same assumption client-go's MutationCache
// makes); 0 when absent or unparseable, i.e. "as old as it gets", so a sweep
// never mistakes it for a newer session.
func objectRV(resourceVersion string) int64 {
	rv, err := strconv.ParseInt(resourceVersion, 10, 64)
	if err != nil || rv < 0 {
		return 0
	}
	return rv
}

// The marker file: line 1 the owner UID, line 2 its resourceVersion at
// materialization (absent in markers written by older agents, read as 0),
// line 3 the owner kind (absent means a claim, as older agents only had those).
func writeMarker(path string, owner SessionOwner) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%s\n%d\n%s\n", owner.UID, owner.Version, owner.Kind)), 0o644)
}

func readMarker(path string) (SessionOwner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SessionOwner{}, err
	}
	lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 4)
	owner := SessionOwner{Kind: OwnerClaim, UID: strings.TrimSpace(lines[0])}
	if len(lines) > 1 {
		if v, perr := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); perr == nil && v > 0 {
			owner.Version = v
		}
	}
	if len(lines) > 2 {
		if kind := strings.TrimSpace(lines[2]); kind != "" {
			owner.Kind = OwnerKind(kind)
		}
	}
	return owner, nil
}
func (s *SessionStore) dir(token string) string {
	return filepath.Join(s.cfg.SessionBase, token)
}

// Materialize writes the session directory for spec. It is idempotent: an
// already complete session of the same owner is left untouched (the library
// may have live state in it), so retries are safe.
func (s *SessionStore) Materialize(token string, spec SessionSpec, nd *NodeDevices) error {
	if err := validateToken(token); err != nil {
		return err
	}
	if len(spec.Claims) == 0 {
		return fmt.Errorf("%s has no devices on this node", spec.Owner)
	}
	if len(spec.Claims) > vgpuconfig.MaxDeviceCount {
		return fmt.Errorf("%s uses %d devices on this node, max %d per session",
			spec.Owner, len(spec.Claims), vgpuconfig.MaxDeviceCount)
	}
	if nd == nil || nd.CudaVersion == nil {
		return fmt.Errorf("node device snapshot has no CUDA version; cannot write session")
	}
	driverVersion := ""
	if nd.DriverVersion != nil {
		driverVersion = nd.DriverVersion.Original()
	}

	data := vgpuconfig.NewResourceDataWithOptions(vgpuconfig.ResourceOption{
		PodNamespace: spec.Owner.Namespace,
		PodName:      spec.Owner.Name,
		PodUID:       spec.Owner.UID,
	},
		vgpuconfig.WithDeviceInfos(spec.Infos),
		vgpuconfig.WithDeviceClaims(spec.Claims),
		vgpuconfig.WithCompatibilityMode(util.SessionMode),
		vgpuconfig.WithComputePolicy(util.FixedComputePolicy),
		vgpuconfig.WithDriverVersion(nvidia.DriverVersion{
			DriverVersion: driverVersion,
			CudaDriverVersion: nvidia.NewCudaVersion(
				nd.CudaVersion.Major(), nd.CudaVersion.Minor(),
			),
		}),
		vgpuconfig.WithMemoryRatio(spec.MemoryRatio),
		vgpuconfig.WithVMemoryNodeEnabled(s.cfg.gateEnabled(util.VirtualMemoryTracking)),
		vgpuconfig.WithSMWatcherEnabled(s.cfg.gateEnabled(util.SharedSMUtilizationWatcher)),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	root := s.dir(token)
	marker := filepath.Join(root, sessionOwnerMarker)
	if owner, err := readMarker(marker); err == nil {
		if owner.UID == spec.Owner.UID {
			klog.V(4).Infof("Session %s for %s already materialized", token, spec.Owner)
			s.indexLocked(token, sessionRef{ownerUID: owner.UID, version: owner.Version})
			return nil
		}
		return fmt.Errorf("session %s already belongs to %s", token, owner)
	}

	for _, sub := range []string{util.Config, sessionLockDir, sessionVMemDir, sessionSMDir} {
		if err := util.EnsureDir(filepath.Join(root, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir session dir: %w", err)
		}
	}
	// pids.config must exist (empty) before the first child registers; the
	// library appends to it, so never truncate an existing one.
	f, err := os.OpenFile(filepath.Join(root, registry.PidsConfig), os.O_CREATE|os.O_WRONLY, pidsFileMode)
	if err != nil {
		return fmt.Errorf("create pids file: %w", err)
	}
	_ = f.Close()

	if err = vgpuconfig.WriteResourceDataToDisk(filepath.Join(root, util.Config, vgpu.VGPUConfigFileName), data); err != nil {
		return fmt.Errorf("write session quota: %w", err)
	}
	// Marker last: its presence means "complete".
	if err = writeMarker(marker, spec.Owner); err != nil {
		return fmt.Errorf("write owner marker: %w", err)
	}
	s.indexLocked(token, sessionRef{ownerUID: spec.Owner.UID, version: spec.Owner.Version})
	klog.Infof("Materialized session %s for %s: %d device(s)", token, spec.Owner, len(spec.Claims))
	return nil
}

// Remove deletes a session directory (idempotent).
func (s *SessionStore) Remove(token string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.RemoveAll(s.dir(token))
	if err == nil {
		s.unindexLocked(token)
		klog.Infof("Removed session %s", token)
	}
	return err
}

// Entry is one on-disk session.
type Entry struct {
	Token string
	// Owner has an empty UID when the marker is missing (incomplete session).
	Owner SessionOwner
}

// List enumerates on-disk sessions.
func (s *SessionStore) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirents, err := os.ReadDir(s.cfg.SessionBase)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, e := range dirents {
		if !e.IsDir() || e.Name() == util.Watcher {
			continue
		}
		if validateToken(e.Name()) != nil {
			continue
		}
		entry := Entry{Token: e.Name()}
		filePath := filepath.Join(s.cfg.SessionBase, e.Name(), sessionOwnerMarker)
		if owner, err := readMarker(filePath); err == nil {
			entry.Owner = owner
		} else if !errors.Is(err, os.ErrNotExist) {
			klog.Warningf("read marker of session %s: %v", e.Name(), err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
