package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
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

type ResourceConfig struct {
	DataBytes    []byte
	ResourceData *vgpu.ResourceDataT
}

type ContainerKey string

func (key ContainerKey) SpiltUIDAndContainerName() (types.UID, string) {
	split := strings.Split(strings.TrimSpace(string(key)), "_")
	return types.UID(split[0]), split[1]
}

func GetContainerKey(uid types.UID, containerName string) ContainerKey {
	key := fmt.Sprintf("%s_%s", uid, containerName)
	return ContainerKey(key)
}

type ContainerLister struct {
	mutex      sync.RWMutex
	basePath   string
	nodeName   string
	podLister  listerv1.PodLister
	containers map[ContainerKey]*ResourceConfig
}

func (c *ContainerLister) addResourceConfig(key ContainerKey, config *ResourceConfig) {
	c.mutex.Lock()
	c.containers[key] = config
	c.mutex.Unlock()
}

func (c *ContainerLister) removeResourceConfig(key ContainerKey) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if config, ok := c.containers[key]; ok {
		_ = syscall.Munmap(config.DataBytes)
		delete(c.containers, key)
	}
}

func (c *ContainerLister) GetResourceData(key ContainerKey) (*vgpu.ResourceDataT, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if config, ok := c.containers[key]; !ok {
		return nil, false
	} else {
		return config.ResourceData.DeepCopy(), true
	}
}

var excludedFolders = map[string]bool{
	util.Checkpoints: true,
	util.Watcher:     true,
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
		_, existKey := c.GetResourceData(containerKey)
		switch {
		case matched && !existKey:
			configFile := filepath.Join(filePath, deviceplugin.VGPUConfigDirName, deviceplugin.VGPUConfigFileName)
			resourceData, data, err := vgpu.MmapResourceDataT(configFile)
			if err != nil && os.IsNotExist(err) {
				// TODO Retaining the old directory is to adapt to the old pods.
				configFile = filepath.Join(filePath, deviceplugin.VGPUConfigFileName)
				resourceData, data, err = vgpu.MmapResourceDataT(configFile)
			}
			if err != nil {
				klog.V(4).Infof("Failed to mmap resource file <%s>: %v", configFile, err)
				continue
			}
			klog.V(3).Infoln("Add vGPU config file:", configFile)
			c.addResourceConfig(containerKey, &ResourceConfig{
				DataBytes:    data,
				ResourceData: resourceData,
			})
		case !matched && fileInfo.ModTime().Add(time.Minute).Before(time.Now()):
			configFile := filepath.Join(filePath, deviceplugin.VGPUConfigFileName)
			klog.V(3).Infoln("Remove vGPU config file:", configFile)
			c.removeResourceConfig(containerKey)
			_ = os.RemoveAll(filePath)
		case !matched && strings.ToLower(os.Getenv("UNIT_TESTING")) == "true":
			c.removeResourceConfig(containerKey)
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
		basePath:   basePath,
		nodeName:   nodeName,
		podLister:  podLister,
		containers: make(map[ContainerKey]*ResourceConfig),
	}
}
