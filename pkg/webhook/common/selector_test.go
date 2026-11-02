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

package common

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/cel/environment"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/utils/ptr"
)

type selectorDevice struct {
	productName string
	uuid        string
}

func (d selectorDevice) cel() dracel.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":       {StringValue: ptr.To(kubeletplugin.VGpuDeviceType)},
		"accessMode": {StringValue: ptr.To(util.AccessModeLocal)},
	}
	if d.productName != "" {
		attrs["productName"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.productName)}
	}
	if d.uuid != "" {
		attrs["uuid"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.uuid)}
	}
	return dracel.Device{Driver: util.DRADriverName, Attributes: attrs}
}

// selects compiles the request's selectors the way the API server validates a
// new object, and reports whether the device passes all of them, as the
// scheduler would evaluate them.
func selects(t *testing.T, selectors []resourceapi.DeviceSelector, device selectorDevice) bool {
	t.Helper()
	compiler := dracel.GetCompiler(dracel.Features{EnableConsumableCapacity: true})
	for _, selector := range selectors {
		result := compiler.CompileCELExpression(selector.CEL.Expression, dracel.Options{EnvType: ptr.To(environment.NewExpressions)})
		require.Nil(t, result.Error, "compile %q", selector.CEL.Expression)
		matches, _, err := result.DeviceMatches(context.Background(), device.cel())
		require.NoError(t, err, "evaluate %q", selector.CEL.Expression)
		if !matches {
			return false
		}
	}
	return true
}

func TestDeviceSelectorsMatchCaseInsensitiveSubstrings(t *testing.T) {
	var (
		a10     = selectorDevice{"NVIDIA A10", "GPU-1d2e3f40-0000-0000-0000-00000000a010"}
		a100    = selectorDevice{"NVIDIA A100-SXM4-80GB", "GPU-1d2e3f40-0000-0000-0000-00000000a100"}
		geforce = selectorDevice{"NVIDIA GeForce RTX 4090", "GPU-5e4b0000-0000-0000-0000-000000004090"}
		// Published by a driver before verbatim UUIDs.
		legacy  = selectorDevice{"NVIDIA H100 80GB HBM3", "gpu-77770000-0000-0000-0000-00000000h100"}
		unnamed = selectorDevice{}
	)
	all := []selectorDevice{a10, a100, geforce, legacy, unnamed}

	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        []selectorDevice
	}{
		{"no annotation", nil, all},
		{"short type name", map[string]string{util.PodIncludeGpuTypeAnnotation: "A10"}, []selectorDevice{a10, a100}},
		{"mixed case name in any case", map[string]string{util.PodIncludeGpuTypeAnnotation: "geforce rtx"}, []selectorDevice{geforce}},
		{"several types, blanks ignored", map[string]string{util.PodIncludeGpuTypeAnnotation: " a100 , ,h100,"}, []selectorDevice{a100, legacy}},
		{"exclude type", map[string]string{util.PodExcludeGpuTypeAnnotation: "A100"}, []selectorDevice{a10, geforce, legacy, unnamed}},
		{"include and exclude both apply", map[string]string{
			util.PodIncludeGpuTypeAnnotation: "A10",
			util.PodExcludeGpuTypeAnnotation: "A100",
		}, []selectorDevice{a10}},
		{"annotation without entries is ignored", map[string]string{
			util.PodIncludeGpuTypeAnnotation: " , ",
			util.PodExcludeGpuTypeAnnotation: ",",
		}, all},
		{"full uuid", map[string]string{util.PodIncludeGPUUUIDAnnotation: "GPU-5e4b0000-0000-0000-0000-000000004090"}, []selectorDevice{geforce}},
		{"uuid prefix in lowercase", map[string]string{util.PodIncludeGPUUUIDAnnotation: "gpu-1d2e3f40"}, []selectorDevice{a10, a100}},
		{"uuid of a lowercased slice", map[string]string{util.PodIncludeGPUUUIDAnnotation: "GPU-77770000"}, []selectorDevice{legacy}},
		{"exclude uuid", map[string]string{util.PodExcludeGPUUUIDAnnotation: "GPU-1d2e3f40-0000-0000-0000-00000000A100"}, []selectorDevice{a10, geforce, legacy, unnamed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: tc.annotations}}
			request := BuildDeviceRequest(pod, "vgpu-manager", ResourceInfo{})
			var got []selectorDevice
			for _, device := range all {
				if selects(t, request.Exactly.Selectors, device) {
					got = append(got, device)
				}
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// Entries are quoted, so an annotation cannot change the expression.
func TestDeviceSelectorQuotesEntries(t *testing.T) {
	selector := attributeSubstringSelector("productName", `A10"] || true || ["`, false)
	require.NotNil(t, selector)
	require.False(t, selects(t, []resourceapi.DeviceSelector{*selector}, selectorDevice{productName: "NVIDIA A100"}))
}
