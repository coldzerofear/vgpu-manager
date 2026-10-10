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

package options

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/component-base/featuregate"
)

// The remote role rules: which combinations of roles, gates and MIG strategy a
// device plugin accepts. NewOptions registers its gate globally, so each case
// builds its own.
func TestValidateRemoteRoles(t *testing.T) {
	for _, test := range []struct {
		name     string
		gates    string
		server   bool
		consumer bool
		mig      string
		wantErr  string
	}{
		{name: "local node", mig: util.MigStrategyMixed},
		{name: "server", gates: "RemoteGPUSupport=true", server: true},
		{name: "consumer", gates: "RemoteGPUSupport=true", consumer: true},
		// A node that serves its GPUs and runs remote pods may still split
		// them with MIG; it is the one with GPUs.
		{name: "server and consumer with MIG", gates: "RemoteGPUSupport=true", server: true, consumer: true, mig: util.MigStrategyMixed},
		{
			name: "gate without a role", gates: "RemoteGPUSupport=true",
			wantErr: "--remote-server or --remote-consumer must be enabled",
		},
		{
			name: "role without the gate", server: true,
			wantErr: "require the RemoteGPUSupport feature gate",
		},
		{
			// Nothing is admitted on a server-only node, so nothing there
			// fails allocation.
			name: "reschedule on a server-only node", gates: "RemoteGPUSupport=true,AllocationFailureReschedule=true", server: true,
			wantErr: "pods are admitted on consumer nodes",
		},
		{
			name: "reschedule on a consumer node", gates: "RemoteGPUSupport=true,AllocationFailureReschedule=true", consumer: true,
		},
		{
			// No GPUs to partition on a consumer-only node.
			name: "MIG on a consumer-only node", gates: "RemoteGPUSupport=true", consumer: true, mig: util.MigStrategyMixed,
			wantErr: "has no GPUs to partition",
		},
		{
			// Single MIG builds no vgpu-number plugin, which the consumer role needs.
			name: "single MIG with the consumer role", gates: "RemoteGPUSupport=true", server: true, consumer: true, mig: util.MigStrategySingle,
			wantErr: "mutually exclusive",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := featuregate.NewFeatureGate()
			require.NoError(t, gate.Add(defaultFeatureGates))
			if test.gates != "" {
				require.NoError(t, gate.Set(test.gates))
			}
			mig := test.mig
			if mig == "" {
				mig = util.MigStrategyNone
			}
			o := &Options{
				FeatureGate:         gate,
				RemoteServer:        test.server,
				RemoteConsumer:      test.consumer,
				RemoteConsumerNum:   defaultRemoteConsumerVGPU,
				RemoteAgentEndpoint: "unix:///etc/vgpu-manager/agent.sock",
				MigStrategy:         mig,
			}

			err := o.Validate()

			if test.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}
