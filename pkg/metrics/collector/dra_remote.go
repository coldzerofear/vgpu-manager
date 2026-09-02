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

package collector

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Remote GPU support for the DRA collector (design v2.2 §11.4).
//
// Local semantics are untouched: a pod scheduled on this node whose devices
// are accessMode=local is accounted exactly as before (cgroup PIDs joined
// with NVML). Two things change for accessMode=remote devices:
//
//  1. Consumers may live on other nodes. They are discovered claim-first:
//     every claim allocating one of this node's remote devices names its
//     consumers in status.reservedFor. Those pods are fetched on demand
//     (small TTL cache) and run through the same accounting closure.
//  2. Their GPU processes are lupine-server children on this node; the
//     consumer's cgroup is useless. The PIDs come from the session's
//     pids.config (SESSION-mode accounting list) under the session base the
//     remote-agent shares with lupine-server. The session of a container is
//     found the way inject mode named it: partition key (pkg/claimresolve)
//     -> claim annotation -> token -> <base>/<token>/pids.config.

const (
	remotePodCacheTTL = time.Minute
	sessionPidsFile   = registry.PidsConfig
)

// podCache answers pod lookups for consumers that are not on this node. The
// node-scoped lister is consulted first; misses go to the API with a TTL.
type podCache struct {
	mu      sync.Mutex
	client  kubernetes.Interface
	entries map[string]podCacheEntry // key: namespace/name
}

type podCacheEntry struct {
	pod     *corev1.Pod // nil = remembered NotFound
	fetched time.Time
}

func newPodCache(client kubernetes.Interface) *podCache {
	return &podCache{client: client, entries: map[string]podCacheEntry{}}
}

func (p *podCache) get(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := namespace + "/" + name
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && time.Since(e.fetched) < remotePodCacheTTL {
		p.mu.Unlock()
		if e.pod == nil {
			return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
		}
		return e.pod, nil
	}
	p.mu.Unlock()

	pod, err := p.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	p.mu.Lock()
	p.entries[key] = podCacheEntry{pod: pod, fetched: time.Now()}
	// Opportunistic sweep so the map cannot grow without bound.
	if len(p.entries) > 4096 {
		for k, e := range p.entries {
			if time.Since(e.fetched) >= remotePodCacheTTL {
				delete(p.entries, k)
			}
		}
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// getPod prefers the node-local lister (no API call for pods on this node).
func (c draGPUCollector) getPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if pod, err := c.podLister.Pods(namespace).Get(name); err == nil && pod != nil {
		return pod, nil
	}
	if c.podCache == nil {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"),
			fmt.Sprintf("%s/%s", namespace, name))
	}
	return c.podCache.get(ctx, namespace, name)
}

// claimReader adapts the collector to claimresolve.Reader.
type claimReader struct{ c draGPUCollector }

func (r claimReader) GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	pod, err := r.c.getPod(ctx, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	pod.DeepCopyInto(obj)
	return nil
}

func (r claimReader) GetResourceClaim(_ context.Context, key client.ObjectKey, obj *v1.ResourceClaim) error {
	claim, err := r.c.claimLister.ResourceClaims(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}
	claim.DeepCopyInto(obj)
	return nil
}

func (c draGPUCollector) remoteEnabled() bool {
	return c.featureGate != nil && c.featureGate.Enabled(util.RemoteGPUSupport) && c.sessionBase != ""
}

// remoteConsumerPods returns, keyed by UID, the pods on OTHER nodes that hold
// an allocation of one of this node's accessMode=remote devices. Pods on
// this node are left to the node-scoped lister (they take the session PID
// path through alloc.remote() regardless).
func (c draGPUCollector) remoteConsumerPods(devInfoNameMap map[string]*DRADeviceInfo) map[types.UID]*corev1.Pod {
	out := map[types.UID]*corev1.Pod{}
	if !c.remoteEnabled() {
		return out
	}
	claims, err := c.claimLister.List(labels.Everything())
	if err != nil {
		klog.V(4).ErrorS(err, "list resourceClaims for remote consumers failed")
		return out
	}
	ctx := context.Background()
	for _, claim := range claims {
		if !claimTouchesRemoteDevice(claim, c.nodeName, devInfoNameMap) {
			continue
		}
		for _, ref := range claim.Status.ReservedFor {
			if ref.APIGroup != "" || ref.Resource != "pods" {
				continue
			}
			pod, err := c.getPod(ctx, claim.Namespace, ref.Name)
			if err != nil || pod == nil || (ref.UID != "" && pod.UID != ref.UID) {
				continue
			}
			if pod.Spec.NodeName == c.nodeName || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			out[pod.UID] = pod
		}
	}
	return out
}

func claimTouchesRemoteDevice(claim *v1.ResourceClaim, nodeName string, devInfoNameMap map[string]*DRADeviceInfo) bool {
	if claim.Status.Allocation != nil {
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver != util.DRADriverName || result.Pool != nodeName {
				continue
			}
			if info, ok := devInfoNameMap[result.Device]; ok && info.accessMode == util.AccessModeRemote {
				return true
			}
		}
	}
	return false
}

// remote reports whether any of the container's results is a remote device
// (a node's devices are all one mode, so this is effectively "is this node
// publish-only").
func (a draContainerAlloc) remote() bool {
	for _, r := range a.results {
		if r.devInfo != nil && r.devInfo.accessMode == util.AccessModeRemote {
			return true
		}
	}
	return false
}

