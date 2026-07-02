package cdi

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	transformroot "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/transform/root"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/sirupsen/logrus"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
)

// cdiRoot is the directory where generated CDI specification files are written.
// It is expected to be mounted from the host (the standard CDI dynamic dir).
const cdiRoot = "/var/run/cdi"

// pluginName identifies this device plugin in CDI container annotation keys.
const pluginName = "vgpu-manager"

// Config holds the parameters required to build a CDI Handler.
type Config struct {
	// Strategies are the configured device list strategies. If none of them is
	// a CDI strategy, New returns a null (no-op) Handler.
	Strategies util.DeviceListStrategies
	// Vendor is the CDI vendor used for qualified device names and spec files.
	Vendor string
	// Class is the CDI device class (e.g. "gpu").
	Class string
	// DeviceIDStrategy controls how devices are named in the spec ("uuid"|"index").
	DeviceIDStrategy string
	// AnnotationPrefix is the prefix used for CDI container annotation keys.
	AnnotationPrefix string
	// NvidiaCDIHookPath is the path to the NVIDIA CDI hook binary referenced
	// from the generated specification.
	NvidiaCDIHookPath string
	// DriverRoot is the driver root as seen by the plugin (used during generation).
	DriverRoot string
	// DevRoot is the device-node root as seen by the plugin.
	DevRoot string
	// TargetDriverRoot is the driver root on the host (written into the spec).
	TargetDriverRoot string
}

type handler struct {
	nvmllib          nvml.Interface
	cdilib           nvcdi.Interface
	vendor           string
	class            string
	annotationPrefix string
	driverRoot       string
	devRoot          string
	targetDriverRoot string
}

// New builds a CDI Handler. When no CDI strategy is enabled a null Handler is
// returned so callers can use it unconditionally.
func New(devicelib *nvidia.DeviceLib, cfg Config) (Handler, error) {
	if !cfg.Strategies.AnyCDIEnabled() {
		return NewNullHandler(), nil
	}

	if cfg.Vendor == "" {
		cfg.Vendor = util.CDIVendor
	}
	if cfg.Class == "" {
		cfg.Class = util.CDIClass
	}
	if cfg.DeviceIDStrategy == "" {
		cfg.DeviceIDStrategy = util.CDIDeviceIDStrategy
	}
	if cfg.AnnotationPrefix == "" {
		cfg.AnnotationPrefix = cdiapi.AnnotationPrefix
	}
	if cfg.NvidiaCDIHookPath == "" {
		cfg.NvidiaCDIHookPath = util.CDIDefaultHookPath
	}
	if cfg.DriverRoot == "" {
		cfg.DriverRoot = util.CDIDefaultDriverRoot
	}
	if cfg.DevRoot == "" {
		cfg.DevRoot = cfg.DriverRoot
	}
	if cfg.TargetDriverRoot == "" {
		cfg.TargetDriverRoot = cfg.DriverRoot
	}

	deviceNamer, err := nvcdi.NewDeviceNamer(cfg.DeviceIDStrategy)
	if err != nil {
		return nil, fmt.Errorf("failed to create CDI device namer: %w", err)
	}

	// Let nvcdi logs see the light of day (emit to standard streams) when we've
	// been configured with verbosity level 5 or higher.
	cdilogger := logrus.New()
	if !klog.V(5).Enabled() {
		cdilogger.SetOutput(io.Discard)
	}

	cdilib, err := nvcdi.New(
		nvcdi.WithDeviceLib(devicelib),
		nvcdi.WithInfoLib(devicelib),
		nvcdi.WithNvmlLib(devicelib),
		nvcdi.WithLogger(cdilogger),
		nvcdi.WithDeviceNamers(deviceNamer),
		nvcdi.WithDriverRoot(cfg.DriverRoot),
		nvcdi.WithDevRoot(cfg.DevRoot),
		nvcdi.WithNVIDIACDIHookPath(cfg.NvidiaCDIHookPath),
		nvcdi.WithVendor(cfg.Vendor),
		nvcdi.WithClass(cfg.Class),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create nvcdi library: %w", err)
	}

	return &handler{
		nvmllib:          devicelib,
		cdilib:           cdilib,
		vendor:           cfg.Vendor,
		class:            cfg.Class,
		annotationPrefix: cfg.AnnotationPrefix,
		driverRoot:       cfg.DriverRoot,
		devRoot:          cfg.DevRoot,
		targetDriverRoot: cfg.TargetDriverRoot,
	}, nil
}

// QualifiedName returns the fully-qualified CDI device name.
func (h *handler) QualifiedName(class, id string) string {
	return cdiparser.QualifiedName(h.vendor, class, id)
}

// GetDeviceAnnotations builds the CDI container annotations for the given
// qualified device names, rewriting the key prefix when a custom one is set.
func (h *handler) GetDeviceAnnotations(responseID string, qualifiedNames []string) (map[string]string, error) {
	annotations, err := cdiapi.UpdateAnnotations(map[string]string{}, pluginName, responseID, qualifiedNames)
	if err != nil {
		return nil, fmt.Errorf("failed to build CDI annotations: %w", err)
	}
	if h.annotationPrefix == cdiapi.AnnotationPrefix {
		return annotations, nil
	}
	updated := make(map[string]string, len(annotations))
	for k, v := range annotations {
		newKey := h.annotationPrefix + strings.TrimPrefix(k, cdiapi.AnnotationPrefix)
		updated[newKey] = v
	}
	return updated, nil
}

// CreateSpecFile generates and writes the CDI specification for the node's GPUs.
// It manages its own NVML lifecycle so it is safe to call regardless of whether
// NVML has been initialized elsewhere.
func (h *handler) CreateSpecFile() error {
	if ret := h.nvmllib.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML for CDI spec generation: %v", ret)
	}
	defer func() { _ = h.nvmllib.Shutdown() }()

	klog.InfoS("Generating CDI specification", "vendor", h.vendor, "class", h.class)
	spec, err := h.cdilib.GetSpec()
	if err != nil {
		return fmt.Errorf("failed to get CDI spec: %w", err)
	}
	if err = h.writeSpec(spec); err != nil {
		return fmt.Errorf("failed to write spec: %w", err)
	}
	return nil
}

func (h *handler) writeSpec(spec spec.Interface) error {
	// Transform the spec to make it aware that it is running inside a container.
	err := transformroot.New(
		transformroot.WithRoot(h.driverRoot),
		transformroot.WithTargetRoot(h.targetDriverRoot),
		transformroot.WithRelativeTo("host"),
	).Transform(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to transform driver root in CDI spec: %w", err)
	}
	specName, err := cdiapi.GenerateNameForSpec(spec.Raw())
	if err != nil {
		return fmt.Errorf("failed to generate CDI spec name: %w", err)
	}
	klog.V(3).Infof("Write CDI spec: %s", specName)
	specPath := filepath.Join(cdiRoot, specName+".yaml")
	if err = spec.Save(specPath); err != nil {
		return fmt.Errorf("failed to save CDI spec %q: %w", specPath, err)
	}
	klog.InfoS("Generated CDI specification", "path", specPath)
	return nil
}
