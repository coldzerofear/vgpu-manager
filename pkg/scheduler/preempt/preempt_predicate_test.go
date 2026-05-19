package preempt

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

// newTestNode builds a 2-device vGPU node with NodeConfigInfo and
// NodeDeviceRegister annotations populated so that device.NewNodeInfo can
// parse them. Each device is 1 vGPU slot, so total node capacity is 2 vGPU.
// The returned device UUID slice (length 2, indexed by device Id) lets test
// callers wire up PodVGPUPreAllocAnnotation against the right devices.
func newTestNode(name string) (*corev1.Node, []string) {
	nodeConfig := device.NodeConfigInfo{
		DeviceSplit:   1,
		CoresScaling:  1,
		MemoryFactor:  1,
		MemoryScaling: 1,
	}
	encodedConfig, _ := nodeConfig.Encode()
	uuid0 := "GPU-" + uuid.NewString()
	uuid1 := "GPU-" + uuid.NewString()
	devs := device.NodeDeviceInfo{
		{
			Id:         0,
			Uuid:       uuid0,
			Core:       util.HundredCore,
			Memory:     12288,
			Type:       "TestGPU",
			Number:     1,
			Numa:       0,
			Capability: 8.0,
			Healthy:    true,
		},
		{
			Id:         1,
			Uuid:       uuid1,
			Core:       util.HundredCore,
			Memory:     12288,
			Type:       "TestGPU",
			Number:     1,
			Numa:       0,
			Capability: 8.0,
			Healthy:    true,
		},
	}
	encodedDevs, _ := devs.Encode()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				util.NodeDeviceRegisterAnnotation: encodedDevs,
				util.NodeConfigInfoAnnotation:     encodedConfig,
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("2"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("2"),
			},
		},
	}
	return node, []string{uuid0, uuid1}
}

// allocatePodOn wires up the PodVGPUPreAllocAnnotation +
// PodPredicateNodeAnnotation so that device.NewNodeInfo's
// addPodUsedResources accounts for this pod's consumption when building a
// node snapshot. Without these annotations the allocator would see the node
// as empty regardless of who's running on it.
func allocatePodOn(pod *corev1.Pod, nodeName string, deviceID int, deviceUUID string) {
	claim := device.DeviceClaim{
		Id:     deviceID,
		Uuid:   deviceUUID,
		Cores:  util.HundredCore,
		Memory: 12288,
	}
	pdc := device.PodDeviceClaim{{
		Name:         pod.Spec.Containers[0].Name,
		DeviceClaims: []device.DeviceClaim{claim},
	}}
	preAlloc, _ := pdc.MarshalText()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[util.PodVGPUPreAllocAnnotation] = preAlloc
	pod.Annotations[util.PodPredicateNodeAnnotation] = nodeName
}

type podOpt func(*corev1.Pod)

func withPriority(p int32) podOpt {
	return func(pod *corev1.Pod) {
		v := p
		pod.Spec.Priority = &v
	}
}

func withPhase(phase corev1.PodPhase) podOpt {
	return func(pod *corev1.Pod) {
		pod.Status.Phase = phase
	}
}

func withDeletionTimestamp() podOpt {
	return func(pod *corev1.Pod) {
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
	}
}

func withOwner(kind string) podOpt {
	return func(pod *corev1.Pod) {
		ctrl := true
		pod.OwnerReferences = []metav1.OwnerReference{{
			Kind:       kind,
			Name:       "test-owner",
			UID:        k8stypes.UID(uuid.NewString()),
			APIVersion: "apps/v1",
			Controller: &ctrl,
		}}
	}
}

func withMirrorAnnotation() podOpt {
	return func(pod *corev1.Pod) {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations["kubernetes.io/config.mirror"] = "static-mirror-hash"
		pod.Annotations["kubernetes.io/config.source"] = "file"
	}
}

func withNodeName(node string) podOpt {
	return func(pod *corev1.Pod) {
		pod.Spec.NodeName = node
	}
}

// newVGPUPod returns a bound (or unbound, via withNodeName) vGPU pod that
// requests vGPUNumber vGPU slots, with sensible defaults that the allocator
// can process.
func newVGPUPod(name, ns string, vGPUNumber int64, opts ...podOpt) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       k8stypes.UID(uuid.NewString()),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "c1",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", vGPUNumber)),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(pod)
	}
	return pod
}