// remoteSessionPIDs returns the NVML-visible PIDs of the container's remote
// sessions: for each claim request the container references, the partition
// the request belongs to, its token from the claim annotation, and the
// session's pids.config. partitionCache is per-scrape (claim UID -> info).
func (c draGPUCollector) remoteSessionPIDs(alloc draContainerAlloc, partitionCache map[types.UID]*claimresolve.PartitionInfo) ([]uint32, []string) {
	ctx := context.Background()
	pidSet := sets.New[uint32]()
	vmemNodeDirSet := sets.New[string]()
	reader := claimReader{c: c}
	for _, ref := range alloc.claims {
		if ref.claim == nil {
			continue
		}
		// Per claim: tokens live in THIS claim's annotations; carrying keys
		// over from another claim would look them up in the wrong object.
		partitionSet := sets.New[string]()
		info, ok := partitionCache[ref.claim.UID]
		if !ok {
			allocated := sets.New[string]()
			for _, result := range ref.claim.Status.Allocation.Devices.Results {
				if result.Driver != util.DRADriverName {
					continue
				}
				if main := remote.MainRequestName(ref.claim, result.Request); main != "" {
					allocated.Insert(main)
				}
			}
			resolved, err := claimresolve.ResolveClaimVGPUPartitionsFromAllocatedRequests(ctx, reader, ref.claim, allocated)
			if err != nil {
				klog.V(4).ErrorS(err, "resolve partitions failed", "resourceClaim", klog.KObj(ref.claim))
			}
			info = resolved
			partitionCache[ref.claim.UID] = info
		}
		// NRI mode (per-container session) keys the token by
		// <podUID>_<containerName>; partition mode by the resolver key.
		nriPartitionKey := remote.NRIPartitionKey(string(alloc.podUID), alloc.name)
		partitionSet.Insert(nriPartitionKey)

		for _, mainRequest := range ref.requests {
			var partitionKey string
			if info != nil {
				partitionKey = info.RequestToPartition[mainRequest]
			}
			if partitionKey == "" {
				partitionKey = remote.PartitionFallbackKey(mainRequest)
			}
			partitionSet.Insert(partitionKey)
		}

		for partitionKey := range partitionSet {
			token := ref.claim.Annotations[remote.SessionAnnotationKey(partitionKey)]
			if token == "" {
				if partitionKey != nriPartitionKey {
					klog.V(5).InfoS("no session token recorded for partition",
						"resourceClaim", klog.KObj(ref.claim), "partition", partitionKey)
				}
				continue
			}
			if pids, err := GetPidsByFilepath(filepath.Join(c.sessionBase, token, sessionPidsFile)); err != nil {
				klog.V(2).ErrorS(err, "GetPidsByFilepath failed", "partitionKey", partitionKey, "token", token)
			} else if len(pids) > 0 {
				pidSet.Insert(pids...)
			}

			vmemNodeDirSet.Insert(filepath.Join(c.sessionBase, token, "."+util.VMemNode))
		}
	}
	return sets.List(pidSet), vmemNodeDirSet.UnsortedList()
}

const (
	pidsConfigLockYields  = 4
	pidsConfigLockSleeps  = 5
	pidsConfigLockBackoff = 200 * time.Microsecond
)

// lockPidsConfigShared takes a shared flock on pids.config without blocking
// (LOCK_SH|LOCK_NB, a few yields then short sleeps), mirroring the library's
// writer-side locking. Only EWOULDBLOCK (a writer holds the lock) is retried.
func lockPidsConfigShared(f *os.File) error {
	fd := int(f.Fd())
	backoff := pidsConfigLockBackoff
	attempts := pidsConfigLockYields + pidsConfigLockSleeps

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return nil
		}

		if errors.Is(err, syscall.EINTR) {
			lastErr = err
			continue
		}

		// Only EWOULDBLOCK (==EAGAIN) indicates that a writer holds an exclusive lock, which is worth trying again
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		lastErr = err

		if attempt < pidsConfigLockYields {
			runtime.Gosched()
		} else {
			time.Sleep(backoff)
		}
	}
	return lastErr
}

// GetPidsByFilepath Read the PID list from the specified file.
func GetPidsByFilepath(filePath string) ([]uint32, error) {
	if filePath == "" {
		return nil, fmt.Errorf("invalid parameter: filePath=%q", filePath)
	}

	f, err := os.OpenFile(filePath, os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("error opening %s: %w", filePath, err)
	}
	defer f.Close()

	// Lock failure is not fatal: a torn read at worst misses/duplicates a
	// line, and the parser tolerates both.
	locked := true
	if err = lockPidsConfigShared(f); err != nil {
		locked = false
		klog.V(4).InfoS("reading pids.config without a shared lock", "filePath", filePath, "err", err)
	}

	pids := readPidsConfig(f)

	if locked {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	return pids, nil
}

// readPidsConfig parses the library's pids.config: one decimal host PID per
// line (see pkg/device/registry persistPids), deduplicated.
func readPidsConfig(f *os.File) []uint32 {
	pidSet := sets.New[uint32]()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		pid, err := strconv.ParseUint(line, 10, 32)
		if err != nil {
			continue
		}
		pidSet.Insert(uint32(pid))
	}
	return pidSet.UnsortedList()
}
