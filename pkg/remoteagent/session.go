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
	"github.com/coldzerofear/vgpu-manager/pkg/metrics/collector"
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
	Minor     int64
	UUID      string
	MemoryMiB int64 // published capacity (= physical * memory ratio)
	Cores     int64 // published capacity (= cores ratio)
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
			mode := stringAttr(&dev, remote.AttrAccessMode)
			uuid := collector.DeviceUUIDFromAttribute(stringAttr(&dev, remote.AttrUUID))
			minor := intAttr(&dev, remote.AttrMinor)
			if mode != remote.AccessModeRemote || uuid == "" || minor < 0 || minor >= vgpuconfig.MaxDeviceCount {
				continue
			}

			d := NodeDevice{Name: dev.Name, Minor: minor, UUID: uuid}
			if q, ok := dev.Capacity[remote.CapacityMemory]; ok {
				d.MemoryMiB = q.Value.Value() >> 20
			}
			if q, ok := dev.Capacity[remote.CapacityCores]; ok {
				d.Cores = q.Value.Value()
			}
			nd.Devices[d.Name] = d

			if nd.CudaVersion == nil {
				if version := versionAttr(&dev, remote.AttrCUDADriverVersion); version != "" {
					if v, err := semver.NewVersion(version); err == nil {
						nd.CudaVersion = v
					}
				}
			}
			if nd.DriverVersion == nil {
				if version := versionAttr(&dev, remote.AttrDriverVersion); version != "" {
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
func stringAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr, ok := dev.Attributes[name]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

func intAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) int64 {
	if attr, ok := dev.Attributes[name]; ok && attr.IntValue != nil {
		return *attr.IntValue
	}
	return -1
}

func versionAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr, ok := dev.Attributes[name]; ok && attr.VersionValue != nil {
		return *attr.VersionValue
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
		return fmt.Errorf("claim %s has no allocation", klog.KObj(claim))
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
		// Slot = host device index (minor), exactly as the local path lays
		// out the config: the library reads slot index as host index
		// (config_allowed_devices) and translates to the container-visible
		// ordinal itself; the provider builds CUDA_VISIBLE_DEVICES from the
		// active slots in ascending host order.
		slot := int(nodeDev.Minor)
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
	if nd.CudaVersion == nil {
		return fmt.Errorf("node device snapshot has no %s attribute; cannot write session", remote.AttrCUDADriverVersion)
	}
	driverVersion := ""
	if nd.DriverVersion != nil {
		driverVersion = nd.DriverVersion.Original()
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
			DriverVersion: driverVersion,
			CudaDriverVersion: nvidia.CudaDriverVersion(
				nd.CudaVersion.Major()*1000 + nd.CudaVersion.Minor()*10,
			),
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
