/*
Copyright The Kubernetes Authors

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

package kubeletplugin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func nonEmptyClaimPayload() (resourceapi.ResourceClaimStatus, PreparedDevices) {
	status := resourceapi.ResourceClaimStatus{
		ReservedFor: []resourceapi.ResourceClaimConsumerReference{
			{Resource: "pods", Name: "pod-1", UID: "pod-uid-1"},
		},
	}
	devices := PreparedDevices{{
		Devices: PreparedDeviceList{{Gpu: &PreparedGpuDevice{Device: &CheckpointedDevice{
			Requests: []string{"req0"}, PoolName: "pool0", DeviceName: "gpu-0", CDIDeviceIDs: []string{"cdi0"},
		}}}},
		ConfigState: DeviceConfigState{MpsControlDaemonID: "mps-1"},
	}}
	return status, devices
}

func TestCheckpointToLatestVersion(t *testing.T) {
	tests := map[string]struct {
		in           *Checkpoint
		wantBootID   string
		wantClaimIDs []string
	}{
		"uses v2 when present": {
			in:           &Checkpoint{V2: &CheckpointV2{NodeBootID: "boot-v2", PreparedClaims: PreparedClaimsByUID{"uid-a": {}}}},
			wantBootID:   "boot-v2",
			wantClaimIDs: []string{"uid-a"},
		},
		"v1 upgraded to v2": {
			in:           &Checkpoint{V1: &CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{"uid-b": {}}}},
			wantClaimIDs: []string{"uid-b"},
		},
		"v2 wins over v1 when both present": {
			in: &Checkpoint{
				V1: &CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{"uid-v1": {}}},
				V2: &CheckpointV2{NodeBootID: "boot-from-v2", PreparedClaims: PreparedClaimsByUID{"uid-v2": {}}},
			},
			wantBootID:   "boot-from-v2",
			wantClaimIDs: []string{"uid-v2"},
		},
		"empty checkpoint yields empty v2": {
			in:           &Checkpoint{},
			wantClaimIDs: []string{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			latest := tc.in.ToLatestVersion()

			require.NotNil(t, latest.V2)
			require.NotNil(t, latest.V2.PreparedClaims)
			assert.Equal(t, tc.wantBootID, latest.GetNodeBootID())

			var gotIDs []string
			for uid := range latest.V2.PreparedClaims {
				gotIDs = append(gotIDs, uid)
			}
			assert.ElementsMatch(t, tc.wantClaimIDs, gotIDs)
		})
	}
}

func TestCheckpointMarshalUnmarshalRoundTrip(t *testing.T) {
	status, devices := nonEmptyClaimPayload()
	orig := &Checkpoint{V2: &CheckpointV2{NodeBootID: "boot-1", PreparedClaims: PreparedClaimsByUID{
		"uid-done": {CheckpointState: ClaimCheckpointStatePrepareCompleted, Name: "claim-done", Namespace: "ns", Status: status, PreparedDevices: devices},
	}}}

	data, err := orig.MarshalCheckpoint()
	require.NoError(t, err)
	got := &Checkpoint{}
	require.NoError(t, got.UnmarshalCheckpoint(data))

	require.NotNil(t, got.V1)
	require.NotNil(t, got.V2)
	require.NoError(t, got.VerifyChecksum())

	assert.Equal(t, "boot-1", got.GetNodeBootID())
	assert.Equal(t, orig.V2.PreparedClaims["uid-done"], got.V2.PreparedClaims["uid-done"])
	assert.Equal(t, PreparedClaimV1{Status: status, PreparedDevices: devices}, got.V1.PreparedClaims["uid-done"])
}

func TestCheckpointChecksum(t *testing.T) {
	orig := &Checkpoint{V2: &CheckpointV2{
		NodeBootID:     "boot-1",
		PreparedClaims: PreparedClaimsByUID{"uid-a": {CheckpointState: ClaimCheckpointStatePrepareCompleted}},
	}}
	data, err := orig.MarshalCheckpoint()
	require.NoError(t, err)

	cp := &Checkpoint{}
	require.NoError(t, cp.UnmarshalCheckpoint(data))
	require.NoError(t, cp.VerifyChecksum())

	cp.V2.NodeBootID = "tampered"
	require.Error(t, cp.VerifyChecksum())

	v1Only := &Checkpoint{V1: &CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{}}}
	require.NoError(t, v1Only.VerifyChecksumV2())
}

func TestCheckpointV1ToV2(t *testing.T) {
	status, devices := nonEmptyClaimPayload()
	v2 := (&CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{
		"uid-a": {Status: status, PreparedDevices: devices},
		"uid-b": {},
	}}).ToV2()

	require.Len(t, v2.PreparedClaims, 2)
	// V1 has no per-claim state, so upgraded claims are treated as completed.
	for uid, c := range v2.PreparedClaims {
		assert.Equalf(t, ClaimCheckpointStatePrepareCompleted, c.CheckpointState, "claim %q", uid)
	}
	assert.Equal(t, status, v2.PreparedClaims["uid-a"].Status)
	assert.Equal(t, devices, v2.PreparedClaims["uid-a"].PreparedDevices)
}

func TestCheckpointV2ToV1DropsNonCompleted(t *testing.T) {
	status, devices := nonEmptyClaimPayload()
	v1 := (&CheckpointV2{PreparedClaims: PreparedClaimsByUID{
		"done":    {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: status, PreparedDevices: devices},
		"started": {CheckpointState: ClaimCheckpointStatePrepareStarted},
		"unset":   {CheckpointState: ClaimCheckpointStateUnset},
	}}).ToV1()

	require.Len(t, v1.PreparedClaims, 1, "only the completed claim is downgradable")
	require.Contains(t, v1.PreparedClaims, "done")
	assert.Equal(t, status, v1.PreparedClaims["done"].Status)
	assert.Equal(t, devices, v1.PreparedClaims["done"].PreparedDevices)
}

func TestCheckpointedDeviceMarshalJSON(t *testing.T) {
	keys := func(t *testing.T, d CheckpointedDevice) map[string]json.RawMessage {
		t.Helper()
		data, err := d.MarshalJSON()
		require.NoError(t, err)
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &m))
		return m
	}

	base := CheckpointedDevice{Requests: []string{"req0"}, PoolName: "pool0", DeviceName: "dev0", CDIDeviceIDs: []string{"cdi0"}}
	unset := keys(t, base)
	assert.NotContains(t, unset, "ShareID")
	assert.NotContains(t, unset, "Metadata")

	share := types.UID("share-1")
	set := base
	set.ShareID = &share
	set.Metadata = &kubeletplugin.DeviceMetadata{}
	m := keys(t, set)
	assert.Contains(t, m, "ShareID")
	assert.Contains(t, m, "Metadata")
}
