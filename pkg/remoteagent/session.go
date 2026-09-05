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
	"strings"
	"sync"

	"github.com/Masterminds/semver"
	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics/collector"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
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
	sessionLockDir     = "." + vgpu.VGPULockDirName
	sessionVMemDir     = "." + util.VMemNode
	sessionSMDir       = "." + util.SMNode
	sessionClaimMarker = ".claim-uid" // agent-private: token -> claim UID, written last

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

// NodeDevice is the agent's view of one device published by this node's
// kubelet-plugin, read back from the node's own ResourceSlice so that the
// agent needs no NVML.
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

// NodeRemoteDevicesFromSlices builds the snapshot from this node's slices.
// Only accessMode=remote devices that carry a uuid and a minor are kept:
// the minor is the host device index, which is also the session config slot
// (library config_allowed_devices treats slot index as host index). The map
// is keyed by device name because allocation results reference devices by
// name.
func NodeRemoteDevicesFromSlices(slices []*resourceapi.ResourceSlice) *NodeDevices {
	nd := &NodeDevices{Devices: map[string]NodeDevice{}}
	for _, slice := range slices {
		for _, dev := range slice.Spec.Devices {
			mode := remote.StringAttr(&dev, remote.AttrAccessMode)
			if mode != remote.AccessModeRemote {
				continue
			}
			uuid := collector.DeviceUUIDFromAttribute(remote.StringAttr(&dev, remote.AttrUUID))
			if uuid == "" {
				continue
			}
			minor := remote.IntAttr(&dev, remote.AttrMinor)
			if minor < 0 || minor >= vgpuconfig.MaxDeviceCount {
				continue
			}
			d := NodeDevice{Name: dev.Name, Minor: minor, UUID: uuid, MemoryRatio: util.HundredCore}
			if ratio := remote.IntAttr(&dev, remote.AttrMemoryRatio); ratio >= 0 {
				d.MemoryRatio = ratio
			}
			if q, ok := dev.Capacity[remote.CapacityCores]; ok {
				d.Cores = q.Value.Value()
			}
			if q, ok := dev.Capacity[remote.CapacityMemory]; ok {
				d.MemoryMiB = q.Value.Value() >> 20
			}
			nd.Devices[d.Name] = d

			if nd.CudaVersion == nil {
				if version := remote.VersionAttr(&dev, remote.AttrCUDADriverVersion); version != "" {
					if v, err := semver.NewVersion(version); err == nil {
						nd.CudaVersion = v
					}
				}
			}
			if nd.DriverVersion == nil {
				if version := remote.VersionAttr(&dev, remote.AttrDriverVersion); version != "" {
					if v, err := semver.NewVersion(version); err == nil {
						nd.DriverVersion = v
					}
				}
			}
		}
	}
	return nd
}

// CudaVersionString returns the CUDA driver version as published, or "" when
// the snapshot has none.
func (nd *NodeDevices) CudaVersionString() string {
	if nd == nil || nd.CudaVersion == nil {
		return ""
	}
	return nd.CudaVersion.Original()
}

// SessionStore materializes and removes session directories under base and
// keeps an in-memory index (token <-> claim UID) so claim events never need a
// directory scan; the periodic GC still walks the disk to catch orphans.
type SessionStore struct {
	cfg     Config
	mu      sync.Mutex
	claimOf map[string]string           // token -> claim UID
	byClaim map[string]sets.Set[string] // claim UID -> tokens
}

func NewSessionStore(cfg Config) *SessionStore {
	return &SessionStore{cfg: cfg, claimOf: map[string]string{}, byClaim: map[string]sets.Set[string]{}}
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
		if e.ClaimUID != "" {
			s.indexLocked(e.Token, e.ClaimUID)
		}
	}
	return nil
}

// TokensOfClaim returns the sessions currently materialized for a claim.
func (s *SessionStore) TokensOfClaim(claimUID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sets.List(s.byClaim[claimUID])
}

func (s *SessionStore) indexLocked(token, claimUID string) {
	s.claimOf[token] = claimUID
	if s.byClaim[claimUID] == nil {
		s.byClaim[claimUID] = sets.New[string]()
	}
	s.byClaim[claimUID].Insert(token)
}

func (s *SessionStore) unindexLocked(token string) {
	if claimUID, ok := s.claimOf[token]; ok {
		delete(s.claimOf, token)
		if set := s.byClaim[claimUID]; set != nil {
			set.Delete(token)
			if set.Len() == 0 {
				delete(s.byClaim, claimUID)
			}
		}
	}
}
func (s *SessionStore) dir(token string) string {
	return filepath.Join(s.cfg.SessionBase, token)
}

