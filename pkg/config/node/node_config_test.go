package node

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

func Test_ParseExcludeDevices(t *testing.T) {
	tests := []struct {
		name   string
		exDevs string
		want   sets.Int
	}{
		{
			name:   "example 1",
			exDevs: "0,0",
			want:   sets.NewInt(0),
		},
		{
			name:   "example 2",
			exDevs: "0,1,3,3",
			want:   sets.NewInt(0, 1, 3),
		},
		{
			name:   "example 3",
			exDevs: "0-3",
			want:   sets.NewInt(0, 1, 2, 3),
		},
		{
			name:   "example 4",
			exDevs: "0-2,4-6",
			want:   sets.NewInt(0, 1, 2, 4, 5, 6),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deviceSet := ParseExcludeDevices(test.exDevs)
			assert.Equal(t, test.want, deviceSet)
		})
	}
}

func Test_MatchNodeName(t *testing.T) {
	tests := []struct {
		name       string
		cmNodeName string
		cuNodeName string
		want       bool
	}{
		{
			name:       "example 1, Equal names",
			cmNodeName: "testNode",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 2",
			cmNodeName: "test.*",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 3",
			cmNodeName: "^test_",
			cuNodeName: "testNode",
			want:       false,
		}, {
			name:       "example 4",
			cmNodeName: "\\.Node$",
			cuNodeName: "test.Node",
			want:       true,
		}, {
			name:       "example 5",
			cmNodeName: `Node$`,
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 6",
			cmNodeName: `(?i)node$`,
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 7",
			cmNodeName: `(?i)node(1|2)$`,
			cuNodeName: "testNode2",
			want:       true,
		}, {
			name:       "example 8",
			cmNodeName: `(?i)node(1|2)$`,
			cuNodeName: "testNode3",
			want:       false,
		}, {
			name:       "example 9",
			cmNodeName: "^test\\.",
			cuNodeName: "test.Node",
			want:       true,
		}, {
			name:       "example 10",
			cmNodeName: "test",
			cuNodeName: "testNode",
			want:       false,
		}, {
			name:       "example 11",
			cmNodeName: "*Node",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 12",
			cmNodeName: "^test",
			cuNodeName: "testNode",
			want:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := matchNodeName(test.cmNodeName, test.cuNodeName)
			assert.Equal(t, test.want, got)
		})
	}
}

func Test_ParseConfig(t *testing.T) {
	tests := []struct {
		name          string
		configPath    string
		configContent string
		configs       []Config
		err           error
	}{{
		name:       "example 1, parse yaml",
		configPath: "/tmp/config.yaml",
		configContent: `
version: v1
config:
 - nodeName: testNode
   cgroupDriver: systemd
   deviceListStrategy: envvar
   deviceSplitCount: 10
   deviceMemoryScaling: 1.0
   deviceMemoryFactor: 1
   deviceCoresScaling: 1.0
   excludeDevices: "0-2"
   gdsEnabled: true
   migStrategy: none
   openKernelModules: true
`,
		configs: []Config{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[string]("envvar"),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[string]("0-2"),
			GDSEnabled:          ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			OpenKernelModules:   ptr.To[bool](true),
		}},
		err: nil,
	}, {
		name:       "example 2, parse json",
		configPath: "/tmp/config.json",
		configContent: `
[
  {
    "nodeName": "testNode",
    "cgroupDriver": "systemd",
    "deviceListStrategy": "envvar",
    "deviceSplitCount": 10,
    "deviceMemoryScaling": 1.0,
    "deviceMemoryFactor": 1,
    "deviceCoresScaling": 1.0,
    "excludeDevices": "0-2",
    "gdsEnabled": true,
    "mofedEnabled": true,
    "migStrategy": "none",
    "openKernelModules": true
  }
]
`,
		configs: []Config{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[string]("envvar"),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[string]("0-2"),
			GDSEnabled:          ptr.To[bool](true),
			MOFEDEnabled:        ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			OpenKernelModules:   ptr.To[bool](true),
		}},
		err: nil,
	}, {
		name:       "example 3, config file format error",
		configPath: "/tmp/config.jsxx",
		configContent: `
[
  {
    "nodeName": "testNode",
    "cgroupDriver": "systemd",
    "deviceListStrategy": "envvar",
    "deviceSplitCount": 10,
    "deviceMemoryScaling": 1.0,
    "deviceMemoryFactor": 1,
    "deviceCoresScaling": 1.0,
    "excludeDevices": "0-2",
    "gdsEnabled": true,
    "mofedEnabled": true,
    "migStrategy": "none",
    "openKernelModules": true
  }
]
`,
		configs: nil,
		err:     fmt.Errorf("unsupported config file format"),
	}, {
		name:       "example 4, config version error",
		configPath: "/tmp/config.yaml",
		configContent: `
version: v0
config: []
`,
		configs: nil,
		err:     fmt.Errorf("unknown config version: v0"),
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := os.WriteFile(test.configPath, []byte(test.configContent), 0755)
			if err != nil {
				t.Error(err)
				return
			}
			defer os.RemoveAll(test.configPath)
			config, err := parseConfig(test.configPath)
			assert.Equal(t, test.err, err)
			assert.Equal(t, test.configs, config)
		})
	}

}
