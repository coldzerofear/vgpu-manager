package validate

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNS = "default"

// newTestScheme builds a scheme that includes core k8s types and DRA resource types.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, k8sscheme.AddToScheme(s))
	require.NoError(t, resourceapi.AddToScheme(s))
	return s
}

// newHandle creates a validateHandle backed by a fake client pre-populated with objs.
func newHandle(t *testing.T, objs ...client.Object) *validateHandle {
	t.Helper()
	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		Build()
	return &validateHandle{
		options: &options.Options{VGPUDeviceClassName: util.VGPUDeviceClassName},
		client:  fakeClient,
	}
}

// vgpuClaim creates a ResourceClaim whose requests all use the vGPU device class.
func vgpuClaim(name string, requestNames ...string) *resourceapi.ResourceClaim {
	reqs := make([]resourceapi.DeviceRequest, len(requestNames))
	for i, rn := range requestNames {
		reqs[i] = resourceapi.DeviceRequest{
			Name: rn,
			Exactly: &resourceapi.ExactDeviceRequest{
				DeviceClassName: util.VGPUDeviceClassName,
			},
		}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Requests: reqs},
		},
	}
}

// podClaim creates a pod-level ResourceClaim entry that points to an actual ResourceClaim.
func podClaim(podClaimName, actualClaimName string) corev1.PodResourceClaim {
	return corev1.PodResourceClaim{
		Name:              podClaimName,
		ResourceClaimName: ptr.To(actualClaimName),
	}
}

// claimRef creates a container-level claim reference, optionally scoped to a specific request.
func claimRef(podClaimName, request string) corev1.ResourceClaim {
	return corev1.ResourceClaim{Name: podClaimName, Request: request}
}

// cont creates a container with the given claim references.
func cont(name string, refs ...corev1.ResourceClaim) corev1.Container {
	return corev1.Container{
		Name:      name,
		Resources: corev1.ResourceRequirements{Claims: refs},
	}
}

// sidecarCont is an init container with restartPolicy: Always (a sidecar),
// which runs concurrently with the app containers.
func sidecarCont(name string, refs ...corev1.ResourceClaim) corev1.Container {
	c := cont(name, refs...)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// pod assembles a Pod from the given pod-level claims and init/app containers.
func pod(podClaims []corev1.PodResourceClaim, inits, apps []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: testNS},
		Spec: corev1.PodSpec{
			ResourceClaims: podClaims,
			InitContainers: inits,
			Containers:     apps,
		},
	}
}

