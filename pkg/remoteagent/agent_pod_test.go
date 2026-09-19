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

package remoteagent

import (
	"context"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

// podClaims is what the scheduler pre-allocates to a two-container remote pod.
func podClaims() device.PodDeviceClaim {
	return device.PodDeviceClaim{
		{Name: "init", DeviceClaims: []device.DeviceClaim{{Id: 1, Uuid: testGPU1, Cores: 100, Memory: 24576}}},
		{Name: "app", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096}}},
	}
}

// newPodModeAgent is an agent serving pod-owned sessions, with pods in both
// the informer cache and the API.
func newPodModeAgent(t *testing.T, pods ...*corev1.Pod) *Agent {
	t.Helper()
	base := t.TempDir()
	objects := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	kubeClient := fake.NewClientset(objects...)
	a := New(Config{
		NodeName: testNode, SessionBase: base, ContainerManagerDir: base,
		SessionOwnerKind: OwnerPod, ServerEndpoint: "127.0.0.1:14833",
		ClientSets: pkgflags.ClientSets{Core: kubeClient},
	})
	require.NoError(t, a.store.Prepare())
	nd, err := NodeDevicesFromNode(testServerNode(t, 1))
	require.NoError(t, err)
	a.podDevices.Store(nd)

	factory := informers.NewSharedInformerFactory(kubeClient, 0)
	a.podInformer = factory.Core().V1().Pods().Informer()
	require.NoError(t, a.podInformer.AddIndexers(podIndexers()))
	a.podCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(), a.podInformer.GetStore(), a.podInformer.GetIndexer(), time.Minute, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.podInformer.RunWithContext(ctx)
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	defer syncCancel()
	require.True(t, cache.WaitForCacheSync(syncCtx.Done(), a.podInformer.HasSynced))
	return a
}

func TestPodForSession(t *testing.T) {
	pod := testRemotePod(t, podClaims())
	a := newPodModeAgent(t, pod)
	ctx := context.Background()
	appToken := remotegpu.SessionToken(string(pod.UID), "app")

	got, container, err := a.podForSession(ctx, appToken, string(pod.UID), pod.Namespace, pod.Name, pod.ResourceVersion)
	require.NoError(t, err)
	assert.Equal(t, pod.UID, got.UID)
	assert.Equal(t, "app", container)

	// Every container the scheduler gave devices to has its own session.
	_, container, err = a.podForSession(ctx, remotegpu.SessionToken(string(pod.UID), "init"), string(pod.UID), pod.Namespace, pod.Name, "")
	require.NoError(t, err)
	assert.Equal(t, "init", container)

	// A token nobody issued, and a token of another pod's container.
	for _, token := range []string{
		remotegpu.SessionToken(string(pod.UID), "sidecar"),
		remotegpu.SessionToken("other-uid", "app"),
	} {
		_, _, err = a.podForSession(ctx, token, string(pod.UID), pod.Namespace, pod.Name, "")
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "token %s", token)
	}

	_, _, err = a.podForSession(ctx, appToken, "unknown-uid", pod.Namespace, "missing", "")
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestPodForSessionRejectsPodsOfOtherNodes(t *testing.T) {
	elsewhere := testRemotePod(t, podClaims())
	elsewhere.Annotations[util.PodPredicateNodeAnnotation] = "another-gpu-node"
	local := testRemotePod(t, podClaims())
	local.Name, local.UID = "local-pod", "local-uid"
	delete(local.Annotations, util.VGPUAccessModeAnnotation) // a local pod, not a remote one
	finished := testRemotePod(t, podClaims())
	finished.Name, finished.UID = "finished-pod", "finished-uid"
	finished.Status.Phase = corev1.PodSucceeded
	a := newPodModeAgent(t, elsewhere, local, finished)

	for _, pod := range []*corev1.Pod{elsewhere, local, finished} {
		_, _, err := a.podForSession(context.Background(), remotegpu.SessionToken(string(pod.UID), "app"),
			string(pod.UID), pod.Namespace, pod.Name, "")
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "pod %s", pod.Name)
	}
}

func TestEnsurePodSessionAndSweep(t *testing.T) {
	pod := testRemotePod(t, podClaims())
	a := newPodModeAgent(t, pod)
	ctx := context.Background()
	appToken := remotegpu.SessionToken(string(pod.UID), "app")
	initToken := remotegpu.SessionToken(string(pod.UID), "init")
	ensure := func(token string) error {
		return a.ensurePodSession(ctx, &remoteagent.EnsureSessionRequest{
			Session: token, ClaimUid: string(pod.UID),
			ClaimNamespace: pod.Namespace, ClaimName: pod.Name,
		}, a.podDevices.Load())
	}

	require.NoError(t, ensure(appToken))
	require.NoError(t, ensure(initToken))
	assert.ElementsMatch(t, []string{appToken, initToken}, a.store.TokensOfOwner(string(pod.UID)))
	require.NoError(t, ensure(appToken), "a repeated request is a no-op")

	// A pod that is only being deleted keeps its sessions: its containers run
	// until the object is gone.
	deleting := pod.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.ResourceVersion = "43"
	a.sweepPod(deleting)
	assert.Len(t, a.store.TokensOfOwner(string(pod.UID)), 2)

	// Finished: nothing of it may keep a session.
	done := pod.DeepCopy()
	done.Status.Phase = corev1.PodSucceeded
	done.ResourceVersion = "44"
	a.sweepPod(done)
	assert.Empty(t, a.store.TokensOfOwner(string(pod.UID)))
}

// A session of the other owner kind was left by an earlier configuration of
// this node and is swept.
func TestGCSessionsRemovesOtherOwnerKind(t *testing.T) {
	pod := testRemotePod(t, podClaims())
	a := newPodModeAgent(t, pod)
	claimSpec := SessionSpec{
		Owner:       SessionOwner{Kind: OwnerClaim, UID: "claim-uid", Namespace: "ns", Name: "claim", Version: 7},
		Infos:       []device.DeviceClaim{{Id: 0, Uuid: testGPU0, Cores: 100, Memory: 12288}},
		Claims:      []device.DeviceClaim{{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096}},
		MemoryRatio: 1,
	}
	require.NoError(t, a.store.Materialize("claimtoken", claimSpec, a.podDevices.Load()))
	require.NoError(t, a.ensurePodSession(context.Background(), &remoteagent.EnsureSessionRequest{
		Session: remotegpu.SessionToken(string(pod.UID), "app"), ClaimUid: string(pod.UID),
		ClaimNamespace: pod.Namespace, ClaimName: pod.Name,
	}, a.podDevices.Load()))

	a.gcSessions(context.Background())

	assert.Empty(t, a.store.TokensOfOwner("claim-uid"), "a claim session cannot be valid in pod mode")
	assert.Len(t, a.store.TokensOfOwner(string(pod.UID)), 1, "the pod's own session stays")
}
