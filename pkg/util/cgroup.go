package util

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	cgroupsystemd "github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/util/qos"
)

var (
	currentCGroupDriver CGroupDriver
	initCGroupOnce      sync.Once
)

type CGroupDriver string

type kubeletConfig struct {
	CgroupDriver string `yaml:"cgroupDriver"`
}

type CgroupName []string

const (
	// systemdSuffix is the cgroup name suffix for systemd
	systemdSuffix string = ".slice"

	KubeletConfigPath = "/var/lib/kubelet/config.yaml"

	CGroupBasePath   = "/sys/fs/cgroup"
	CGroupDevicePath = CGroupBasePath + "/devices"

	SYSTEMD  CGroupDriver = "systemd"
	CGROUPFS CGroupDriver = "cgroupfs"
)

func InitializeCGroupDriver(cgroupDriver string) {
	initCGroupOnce.Do(func() {
		switch strings.ToLower(cgroupDriver) {
		case string(SYSTEMD):
			currentCGroupDriver = SYSTEMD
		case string(CGROUPFS):
			currentCGroupDriver = CGROUPFS
		default:
			configBytes, err := os.ReadFile(KubeletConfigPath)
			if err != nil {
				klog.Exitf("Read kubelet config file <%s> failed: %s", KubeletConfigPath, err.Error())
			}
			var kubelet kubeletConfig
			if err = yaml.Unmarshal(configBytes, &kubelet); err != nil {
				klog.Exitf("Failed to unmarshal kubelet config: %s", err.Error())
			}
			currentCGroupDriver = CGroupDriver(kubelet.CgroupDriver)
			if currentCGroupDriver != SYSTEMD && currentCGroupDriver != CGROUPFS {
				klog.Exitf("Invalid CGroup driver in kubelet config: %s", currentCGroupDriver)
			}
		}
	})
	klog.Infof("Current CGroup driver is %s", currentCGroupDriver)
}

func NewPodCgroupName(pod *corev1.Pod) CgroupName {
	podQos := pod.Status.QOSClass
	if len(podQos) == 0 {
		podQos = qos.GetPodQOS(pod)
	}
	var cgroupName CgroupName
	switch podQos {
	case corev1.PodQOSGuaranteed:
		cgroupName = append(cgroupName, "kubepods")
	case corev1.PodQOSBurstable:
		cgroupName = append(cgroupName, "kubepods", strings.ToLower(string(corev1.PodQOSBurstable)))
	case corev1.PodQOSBestEffort:
		cgroupName = append(cgroupName, "kubepods", strings.ToLower(string(corev1.PodQOSBestEffort)))
	}
	cgroupName = append(cgroupName, "pod"+string(pod.UID))
	return cgroupName
}

// cgroupName.ToSystemd converts the internal cgroup name to a systemd name.
// For example, the name {"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes
// "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
// This function always expands the systemd name into the cgroupfs form. If only
// the last part is needed, use path.Base(...) on it to discard the rest.
func (cgroupName CgroupName) ToSystemd() string {
	if len(cgroupName) == 0 || (len(cgroupName) == 1 && cgroupName[0] == "") {
		return "/"
	}
	newparts := []string{}
	for _, part := range cgroupName {
		part = escapeSystemdCgroupName(part)
		newparts = append(newparts, part)
	}
	result, err := cgroupsystemd.ExpandSlice(strings.Join(newparts, "-") + systemdSuffix)
	if err != nil {
		// Should never happen...
		panic(fmt.Errorf("error converting cgroup name [%v] to systemd format: %v", cgroupName, err))
	}
	return result
}

func escapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "-", "_", -1)
}

func (cgroupName CgroupName) ToCgroupfs() string {
	return "/" + path.Join(cgroupName...)
}

// GetK8sPodDeviceCGroupFullPath Obtain the full path of the cgroup device subsystem of the pod.
func GetK8sPodDeviceCGroupFullPath(podCGroupPath string) string {
	return filepath.Join(CGroupDevicePath, podCGroupPath)
}

// GetK8sPodCGroupFullPath Obtain the cgroupv2 full path of the pod.
func GetK8sPodCGroupFullPath(podCGroupPath string) string {
	return filepath.Join(CGroupBasePath, podCGroupPath)
}

// GetK8sPodCGroupPath Obtain the relative path of pod cgroup for k8s.
func GetK8sPodCGroupPath(pod *corev1.Pod) (string, error) {
	cgroupName := NewPodCgroupName(pod)
	switch currentCGroupDriver {
	case SYSTEMD:
		return cgroupName.ToSystemd(), nil
	case CGROUPFS:
		return cgroupName.ToCgroupfs(), nil
	default:
		return "", fmt.Errorf("unknown CGroup driver: %s", currentCGroupDriver)
	}
}

