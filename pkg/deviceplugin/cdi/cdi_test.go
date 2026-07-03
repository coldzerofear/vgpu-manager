package cdi

import (
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

// Test_New_NoCDIStrategy verifies that a null handler is returned (and the
// nvml/device/info handles are never touched) when no CDI strategy is enabled.
func Test_New_NoCDIStrategy(t *testing.T) {
	for _, strategies := range []util.DeviceListStrategies{
		nil,
		{util.DeviceListStrategyEnvvar},
		{util.DeviceListStrategyEnvvar, util.DeviceListStrategyVolumeMounts},
	} {
		h, err := New(nil, Config{Strategies: strategies})
		assert.NoError(t, err)
		// null handler: no-op behavior.
		assert.NoError(t, h.CreateSpecFile())
		assert.Equal(t, "", h.QualifiedName(util.CDIClass, "GPU-123"))
		ann, err := h.GetDeviceAnnotations("resp-1", []string{"x"})
		assert.NoError(t, err)
		assert.Nil(t, ann)
		_, isNull := h.(*null)
		assert.True(t, isNull, "expected a null handler for strategies %v", strategies)
	}
}

func Test_NullHandler(t *testing.T) {
	h := NewNullHandler()
	assert.NoError(t, h.CreateSpecFile())
	assert.Equal(t, "", h.QualifiedName("gpu", "GPU-123"))
	ann, err := h.GetDeviceAnnotations("resp-1", []string{"a", "b"})
	assert.NoError(t, err)
	assert.Nil(t, ann)
}

func Test_QualifiedName(t *testing.T) {
	h := &handler{vendor: util.CDIVendor, class: util.CDIClass}
	assert.Equal(t, "k8s.device-plugin.nvidia.com/gpu=GPU-123", h.QualifiedName("gpu", "GPU-123"))
}

func Test_GetDeviceAnnotations_DefaultPrefix(t *testing.T) {
	h := &handler{vendor: util.CDIVendor, annotationPrefix: cdiapi.AnnotationPrefix}
	names := []string{
		h.QualifiedName(util.CDIClass, "GPU-1"),
		h.QualifiedName(util.CDIClass, "GPU-2"),
	}
	ann, err := h.GetDeviceAnnotations("resp-1", names)
	assert.NoError(t, err)
	assert.Len(t, ann, 1)
	for k, v := range ann {
		assert.True(t, strings.HasPrefix(k, cdiapi.AnnotationPrefix), "key %q should use default prefix", k)
		assert.Contains(t, v, "k8s.device-plugin.nvidia.com/gpu=GPU-1")
		assert.Contains(t, v, "k8s.device-plugin.nvidia.com/gpu=GPU-2")
	}
}

func Test_GetDeviceAnnotations_CustomPrefix(t *testing.T) {
	const customPrefix = "vgpu.example.com/"
	h := &handler{vendor: util.CDIVendor, annotationPrefix: customPrefix}
	names := []string{h.QualifiedName(util.CDIClass, "GPU-1")}
	ann, err := h.GetDeviceAnnotations("resp-1", names)
	assert.NoError(t, err)
	assert.Len(t, ann, 1)
	for k := range ann {
		assert.True(t, strings.HasPrefix(k, customPrefix), "key %q should use custom prefix", k)
		assert.False(t, strings.HasPrefix(k, cdiapi.AnnotationPrefix), "key %q must not keep default prefix", k)
	}
}
