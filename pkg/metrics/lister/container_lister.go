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
	mutex          sync.RWMutex
	basePath       string
	nodeName       string
	podLister      listerv1.PodLister
	containerDatas map[ContainerKey]*vgpu.MmapResourceData
	containerVMems map[ContainerKey]*vmem.MmapDeviceVMemory
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

func (c *ContainerLister) GetResourceDataT(key ContainerKey) (*vgpu.ResourceDataT, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if data, ok := c.containerDatas[key]; !ok {
		return nil, false
	} else {
		return data.CopyResource(), true
	}
}

func (c *ContainerLister) getResourceData(key ContainerKey) (*vgpu.MmapResourceData, bool) {
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
	entries, err := os.ReadDir(c.basePath)
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
		filePath := filepath.Join(c.basePath, entry.Name())
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			klog.Warningf("File path <%s> detection failed: %v", filePath, err)
			continue
		}
		matched := keySet.Has(containerKey)
		resourceData, existCfg := c.getResourceData(containerKey)
		resourceVMem, existVMem := c.GetResourceVMem(containerKey)
		switch {
		case matched:
			if !existCfg {
				configFile := filepath.Join(filePath, util.Config, dpvgpu.VGPUConfigFileName)
				resourceData, err = vgpu.NewMmapResourceData(configFile)
				if err != nil && !os.IsNotExist(err) {
					klog.V(4).ErrorS(err, "Failed to new device config", "filePath", configFile)
				}
				if err == nil && resourceData != nil {
					klog.V(3).InfoS("Add vGPU config file", "filePath", configFile)
					c.addResourceData(containerKey, resourceData)
				}
			} else {
				reload, err := resourceData.NeedsReload()
				if err != nil {
					if os.IsNotExist(err) {
						klog.V(3).InfoS("Detected that the Resource file has been deleted", "containerKey", containerKey.String())
						c.mutex.Lock()
						c.removeResourceData(containerKey)
						c.mutex.Unlock()
					} else {
						klog.V(2).ErrorS(err, "Resource file NeedsReload failed", "containerKey", containerKey.String())
					}
				}
				if reload {
					klog.V(3).InfoS("Detected that Resource file has been changed", "containerKey", containerKey.String())
					if err = resourceData.Reload(); err != nil {
						klog.V(1).ErrorS(err, "", "containerKey", containerKey.String())
					}
				}
			}
			if !existVMem {
				configFile := filepath.Join(filePath, util.VMemNode, util.VMemNodeFile)
				resourceVMem, err = vmem.NewMmapDeviceVMemory(configFile)
				if err != nil && !os.IsNotExist(err) {
					klog.V(4).ErrorS(err, "Failed to new device vMemory", "filePath", configFile)
				}
				if err == nil && resourceVMem != nil {
					klog.V(3).InfoS("Add vGPU vMemory file", "filePath", configFile)
					c.addResourceVMem(containerKey, resourceVMem)
				}
			} else {
				reload, err := resourceVMem.NeedsReload()
				if err != nil {
					if os.IsNotExist(err) {
						klog.V(3).InfoS("Detected that the vMemory file has been deleted", "containerKey", containerKey.String())
						c.mutex.Lock()
						c.removeResourceVMem(containerKey)
						c.mutex.Unlock()
					} else {
						klog.V(2).ErrorS(err, "vMemory file NeedsReload failed", "containerKey", containerKey.String())
					}
				}
				if reload {
					klog.V(3).InfoS("Detected that vMemory file has been changed", "containerKey", containerKey.String())
					if err = resourceVMem.Reload(); err != nil {
						klog.V(1).ErrorS(err, "", "containerKey", containerKey.String())
					}
				}
			}
		case !matched && fileInfo.ModTime().Add(2*time.Minute).Before(time.Now()):
			klog.V(3).Infoln("Remove vGPU container:", containerKey.String())
			c.removeContainer(containerKey)
			_ = os.RemoveAll(filePath)
		case !matched && strings.ToLower(os.Getenv("UNIT_TESTING")) == "true":
			c.removeContainer(containerKey)
			_ = os.RemoveAll(filePath)
		}
	}
	return nil
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

func NewContainerLister(basePath, nodeName string, podLister listerv1.PodLister) *ContainerLister {
	return &ContainerLister{
		basePath:       basePath,
		nodeName:       nodeName,
		podLister:      podLister,
		containerDatas: make(map[ContainerKey]*vgpu.MmapResourceData),
		containerVMems: make(map[ContainerKey]*vmem.MmapDeviceVMemory),
	}
}
