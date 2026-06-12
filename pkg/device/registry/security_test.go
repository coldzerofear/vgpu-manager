package registry

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func Test_parsePodUIDFromCgroup(t *testing.T) {
	const uid = "12345678-1234-1234-1234-1234567890ab"
	tests := []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{
			name: "cgroupfs v1",
			data: "10:devices:/kubepods/besteffort/pod" + uid + "/abc123\n",
			want: uid, ok: true,
		},
		{
			name: "systemd v2 unified (underscores + .slice)",
			data: "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod12345678_1234_1234_1234_1234567890ab.slice/cri-containerd-deadbeef.scope\n",
			want: uid, ok: true,
		},
		{
			name: "burstable systemd",
			data: "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_1234567890AB.slice/x.scope\n",
			want: uid, ok: true,
		},
		{
			name: "host process, no pod",
			data: "0::/system.slice/kubelet.service\n",
			want: "", ok: false,
		},
		{
			name: "empty",
			data: "",
			want: "", ok: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePodUIDFromCgroup([]byte(tt.data))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func cand(uid string) TargetCandidate {
	return TargetCandidate{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: k8stypes.UID(uid)}}}
}

func Test_candidatesIncludePod(t *testing.T) {
	cands := []TargetCandidate{cand("aaa"), cand("BBB")}
	assert.True(t, candidatesIncludePod(cands, "aaa"))
	assert.True(t, candidatesIncludePod(cands, "bbb"), "case-insensitive")
	assert.False(t, candidatesIncludePod(cands, "ccc"))
	assert.False(t, candidatesIncludePod(nil, "aaa"))
	// nil pod is skipped, not a panic.
	assert.False(t, candidatesIncludePod([]TargetCandidate{{Pod: nil}}, "aaa"))
}

func Test_acquireSlot_globalAndPerCaller(t *testing.T) {
	s := &DeviceRegistryServerImpl{
		inFlight:  make(chan struct{}, maxInFlightResolves),
		perCaller: make(map[string]int),
	}
	// Per-caller cap: the (maxPerCallerResolves+1)-th from one caller is rejected.
	var releases []func()
	for i := 0; i < maxPerCallerResolves; i++ {
		rel, ok := s.acquireSlot("pod-a")
		assert.True(t, ok, "slot %d for pod-a should be granted", i)
		releases = append(releases, rel)
	}
	_, ok := s.acquireSlot("pod-a")
	assert.False(t, ok, "over per-caller cap must be rejected")
	// A different caller still gets a slot.
	relB, ok := s.acquireSlot("pod-b")
	assert.True(t, ok, "different caller must still be served")
	// Releasing one pod-a slot frees capacity for pod-a again.
	releases[0]()
	rel, ok := s.acquireSlot("pod-a")
	assert.True(t, ok, "after release pod-a should be grantable again")
	rel()
	relB()
	for _, r := range releases[1:] {
		r()
	}
	// All released → map cleaned up, global budget fully returned.
	assert.Empty(t, s.perCaller)
	assert.Len(t, s.inFlight, 0)
}

func Test_acquireSlot_globalCap(t *testing.T) {
	s := &DeviceRegistryServerImpl{
		inFlight:  make(chan struct{}, 2), // tiny global budget
		perCaller: make(map[string]int),
	}
	r1, ok := s.acquireSlot("a")
	assert.True(t, ok)
	r2, ok := s.acquireSlot("b")
	assert.True(t, ok)
	_, ok = s.acquireSlot("c")
	assert.False(t, ok, "global budget exhausted must reject")
	r1()
	_, ok2 := s.acquireSlot("c")
	assert.True(t, ok2, "after a global release a new caller is served")
	r2()
	// drain
	s.perCallerMu.Lock()
	_ = s.perCaller
	s.perCallerMu.Unlock()
}

// race smoke test: concurrent acquire/release must not deadlock or corrupt counts.
func Test_acquireSlot_concurrent(t *testing.T) {
	s := &DeviceRegistryServerImpl{
		inFlight:  make(chan struct{}, maxInFlightResolves),
		perCaller: make(map[string]int),
	}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rel, ok := s.acquireSlot("k"); ok {
				rel()
			}
		}()
	}
	wg.Wait()
	assert.Empty(t, s.perCaller)
	assert.Len(t, s.inFlight, 0)
}