// Materialize writes the session for the partition of `claim` named by
// `requests` (main request names; empty = every request) on pool
// `poolName`. It is idempotent: an already complete session is left
// untouched (the library may have live state in it), so kubelet retries of
// NodePrepare are safe. When several results of the partition land on the
// same physical device (the webhook normally prevents this), the largest
// share wins — the config has one slot per device.
func (s *SessionStore) Materialize(token string, claim *resourceapi.ResourceClaim, nd *NodeDevices, requests []string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	if claim.Status.Allocation == nil {
		return fmt.Errorf("claim %s has no allocation", klog.KObj(claim))
	}

	poolName := s.cfg.NodeName
	memoryRatio := int64(util.HundredCore)
	results := remote.FilterResultsByRequests(claim, claim.Status.Allocation.Devices.Results, requests)
	infoBySlot := map[int]device.DeviceClaim{}
	claimBySlot := map[int]device.DeviceClaim{}
	for _, result := range results {
		if result.Driver != s.cfg.DriverName || result.Pool != poolName {
			continue
		}
		dev, ok := nd.Devices[result.Device]
		if !ok {
			return fmt.Errorf("allocated device %s is not published by this node (pool %s)", result.Device, poolName)
		}
		if memoryRatio == util.HundredCore && dev.MemoryRatio != memoryRatio {
			memoryRatio = dev.MemoryRatio
		}
		// Slot = host device index (minor), exactly as the local path lays
		// out the config: the library reads slot index as host index
		// (config_allowed_devices) and translates to the container-visible
		// ordinal itself.
		slot := int(dev.Minor)
		infoBySlot[slot] = device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: dev.Cores, Memory: dev.MemoryMiB}

		cores, memoryMiB := dev.Cores, dev.MemoryMiB
		if q, ok := result.ConsumedCapacity[remote.CapacityCores]; ok {
			cores = q.Value()
		}
		if q, ok := result.ConsumedCapacity[remote.CapacityMemory]; ok {
			memoryMiB = q.Value() >> 20
		}
		if prev, dup := claimBySlot[slot]; dup {
			klog.Warningf("session %s: device %s allocated more than once in one partition; taking the larger share", token, result.Device)
			cores = max(cores, prev.Cores)
			memoryMiB = max(memoryMiB, prev.Memory)
		}
		claimBySlot[slot] = device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: cores, Memory: memoryMiB}
	}
	var infos, claims []device.DeviceClaim
	for _, slot := range sets.List(sets.KeySet(claimBySlot)) {
		infos = append(infos, infoBySlot[slot])
		claims = append(claims, claimBySlot[slot])
	}
	if len(claims) == 0 {
		return fmt.Errorf("claim %s has no devices allocated from pool %s", klog.KObj(claim), poolName)
	}
	if nd.CudaVersion == nil {
		return fmt.Errorf("node device snapshot has no %s attribute; cannot write session", remote.AttrCUDADriverVersion)
	}
	driverVersion := ""
	if nd.DriverVersion != nil {
		driverVersion = nd.DriverVersion.Original()
	}
	if len(claims) > vgpuconfig.MaxDeviceCount {
		return fmt.Errorf("claim %s allocates %d devices on this node, max %d per session", klog.KObj(claim), len(claims), vgpuconfig.MaxDeviceCount)
	}

	data := vgpuconfig.NewResourceDataWithOptions(vgpuconfig.ResourceOption{
		PodNamespace: claim.Namespace,
		PodName:      claim.Name,
		PodUID:       string(claim.UID),
	},
		vgpuconfig.WithDeviceInfos(infos),
		vgpuconfig.WithDeviceClaims(claims),
		vgpuconfig.WithCompatibilityMode(util.SessionMode),
		vgpuconfig.WithComputePolicy(util.FixedComputePolicy),
		vgpuconfig.WithDriverVersion(nvidia.DriverVersion{
			DriverVersion: driverVersion,
			CudaDriverVersion: nvidia.NewCudaVersion(
				nd.CudaVersion.Major(), nd.CudaVersion.Minor(),
			),
		}),
		vgpuconfig.WithMemoryRatio(float64(memoryRatio)/float64(util.HundredCore)),
		vgpuconfig.WithVMemoryNodeEnabled(s.cfg.gateEnabled(util.VirtualMemoryTracking)),
		vgpuconfig.WithSMWatcherEnabled(s.cfg.gateEnabled(util.SharedSMUtilizationWatcher)),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	root := s.dir(token)
	marker := filepath.Join(root, sessionClaimMarker)
	if existing, err := os.ReadFile(marker); err == nil {
		if strings.TrimSpace(string(existing)) == string(claim.UID) {
			klog.V(4).Infof("Session %s for claim %s already materialized", token, klog.KObj(claim))
			s.indexLocked(token, string(claim.UID))
			return nil
		}
		return fmt.Errorf("session %s already belongs to claim %s", token, strings.TrimSpace(string(existing)))
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
	if err = os.WriteFile(marker, []byte(claim.UID+"\n"), 0o644); err != nil {
		return fmt.Errorf("write claim marker: %w", err)
	}
	s.indexLocked(token, string(claim.UID))
	klog.Infof("Materialized session %s for claim %s (requests %v): %d device(s)", token, klog.KObj(claim), requests, len(claims))
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
	Token    string
	ClaimUID string // empty when the marker is missing (incomplete session)
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
		filePath := filepath.Join(s.cfg.SessionBase, e.Name(), sessionClaimMarker)
		if b, err := os.ReadFile(filePath); err == nil {
			entry.ClaimUID = strings.TrimSpace(string(b))
		} else if !errors.Is(err, os.ErrNotExist) {
			klog.Warningf("read marker of session %s: %v", e.Name(), err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