// newPlainPod returns a non-vGPU pod. Used to ensure passthrough works for
// pods outside our scope.
func newPlainPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       k8stypes.UID(uuid.NewString()),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// newPreemptPluginWithSync builds a vgpuPreempt plugin backed by a fake
// clientset prepopulated with the given pods and nodes. Informers are started
// and synced before returning so listers contain the fixture objects.
func newPreemptPluginWithSync(t *testing.T, pods []*corev1.Pod, nodes []*corev1.Node) (*vgpuPreempt, context.CancelFunc) {
	t.Helper()
	objs := make([]runtime.Object, 0, len(pods)+len(nodes))
	for _, p := range pods {
		objs = append(objs, p)
	}
	for _, n := range nodes {
		objs = append(objs, n)
	}
	k8sClient := fake.NewClientset(objs...)
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})

	// We need the filter plugin's PodLister so the indexer
	// IndexerKeyPodRequestVGPU is registered on the shared informer.
	filterPlugin, err := filter.New(k8sClient, factory, recorder, false)
	if err != nil {
		t.Fatalf("filter.New: %v", err)
	}
	plugin, err := New(factory, recorder, filterPlugin.GetPodLister())
	if err != nil {
		t.Fatalf("preempt.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	cleanup := func() {
		cancel()
		broadcaster.Shutdown()
	}
	return plugin, cleanup
}

// metaUIDs extracts UIDs from a MetaVictims for assertion-friendly comparison.
func metaUIDs(m *extenderv1.MetaVictims) []string {
	out := make([]string, 0, len(m.Pods))
	for _, p := range m.Pods {
		out = append(out, p.UID)
	}
	return out
}

// ---------------------------------------------------------------------------
// isProtectedFromPreemption — pure unit tests
// ---------------------------------------------------------------------------

func Test_isProtectedFromPreemption(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "normal running vGPU pod is not protected",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1")),
			want: false,
		},
		{
			name: "DeletionTimestamp set is protected (already terminating)",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withDeletionTimestamp()),
			want: true,
		},
		{
			name: "phase Succeeded is protected",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withPhase(corev1.PodSucceeded)),
			want: true,
		},
		{
			name: "phase Failed is protected",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withPhase(corev1.PodFailed)),
			want: true,
		},
		{
			name: "mirror pod is protected (via IsCriticalPod)",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withMirrorAnnotation()),
			want: true,
		},
		{
			name: "system-critical priority is protected (via IsCriticalPod)",
			// SystemCriticalPriority = 2_000_000_000
			pod:  newVGPUPod("p", "ns", 1, withPriority(2_000_000_000), withNodeName("node1")),
			want: true,
		},
		{
			name: "DaemonSet-owned pod is protected",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withOwner("DaemonSet")),
			want: true,
		},
		{
			name: "ReplicaSet-owned pod is NOT protected (default replicas)",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withOwner("ReplicaSet")),
			want: false,
		},
		{
			name: "StatefulSet-owned pod is NOT protected (matches K8s default)",
			pod:  newVGPUPod("p", "ns", 1, withPriority(10), withNodeName("node1"), withOwner("StatefulSet")),
			want: false,
		},
		{
			name: "pre-bind window: NodeName empty + ShouldCountPodDeviceAllocation true is protected",
			pod: func() *corev1.Pod {
				p := newVGPUPod("p", "ns", 1, withPriority(10))
				p.Spec.NodeName = ""
				// Make ShouldCountPodDeviceAllocation return true: no condition,
				// no phase=Failed label.
				return p
			}(),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isProtectedFromPreemption(tt.pod))
		})
	}
}

// ---------------------------------------------------------------------------
// sortVictimsByPreference — pure unit tests
// ---------------------------------------------------------------------------

func Test_sortVictimsByPreference(t *testing.T) {
	now := time.Now()
	mkPod := func(name string, prio int32, createdAgo time.Duration) *corev1.Pod {
		p := newVGPUPod(name, "ns", 1, withPriority(prio), withNodeName("node1"))
		p.CreationTimestamp = metav1.NewTime(now.Add(-createdAgo))
		return p
	}

	t.Run("lower priority comes first", func(t *testing.T) {
		high := mkPod("high", 100, time.Minute)
		low := mkPod("low", 10, time.Minute)
		mid := mkPod("mid", 50, time.Minute)
		pods := []*corev1.Pod{high, low, mid}
		sortVictimsByPreference(pods)
		assert.Equal(t, "low", pods[0].Name, "lowest priority should be first")
		assert.Equal(t, "mid", pods[1].Name)
		assert.Equal(t, "high", pods[2].Name)
	})

	t.Run("equal priority: newer creation time comes first", func(t *testing.T) {
		older := mkPod("older", 10, time.Hour)
		newer := mkPod("newer", 10, time.Second)
		pods := []*corev1.Pod{older, newer}
		sortVictimsByPreference(pods)
		assert.Equal(t, "newer", pods[0].Name, "newer pod (less invested work) preferred for eviction")
		assert.Equal(t, "older", pods[1].Name)
	})

	t.Run("empty slice does not panic", func(t *testing.T) {
		var pods []*corev1.Pod
		assert.NotPanics(t, func() { sortVictimsByPreference(pods) })
	})
}

