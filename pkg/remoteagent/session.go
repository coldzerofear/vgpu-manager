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
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
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
const (
	sessionConfigDir   = "config"
	sessionLockDir     = ".vgpu_lock"
	sessionVMemDir     = ".vmem_node"
	sessionSMDir       = ".sm_node"
	sessionConfigFile  = "vgpu.config"
	sessionPidsFile    = "pids.config"
	sessionClaimMarker = ".claim-uid" // agent-private: token -> claim UID, written last
	watcherDir         = "watcher"

	pidsFileMode = 0o644
)

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
	Name      string
	UUID      string
	MemoryMiB int64 // published capacity (= physical * memory ratio)
	Cores     int64 // published capacity (= cores ratio)
}

// NodeDevices is the node-level snapshot used to materialize sessions.
type NodeDevices struct {
	CudaDriverVersion nvidia.CudaDriverVersion
	CudaVersionString string
	DriverVersion     string
	Devices           map[string]NodeDevice // by device name
}

// NodeDevicesFromSlices builds the snapshot from the slices of pool
// `poolName` (= the node name). Devices without a uuid are skipped.
func NodeDevicesFromSlices(slices []*resourceapi.ResourceSlice, poolName string) *NodeDevices {
	nd := &NodeDevices{Devices: map[string]NodeDevice{}}
	for _, slice := range slices {
		if slice.Spec.Pool.Name != poolName {
			continue
		}
		for _, dev := range slice.Spec.Devices {
			uuid := stringAttr(&dev, remote.AttrUUID)
			if uuid == "" {
				continue
			}
			d := NodeDevice{Name: dev.Name, UUID: uuid}
			if q, ok := dev.Capacity[remote.CapacityMemory]; ok {
				d.MemoryMiB = q.Value.Value() >> 20
			}
			if q, ok := dev.Capacity[remote.CapacityCores]; ok {
				d.Cores = q.Value.Value()
			}
			nd.Devices[dev.Name] = d

			if nd.CudaVersionString == "" {
				if attr, ok := dev.Attributes[remote.AttrCUDADriverVersion]; ok && attr.VersionValue != nil {
					if v, err := semver.NewVersion(*attr.VersionValue); err == nil {
						nd.CudaVersionString = *attr.VersionValue
						nd.CudaDriverVersion = nvidia.CudaDriverVersion(v.Major()*1000 + v.Minor()*10)
					}
				}
			}
			if nd.DriverVersion == "" {
				if attr, ok := dev.Attributes["driverVersion"]; ok && attr.VersionValue != nil {
					nd.DriverVersion = *attr.VersionValue
				}
			}
		}
	}
	return nd
}

func stringAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr, ok := dev.Attributes[name]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

// SessionStore materializes and removes session directories under base.
type SessionStore struct {
	base      string
	smWatcher bool
	mu        sync.Mutex
}

func NewSessionStore(base string, smWatcher bool) *SessionStore {
	return &SessionStore{base: base, smWatcher: smWatcher}
}

