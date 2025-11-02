package cgroup

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	criapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/utils/ptr"

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
	KubeadmFlagsPath  = "/var/lib/kubelet/kubeadm-flags.env"

	CGroupBasePath   = "/sys/fs/cgroup"
	CGroupDevicePath = CGroupBasePath + "/devices"

	SYSTEMD  CGroupDriver = "systemd"
	CGROUPFS CGroupDriver = "cgroupfs"
)

func MustInitCGroupDriver(cgroupDriver string) {
	initCGroupOnce.Do(func() {
		switch strings.ToLower(cgroupDriver) {
		case string(SYSTEMD):
			currentCGroupDriver = SYSTEMD
		case string(CGROUPFS):
			currentCGroupDriver = CGROUPFS
		default:
			getCgroupDriverFuncs := []func() (CGroupDriver, error){
				readKubeletConfigCgroupDriver,
				readKubeadmFlagsCgroupDriver,
				getCgroupDriverFromKubeletProcess,
				detectCgroupDriver,
			}
			var err error
			for _, getCgroupDriver := range getCgroupDriverFuncs {
				currentCGroupDriver, err = getCgroupDriver()
				if err != nil {
					klog.V(4).ErrorS(err, "Attempt to extract cgroup driver failed")
				} else {
					break
				}
			}
			if err != nil {
				klog.Exitln("Unable to detect a valid cgroup driver")
			}
		}
	})
	klog.Infof("Current environment cgroup driver is '%s'", currentCGroupDriver)
}

// readKubeletConfigCgroupDriver Extract cgroup driver from kubelet configuration file
func readKubeletConfigCgroupDriver() (CGroupDriver, error) {
	configBytes, err := os.ReadFile(KubeletConfigPath)
	if err != nil {
		return "", fmt.Errorf("read kubelet-config file <%s> failed: %v", KubeletConfigPath, err)
	}
	var kubelet kubeletConfig
	if err = yaml.Unmarshal(configBytes, &kubelet); err != nil {
		return "", fmt.Errorf("failed to unmarshal kubelet-config: %v", err)
	}
	switch kubelet.CgroupDriver {
	case string(SYSTEMD):
		return SYSTEMD, nil
	case string(CGROUPFS):
		return CGROUPFS, nil
	case "":
		return "", fmt.Errorf("cgroup driver not found in kubelet-config <%s>", KubeletConfigPath)
	default:
		return "", fmt.Errorf("invalid cgroup driver in kubelet-config: %s", kubelet.CgroupDriver)
	}
}

// readKubeadmFlagsCgroupDriver Extract cgroup driver from kubeadm-flags.env file
func readKubeadmFlagsCgroupDriver() (CGroupDriver, error) {
	configBytes, err := os.ReadFile(KubeadmFlagsPath)
	if err != nil {
		return "", fmt.Errorf("read kubeadm-flags file <%s> failed: %v", KubeadmFlagsPath, err)
	}
	content := string(configBytes)

	// Look for --cgroup-driver in command line arguments
	if !strings.Contains(content, "--cgroup-driver") {
		return "", fmt.Errorf("cgroup driver not found in kubelet-flags <%s>", KubeadmFlagsPath)
	}
	driver := ""
	lines := strings.Split(content, "\n")
out:
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "--cgroup-driver") {
			continue
		}
		parts := strings.Fields(line)
		for i, part := range parts {
			if part == "--cgroup-driver" && i+1 < len(parts) {
				driver = parts[i+1]
				if driver == string(SYSTEMD) || driver == string(CGROUPFS) {
					break out
				}
			}
		}
	}
	switch driver {
	case string(SYSTEMD):
		return SYSTEMD, nil
	case string(CGROUPFS):
		return CGROUPFS, nil
	default:
		return "", fmt.Errorf("invalid cgroup driver in kubelet-flags: %s", driver)
	}
}

// getCgroupDriverFromKubeletProcess gets cgroup driver by finding kubelet process and parsing its command line arguments
// This function is separated for easier unit testing
func getCgroupDriverFromKubeletProcess() (CGroupDriver, error) {
	// Find kubelet process
	kubeletPID, err := findKubeletProcess()
	if err != nil {
		//klog.V(4).Infof("Failed to find kubelet process: %v", err)
		return "", err
	}

	// Read command line arguments from /proc/<pid>/cmdline
	cmdline, err := readProcessCmdline(kubeletPID)
	if err != nil {
		//klog.V(4).Infof("Failed to read kubelet cmdline: %v", err)
		return "", err
	}

	// Parse cgroup driver from command line arguments
	return parseCgroupDriverFromCmdline(cmdline)
}