// ---------------------------------------------------------------------------
// resolveVictimsMap — input mode normalization
// ---------------------------------------------------------------------------

func Test_resolveVictimsMap(t *testing.T) {
	t.Run("NodeNameToVictims is returned directly when set", func(t *testing.T) {
		plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
		defer cleanup()

		pod := newVGPUPod("victim", "ns", 1, withPriority(5), withNodeName("n1"))
		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToVictims: map[string]*extenderv1.Victims{
				"n1": {Pods: []*corev1.Pod{pod}, NumPDBViolations: 0},
			},
		}
		got, err := plugin.resolveVictimsMap(args)
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, pod.UID, got["n1"].Pods[0].UID)
	})

	t.Run("MetaVictims resolve UIDs via lister", func(t *testing.T) {
		pod := newVGPUPod("victim", "ns", 1, withPriority(5), withNodeName("n1"))
		plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{pod}, nil)
		defer cleanup()

		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"n1": {
					Pods:             []*extenderv1.MetaPod{{UID: string(pod.UID)}},
					NumPDBViolations: 0,
				},
			},
		}
		got, err := plugin.resolveVictimsMap(args)
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, pod.UID, got["n1"].Pods[0].UID)
	})

	t.Run("unresolvable UID is silently dropped", func(t *testing.T) {
		realPod := newVGPUPod("victim", "ns", 1, withPriority(5), withNodeName("n1"))
		plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{realPod}, nil)
		defer cleanup()

		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"n1": {
					Pods: []*extenderv1.MetaPod{
						{UID: string(realPod.UID)},
						{UID: "ghost-uid-not-in-lister"},
					},
				},
			},
		}
		got, err := plugin.resolveVictimsMap(args)
		assert.NoError(t, err)
		assert.Len(t, got["n1"].Pods, 1, "ghost UID should be dropped, real one kept")
		assert.Equal(t, realPod.UID, got["n1"].Pods[0].UID)
	})

	t.Run("all UIDs unresolvable: node is omitted entirely", func(t *testing.T) {
		plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
		defer cleanup()

		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"n1": {Pods: []*extenderv1.MetaPod{{UID: "ghost"}}},
			},
		}
		got, err := plugin.resolveVictimsMap(args)
		assert.NoError(t, err)
		assert.Len(t, got, 0, "node with zero resolvable UIDs must be omitted")
	})

	t.Run("both inputs empty returns nil", func(t *testing.T) {
		plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
		defer cleanup()

		got, err := plugin.resolveVictimsMap(extenderv1.ExtenderPreemptionArgs{})
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
}

// ---------------------------------------------------------------------------
// passthrough — both input forms down-convert to MetaVictims output
// ---------------------------------------------------------------------------

func Test_passthrough(t *testing.T) {
	t.Run("NodeNameToVictims down-converted to MetaVictims", func(t *testing.T) {
		pod := newVGPUPod("p", "ns", 1)
		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToVictims: map[string]*extenderv1.Victims{
				"n1": {Pods: []*corev1.Pod{pod}, NumPDBViolations: 3},
			},
		}
		got := passthrough(args)
		assert.Len(t, got.NodeNameToMetaVictims, 1)
		mv := got.NodeNameToMetaVictims["n1"]
		assert.Equal(t, int64(3), mv.NumPDBViolations)
		assert.Equal(t, []string{string(pod.UID)}, metaUIDs(mv))
	})

	t.Run("NodeNameToMetaVictims passed through unchanged", func(t *testing.T) {
		args := extenderv1.ExtenderPreemptionArgs{
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"n1": {Pods: []*extenderv1.MetaPod{{UID: "u1"}}, NumPDBViolations: 7},
			},
		}
		got := passthrough(args)
		assert.Len(t, got.NodeNameToMetaVictims, 1)
		assert.Equal(t, int64(7), got.NodeNameToMetaVictims["n1"].NumPDBViolations)
		assert.Equal(t, []string{"u1"}, metaUIDs(got.NodeNameToMetaVictims["n1"]))
	})
}

