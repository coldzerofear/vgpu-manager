/*
Copyright 2026 coldzerofear

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

package links

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
)

// clusterA / clusterB are two distinct non-zero fabric cluster UUIDs.
var (
	clusterA = [16]uint8{1}
	clusterB = [16]uint8{2}
)

// fabricDevice answers GetGpuFabricInfo (the v1 entry point) with a canned
// reading. Everything else is inherited from the nil embedded interface and
// must not be called.
type fabricDevice struct {
	device.Device
	info nvml.GpuFabricInfo
	ret  nvml.Return
}

func (f *fabricDevice) GetGpuFabricInfo() (nvml.GpuFabricInfo, nvml.Return) {
	return f.info, f.ret
}

func attachedTo(cluster [16]uint8, cliqueID uint32) *fabricDevice {
	return &fabricDevice{
		info: nvml.GpuFabricInfo{
			ClusterUuid: cluster,
			CliqueId:    cliqueID,
			State:       nvml.GPU_FABRIC_STATE_COMPLETED,
			Status:      uint32(nvml.SUCCESS),
		},
		ret: nvml.SUCCESS,
	}
}

// nvmlExporting builds an nvml.Interface whose symbol lookup succeeds for
// exactly the named symbols.
func nvmlExporting(symbols ...string) nvml.Interface {
	exported := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		exported[s] = true
	}
	ext := &mock.ExtendedInterface{
		LookupSymbolFunc: func(s string) error {
			if exported[s] {
				return nil
			}
			return fmt.Errorf("symbol %s not found", s)
		},
	}
	return &mock.Interface{
		ExtensionsFunc: func() nvml.ExtendedInterface { return ext },
	}
}

func TestDifferentFabric(t *testing.T) {
	// An unknown key must never be read as "a different fabric": that would
	// turn missing information into a decision to cut NVLink edges.
	for _, tc := range []struct {
		name          string
		clique1       string
		clique2       string
		wantDifferent bool
	}{
		{name: "same fabric", clique1: "a.0", clique2: "a.0"},
		{name: "different fabric", clique1: "a.0", clique2: "a.1", wantDifferent: true},
		{name: "different cluster", clique1: "a.0", clique2: "b.0", wantDifferent: true},
		{name: "first unknown", clique1: "", clique2: "a.0"},
		{name: "second unknown", clique1: "a.0", clique2: ""},
		{name: "both unknown", clique1: "", clique2: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DifferentFabric(tc.clique1, tc.clique2); got != tc.wantDifferent {
				t.Fatalf("DifferentFabric(%q, %q) = %v, want %v", tc.clique1, tc.clique2, got, tc.wantDifferent)
			}
		})
	}
}

func TestCliqueKey(t *testing.T) {
	t.Run("completed fabric yields cluster uuid and clique id", func(t *testing.T) {
		got, err := cliqueKey(clusterA, 7, nvml.GPU_FABRIC_STATE_COMPLETED, uint32(nvml.SUCCESS))
		if err != nil {
			t.Fatalf("cliqueKey returned error: %v", err)
		}
		want := "01000000000000000000000000000000.7"
		if got != want {
			t.Fatalf("cliqueKey = %q, want %q", got, want)
		}
	})

	// Each rejection below would otherwise produce a key that looks usable but
	// does not describe the fabric's final shape.
	t.Run("fabric still initialising is not an identity", func(t *testing.T) {
		for _, state := range []uint8{nvml.GPU_FABRIC_STATE_NOT_STARTED, nvml.GPU_FABRIC_STATE_IN_PROGRESS} {
			_, err := cliqueKey(clusterA, 7, state, uint32(nvml.SUCCESS))
			if !errors.Is(err, errFabricPending) {
				t.Fatalf("state %d: err = %v, want errFabricPending", state, err)
			}
		}
	})

	t.Run("a fabric that is not supported is not pending", func(t *testing.T) {
		_, err := cliqueKey(clusterA, 7, nvml.GPU_FABRIC_STATE_NOT_SUPPORTED, uint32(nvml.SUCCESS))
		if err == nil || errors.Is(err, errFabricPending) {
			t.Fatalf("err = %v, want a non-pending error", err)
		}
	})

	t.Run("failed fabric status is not an identity", func(t *testing.T) {
		if _, err := cliqueKey(clusterA, 7, nvml.GPU_FABRIC_STATE_COMPLETED, uint32(nvml.ERROR_TIMEOUT)); err == nil {
			t.Fatal("expected an error for a failed fabric status")
		}
	})

	t.Run("zero cluster uuid is not an identity", func(t *testing.T) {
		if _, err := cliqueKey([16]uint8{}, 7, nvml.GPU_FABRIC_STATE_COMPLETED, uint32(nvml.SUCCESS)); err == nil {
			t.Fatal("expected an error for an empty cluster uuid")
		}
	})
}

func TestFabricCliques(t *testing.T) {
	t.Run("reads every device when NVML exports fabric info", func(t *testing.T) {
		devs := []device.Device{
			attachedTo(clusterA, 0),
			attachedTo(clusterA, 1),
		}
		got := FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), devs)
		if len(got) != 2 || got[0] == "" || got[1] == "" {
			t.Fatalf("FabricCliques = %q, want two non-empty keys", got)
		}
		if got[0] == got[1] {
			t.Fatalf("FabricCliques = %q, want distinct keys for distinct clique ids", got)
		}
	})

	t.Run("a device that cannot answer is unknown, not a failure", func(t *testing.T) {
		devs := []device.Device{
			attachedTo(clusterA, 0),
			&fabricDevice{ret: nvml.ERROR_NOT_SUPPORTED},
		}
		got := FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), devs)
		if got[0] == "" {
			t.Fatal("expected the answering device to keep its key")
		}
		if got[1] != "" {
			t.Fatalf("got[1] = %q, want the unknown key", got[1])
		}
	})

	// An NVML without these symbols must not be called at all: the cgo
	// bindings abort the process on a missing entry point.
	t.Run("no fabric symbols leaves every identity unknown", func(t *testing.T) {
		devs := []device.Device{
			&fabricDevice{ret: nvml.ERROR_UNKNOWN},
			&fabricDevice{ret: nvml.ERROR_UNKNOWN},
		}
		got := FabricCliques(nvmlExporting(), devs)
		if len(got) != 2 || got[0] != "" || got[1] != "" {
			t.Fatalf("FabricCliques = %q, want two unknown keys", got)
		}
	})

	t.Run("nil nvml library leaves every identity unknown", func(t *testing.T) {
		got := FabricCliques(nil, []device.Device{&fabricDevice{}})
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("FabricCliques = %q, want one unknown key", got)
		}
	})
}

// sequenceDevice answers GetGpuFabricInfo with the next canned reading on each
// call, repeating the last one once they run out.
type sequenceDevice struct {
	device.Device
	infos []nvml.GpuFabricInfo
	calls int
}

func (s *sequenceDevice) GetGpuFabricInfo() (nvml.GpuFabricInfo, nvml.Return) {
	info := s.infos[min(s.calls, len(s.infos)-1)]
	s.calls++
	return info, nvml.SUCCESS
}

func inState(state uint8) nvml.GpuFabricInfo {
	return nvml.GpuFabricInfo{ClusterUuid: clusterA, CliqueId: 1, State: state, Status: uint32(nvml.SUCCESS)}
}

// shortSettle shrinks the settle wait for one test and counts the sleeps.
func shortSettle(t *testing.T, timeout time.Duration) *int {
	t.Helper()
	oldTimeout, oldInterval, oldSleep := fabricSettleTimeout, fabricSettleInterval, sleep
	t.Cleanup(func() { fabricSettleTimeout, fabricSettleInterval, sleep = oldTimeout, oldInterval, oldSleep })
	sleeps := 0
	fabricSettleTimeout, fabricSettleInterval = timeout, time.Millisecond
	sleep = func(d time.Duration) { sleeps++; time.Sleep(d) }
	return &sleeps
}

func TestFabricCliquesWaitsForInitialisingFabric(t *testing.T) {
	t.Run("a fabric that completes while waiting gets its identity", func(t *testing.T) {
		sleeps := shortSettle(t, time.Minute)
		dev := &sequenceDevice{infos: []nvml.GpuFabricInfo{
			inState(nvml.GPU_FABRIC_STATE_NOT_STARTED),
			inState(nvml.GPU_FABRIC_STATE_IN_PROGRESS),
			inState(nvml.GPU_FABRIC_STATE_COMPLETED),
		}}
		got := FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), []device.Device{dev, attachedTo(clusterA, 1)})
		if got[0] == "" || got[1] == "" {
			t.Fatalf("FabricCliques = %q, want both identities", got)
		}
		if *sleeps != 2 {
			t.Fatalf("slept %d times, want 2", *sleeps)
		}
	})

	// Only the pending device is re-read; one already answered is not asked again.
	t.Run("settled devices are not re-read", func(t *testing.T) {
		shortSettle(t, time.Minute)
		settled := &sequenceDevice{infos: []nvml.GpuFabricInfo{inState(nvml.GPU_FABRIC_STATE_COMPLETED)}}
		pending := &sequenceDevice{infos: []nvml.GpuFabricInfo{
			inState(nvml.GPU_FABRIC_STATE_IN_PROGRESS),
			inState(nvml.GPU_FABRIC_STATE_COMPLETED),
		}}
		FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), []device.Device{settled, pending})
		if settled.calls != 1 {
			t.Fatalf("settled device read %d times, want 1", settled.calls)
		}
	})

	t.Run("a fabric that never completes is unknown after the timeout", func(t *testing.T) {
		shortSettle(t, 5*time.Millisecond)
		dev := &sequenceDevice{infos: []nvml.GpuFabricInfo{inState(nvml.GPU_FABRIC_STATE_IN_PROGRESS)}}
		got := FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), []device.Device{dev, attachedTo(clusterA, 1)})
		if got[0] != "" || got[1] == "" {
			t.Fatalf("FabricCliques = %q, want the pending device unknown and the other kept", got)
		}
	})

	// A node without fabric support must not pay the settle wait.
	t.Run("an unsupported fabric never waits", func(t *testing.T) {
		sleeps := shortSettle(t, time.Minute)
		dev := &sequenceDevice{infos: []nvml.GpuFabricInfo{inState(nvml.GPU_FABRIC_STATE_NOT_SUPPORTED)}}
		FabricCliques(nvmlExporting("nvmlDeviceGetGpuFabricInfo"), []device.Device{dev})
		if *sleeps != 0 {
			t.Fatalf("slept %d times, want 0", *sleeps)
		}
	})
}