// parseCgroupDriverFromCmdline parses cgroup driver from command line arguments
func parseCgroupDriverFromCmdline(args []string) (CGroupDriver, error) {
	// First, try to find --cgroup-driver parameter
	for i, arg := range args {
		// Handle both formats: "--cgroup-driver=value" and "--cgroup-driver value"
		var driver string
		if strings.HasPrefix(arg, "--cgroup-driver=") {
			driver = strings.TrimPrefix(arg, "--cgroup-driver=")

		} else if arg == "--cgroup-driver" && i+1 < len(args) {
			driver = args[i+1]
		}
		if driver == string(SYSTEMD) {
			return SYSTEMD, nil
		}
		if driver == string(CGROUPFS) {
			return CGROUPFS, nil
		}
	}
	return "", fmt.Errorf("failed to parse cgroup driver from args: %v", args)
}

// findKubeletProcess finds the kubelet process PID by reading /proc filesystem
func findKubeletProcess() (int, error) {
	procDir, err := os.Open("/proc")
	if err != nil {
		return 0, fmt.Errorf("failed to open /proc: %v", err)
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(0)
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc directory: %v", err)
	}

	for _, entry := range entries {
		// Check if entry is a numeric PID
		pid, err := strconv.Atoi(entry)
		if err != nil {
			continue // Skip non-numeric entries
		}

		// Read process comm file to get process name
		commPath := filepath.Join("/proc", entry, "comm")
		commData, err := os.ReadFile(commPath)
		if err != nil {
			continue // Skip if we can't read comm file
		}

		// Check if this is kubelet process
		comm := strings.TrimSpace(string(commData))
		if comm == "kubelet" {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("kubelet process not found")
}

// readProcessCmdline reads the command line arguments from /proc/<pid>/cmdline
func readProcessCmdline(pid int) ([]string, error) {
	cmdlinePath := filepath.Join("/proc", strconv.Itoa(pid), "cmdline")
	cmdlineData, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cmdline file: %v", err)
	}

	// Split by null bytes and filter out empty strings
	var args []string
	for _, arg := range strings.Split(string(cmdlineData), "\x00") {
		if arg != "" {
			args = append(args, arg)
		}
	}

	return args, nil
}

// detectCgroupDriver detects the cgroup driver (cgroupfs or systemd) on the system
func detectCgroupDriver() (CGroupDriver, error) {
	_, err1 := os.Stat("/sys/fs/cgroup/kubepods.slice")
	_, err2 := os.Stat("/sys/fs/cgroup/kubepods")
	switch {
	case err1 == nil && err2 != nil:
		return SYSTEMD, nil
	case err2 == nil && err1 != nil:
		return CGROUPFS, nil
	}

	// Check if systemd is managing cgroups by looking for systemd cgroup hierarchy
	// In systemd-managed systems, there's typically a systemd slice at the root
	if _, err := os.Stat("/sys/fs/cgroup/system.slice"); err == nil {
		return SYSTEMD, nil
	}

	// Check if we can find systemd cgroup paths
	if _, err := os.Stat("/sys/fs/cgroup/systemd"); err == nil {
		return SYSTEMD, nil
	}

	// Check for cgroupfs by looking for traditional cgroup hierarchy
	// In cgroupfs systems, we typically see individual controller directories
	if _, err := os.Stat("/sys/fs/cgroup/cpu"); err == nil {
		return CGROUPFS, nil
	}

	// Additional check for cgroup v2 with cgroupfs driver
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		// Check if systemd is not managing this hierarchy
		if _, err := os.Stat("/sys/fs/cgroup/system.slice"); err != nil {
			return CGROUPFS, nil
		}
	}

	// Check for hybrid mode where systemd might be managing some controllers
	if _, err := os.Stat("/sys/fs/cgroup/unified"); err == nil {
		// In hybrid mode, check if systemd is managing the unified hierarchy
		if _, err := os.Stat("/sys/fs/cgroup/unified/system.slice"); err == nil {
			return SYSTEMD, nil
		}
		return CGROUPFS, nil
	}

	return "", fmt.Errorf("unable to detect cgroup driver on system")
}