func TestCheckResourceClaimRequests(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		objs    []client.Object // ResourceClaims pre-created in the fake API server
		pod     *corev1.Pod
		wantErr string // substring expected in the error; empty means no error
	}{
		// ---------------------------------------------------------------
		// Fast-path: no DRA requests at all
		// ---------------------------------------------------------------
		{
			name: "no resourceClaims on pod: pass",
			pod:  pod(nil, nil, []corev1.Container{cont("app")}),
		},

		// ---------------------------------------------------------------
		// Rule 2: one container, multiple vGPU requests under the same claim
		// ---------------------------------------------------------------
		{
			name: "rule2: app container hits two vGPU requests in one claim: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1", "req2")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Rule 1: one container references more than one vGPU claim → error
		// ---------------------------------------------------------------
		{
			name: "rule1: app container references two distinct vGPU claims: error",
			objs: []client.Object{
				vgpuClaim("claim-x", "req1"),
				vgpuClaim("claim-y", "req2"),
			},
			pod: pod(
				[]corev1.PodResourceClaim{
					podClaim("pc-x", "claim-x"),
					podClaim("pc-y", "claim-y"),
				},
				nil,
				[]corev1.Container{cont("app-a",
					claimRef("pc-x", ""),
					claimRef("pc-y", ""),
				)},
			),
			wantErr: "references multiple vgpu claims",
		},

		// ---------------------------------------------------------------
		// Sequential (non-restartable) init containers never overlap, so any
		// number of them may share (sequentially reuse) one vGPU request.
		// ---------------------------------------------------------------
		{
			name: "two sequential init containers share one vGPU request: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{
					cont("init-a", claimRef("pc-x", "")),
					cont("init-b", claimRef("pc-x", "")),
				},
				nil,
			),
		},
		{
			name: "multiple sequential init containers and one app share one vGPU request: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{
					cont("init-a", claimRef("pc-x", "")),
					cont("init-b", claimRef("pc-x", "")),
				},
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Rule 4: two app containers share the same vGPU request → error
		// ---------------------------------------------------------------
		{
			name: "rule4: two app containers share one vGPU request: error",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{
					cont("app-a", claimRef("pc-x", "")),
					cont("app-b", claimRef("pc-x", "")),
				},
			),
			wantErr: "referenced by multiple app containers",
		},

		// ---------------------------------------------------------------
		// Rule 5: init and app share the same vGPU request → pass
		// ---------------------------------------------------------------
		{
			name: "rule5: init and app containers share one vGPU request: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{cont("init-a", claimRef("pc-x", ""))},
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// A sidecar (restartable init) overlaps the app phase and every later
		// init container, so it must be the SOLE user of its vGPU request.
		// Sharing with an app container → error.
		// ---------------------------------------------------------------
		{
			name: "sidecar and app containers share one vGPU request: error",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{sidecarCont("side-a", claimRef("pc-x", ""))},
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
			wantErr: "must be the sole user",
		},
		// Sharing with an init container → error (the sidecar may overlap that
		// init, so strict non-overlap forbids it).
		{
			name: "sidecar and init container share one vGPU request: error",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{
					sidecarCont("side-a", claimRef("pc-x", "")),
					cont("init-a", claimRef("pc-x", "")),
				},
				nil,
			),
			wantErr: "must be the sole user",
		},
		// Two sidecars sharing one request → error (concurrent).
		{
			name: "two sidecars share one vGPU request: error",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{
					sidecarCont("side-a", claimRef("pc-x", "")),
					sidecarCont("side-b", claimRef("pc-x", "")),
				},
				nil,
			),
			wantErr: "multiple sidecar containers",
		},
		// ---------------------------------------------------------------
		// A non-restartable init container may still share with an app
		// container even when an unrelated sidecar exists on the pod.
		// ---------------------------------------------------------------
		{
			name: "non-restartable init shares with app; unrelated sidecar present: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{
					sidecarCont("side-a"),
					cont("init-a", claimRef("pc-x", "")),
				},
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Rule 7 — error:
		// init-a takes ALL requests of claim-x (req1 + req2);
		// app-a takes only req1, app-b takes only req2.
		// init-a now overlaps with two distinct app containers → error.
		// ---------------------------------------------------------------
		{
			name: "rule7: init overlaps two app containers via different requests of the same claim: error",
			objs: []client.Object{vgpuClaim("claim-x", "req1", "req2")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{cont("init-a", claimRef("pc-x", ""))},
				[]corev1.Container{
					cont("app-a", claimRef("pc-x", "req1")),
					cont("app-b", claimRef("pc-x", "req2")),
				},
			),
			wantErr: "overlaps vgpu requests with multiple app containers",
		},

		// ---------------------------------------------------------------
		// Rule 7 — pass: init overlaps ALL requests of the same single app container
		// ---------------------------------------------------------------
		{
			name: "rule7: init overlaps all requests of the same app container (full overlap): pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1", "req2")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				[]corev1.Container{cont("init-a", claimRef("pc-x", ""))},
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// Rule 7 — pass: init overlaps a subset of one app container's requests (partial overlap)
		{
			name: "rule7: init partially overlaps one app container (subset of requests): pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1", "req2")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				// init-a only takes req1 specifically
				[]corev1.Container{cont("init-a", claimRef("pc-x", "req1"))},
				// app-a takes all (req1 + req2)
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// Rule 7 — pass: multiple init containers, each overlaps a different single app container
		{
			name: "rule7: init-a overlaps only app-a, init-b overlaps only app-b: pass",
			objs: []client.Object{
				vgpuClaim("claim-x", "req1"),
				vgpuClaim("claim-y", "req2"),
			},
			pod: pod(
				[]corev1.PodResourceClaim{
					podClaim("pc-x", "claim-x"),
					podClaim("pc-y", "claim-y"),
				},
				[]corev1.Container{
					cont("init-a", claimRef("pc-x", "")),
					cont("init-b", claimRef("pc-y", "")),
				},
				[]corev1.Container{
					cont("app-a", claimRef("pc-x", "")),
					cont("app-b", claimRef("pc-y", "")),
				},
			),
		},

		// Rule 7 — pass: init and app use completely disjoint request keys (no overlap at all)
		{
			name: "rule7: init and app use entirely separate claim requests (no overlap): pass",
			objs: []client.Object{
				vgpuClaim("claim-x", "req1"),
				vgpuClaim("claim-y", "req2"),
			},
			pod: pod(
				[]corev1.PodResourceClaim{
					podClaim("pc-x", "claim-x"),
					podClaim("pc-y", "claim-y"),
				},
				[]corev1.Container{cont("init-a", claimRef("pc-x", ""))},
				[]corev1.Container{cont("app-a", claimRef("pc-y", ""))},
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandle(t, tt.objs...)
			err := h.checkResourceClaimRequests(ctx, tt.pod)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// existingPod creates a pre-existing pod (already running in the cluster) with a given name.
func existingPod(name string, podClaims []corev1.PodResourceClaim, inits, apps []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: corev1.PodSpec{
			ResourceClaims: podClaims,
			InitContainers: inits,
			Containers:     apps,
		},
	}
}

// vgpuClaimReserved creates a vGPU ResourceClaim with status.reservedFor pointing to podName.
func vgpuClaimReserved(name, podName string, requestNames ...string) *resourceapi.ResourceClaim {
	claim := vgpuClaim(name, requestNames...)
	claim.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{
		{Resource: "pods", Name: podName},
	}
	return claim
}

func TestCheckCrossPodsVGPURequestConflict(t *testing.T) {
	ctx := context.Background()

	// podWithTemplateClaim builds an existing pod that referenced a claim via template;
	// the resolved actual claim name is recorded in status.resourceClaimStatuses.
	podWithTemplateClaim := func(name, podClaimName, templateName, resolvedClaimName string,
		apps []corev1.Container) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec: corev1.PodSpec{
				ResourceClaims: []corev1.PodResourceClaim{{
					Name:                      podClaimName,
					ResourceClaimTemplateName: ptr.To(templateName),
				}},
				Containers: apps,
			},
			Status: corev1.PodStatus{
				ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{
					Name:              podClaimName,
					ResourceClaimName: ptr.To(resolvedClaimName),
				}},
			},
		}
	}

	tests := []struct {
		name    string
		objs    []client.Object
		pod     *corev1.Pod
		wantErr string
	}{
		// ---------------------------------------------------------------
		// Fast-path: no DRA requests at all
		// ---------------------------------------------------------------
		{
			name: "no DRA requests: pass",
			pod:  pod(nil, nil, []corev1.Container{cont("app")}),
		},

		// ---------------------------------------------------------------
		// Template claims are per-pod and cannot be shared → skip
		// ---------------------------------------------------------------
		{
			name: "template claim only: pass",
			pod: func() *corev1.Pod {
				p := pod(nil, nil, []corev1.Container{cont("app-a", claimRef("pc-x", ""))})
				p.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name:                      "pc-x",
					ResourceClaimTemplateName: ptr.To("my-template"),
				}}
				return p
			}(),
		},

		// ---------------------------------------------------------------
		// Named claim does not exist yet (will be created later) → skip
		// ---------------------------------------------------------------
		{
			name: "named claim not yet created: pass",
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Named claim exists but has no reservedFor entries → nothing to compare
		// ---------------------------------------------------------------
		{
			name: "named claim with no reservedFor: pass",
			objs: []client.Object{vgpuClaim("claim-x", "req1")},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Named claim with reservedFor but requests are non-vGPU → skip
		// ---------------------------------------------------------------
		{
			name: "non-vGPU named claim with reservedFor: pass",
			objs: []client.Object{
				func() *resourceapi.ResourceClaim {
					c := &resourceapi.ResourceClaim{
						ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "claim-x"},
						Spec: resourceapi.ResourceClaimSpec{
							Devices: resourceapi.DeviceClaim{
								Requests: []resourceapi.DeviceRequest{{
									Name:    "req1",
									Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: "some-other-class"},
								}},
							},
						},
					}
					c.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{
						{Resource: "pods", Name: "existing-pod"},
					}
					return c
				}(),
				existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Both pods reference all requests of the same claim → error
		// ---------------------------------------------------------------
		{
			name: "cross-pod: both pods use all requests of the same claim: error",
			objs: []client.Object{
				vgpuClaimReserved("claim-x", "existing-pod", "req1"),
				existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
			wantErr: "cross-pod sharing of the same vgpu request is not allowed",
		},

		// ---------------------------------------------------------------
		// Both pods use the same specific scoped request → error
		// ---------------------------------------------------------------
		{
			name: "cross-pod: both pods use the same specific request: error",
			objs: []client.Object{
				vgpuClaimReserved("claim-x", "existing-pod", "req1", "req2"),
				existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", "req1"))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", "req1"))},
			),
			wantErr: "cross-pod sharing of the same vgpu request is not allowed",
		},

		// ---------------------------------------------------------------
		// Pods use different specific requests from the same claim → pass
		// ---------------------------------------------------------------
		{
			name: "cross-pod: pods use different specific requests: pass",
			objs: []client.Object{
				vgpuClaimReserved("claim-x", "existing-pod", "req1", "req2"),
				existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", "req1"))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", "req2"))},
			),
		},

		// ---------------------------------------------------------------
		// New pod references the claim in spec.resourceClaims but no
		// container actually has a claimRef to it → newPodReqs is empty → pass
		// ---------------------------------------------------------------
		{
			name: "new pod references claim but no container uses it: pass",
			objs: []client.Object{
				vgpuClaimReserved("claim-x", "existing-pod", "req1"),
				existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a")}, // no claim refs in container
			),
		},

		// ---------------------------------------------------------------
		// Stale reservation: ref.UID does not match the existing pod's UID → skip
		// ---------------------------------------------------------------
		{
			name: "stale UID in reservedFor: pass",
			objs: func() []client.Object {
				claim := vgpuClaim("claim-x", "req1")
				claim.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{
					{Resource: "pods", Name: "existing-pod", UID: "stale-uid"},
				}
				ep := existingPod("existing-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
				)
				ep.UID = "current-uid"
				return []client.Object{claim, ep}
			}(),
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Self-reference: reserved pod has the same name as the new pod → skip
		// ---------------------------------------------------------------
		{
			name: "self-reference in reservedFor: pass",
			objs: []client.Object{
				// "test-pod" is the name the `pod` helper always sets
				vgpuClaimReserved("claim-x", "test-pod", "req1"),
				existingPod("test-pod",
					[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
					nil,
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
				),
			},
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
		},

		// ---------------------------------------------------------------
		// Existing pod referenced the claim via template (resolved name in status)
		// → findPodClaimNameForActualClaim must detect it → conflict → error
		// ---------------------------------------------------------------
		{
			name: "existing pod used template-resolved claim: error",
			objs: func() []client.Object {
				claim := vgpuClaim("claim-x", "req1")
				claim.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{
					{Resource: "pods", Name: "existing-pod"},
				}
				ep := podWithTemplateClaim("existing-pod", "pc-x", "my-template", "claim-x",
					[]corev1.Container{cont("app-a", claimRef("pc-x", ""))})
				return []client.Object{claim, ep}
			}(),
			pod: pod(
				[]corev1.PodResourceClaim{podClaim("pc-x", "claim-x")},
				nil,
				[]corev1.Container{cont("app-a", claimRef("pc-x", ""))},
			),
			wantErr: "cross-pod sharing of the same vgpu request is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandle(t, tt.objs...)
			err := h.checkCrossPodsVGPURequestConflict(ctx, tt.pod)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}
