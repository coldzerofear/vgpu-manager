package device

import (
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_CanNotCrossNumaNode(t *testing.T) {
	testCases := []struct {
		name      string
		gpuNumber int
		devices   []*DeviceInfo
		want      bool
	}{
		{
			name:      "Single device ignore numa",
			gpuNumber: 1,
			devices:   nil,
			want:      false,
		},
		{
			name:      "Multi device matching numa",
			gpuNumber: 2,
			devices: []*DeviceInfo{
				NewFakeDeviceInfo(0, 0, 0, 0, 0, 0, 0, 0),
				NewFakeDeviceInfo(1, 0, 0, 0, 0, 0, 0, 0),
			},
			want: true,
		},
		{
			name:      "Multi device not match numa",
			gpuNumber: 2,
			devices: []*DeviceInfo{
				NewFakeDeviceInfo(0, 0, 0, 0, 0, 0, 0, 0),
				NewFakeDeviceInfo(1, 0, 0, 0, 0, 0, 0, 1),
			},
			want: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, ok := CanNotCrossNumaNode(testCase.gpuNumber, testCase.devices)
			assert.Equal(t, testCase.want, ok)
		})
	}
}

func Test_NumaDeviceScoreSort(t *testing.T) {
	devicesExample1 := []*DeviceInfo{
		NewFakeDeviceInfo(0, 2, 10, 20, 100, 2000, 12000, 0),
		NewFakeDeviceInfo(1, 4, 10, 40, 100, 4000, 12000, 0),
		NewFakeDeviceInfo(2, 3, 10, 30, 100, 3000, 12000, 0),
		NewFakeDeviceInfo(3, 4, 10, 40, 100, 4000, 12000, 0),
		NewFakeDeviceInfo(4, 2, 10, 20, 100, 2000, 12000, 1),
		NewFakeDeviceInfo(5, 5, 10, 50, 100, 5000, 12000, 1),
		NewFakeDeviceInfo(6, 3, 10, 30, 100, 3000, 12000, 1),
		NewFakeDeviceInfo(7, 2, 10, 20, 100, 2000, 12000, 1),
	}
	devicesExample2 := []*DeviceInfo{
		NewFakeDeviceInfo(0, 2, 10, 20, 100, 2000, 12000, 0),
		NewFakeDeviceInfo(1, 5, 10, 50, 100, 5000, 12000, 0),
		NewFakeDeviceInfo(2, 3, 10, 30, 100, 3000, 12000, 0),
		NewFakeDeviceInfo(3, 2, 10, 20, 100, 2000, 12000, 0),
		NewFakeDeviceInfo(4, 2, 10, 20, 100, 2000, 12000, 1),
		NewFakeDeviceInfo(5, 4, 10, 40, 100, 4000, 12000, 1),
		NewFakeDeviceInfo(6, 3, 10, 30, 100, 3000, 12000, 1),
		NewFakeDeviceInfo(7, 4, 10, 40, 100, 4000, 12000, 1),
	}

	testCases := []struct {
		name           string
		devices        []*DeviceInfo
		binpackNumaIds []int
		spreadNumaIds  []int
	}{{
		name:           "example 1",
		devices:        devicesExample1,
		binpackNumaIds: []int{0, 1},
		spreadNumaIds:  []int{1, 0},
	}, {
		name:           "example 2",
		devices:        devicesExample2,
		binpackNumaIds: []int{1, 0},
		spreadNumaIds:  []int{0, 1},
	}}
	containsDeviceSlice := func(t *testing.T, devSlice, subSlice []*DeviceInfo) {
		for _, subDev := range subSlice {
			ok := slices.ContainsFunc(devSlice, func(dev *DeviceInfo) bool {
				return reflect.DeepEqual(dev, subDev)
			})
			if !ok {
				t.Fatalf("device list does not include %+v", subDev)
			}
		}
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			numaDevices := NewNumaDevices(testCase.devices)
			var binpackNumaIds []int
			numaDevices.NumaScoreBinpackCallback(func(numaNode int, devices []*DeviceInfo) (done bool) {
				for _, device := range devices {
					assert.Equal(t, numaNode, device.GetNUMA())
				}
				containsDeviceSlice(t, testCase.devices, devices)
				binpackNumaIds = append(binpackNumaIds, numaNode)
				return done
			})
			assert.Equal(t, testCase.binpackNumaIds, binpackNumaIds)

			var spreadNumaIds []int
			numaDevices.NumaScoreSpreadCallback(func(numaNode int, devices []*DeviceInfo) (done bool) {
				for _, device := range devices {
					assert.Equal(t, numaNode, device.GetNUMA())
				}
				containsDeviceSlice(t, testCase.devices, devices)
				spreadNumaIds = append(spreadNumaIds, numaNode)
				return done
			})
			assert.Equal(t, testCase.spreadNumaIds, spreadNumaIds)
		})
	}
}
