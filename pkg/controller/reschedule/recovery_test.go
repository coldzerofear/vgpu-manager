/*
Copyright 2025-2026 coldzerofear

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

package reschedule

import (
	"os"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testDomain is installed process-wide (the global domain is set once, the way
// main does it from --domain), so the cleanup keys in this package are only
// correct if they are read after that point.
const testDomain = "example.com"

func TestMain(m *testing.M) {
	util.MustInitGlobalDomain(testDomain)
	os.Exit(m.Run())
}

func TestCleanupMetadata(t *testing.T) {
	// The keys follow the configured domain, not the built-in default.
	require.True(t, strings.HasPrefix(util.PodAssignedPhaseLabel, testDomain+"/"))
	require.True(t, strings.HasPrefix(util.PodVGPUPreAllocAnnotation, testDomain+"/"))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
			Labels: map[string]string{
				util.PodAssignedPhaseLabel: "success",
				util.PodMetricsNodeLabel:   "node1",
				"app":                      "test",
			},
			Annotations: map[string]string{
				util.PodVGPUPreAllocAnnotation:  "0,0,GPU-uuid",
				util.PodVGPURealAllocAnnotation: "0,0,GPU-uuid",
				util.PodPredicateNodeAnnotation: "node1",
				util.PodPredicateTimeAnnotation: "1758000000",
				util.DRAOriResAnnotation:        "{}",
				// A key of the default domain is not ours once --domain is set.
				util.NvidiaDomain + "/predicate-node": "node2",
				"other.io/keep":                       "keep",
			},
		},
	}

	CleanupMetadata(pod)

	// Asserted on the keys themselves, not on the package's own key list: a
	// list that drifted with the domain would otherwise agree with the bug.
	for _, label := range []string{util.PodAssignedPhaseLabel, util.PodMetricsNodeLabel} {
		_, ok := util.HasLabel(pod, label)
		require.False(t, ok, "label %s should have been removed", label)
	}
	for _, anno := range []string{
		util.PodVGPUPreAllocAnnotation, util.PodVGPURealAllocAnnotation,
		util.PodPredicateNodeAnnotation, util.PodPredicateTimeAnnotation,
	} {
		_, ok := util.HasAnnotation(pod, anno)
		require.False(t, ok, "annotation %s should have been removed", anno)
	}
	require.Equal(t, "test", pod.Labels["app"])
	require.Equal(t, "keep", pod.Annotations["other.io/keep"])
	require.Equal(t, "node2", pod.Annotations[util.NvidiaDomain+"/predicate-node"])
	// CleanupMetadata leaves the DRA annotation to CleanupDRAMetadata.
	require.Equal(t, "{}", pod.Annotations[util.DRAOriResAnnotation])
}

func TestCleanupDRAMetadata(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
			Annotations: map[string]string{
				util.DRAOriResAnnotation: "{}",
				"other.io/keep":          "keep",
			},
		},
	}

	CleanupDRAMetadata(pod)

	_, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	require.False(t, ok)
	require.Equal(t, "keep", pod.Annotations["other.io/keep"])
}

// Cleaning a pod without labels or annotations must not panic.
func TestCleanupMetadataEmpty(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-pod"}}
	CleanupMetadata(pod)
	CleanupDRAMetadata(pod)
	require.Empty(t, pod.Labels)
	require.Empty(t, pod.Annotations)
}
