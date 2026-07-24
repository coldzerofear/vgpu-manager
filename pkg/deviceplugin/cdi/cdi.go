package cdi

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvlib/info"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/transform"
	transformroot "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/transform/root"
	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
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
	TargetDevRoot    string

	GDSEnabled     bool
	MOFEDEnabled   bool
	GDRCopyEnabled bool

	ImexChannels imex.Channels
}

type handler struct {
	infolib          info.Interface
	nvmllib          nvml.Interface
	devicelib        device.Interface
	vendor           string
	class            string
	annotationPrefix string
	driverRoot       string
	devRoot          string
	targetDriverRoot string
	targetDevRoot    string
	cdilibs          map[string]nvcdi.SpecGenerator
	additionalModes  []string
}

// New builds a CDI Handler. When no CDI strategy is enabled a null Handler is
// returned so callers can use it unconditionally.
func New(devicelib *nvidia.DeviceLib, cfg Config) (Handler, error) {
	if !cfg.Strategies.AnyCDIEnabled() {
		return NewNullHandler(), nil
	}
	hasNVML, _ := devicelib.HasNvml()
	if !hasNVML {
		klog.Warning("No valid resources detected, creating a null CDI handler")
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
	if cfg.TargetDevRoot == "" {
		cfg.TargetDevRoot = cfg.DevRoot
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

	cdilibs := make(map[string]nvcdi.SpecGenerator)

	cdilibs["gpu"], err = nvcdi.New(
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

	if len(cfg.ImexChannels) > 0 {
		cdilibs["imex-channel"] = &imexChannelCDILib{
			vendor:       cfg.Vendor,
			imexChannels: cfg.ImexChannels,
		}
	}

	var additionalModes []string
	if cfg.GDRCopyEnabled {
		additionalModes = append(additionalModes, "gdrcopy")
	}
	if cfg.GDSEnabled {
		additionalModes = append(additionalModes, "gds")
	}
	if cfg.MOFEDEnabled {
		additionalModes = append(additionalModes, "mofed")
	}

	for _, mode := range additionalModes {
		lib, err := nvcdi.New(
			nvcdi.WithInfoLib(devicelib),
			nvcdi.WithLogger(cdilogger),
			nvcdi.WithNVIDIACDIHookPath(cfg.NvidiaCDIHookPath),
			nvcdi.WithDriverRoot(cfg.DriverRoot),
			nvcdi.WithDevRoot(cfg.DevRoot),
			nvcdi.WithVendor(cfg.Vendor),
			nvcdi.WithMode(mode),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create nvcdi library: %v", err)
		}
		cdilibs[mode] = lib
	}

	return &handler{
		infolib:          devicelib,
		devicelib:        devicelib,
		nvmllib:          devicelib,
		cdilibs:          cdilibs,
		vendor:           cfg.Vendor,
		class:            cfg.Class,
		annotationPrefix: cfg.AnnotationPrefix,
		driverRoot:       cfg.DriverRoot,
		devRoot:          cfg.DevRoot,
		targetDriverRoot: cfg.TargetDriverRoot,
		targetDevRoot:    cfg.TargetDevRoot,
		additionalModes:  additionalModes,
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

	// update annotations if a custom CDI prefix is configured
	updatedAnnotations := make(map[string]string, len(annotations))
	for k, v := range annotations {
		newKey := h.annotationPrefix + strings.TrimPrefix(k, cdiapi.AnnotationPrefix)
		updatedAnnotations[newKey] = v
	}
	return updatedAnnotations, nil
}

// CreateSpecFile generates and writes the CDI specification for the node's GPUs.
// It manages its own NVML lifecycle so it is safe to call regardless of whether
// NVML has been initialized elsewhere.
func (h *handler) CreateSpecFile() error {
	var emptySpecs []string
	for class, cdilib := range h.cdilibs {
		klog.V(3).Infof("Generating CDI spec for resource: %s/%s", h.vendor, class)

		if class == "gpu" {
			ret := h.nvmllib.Init()
			if ret != nvml.SUCCESS {
				return fmt.Errorf("failed to initialize NVML: %v", ret)
			}
			defer func() {
				_ = h.nvmllib.Shutdown()
			}()
		}

		spec, err := cdilib.GetSpec()
		if err != nil {
			return fmt.Errorf("failed to get CDI spec: %v", err)
		}
		// TODO: Once the NewDriverTransformer is merged in container-toolkit we can instantiate it directly.
		transformer := h.getRootTransformer()
		if err := transformer.Transform(spec.Raw()); err != nil {
			return fmt.Errorf("failed to transform driver root in CDI spec: %v", err)
		}

		specName, err := cdiapi.GenerateNameForSpec(spec.Raw())
		if err != nil {
			return fmt.Errorf("failed to generate CDI spec name: %w", err)
		}
		klog.V(3).Infof("Write CDI spec: %s", specName)
		specPath := filepath.Join(cdiRoot, specName+".json")
		if err = spec.Save(specPath); err != nil {
			// TODO: This is a brittle check since it relies on exact string matches.
			// We should pull this functionality into the CDI tooling instead.
			if strings.Contains(err.Error(), "invalid device, empty device edits") {
				klog.ErrorS(err, "Ignoring empty CDI specs", "vendor", h.vendor, "class", class)
				emptySpecs = append(emptySpecs, class)
				continue
			}
			return fmt.Errorf("failed to save CDI spec %q: %w", specPath, err)
		}
		klog.V(3).Infoln("Generated CDI specification", "path", specPath)
	}
	// Remove the classes with empty specs from the supported types.
	for _, emptySpec := range emptySpecs {
		delete(h.cdilibs, emptySpec)
	}
	return nil
}

// getRootTransformer rewrites the driver/dev root paths in the generated spec
// from the plugin's view to the host's view. Mirrors the NVIDIA device plugin.
func (h *handler) getRootTransformer() transform.Transformer {
	driverRootTransformer := transformroot.New(
		transformroot.WithRoot(h.driverRoot),
		transformroot.WithTargetRoot(h.targetDriverRoot),
		transformroot.WithRelativeTo("host"),
	)
	if h.devRoot == h.driverRoot || h.devRoot == "" {
		return driverRootTransformer
	}
	ensureDev := func(p string) string {
		return filepath.Join(strings.TrimSuffix(filepath.Clean(p), "/dev"), "/dev")
	}
	devRootTransformer := transformroot.New(
		transformroot.WithRoot(ensureDev(h.devRoot)),
		transformroot.WithTargetRoot(ensureDev(h.targetDevRoot)),
		transformroot.WithRelativeTo("host"),
	)
	return transform.Merge(driverRootTransformer, devRootTransformer)
}

// AdditionalDevices returns the optional CDI devices based on the device plugin
// configuration.
// Here we check for requested modes as well as whether the modes have a valid
// CDI spec associated with them.
func (h *handler) AdditionalDevices() []string {
	var devices []string
	for _, mode := range h.additionalModes {
		if h.cdilibs[mode] == nil {
			continue
		}
		devices = append(devices, h.QualifiedName(mode, "all"))
	}
	return devices
}
