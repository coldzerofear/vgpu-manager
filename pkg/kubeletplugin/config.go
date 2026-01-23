package kubeletplugin

import (
	"fmt"
	"os"
	"path/filepath"

	pkgflags "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/flags"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

type Flags struct {
	KubeClientConfig pkgflags.KubeClientConfig

	NodeName                      string
	Namespace                     string
	CdiRoot                       string
	ContainerDriverRoot           string
	HostDriverRoot                string
	NvidiaCDIHookPath             string
	KubeletRegistrarDirectoryPath string
	KubeletPluginsDirectoryPath   string
	HealthcheckPort               int
	AdditionalXidsToIgnore        string
	HostManagerDir                string
}

type Config struct {
	*Flags
	pkgflags.ClientSets
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.Flags.KubeletPluginsDirectoryPath, util.DRADriverName)
}

// change to config
// If 'f.nvidiaCDIHookPath' is already set (from the command line), do nothing.
// If 'f.nvidiaCDIHookPath' is empty, it copies the nvidia-cdi-hook binary from
// /usr/bin/nvidia-cdi-hook to DriverPluginPath and sets 'f.nvidiaCDIHookPath'
// to this path. The /usr/bin/nvidia-cdi-hook is present in the current
// container image because it is copied from the toolkit image into this
// container at build time.
func (c Config) SetNvidiaCDIHookPath() error {
	if c.Flags.NvidiaCDIHookPath != "" {
		return nil
	}

	sourcePath := "/usr/bin/nvidia-cdi-hook"
	targetPath := filepath.Join(c.DriverPluginPath(), "nvidia-cdi-hook")

	input, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("error reading nvidia-cdi-hook: %w", err)
	}

	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("error copying nvidia-cdi-hook: %w", err)
	}

	c.Flags.NvidiaCDIHookPath = targetPath

	return nil
}
