/*
Copyright The Kubernetes Authors.
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubeletplugin

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/fabricmanager"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
)

func TestValidateNoOverlappingPreparedDevices(t *testing.T) {
	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:00.0": {
				"gpu-0":  &AllocatableDevice{Gpu: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{Minor: 0}}},
				"vfio-0": &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0}},
			},
		},
	}

	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: util.DRADriverName, Device: "gpu-0"},
									{Driver: util.DRADriverName, Device: "vfio-0"},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		requestDevice        string
		expectErr            bool
	}{
		{
			name:                 "gpu overlap rejected when consumable shares disabled",
			featureGate:          false,
			consumableSharesFlag: "disabled",
			requestDevice:        "gpu-0",
			expectErr:            true,
		},
		{
			name:                 "gpu overlap allowed when consumable shares enabled and matching configs",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			requestDevice:        "gpu-0",
			expectErr:            false,
		},
		{
			name:                 "vfio overlap rejected even when consumable shares enabled",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			requestDevice:        "vfio-0",
			expectErr:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			state := &DeviceState{
				config: &Config{
					Flags: &Flags{
						ConsumableShares: tc.consumableSharesFlag,
					},
				},
				perGPUAllocatable: perGPU,
			}

			incomingClaim := &resourceapi.ResourceClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{
								{Driver: util.DRADriverName, Device: tc.requestDevice},
							},
						},
					},
				},
			}

			err := state.validateNoOverlappingPreparedDevices(checkpoint, incomingClaim)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

//func TestApplySharingConfigMpsDisallowedWithConsumableShares(t *testing.T) {
//	perGPU := &PerGPUAllocatableDevices{
//		allocatablesMap: map[PCIBusID]AllocatableDevices{
//			"0000:00:00.0": {
//				"gpu-0": &AllocatableDevice{Gpu: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-0000", CudaComputeCapability: "8.0"}}},
//				"gpu-1": &AllocatableDevice{Gpu: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-0001", CudaComputeCapability: "8.0"}}},
//			},
//		},
//	}
//
//	config := &configapi.GpuSharing{
//		Strategy:  configapi.MpsStrategy,
//		MpsConfig: &configapi.MpsConfig{},
//	}
//
//	claim := &resourceapi.ResourceClaim{
//		ObjectMeta: metav1.ObjectMeta{UID: "test-claim-uid"},
//	}
//
//	results := []*resourceapi.DeviceRequestAllocationResult{
//		{Request: "req-0", Device: "gpu-0"},
//		{Request: "req-1", Device: "gpu-1"},
//	}
//
//	t.Run("disallowed when consumable shares enabled", func(t *testing.T) {
//		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
//			string(featuregates.MPSSupport):       true,
//			string(featuregates.ConsumableShares): true,
//		}))
//
//		state := &DeviceState{
//			config: &Config{
//				Flags: &Flags{
//					ConsumableShares: "unlimited",
//				},
//			},
//			perGPUAllocatable: perGPU,
//		}
//
//		_, err := state.applySharingConfig(context.Background(), config, claim, results, nil)
//		require.Error(t, err)
//		require.Contains(t, err.Error(), "MPS sharing is not supported when consumable shares is enabled")
//	})
//
//	t.Run("allowed when consumable shares disabled", func(t *testing.T) {
//		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
//			string(featuregates.MPSSupport):       true,
//			string(featuregates.ConsumableShares): false,
//		}))
//
//		cfg := &Config{
//			Flags: &Flags{
//				NodeName:         "node-a",
//				Namespace:        "default",
//				ConsumableShares: "disabled",
//			},
//		}
//		state := &DeviceState{
//			config:            cfg,
//			mpsManager:        NewMpsManager(cfg, nil, "/", "/templates/mps-control-daemon.tmpl.yaml"),
//			perGPUAllocatable: perGPU,
//		}
//
//		// Verify that applySharingConfig passes the consumable shares check (and reaches MPS daemon start)
//		defer func() {
//			r := recover()
//			// Panic happens on uninitialized clientset inside Start(), which proves it passed consumable shares check
//			if r == nil {
//				t.Log("applySharingConfig completed without panic")
//			}
//		}()
//
//		_, err := state.applySharingConfig(context.Background(), config, claim, results, nil)
//		if err != nil {
//			require.NotContains(t, err.Error(), "MPS sharing is not supported when consumable shares is enabled")
//		}
//	})
//}

func TestSharingReferenceCountingHelpers(t *testing.T) {
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpuDevice{
										Info: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-1111"}},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
								{
									Mig: &PreparedMigDevice{
										Concrete: &MigLiveTuple{MigUUID: "MIG-2222"},
										Device: &CheckpointedDevice{
											DeviceName: "mig-0",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Active claim claim-1 uses GPU-1111 and mig-0 (MIG-2222).
	// Releasing claim-2 (a different claim) should detect that GPU-1111 and MIG-2222 are in use.
	require.True(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-1111"))
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-9999"))

	// Releasing claim-1 should return false because claim-1 is being released.
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-1", "GPU-1111"))

	require.True(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-2222", "mig-0"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-9999", "mig-9"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-1", "MIG-2222", "mig-0"))

	var cpNil *Checkpoint
	require.False(t, isGpuUUIDInUseByOtherClaims(cpNil, "claim-1", "GPU-1111"))
	require.False(t, isMigDeviceInUseByOtherClaims(cpNil, "claim-1", "MIG-2222", "mig-0"))
}

type testFMClient struct {
	partitions     []fabricmanager.Partition
	deactivatedIDs []int
}

func (c *testFMClient) Init() error     { return nil }
func (c *testFMClient) Shutdown() error { return nil }
func (c *testFMClient) GetSupportedFabricPartitions() ([]fabricmanager.Partition, error) {
	return c.partitions, nil
}
func (c *testFMClient) ActivateFabricPartition(id int) error { return nil }
func (c *testFMClient) DeactivateFabricPartition(id int) error {
	c.deactivatedIDs = append(c.deactivatedIDs, id)
	return nil
}
func (c *testFMClient) IsFabricPartitionActive(id int) (bool, error) {
	return false, nil
}

func TestDeactivateFabricPartitionRefCounting(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.FabricManagerPartitioning): true,
		string(featuregates.ConsumableShares):          true,
	}))

	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: util.DRADriverName, Device: "gpu-0"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpuDevice{
										Info: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-0000"}, gpuModuleID: 1},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
							},
						},
					},
				},
				"claim-2": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: util.DRADriverName, Device: "gpu-0"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpuDevice{
										Info: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-0000"}, gpuModuleID: 1},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	fmClient := &testFMClient{
		partitions: []fabricmanager.Partition{
			{
				ID:       1,
				IsActive: true,
				GPUs: []fabricmanager.PartitionGPU{
					{PhysicalID: 1, UUID: "GPU-0000"},
				},
			},
		},
	}
	fmManager, err := fabricmanager.Open(fmClient)
	require.NoError(t, err)

	state := &DeviceState{
		config: &Config{
			Flags: &Flags{
				ConsumableShares: "unlimited",
			},
		},
		fmManager: fmManager,
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:00:00.0": {
					"gpu-0": &AllocatableDevice{Gpu: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-0000"}, gpuModuleID: 1}},
				},
			},
		},
	}

	pc1 := checkpoint.V2.PreparedClaims["claim-1"]

	// Case 1: Unpreparing claim-1 while claim-2 is still active on GPU-0000.
	// isGpuUUIDInUseByOtherClaims returns true -> deactivateFabricPartition returns nil early without calling FM DeactivatePartition.
	err = state.deactivateFabricPartition("claim-1", &pc1, checkpoint)
	require.NoError(t, err)
	require.Empty(t, fmClient.deactivatedIDs, "FM partition deactivation MUST NOT be called while claim-2 is active")

	// Case 2: Remove claim-2 from checkpoint so claim-1 is the sole claim.
	// Now deactivateFabricPartition calls FM DeactivatePartition(1).
	delete(checkpoint.V2.PreparedClaims, "claim-2")

	err = state.deactivateFabricPartition("claim-1", &pc1, checkpoint)
	require.NoError(t, err)
	require.Equal(t, []int{1}, fmClient.deactivatedIDs, "FM partition deactivation MUST be called when no active claims remain")
}
