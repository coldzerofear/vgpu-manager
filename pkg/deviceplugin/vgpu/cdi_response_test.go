package vgpu

import (
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/cdi"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// fakeCDIHandler is a deterministic cdi.Handler stub for testing UpdateResponseForCDI.
type fakeCDIHandler struct{}

func (fakeCDIHandler) CreateSpecFile() error { return nil }

func (fakeCDIHandler) QualifiedName(class, id string) string {
	return util.CDIVendor + "/" + class + "=" + id
}

func (f fakeCDIHandler) GetDeviceAnnotations(responseID string, names []string) (map[string]string, error) {
	return map[string]string{"cdi.k8s.io/vgpu-manager_" + responseID: strings.Join(names, ",")}, nil
}

func (f fakeCDIHandler) AdditionalDevices() []string {
	return nil
}

var _ cdi.Handler = fakeCDIHandler{}

func newResp() *pluginapi.ContainerAllocateResponse {
	return &pluginapi.ContainerAllocateResponse{Envs: make(map[string]string)}
}

func Test_UpdateResponseForCDI_NoCDIStrategy(t *testing.T) {
	resp := newResp()
	err := UpdateResponseForCDI(nil, resp, util.DeviceListStrategies{util.DeviceListStrategyEnvvar},
		fakeCDIHandler{}, "GPU-1", "GPU-2")
	assert.NoError(t, err)
	assert.Empty(t, resp.Annotations)
	assert.Empty(t, resp.CdiDevices)
}

func Test_UpdateResponseForCDI_Annotations(t *testing.T) {
	resp := newResp()
	err := UpdateResponseForCDI(nil, resp, util.DeviceListStrategies{util.DeviceListStrategyCDIAnnotations},
		fakeCDIHandler{}, "GPU-1", "GPU-2")
	assert.NoError(t, err)
	assert.Len(t, resp.Annotations, 1)
	assert.Empty(t, resp.CdiDevices)
	for _, v := range resp.Annotations {
		assert.Contains(t, v, "k8s.device-plugin.nvidia.com/gpu=GPU-1")
		assert.Contains(t, v, "k8s.device-plugin.nvidia.com/gpu=GPU-2")
	}
}

func Test_UpdateResponseForCDI_CRI(t *testing.T) {
	resp := newResp()
	err := UpdateResponseForCDI(nil, resp, util.DeviceListStrategies{util.DeviceListStrategyCDICRI},
		fakeCDIHandler{}, "GPU-1", "GPU-2")
	assert.NoError(t, err)
	assert.Empty(t, resp.Annotations)
	assert.Len(t, resp.CdiDevices, 2)
	assert.Equal(t, "k8s.device-plugin.nvidia.com/gpu=GPU-1", resp.CdiDevices[0].Name)
	assert.Equal(t, "k8s.device-plugin.nvidia.com/gpu=GPU-2", resp.CdiDevices[1].Name)
}

func Test_UpdateResponseForCDI_Combined(t *testing.T) {
	resp := newResp()
	err := UpdateResponseForCDI(nil, resp,
		util.DeviceListStrategies{util.DeviceListStrategyCDIAnnotations, util.DeviceListStrategyCDICRI},
		fakeCDIHandler{}, "GPU-1")
	assert.NoError(t, err)
	assert.Len(t, resp.Annotations, 1)
	assert.Len(t, resp.CdiDevices, 1)
}
