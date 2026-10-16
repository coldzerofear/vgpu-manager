/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package node

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
   openKernelModules: true
   imex:
     channelIDs:
      - 100
      - 200
     required: true
`,
		configs: []ConfigSpec{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[util.DeviceListStrategies](util.DeviceListStrategies{"envvar"}),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[IDStore](NewIntIDStore(0, 1, 2)),
			GDSEnabled:          ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			OpenKernelModules:   ptr.To[bool](true),
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
    "openKernelModules": true,
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
			DeviceListStrategy:  ptr.To[util.DeviceListStrategies](util.DeviceListStrategies{"envvar"}),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[IDStore](NewIntIDStore(0, 1, 2)),
			GDSEnabled:          ptr.To[bool](true),
			MOFEDEnabled:        ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			OpenKernelModules:   ptr.To[bool](true),
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
	}, {
		name:       "example 5, support json5",
		configPath: "/tmp/config.json",
		configContent: `
[
  {
	// this is a comment
    "nodeName": "testNode",
    "cgroupDriver": "systemd",
    "deviceListStrategy": "envvar",
    "deviceSplitCount": 10,
	/*
        this is a comment
    */
    "deviceMemoryScaling": 1.0,
    "deviceMemoryFactor": 1,
    "deviceCoresScaling": 1.0,
    "excludeDevices": "0..2",
    "gdsEnabled": true,
    "mofedEnabled": true,
    "migStrategy": "none",
    "openKernelModules": true,
    imex: {
      "channelIDs": [100, 200,],
      "required": true,
    },
  }
]
`,
		configs: []ConfigSpec{{
			NodeName:            "testNode",
			CGroupDriver:        ptr.To[string]("systemd"),
			DeviceListStrategy:  ptr.To[util.DeviceListStrategies](util.DeviceListStrategies{"envvar"}),
			DeviceSplitCount:    ptr.To[int](10),
			DeviceMemoryScaling: ptr.To[float64](1),
			DeviceMemoryFactor:  ptr.To[int](1),
			DeviceCoresScaling:  ptr.To[float64](1),
			ExcludeDevices:      ptr.To[IDStore](NewIntIDStore(0, 1, 2)),
			GDSEnabled:          ptr.To[bool](true),
			MOFEDEnabled:        ptr.To[bool](true),
			MigStrategy:         ptr.To[string]("none"),
			OpenKernelModules:   ptr.To[bool](true),
			Imex: ptr.To[imex.Imex](imex.Imex{
				ChannelIDs: []int{100, 200},
				Required:   true,
			}),
		}},
		err: nil,
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
	config, err := NewNodeConfig(
		WithNodeNameOption("testNode"),
		WithCGroupDriverOption("systemd"),
		WithDeviceListStrategyOption([]string{"envvar"}),
		WithDeviceSplitCountOption(10),
		WithDeviceMemoryScalingOption(1),
		WithDeviceMemoryFactorOption(1),
		WithDeviceCoresScalingOption(1),
		WithExcludeDevicesOption("0,1,2"),
		WithGDSEnabledOption(true),
		WithMOFEDEnabledOption(true),
		WithMigStrategyOption("none"),
		WithOpenKernelModulesOption(true),
		WithIMEXOption([]int{100, 200}, true),
	)
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
  "openKernelModules": true,
  "imex": {
    "channelIDs": [
      100,
      200
    ],
    "required": true
  }
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
      openKernelModules: true
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

// baseValidNodeConfig is a spec every other field of which passes validation,
// so a test can assert on exactly the field it sets.
func baseValidNodeConfig() NodeConfigSpec {
	spec := NodeConfigSpec{}
	for _, opt := range []Option{
		WithDeviceListStrategyOption([]string{string(util.DeviceListStrategyEnvvar)}),
		WithDeviceSplitCountOption(10),
		WithDeviceMemoryScalingOption(1),
		WithDeviceMemoryFactorOption(1),
		WithDeviceCoresScalingOption(1),
		WithDevicePluginPathOption("/var/lib/kubelet/device-plugins"),
		WithMigStrategyOption(util.MigStrategyNone),
	} {
		opt(&spec)
	}
	return spec
}

// The memory override describes hardware NVML cannot describe (a GPU with no
// framebuffer of its own, e.g. GB10). It is a size in MiB, unset by default.
func Test_DeviceMemoryOverride(t *testing.T) {
	var unset NodeConfigSpec
	assert.Equal(t, 0, unset.GetDeviceMemoryOverride(), "unset must read as disabled")

	spec := NodeConfigSpec{}
	WithDeviceMemoryOverrideOption(65536)(&spec)
	assert.Equal(t, 65536, spec.GetDeviceMemoryOverride())

	valid := baseValidNodeConfig()
	WithDeviceMemoryOverrideOption(65536)(&valid)
	assert.Empty(t, valid.checkNodeConfig())

	negative := baseValidNodeConfig()
	WithDeviceMemoryOverrideOption(-1)(&negative)
	errs := negative.checkNodeConfig()
	if assert.Len(t, errs, 1) {
		assert.Contains(t, errs[0].Error(), "deviceMemoryOverride")
	}
}

// The override is only used for a GPU that shares its memory with the host, so
// there is nothing to oversell: the configuration is refused outright rather
// than handing out memory the host also needs.
func Test_DeviceMemoryOverrideRefusesOversold(t *testing.T) {
	tests := map[string]struct {
		override int
		scaling  float64
		wantErr  bool
	}{
		"override without oversold":    {override: 65536, scaling: 1},
		"oversold without an override": {override: 0, scaling: 2},
		"override with oversold":       {override: 65536, scaling: 2, wantErr: true},
		"override with a hair over 1":  {override: 65536, scaling: 1.01, wantErr: true},
		// Undersold is a real configuration (hand out less than the card has),
		// and it does not overcommit anything.
		"override with undersold": {override: 65536, scaling: 0.5},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := baseValidNodeConfig()
			WithDeviceMemoryOverrideOption(tc.override)(&spec)
			WithDeviceMemoryScalingOption(tc.scaling)(&spec)

			errs := spec.checkNodeConfig()

			if !tc.wantErr {
				assert.Empty(t, errs)
				return
			}
			if assert.Len(t, errs, 1) {
				assert.Contains(t, errs[0].Error(), "deviceMemoryScaling must be 1")
			}
		})
	}
}

// loadConfigSpec copies the matched file entry field by field, so a field added
// to ConfigSpec is silently ignored until someone remembers to copy it too -
// which is how gdrcopyEnabled and deviceMemoryOverride were both lost. This
// pins every field: the file below sets all of them, so a new field makes the
// test fail until it is added here and to loadConfigSpec.
func Test_loadConfigSpecCopiesEveryField(t *testing.T) {
	const everyField = `
version: v1
configs:
  - nodeName: demo
    cgroupDriver: systemd
    deviceListStrategy: envvar
    deviceSplitCount: 5
    deviceMemoryScaling: 1
    deviceMemoryFactor: 1
    deviceCoresScaling: 1
    deviceMemoryOverride: 65536
    excludeDevices: "0"
    gdsEnabled: true
    mofedEnabled: true
    gdrcopyEnabled: true
    migStrategy: none
    openKernelModules: true
    imex:
      channelIDs: [0]
      required: true
`
	path := fmt.Sprintf("%s/nodeConfig.yaml", t.TempDir())
	require.NoError(t, os.WriteFile(path, []byte(everyField), 0o600))

	spec := NodeConfigSpec{ConfigSpec: ConfigSpec{NodeName: "demo"}, nodeConfigPath: path}
	require.NoError(t, loadConfigSpec(&spec))

	value := reflect.ValueOf(spec.ConfigSpec)
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if field.Type.Kind() != reflect.Ptr {
			// nodeName is the matcher, not a copied setting.
			continue
		}
		assert.False(t, value.Field(i).IsNil(),
			"ConfigSpec.%s was not copied out of the config file: set it in this test's "+
				"config and copy it in loadConfigSpec", field.Name)
	}
}
