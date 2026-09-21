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

package validate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// A remote vGPU pod is given its GPU server's address, which may be an
// in-cluster DNS name, so it has to be able to resolve one.
func TestCheckClusterDNS(t *testing.T) {
	for _, test := range []struct {
		name        string
		policy      corev1.DNSPolicy
		hostNetwork bool
		dnsConfig   *corev1.PodDNSConfig
		wantField   string
	}{
		{name: "default policy", policy: ""},
		{name: "cluster first", policy: corev1.DNSClusterFirst},
		{name: "cluster first with host net", policy: corev1.DNSClusterFirstWithHostNet, hostNetwork: true},
		{
			name: "node resolver", policy: corev1.DNSDefault,
			wantField: "spec.dnsPolicy",
		},
		{
			// Kubernetes makes this behave as Default; the pod would resolve
			// through the node and never see the cluster zone.
			name: "cluster first on the host network", policy: corev1.DNSClusterFirst, hostNetwork: true,
			wantField: "spec.dnsPolicy",
		},
		{
			name: "none without nameservers", policy: corev1.DNSNone,
			wantField: "spec.dnsConfig",
		},
		{
			// Its own resolver may well answer for the cluster zone; that is
			// the author's call, not ours.
			name: "none with nameservers", policy: corev1.DNSNone,
			dnsConfig: &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{
				DNSPolicy:   test.policy,
				HostNetwork: test.hostNetwork,
				DNSConfig:   test.dnsConfig,
			}}

			errs := checkClusterDNS(pod)

			if test.wantField == "" {
				assert.Empty(t, errs)
				return
			}
			if assert.Len(t, errs, 1) {
				assert.Equal(t, test.wantField, errs[0].Field)
			}
		})
	}
}