// Prepare creates the base skeleton the server needs before it starts.
func (s *SessionStore) Prepare() error {
	for _, dir := range []string{s.base, filepath.Join(s.base, watcherDir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}

func (s *SessionStore) dir(token string) string {
	return filepath.Join(s.base, token)
}

// Materialize writes the session for `claim`'s allocation on pool
// `poolName`. It is idempotent: an already complete session is left
// untouched (the library may have live state in it), so kubelet retries of
// NodePrepare are safe.
func (s *SessionStore) Materialize(token string, claim *resourceapi.ResourceClaim, nd *NodeDevices, poolName, driverName string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	if claim.Status.Allocation == nil {
		return fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name)
	}

	var infos, claims []device.DeviceClaim
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != driverName || result.Pool != poolName {
			continue
		}
		nodeDev, ok := nd.Devices[result.Device]
		if !ok {
			return fmt.Errorf("allocated device %s is not published by this node (pool %s)", result.Device, poolName)
		}
		// Slot order = allocation order; the provider publishes
		// CUDA_VISIBLE_DEVICES from the active slots in that order
		// (library/src/checkpoint_provider.c), and vgpu-session-config
		// follows the same convention.
		slot := len(infos)
		infos = append(infos, device.DeviceClaim{Id: slot, Uuid: nodeDev.UUID, Cores: nodeDev.Cores, Memory: nodeDev.MemoryMiB})

		cores, memoryMiB := nodeDev.Cores, nodeDev.MemoryMiB
		if q, ok := result.ConsumedCapacity[remote.CapacityCores]; ok {
			cores = q.Value()
		}
		if q, ok := result.ConsumedCapacity[remote.CapacityMemory]; ok {
			memoryMiB = q.Value() >> 20
		}
		claims = append(claims, device.DeviceClaim{Id: slot, Uuid: nodeDev.UUID, Cores: cores, Memory: memoryMiB})
	}
	if len(claims) == 0 {
		return fmt.Errorf("claim %s/%s has no devices allocated from pool %s", claim.Namespace, claim.Name, poolName)
	}
	if len(claims) > vgpuconfig.MaxDeviceCount {
		return fmt.Errorf("claim %s/%s allocates %d devices on this node, max %d per session", claim.Namespace, claim.Name, len(claims), vgpuconfig.MaxDeviceCount)
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
		vgpuconfig.WithMemoryRatio(1),
		vgpuconfig.WithDriverVersion(nvidia.DriverVersion{
			DriverVersion:     nd.DriverVersion,
			CudaDriverVersion: nd.CudaDriverVersion,
		}),
		vgpuconfig.WithVMemoryNodeEnabled(true),
		vgpuconfig.WithSMWatcherEnabled(s.smWatcher),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	root := s.dir(token)
	marker := filepath.Join(root, sessionClaimMarker)
	if existing, err := os.ReadFile(marker); err == nil {
		if strings.TrimSpace(string(existing)) == string(claim.UID) {
			klog.V(4).Infof("Session %s for claim %s/%s already materialized", token, claim.Namespace, claim.Name)
			return nil
		}
		return fmt.Errorf("session %s already belongs to claim %s", token, strings.TrimSpace(string(existing)))
	}

	for _, sub := range []string{sessionConfigDir, sessionLockDir, sessionVMemDir, sessionSMDir} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir session dir: %w", err)
		}
	}
	// pids.config must exist (empty) before the first child registers; the
	// library appends to it, so never truncate an existing one.
	f, err := os.OpenFile(filepath.Join(root, sessionPidsFile), os.O_CREATE|os.O_WRONLY, pidsFileMode)
	if err != nil {
		return fmt.Errorf("create pids file: %w", err)
	}
	_ = f.Close()

	if err := vgpuconfig.WriteResourceDataToDisk(filepath.Join(root, sessionConfigDir, sessionConfigFile), data); err != nil {
		return fmt.Errorf("write session quota: %w", err)
	}
	// Marker last: its presence means "complete".
	if err := os.WriteFile(marker, []byte(claim.UID+"\n"), 0o644); err != nil {
		return fmt.Errorf("write claim marker: %w", err)
	}
	klog.Infof("Materialized session %s for claim %s/%s: %d device(s)", token, claim.Namespace, claim.Name, len(claims))
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
	dirents, err := os.ReadDir(s.base)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, e := range dirents {
		if !e.IsDir() || e.Name() == watcherDir || validateToken(e.Name()) != nil {
			continue
		}
		entry := Entry{Token: e.Name()}
		if b, err := os.ReadFile(filepath.Join(s.base, e.Name(), sessionClaimMarker)); err == nil {
			entry.ClaimUID = strings.TrimSpace(string(b))
		} else if !errors.Is(err, os.ErrNotExist) {
			klog.Warningf("read marker of session %s: %v", e.Name(), err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
