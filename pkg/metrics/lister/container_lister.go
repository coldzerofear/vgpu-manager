/*
Copyright 2024-2026 coldzerofear

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

package lister

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vmem"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	dpvgpu "github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type ContainerKey string

func NewContainerKey(key string) (ContainerKey, error) {
	split := strings.SplitN(key, "_", 2)
	switch len(split) {
	case 2:
		return ContainerKey(key), nil
	default:
		return "", fmt.Errorf("key format error: %s", key)
	}
}

func (key ContainerKey) String() string {
	return strings.Replace(string(key), "_", "/", 1)
}

func GetContainerKey(uid types.UID, containerName string) ContainerKey {
	key := fmt.Sprintf("%s_%s", uid, containerName)
	contKey, _ := NewContainerKey(key)
	return contKey
}

type ContainerLister struct {
	mutex       sync.RWMutex
	nodeName    string
	managerRoot string
	// sessionBase is the remote GPU session root on this node; empty when
	// the node serves no remote pods. The sessions of the remote pods this
	// node's GPUs serve hold the same two regions as a local container
	// directory, so they are tracked under the same keys (see
	// updateRemoteSessions).
	sessionBase    string
	podLister      listerv1.PodLister
	containerDatas map[ContainerKey]*vgpu.MmapResourceData
	containerVMems map[ContainerKey]*vmem.MmapDeviceVMemory
	// sessionKeys are the keys currently backed by a session directory, which
	// this lister reads but never removes -- the agent owns them.
	sessionKeys sets.Set[ContainerKey]
}

// removeResourceData and removeResourceVMem mutate the underlying maps and must
// be called with c.mutex held for writing.
func (c *ContainerLister) removeResourceData(key ContainerKey) {
	if d, ok := c.containerDatas[key]; ok {
		_ = d.Close()
		delete(c.containerDatas, key)
	}
}

func (c *ContainerLister) removeResourceVMem(key ContainerKey) {
	if n, ok := c.containerVMems[key]; ok {
		_ = n.Close()
		delete(c.containerVMems, key)
	}
}

func (c *ContainerLister) addResourceData(key ContainerKey, data *vgpu.MmapResourceData) {
	c.mutex.Lock()
	c.removeResourceData(key)
	c.containerDatas[key] = data
	c.mutex.Unlock()
}

func (c *ContainerLister) removeContainer(key ContainerKey) {
	c.mutex.Lock()
	c.removeResourceData(key)
	c.removeResourceVMem(key)
	c.mutex.Unlock()
}

func (c *ContainerLister) addResourceVMem(key ContainerKey, data *vmem.MmapDeviceVMemory) {
	c.mutex.Lock()
	c.removeResourceVMem(key)
	c.containerVMems[key] = data
	c.mutex.Unlock()
}

func (c *ContainerLister) GetResourceVMem(key ContainerKey) (*vmem.MmapDeviceVMemory, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	data, ok := c.containerVMems[key]
	return data, ok
}

func (c *ContainerLister) GetResourceData(key ContainerKey) (*vgpu.MmapResourceData, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	data, ok := c.containerDatas[key]
	return data, ok
}

var excludedFolders = map[string]bool{
	util.Checkpoints: true,
	util.Watcher:     true,
	util.Registry:    true,
	util.Claims:      true,
	util.Tools:       true,
	util.Driver:      true,
}

func (c *ContainerLister) collectContainerKey(pods []*corev1.Pod) sets.Set[ContainerKey] {
	setKeys := sets.New[ContainerKey]()
	for _, pod := range pods {
		// Filter scheduling node
		if pod.Spec.NodeName != c.nodeName {
			continue
		}
		// Include regular containers, sidecars, and currently-running
		// sequential init containers. A completed sequential init container is
		// intentionally excluded so its directory becomes an orphan and is
		// reclaimed (and its stale usage stops being reported).
		for _, name := range util.CollectableContainerNames(pod) {
			setKeys.Insert(GetContainerKey(pod.UID, name))
		}
	}
	return setKeys
}

func (c *ContainerLister) update() error {
	entries, err := os.ReadDir(c.managerRoot)
	if err != nil {
		return err
	}
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return err
	}
	keySet := c.collectContainerKey(pods)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Exclude some folders to prevent accidental deletion.
		if excludedFolders[entry.Name()] {
			continue
		}
		containerKey, err := NewContainerKey(entry.Name())
		if err != nil {
			continue
		}
		filePath := filepath.Join(c.managerRoot, entry.Name())
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			klog.Warningf("File path <%s> detection failed: %v", filePath, err)
			continue
		}
		matched := keySet.Has(containerKey)
		switch {
		case matched:
			c.syncResourceData(containerKey, filepath.Join(filePath, util.Config, dpvgpu.VGPUConfigFileName))
			c.syncResourceVMem(containerKey, filepath.Join(filePath, util.VMemNode, util.VMemNodeFile))
		case !matched && fileInfo.ModTime().Add(2*time.Minute).Before(time.Now()):
			klog.V(3).Infoln("Remove vGPU container:", containerKey.String())
			c.removeContainer(containerKey)
			_ = os.RemoveAll(filePath)
		case !matched && strings.ToLower(os.Getenv("UNIT_TESTING")) == "true":
			c.removeContainer(containerKey)
			_ = os.RemoveAll(filePath)
		}
	}
	c.updateRemoteSessions(pods)
	return nil
}

// syncResourceData maps the container's quota region, or reloads the mapping
// already held; a region that is gone drops it.
func (c *ContainerLister) syncResourceData(key ContainerKey, configFile string) {
	resourceData, exist := c.GetResourceData(key)
	if !exist {
		data, err := vgpu.NewMmapResourceData(configFile)
		if err != nil {
			if !os.IsNotExist(err) {
				klog.V(4).ErrorS(err, "Failed to new device config", "filePath", configFile)
			}
			return
		}
		klog.V(3).InfoS("Add vGPU config file", "filePath", configFile)
		c.addResourceData(key, data)
		return
	}
	reload, err := resourceData.NeedsReload()
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(3).InfoS("Detected that the Resource file has been deleted", "containerKey", key.String())
			c.mutex.Lock()
			c.removeResourceData(key)
			c.mutex.Unlock()
		} else {
			klog.V(2).ErrorS(err, "Resource file NeedsReload failed", "containerKey", key.String())
		}
	}
	if reload {
		klog.V(3).InfoS("Detected that Resource file has been changed", "containerKey", key.String())
		if err = resourceData.Reload(); err != nil {
			klog.V(1).ErrorS(err, "", "containerKey", key.String())
		}
	}
}

// syncResourceVMem does the same for the virtual-memory region.
func (c *ContainerLister) syncResourceVMem(key ContainerKey, configFile string) {
	resourceVMem, exist := c.GetResourceVMem(key)
	if !exist {
		data, err := vmem.NewMmapDeviceVMemory(configFile)
		if err != nil {
			if !os.IsNotExist(err) {
				klog.V(4).ErrorS(err, "Failed to new device vMemory", "filePath", configFile)
			}
			return
		}
		klog.V(3).InfoS("Add vGPU vMemory file", "filePath", configFile)
		c.addResourceVMem(key, data)
		return
	}
	reload, err := resourceVMem.NeedsReload()
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(3).InfoS("Detected that the vMemory file has been deleted", "containerKey", key.String())
			c.mutex.Lock()
			c.removeResourceVMem(key)
			c.mutex.Unlock()
		} else {
			klog.V(2).ErrorS(err, "vMemory file NeedsReload failed", "containerKey", key.String())
		}
	}
	if reload {
		klog.V(3).InfoS("Detected that vMemory file has been changed", "containerKey", key.String())
		if err = resourceVMem.Reload(); err != nil {
			klog.V(1).ErrorS(err, "", "containerKey", key.String())
		}
	}
}

// updateRemoteSessions tracks the sessions of the remote pods this node's GPUs
// serve. Those pods run on other nodes, so they have no container directory
// here; their quota and virtual-memory regions live in the session directory
// instead, under a token derived from the pod and the container. Nothing is
// deleted here: the sessions belong to the agent, which sweeps them by pod.
func (c *ContainerLister) updateRemoteSessions(pods []*corev1.Pod) {
	if c.sessionBase == "" {
		return
	}
	live := sets.New[ContainerKey]()
	for _, pod := range pods {
		if mode, _ := util.PodVGPUAccessMode(pod); mode != util.AccessModeRemote {
			continue
		}
		if util.PodPlanSchedulingNode(pod) != c.nodeName {
			continue
		}
		for _, name := range util.CollectableContainerNames(pod) {
			key := GetContainerKey(pod.UID, name)
			token := remotegpu.SessionToken(string(pod.UID), name)
			live.Insert(key)
			c.syncResourceData(key, remotegpu.SessionQuotaFile(c.sessionBase, token))
			c.syncResourceVMem(key, remotegpu.SessionVMemFile(c.sessionBase, token))
		}
	}
	// Drop the mappings of sessions that are no longer served here.
	for key := range c.sessionKeys.Difference(live) {
		c.removeContainer(key)
	}
	c.sessionKeys = live
}

func (c *ContainerLister) Start(interval time.Duration, stopChan <-chan struct{}) {
	go func() {
		klog.InfoS("Container lister start", "interval", interval.String())
		scanResourceFiles := func() {
			if err := c.update(); err != nil {
				klog.V(1).ErrorS(err, "Failed to update container lister")
			}
		}
		wait.Until(scanResourceFiles, interval, stopChan)
		klog.Infof("Container lister Stopped.")
	}()
}

// NewContainerLister reads the resource regions of this node's containers.
// sessionBase is the remote GPU session root, empty on a node that serves no
// remote pods.
func NewContainerLister(nodeName, managerRoot, sessionBase string, podLister listerv1.PodLister) *ContainerLister {
	return &ContainerLister{
		nodeName:       nodeName,
		podLister:      podLister,
		managerRoot:    managerRoot,
		sessionBase:    sessionBase,
		containerDatas: make(map[ContainerKey]*vgpu.MmapResourceData),
		containerVMems: make(map[ContainerKey]*vmem.MmapDeviceVMemory),
		sessionKeys:    sets.New[ContainerKey](),
	}
}
