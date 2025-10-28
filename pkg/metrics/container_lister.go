package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vmem"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
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

func (key ContainerKey) SpiltUIDAndContainerName() (types.UID, string) {
	split := strings.Split(strings.TrimSpace(string(key)), "_")
	return types.UID(split[0]), split[1]
}

func (key ContainerKey) String() string {
	uid, contName := key.SpiltUIDAndContainerName()
	return fmt.Sprintf("%s/%s", uid, contName)
}

func GetContainerKey(uid types.UID, containerName string) ContainerKey {
	key := fmt.Sprintf("%s_%s", uid, containerName)
	return ContainerKey(key)
}

type ContainerLister struct {
	mutex          sync.RWMutex
	basePath       string
	nodeName       string
	podLister      listerv1.PodLister
	containerDatas map[ContainerKey]*vgpu.ResourceData
	containerVMems map[ContainerKey]*vmem.DeviceVMemory
}

func (c *ContainerLister) addResourceData(key ContainerKey, data *vgpu.ResourceData) {
	c.mutex.Lock()
	c.containerDatas[key] = data
	c.mutex.Unlock()
}

func (c *ContainerLister) removeContainer(key ContainerKey) {
	c.mutex.Lock()
	if data, ok := c.containerDatas[key]; ok {
		_ = data.Munmap()
		delete(c.containerDatas, key)
	}
	if node, ok := c.containerVMems[key]; ok {
		_ = node.Munmap()
		delete(c.containerVMems, key)
	}
	c.mutex.Unlock()
}

func (c *ContainerLister) addResourceVMem(key ContainerKey, data *vmem.DeviceVMemory) {
	c.mutex.Lock()
	c.containerVMems[key] = data
	c.mutex.Unlock()
}

func (c *ContainerLister) GetResourceVMem(key ContainerKey) (*vmem.DeviceVMemory, bool) {
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
		return data.GetCfg().DeepCopy(), true
	}
}

var excludedFolders = map[string]bool{
	util.Checkpoints: true,
	util.Watcher:     true,
	util.Registry:    true,
}

func (c *ContainerLister) collectContainerKey(pods []*corev1.Pod) sets.Set[ContainerKey] {
	setKeys := sets.New[ContainerKey]()
	for _, pod := range pods {
		// Filter scheduling node
		if pod.Spec.NodeName != c.nodeName {
			continue
		}
		for _, container := range pod.Spec.Containers {
			key := GetContainerKey(pod.UID, container.Name)
			setKeys.Insert(key)
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
		filePath := filepath.Join(c.basePath, entry.Name())
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			klog.Warningf("File path <%s> detection failed: %v", filePath, err)
			continue
		}
		containerKey := ContainerKey(entry.Name())
		matched := keySet.Has(containerKey)
		_, existCfg := c.GetResourceDataT(containerKey)
		_, existVMem := c.GetResourceVMem(containerKey)
		switch {
		case matched && !existCfg:
			configFile := filepath.Join(filePath, util.Config, deviceplugin.VGPUConfigFileName)
			resourceData, err := vgpu.NewResourceData(configFile)
			if err != nil && os.IsNotExist(err) {
				// TODO Retaining the old directory is to adapt to the old pods.
				configFile = filepath.Join(filePath, deviceplugin.VGPUConfigFileName)
				resourceData, err = vgpu.NewResourceData(configFile)
			}
			if err != nil {
				klog.V(4).ErrorS(err, "Failed to new device config", "filePath", configFile)
				continue
			}
			klog.V(3).Infoln("Add vGPU config file:", configFile)
			c.addResourceData(containerKey, resourceData)
		case matched && !existVMem:
			configFile := filepath.Join(filePath, util.VMemNode, util.VMemNodeFile)
			resourceVMem, err := vmem.NewDeviceVMemory(configFile)
			if err != nil && !os.IsNotExist(err) {
				klog.V(4).ErrorS(err, "Failed to new device vMemory", "filePath", configFile)
				continue
			}
			klog.V(3).Infoln("Add vGPU vMemory file:", configFile)
			c.addResourceVMem(containerKey, resourceVMem)
		case !matched && fileInfo.ModTime().Add(time.Minute).Before(time.Now()):
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
		containerDatas: make(map[ContainerKey]*vgpu.ResourceData),
		containerVMems: make(map[ContainerKey]*vmem.DeviceVMemory),
	}
}
