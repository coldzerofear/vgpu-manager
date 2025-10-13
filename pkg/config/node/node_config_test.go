package node

import (
	"fmt"
	"os"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

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

func Test_parseConfigTemplate(t *testing.T) {
	tests := []struct {
		name          string
		configPath    string
		configContent string
		configs       []ConfigSpec
		err           error
	}{{
		name:       "example 1, parse yaml",
		configPath: "/tmp/config.yaml",
		configContent: `
version: v1
configs:
 - nodeName: testNode
   cgroupDriver: systemd
   deviceListStrategy: envvar
   deviceSplitCount: 10
   deviceMemoryScaling: 1.0
   deviceMemoryFactor: 1
   deviceCoresScaling: 1.0
   excludeDevices: "0..2"
   gdsEnabled: true
   migStrategy: none
   imex:
     channelIDs:
      - 100
      - 200
     required: true
`,
		configs: []ConfigSpec{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[string]("envvar"),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[IDStore](NewIntIDStore(0, 1, 2)),
			GDSEnabled:          ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			Imex: ptr.To[imex.Imex](imex.Imex{
				ChannelIDs: []int{100, 200},
				Required:   true,
			}),
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
    "excludeDevices": "0..2",
    "gdsEnabled": true,
    "mofedEnabled": true,
    "migStrategy": "none",
    "imex": {
      "channelIDs": [100, 200],
      "required": true
    }
  }
]
`,
		configs: []ConfigSpec{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[string]("envvar"),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[IDStore](NewIntIDStore(0, 1, 2)),
			GDSEnabled:          ptr.To[bool](true),
			MOFEDEnabled:        ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			Imex: ptr.To[imex.Imex](imex.Imex{
				ChannelIDs: []int{100, 200},
				Required:   true,
			}),
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
    "deviceListStrategy": "envvar"
  }
]
`,
		configs: nil,
		err:     fmt.Errorf("unsupported config file format: config.jsxx"),
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
				t.Fatal(err)
			}
			defer os.RemoveAll(test.configPath)
			config, err := parseConfigTemplate(test.configPath)
			if err != nil {
				assert.Equal(t, test.err, err)
			}
			if config != nil {
				assert.Equal(t, test.configs, config.Configs)
			}
		})
	}

}

func Test_NodeConfigToString(t *testing.T) {
	config, err := NewNodeConfig(func(spec *NodeConfigSpec) {
		spec.NodeName = "testNode"
		spec.CGroupDriver = ptr.To[string]("systemd")
		spec.DeviceListStrategy = ptr.To[string]("envvar")
		spec.DeviceSplitCount = ptr.To[int](10)
		spec.DeviceMemoryScaling = ptr.To[float64](1)
		spec.DeviceMemoryFactor = ptr.To[int](1)
		spec.DeviceCoresScaling = ptr.To[float64](1)
		spec.ExcludeDevices = ptr.To[IDStore](NewIntIDStore(0, 1, 2))
		spec.GDSEnabled = ptr.To[bool](true)
		spec.MOFEDEnabled = ptr.To[bool](true)
		spec.MigStrategy = ptr.To[string]("none")
		spec.Imex = ptr.To[imex.Imex](imex.Imex{
			ChannelIDs: []int{100, 200},
			Required:   true,
		})
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		configFunc func() *NodeConfigSpec
		want       string
	}{{
		name: "example1, json string",
		configFunc: func() *NodeConfigSpec {
			config.nodeConfigPath = "/config.json"
			return config
		},
		want: `{
  "nodeName": "testNode",
  "cgroupDriver": "systemd",
  "deviceListStrategy": "envvar",
  "deviceSplitCount": 10,
  "deviceMemoryScaling": 1,
  "deviceMemoryFactor": 1,
  "deviceCoresScaling": 1,
  "excludeDevices": [
    "0",
    "1",
    "2"
  ],
  "gdsEnabled": true,
  "mofedEnabled": true,
  "migStrategy": "none",
  "imex": {
    "channelIDs": [
      100,
      200
    ],
    "required": true
  },
  "CheckFields": false
}`,
	}, {
		name: "example2, yaml string",
		configFunc: func() *NodeConfigSpec {
			config.nodeConfigPath = "/config.yaml"
			return config
		},
		want: `version: v1
configs:
    - nodeName: testNode
      cgroupDriver: systemd
      deviceListStrategy: envvar
      deviceSplitCount: 10
      deviceMemoryScaling: 1
      deviceMemoryFactor: 1
      deviceCoresScaling: 1
      excludeDevices:
        - "0"
        - "1"
        - "2"
      gdsEnabled: true
      mofedEnabled: true
      migStrategy: none
      imex:
        channelIDs:
            - 100
            - 200
        required: true
`,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.configFunc().String()
			assert.Equal(t, test.want, result)
		})
	}
}