// ---------------------------------------------------------------------------
// Preempt — end-to-end behavior
// ---------------------------------------------------------------------------

func Test_Preempt_NilPodPassthrough(t *testing.T) {
	plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
	defer cleanup()
	args := extenderv1.ExtenderPreemptionArgs{
		Pod: nil,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			"n1": {Pods: []*extenderv1.MetaPod{{UID: "u1"}}, NumPDBViolations: 0},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	// passthrough echoes input
	assert.Len(t, res.NodeNameToMetaVictims, 1)
	assert.Equal(t, []string{"u1"}, metaUIDs(res.NodeNameToMetaVictims["n1"]))
}

func Test_Preempt_NonVGPUPodPassthrough(t *testing.T) {
	plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
	defer cleanup()
	args := extenderv1.ExtenderPreemptionArgs{
		Pod: newPlainPod("preemptor", "ns"),
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			"n1": {Pods: []*extenderv1.MetaPod{{UID: "u1"}}, NumPDBViolations: 0},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	assert.Len(t, res.NodeNameToMetaVictims, 1)
	assert.Equal(t, []string{"u1"}, metaUIDs(res.NodeNameToMetaVictims["n1"]))
}

func Test_Preempt_EmptyInput(t *testing.T) {
	plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
	defer cleanup()
	preemptor := newVGPUPod("hi-prio", "ns", 1, withPriority(100))
	res := plugin.Preempt(context.Background(), extenderv1.ExtenderPreemptionArgs{Pod: preemptor})
	assert.Len(t, res.NodeNameToMetaVictims, 0)
}

// Test_Preempt_HappyPath_MetaVictims represents the production wire format
// (NodeCacheCapable: true). The preemptor needs 1 vGPU on a 2-slot node where
// both slots are taken by allocated low-priority pods (one device each).
// In-tree default preemption would propose to evict one of them; our extender
// resolves the UID, verifies via the allocator that the resulting 1 free slot
// is enough, and returns the same victim.
func Test_Preempt_HappyPath_MetaVictims(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	lowA := newVGPUPod("low-a", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowA, node.Name, 0, devUUIDs[0])
	lowB := newVGPUPod("low-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{lowA, lowB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {
				Pods:             []*extenderv1.MetaPod{{UID: string(lowB.UID)}},
				NumPDBViolations: 0,
			},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	if assert.Contains(t, res.NodeNameToMetaVictims, node.Name) {
		assert.Equal(t, []string{string(lowB.UID)}, metaUIDs(res.NodeNameToMetaVictims[node.Name]))
	}
}

// Test_Preempt_HappyPath_FullVictims covers the legacy NodeCacheCapable=false
// wire format. Same expected behavior as the MetaVictims path.
func Test_Preempt_HappyPath_FullVictims(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	lowA := newVGPUPod("low-a", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowA, node.Name, 0, devUUIDs[0])
	lowB := newVGPUPod("low-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{lowA, lowB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToVictims: map[string]*extenderv1.Victims{
			node.Name: {Pods: []*corev1.Pod{lowB}, NumPDBViolations: 0},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	if assert.Contains(t, res.NodeNameToMetaVictims, node.Name) {
		assert.Equal(t, []string{string(lowB.UID)}, metaUIDs(res.NodeNameToMetaVictims[node.Name]))
	}
}

// Test_Preempt_ProtectedVictim_DropsNode: both occupants are DaemonSet-owned,
// so both protected, no additional candidates exist → the node must be
// dropped from the result entirely (scheduler's ignorable=true will skip it).
func Test_Preempt_ProtectedVictim_DropsNode(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	dsA := newVGPUPod("ds-a", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
	allocatePodOn(dsA, node.Name, 0, devUUIDs[0])
	dsB := newVGPUPod("ds-b", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
	allocatePodOn(dsB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{dsA, dsB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {Pods: []*extenderv1.MetaPod{{UID: string(dsB.UID)}}},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	assert.NotContains(t, res.NodeNameToMetaVictims, node.Name,
		"node should be dropped: only DaemonSet candidates exist and they are protected")
}

// Test_Preempt_ProtectedVictim_AddsAdditional: in-tree proposed a DaemonSet
// pod (which we refuse), but a non-protected lower-priority pod is available
// to swap in. The result should contain the replacement, not the original.
func Test_Preempt_ProtectedVictim_AddsAdditional(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	dsA := newVGPUPod("ds-a", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
	allocatePodOn(dsA, node.Name, 0, devUUIDs[0])
	regularB := newVGPUPod("regular-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(regularB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{dsA, regularB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {Pods: []*extenderv1.MetaPod{{UID: string(dsA.UID)}}},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	if assert.Contains(t, res.NodeNameToMetaVictims, node.Name) {
		uids := metaUIDs(res.NodeNameToMetaVictims[node.Name])
		assert.NotContains(t, uids, string(dsA.UID), "DaemonSet pod must be excluded")
		assert.Contains(t, uids, string(regularB.UID), "regular pod must be selected as replacement")
	}
}

// Test_pdbViolationsUpperBound covers the formula in isolation. The formula
// must (1) never under-report (safety property), (2) maintain the invariant
// NumPDBViolations <= len(refined), (3) pass through cleanly when nothing
// changes.
func Test_pdbViolationsUpperBound(t *testing.T) {
	tests := []struct {
		name      string
		original  int64
		keptLen   int
		addedLen  int
		want      int64
	}{
		{
			name:     "no change passes through",
			original: 3, keptLen: 5, addedLen: 0,
			want: 3,
		},
		{
			name:     "drop only: cap at kept length when original exceeds it",
			original: 5, keptLen: 2, addedLen: 0,
			want: 2,
		},
		{
			name:     "drop only: original fits within kept",
			original: 1, keptLen: 3, addedLen: 0,
			want: 1,
		},
		{
			name:     "add only: assume every added victim is a violator",
			original: 2, keptLen: 4, addedLen: 1,
			want: 3, // 2 carried over + 1 added
		},
		{
			name:     "drop AND add: capped carry-over plus added",
			original: 5, keptLen: 1, addedLen: 2,
			want: 3, // min(5, 1) = 1 carried over + 2 added
		},
		{
			name:     "all original violators dropped (worst-case caller assumption)",
			original: 4, keptLen: 0, addedLen: 0,
			want: 0,
		},
		{
			name:     "zero original, only adds",
			original: 0, keptLen: 0, addedLen: 3,
			want: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pdbViolationsUpperBound(tt.original, tt.keptLen, tt.addedLen)
			assert.Equal(t, tt.want, got)
			// Invariant: result must not exceed total returned victim count.
			assert.LessOrEqual(t, got, int64(tt.keptLen+tt.addedLen),
				"upper bound must satisfy result <= keptLen + addedLen")
		})
	}
}

// Test_Preempt_NumPDBViolations_PassedThrough: the no-change case. In-tree
// proposed 1 victim with NumPDBViolations=1; we keep it as-is (not
// protected, allocator agrees), so output count should match input.
func Test_Preempt_NumPDBViolations_PassedThrough(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	lowA := newVGPUPod("low-a", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowA, node.Name, 0, devUUIDs[0])
	lowB := newVGPUPod("low-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{lowA, lowB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {
				Pods:             []*extenderv1.MetaPod{{UID: string(lowB.UID)}},
				NumPDBViolations: 1,
			},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	if assert.Contains(t, res.NodeNameToMetaVictims, node.Name) {
		assert.Equal(t, int64(1), res.NodeNameToMetaVictims[node.Name].NumPDBViolations,
			"no-change case: count passes through")
	}
}

// Test_Preempt_NumPDBViolations_OverEstimateOnSwap: in-tree proposed 1
// victim (DaemonSet, NumPDBViolations=0). We drop it (protected) and add 1
// replacement. Output: min(0, 0) + 1 = 1 — we assume the worst about the
// added victim, deliberately erring high to avoid luring the scheduler into
// our node with a too-rosy report.
func Test_Preempt_NumPDBViolations_OverEstimateOnSwap(t *testing.T) {
	node, devUUIDs := newTestNode("node1")
	dsA := newVGPUPod("ds-a", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
	allocatePodOn(dsA, node.Name, 0, devUUIDs[0])
	regularB := newVGPUPod("regular-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(regularB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{dsA, regularB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {
				Pods:             []*extenderv1.MetaPod{{UID: string(dsA.UID)}},
				NumPDBViolations: 0,
			},
		},
	}
	res := plugin.Preempt(context.Background(), args)
	if assert.Contains(t, res.NodeNameToMetaVictims, node.Name) {
		// Dropped dsA (protected), added regularB. Original count 0, kept
		// from input 0, added 1 → min(0,0) + 1 = 1.
		assert.Equal(t, int64(1), res.NodeNameToMetaVictims[node.Name].NumPDBViolations,
			"swap case: added victim conservatively treated as a violator")
	}
}