func GetK8sPodContainerCGroupFullPath(pod *corev1.Pod, containerName string,
	getFullPath func(string) string) (string, error) {
	var (
		runtimeName string
		containerId string
	)
	containerStatus, ok := GetContainerStatus(pod, containerName)
	if !ok {
		return "", fmt.Errorf("failed to obtain container cgroup path")
	}
	runtimeName, containerId = ParseContainerRuntime(containerStatus.ContainerID)
	cgroupName := NewPodCgroupName(pod)
	switch currentCGroupDriver {
	case SYSTEMD:
		return convertSystemdFullPath(runtimeName, containerId, cgroupName, getFullPath)
	case CGROUPFS:
		return convertCGroupfsFullPath(runtimeName, containerId, cgroupName, getFullPath)
	default:
		return "", fmt.Errorf("unknown CGroup driver: %s", currentCGroupDriver)
	}
}

func PathIsNotExist(fullPath string) bool {
	_, err := os.Stat(fullPath)
	return os.IsNotExist(err)
}

func convertCGroupfsFullPath(runtimeName, containerId string,
	cgroupName CgroupName, getFullPath func(string) string) (string, error) {
	fullPath := getFullPath(filepath.Join(cgroupName.ToCgroupfs(), containerId))
	if !PathIsNotExist(fullPath) {
		return fullPath, nil
	}
	fullPath = getFullPath(filepath.Join("system.slice", cgroupName[len(cgroupName)-1]))
	if !PathIsNotExist(fullPath) {
		return fullPath, nil
	}
	klog.Infof("Possible upgrade required to adapt container runtime <%s> CGroup driver <%s>",
		runtimeName, "cgroupfs")
	return "", fmt.Errorf("container CGroup full path <%s> not exist", fullPath)
}

func convertSystemdFullPath(runtimeName, containerId string,
	cgroupName CgroupName, getFullPath func(string) string) (string, error) {
	var toSystemd = func(cgroupName CgroupName) string {
		if len(cgroupName) == 0 || (len(cgroupName) == 1 && cgroupName[0] == "") {
			return "/"
		}
		var newparts []string
		for _, part := range cgroupName {
			part = strings.Replace(part, "-", "_", -1)
			newparts = append(newparts, part)
		}
		return strings.Join(newparts, "-") + ".slice"
	}
	cgroupPath := fmt.Sprintf("%s/%s-%s.scope", cgroupName.ToSystemd(),
		SystemdPathPrefixOfRuntime(runtimeName), containerId)
	fullPath := getFullPath(cgroupPath)
	if !PathIsNotExist(fullPath) {
		return fullPath, nil
	}
	switch runtimeName {
	case "containerd":
		klog.Warningf("CGroup full path <%s> not exist", fullPath)
		cgroupPath = fmt.Sprintf("system.slice/%s.service/%s:%s:%s", runtimeName,
			toSystemd(cgroupName), SystemdPathPrefixOfRuntime(runtimeName), containerId)
		fullPath = getFullPath(cgroupPath)
		if !PathIsNotExist(fullPath) {
			return fullPath, nil
		}
	case "docker":
		klog.Warningf("CGroup full path <%s> not exist", fullPath)
		cgroupPath = fmt.Sprintf("%s/%s", cgroupName.ToSystemd(), containerId)
		fullPath = getFullPath(cgroupPath)
		if !PathIsNotExist(fullPath) {
			return fullPath, nil
		}
	default:
	}
	klog.Infof("Possible upgrade required to adapt container runtime <%s> CGroup driver <%s>",
		runtimeName, "systemd")
	return "", fmt.Errorf("container CGroup full path <%s> not exist", fullPath)
}

func GetContainerStatus(pod *corev1.Pod, containerName string) (*corev1.ContainerStatus, bool) {
	for i, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return &pod.Status.ContainerStatuses[i], true
		}
	}
	return nil, false
}

func GetContainerRuntime(pod *corev1.Pod, containerName string) (runtimeName string, containerId string) {
	if status, ok := GetContainerStatus(pod, containerName); ok {
		runtimeName, containerId = ParseContainerRuntime(status.ContainerID)
	}
	return
}

func ParseContainerRuntime(podContainerId string) (runtimeName string, containerId string) {
	if splits := strings.Split(podContainerId, "://"); len(splits) == 2 {
		runtimeName = splits[0]
		containerId = splits[1]
	}
	return
}

func SystemdPathPrefixOfRuntime(runtimeName string) string {
	switch runtimeName {
	case "cri-o":
		return "crio"
	case "containerd":
		return "cri-containerd"
	default:
		if runtimeName != "docker" {
			klog.Warningf("prefix of container runtime %s was not tested. Maybe not correct!", runtimeName)
		}
		return runtimeName
	}
}

func SplitK8sCGroupBasePath(cgroupFullPath string) string {
	basePath := cgroupFullPath
	for {
		split, _ := filepath.Split(basePath)
		split = filepath.Clean(split)
		if strings.Contains(split, "kubepods") {
			basePath = split
			continue
		}
		break
	}
	return basePath
}