func getCgroupDriverFromCRI(ctx context.Context, runtimeServer criapi.RuntimeService) (*CGroupDriver, error) {
	klog.V(4).InfoS("Getting CRI runtime configuration information")

	var (
		cgroupDriver  *CGroupDriver
		runtimeConfig *runtimeapi.RuntimeConfigResponse
		err           error
	)
	// Retry a couple of times, hoping that any errors are transient.
	// Fail quickly on known, non transient errors.
	for i := 0; i < 3; i++ {
		runtimeConfig, err = runtimeServer.RuntimeConfig(ctx)
		if err != nil {
			s, ok := status.FromError(err)
			if !ok || s.Code() != codes.Unimplemented {
				// We could introduce a backoff delay or jitter, but this is largely catching cases
				// where the runtime is still starting up and we request too early.
				// Give it a little more time.
				time.Sleep(time.Second * 2)
				continue
			}
			// CRI implementation doesn't support RuntimeConfig, fallback
			klog.InfoS("CRI implementation should be updated to support RuntimeConfig when KubeletCgroupDriverFromCRI feature gate has been enabled. Falling back to using cgroupDriver from kubelet config.")
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}

	// Calling GetLinux().GetCgroupDriver() won't segfault, but it will always default to systemd
	// which is not intended by the fields not being populated
	linuxConfig := runtimeConfig.GetLinux()
	if linuxConfig == nil {
		return nil, nil
	}

	switch d := linuxConfig.GetCgroupDriver(); d {
	case runtimeapi.CgroupDriver_SYSTEMD:
		cgroupDriver = ptr.To(SYSTEMD)
	case runtimeapi.CgroupDriver_CGROUPFS:
		cgroupDriver = ptr.To(CGROUPFS)
	default:
		return nil, fmt.Errorf("runtime returned an unknown cgroup driver %d", d)
	}
	klog.InfoS("Using cgroup driver setting received from the CRI runtime", "cgroupDriver", *cgroupDriver)
	return cgroupDriver, nil
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

func convertCGroupfsFullPath(runtimeName, containerId string,
	cgroupName CgroupName, getFullPath func(string) string) (string, error) {
	fullPath := getFullPath(filepath.Join(cgroupName.ToCgroupfs(), containerId))
	if !util.PathIsNotExist(fullPath) {
		return fullPath, nil
	}
	fullPath = getFullPath(filepath.Join("system.slice", cgroupName[len(cgroupName)-1]))
	if !util.PathIsNotExist(fullPath) {
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
	if !util.PathIsNotExist(fullPath) {
		return fullPath, nil
	}
	switch runtimeName {
	case "containerd":
		klog.Warningf("CGroup full path <%s> not exist", fullPath)
		cgroupPath = fmt.Sprintf("system.slice/%s.service/%s:%s:%s", runtimeName,
			toSystemd(cgroupName), SystemdPathPrefixOfRuntime(runtimeName), containerId)
		fullPath = getFullPath(cgroupPath)
		if !util.PathIsNotExist(fullPath) {
			return fullPath, nil
		}
	case "docker":
		klog.Warningf("CGroup full path <%s> not exist", fullPath)
		cgroupPath = fmt.Sprintf("%s/%s", cgroupName.ToSystemd(), containerId)
		fullPath = getFullPath(cgroupPath)
		if !util.PathIsNotExist(fullPath) {
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

func GetContainerPidsFunc(pod *corev1.Pod, containerName string, fullPath func(string) string, fns ...func(pid int)) []int {
	cgroupFullPath, err := GetK8sPodContainerCGroupFullPath(pod, containerName, fullPath)
	if err != nil {
		klog.Errorln(err)
		return nil
	}
	klog.V(4).InfoS("Get container cgroup path", "pod",
		klog.KObj(pod), "container", containerName, "cgroupPath", cgroupFullPath)
	pids, err := cgroups.GetAllPids(cgroupFullPath)
	if err != nil {
		klog.ErrorS(err, "Failed to retrieve container pids",
			"pod", klog.KObj(pod), "container", containerName)
		return nil
	}
	klog.V(5).InfoS("Get container all pids", "pod", klog.KObj(pod),
		"container", containerName, "cgroupPath", cgroupFullPath, "pids", pids)
	if len(fns) > 0 {
		for _, pid := range pids {
			for _, fn := range fns {
				fn(pid)
			}
		}
	}
	return pids
}
