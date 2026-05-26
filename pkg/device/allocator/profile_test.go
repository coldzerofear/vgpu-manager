package allocator

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// containerSpec describes one container's vGPU request triple. A value of
// -1 means "do not set this resource on the container" (lets the test
// exercise the implicit-fill rules); 0 sets the resource to the literal
// Quantity "0" which still counts as 0 — equivalent to -1 for our purposes
// since GetResourceOfContainer treats both as 0.
type containerSpec struct {
	num   int64
	cores int64
	mem   int64
}

// nonVGPU is a marker for a container that requests no vGPU at all — the
// loop in NewRequestProfile must skip it via IsVGPURequiredContainer.
var nonVGPU = containerSpec{num: 0}

// makePod builds a *corev1.Pod with one container per spec. Resources go
// into Limits (which is what GetResourceOfContainer reads).
func makePod(specs ...containerSpec) *corev1.Pod {
	containers := make([]corev1.Container, len(specs))
	for i, s := range specs {
		limits := corev1.ResourceList{}
		if s.num > 0 {
			limits[corev1.ResourceName(util.VGPUNumberResourceName)] = *resource.NewQuantity(s.num, resource.DecimalSI)
		}
		if s.cores > 0 {
			limits[corev1.ResourceName(util.VGPUCoreResourceName)] = *resource.NewQuantity(s.cores, resource.DecimalSI)
		}
		if s.mem > 0 {
			limits[corev1.ResourceName(util.VGPUMemoryResourceName)] = *resource.NewQuantity(s.mem, resource.DecimalSI)
		}
		containers[i] = corev1.Container{
			Name:      fmt.Sprintf("c%d", i),
			Resources: corev1.ResourceRequirements{Limits: limits},
		}
	}
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: containers}}
}

// Test_NewRequestProfile is intentionally exhaustive — each row documents
// what weights come out for one realistic (or pathological) pod shape, so
// readers can predict the scoring impact at a glance without re-deriving
// the math from profile.go.
//
// Weights printed in the wantNum/wantMem/wantCore columns use the
// per-container raw expansion:
//
//	rNum  = sum cNum
//	rMem  = sum (cMem == 0 ? cNum : cNum * cMem)
//	rCore = sum (cCore == 0 && cMem == 0 ? cNum :
//	             cCore  > 0              ? cNum * cCore / 100 :
//	                                       0)
//
// Then weights = each / (rNum + rMem + rCore), with a uniform 1/3 each
// fallback when the sum is zero.
func Test_NewRequestProfile(t *testing.T) {
	// One-third constant kept here so test rows read 0.333 / 0.333 / 0.333
	// instead of inline arithmetic; tolerance defaults to 1e-9 unless a
	// row sets its own (used for "huge MB-typed memory" rows where the
	// num/core dimensions round to effectively zero).
	const third = 1.0 / 3

	testCases := []struct {
		name                              string
		specs                             []containerSpec
		wantNum, wantMem, wantCore, delta float64
	}{
		// ---------- single-container, implicit-fill cases ----------
		{
			name:    "1 vGPU only -> whole card, uniform weights",
			specs:   []containerSpec{{num: 1}},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			name:    "2 vGPUs only -> 2 whole cards, still uniform",
			specs:   []containerSpec{{num: 2}},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			name:    "8 vGPUs only -> 8 whole cards, still uniform",
			specs:   []containerSpec{{num: 8}},
			wantNum: third, wantMem: third, wantCore: third,
		},

		// ---------- explicit cores only (memory stays implicit-full) ----------
		{
			// rNum=1, rMem=1 (implicit), rCore=0.5 -> sum=2.5
			name:    "1 vGPU + 50 cores -> mem still implicit-full",
			specs:   []containerSpec{{num: 1, cores: 50}},
			wantNum: 0.4, wantMem: 0.4, wantCore: 0.2,
		},
		{
			// rNum=1, rMem=1, rCore=0.25 -> sum=2.25
			name:    "1 vGPU + 25 cores",
			specs:   []containerSpec{{num: 1, cores: 25}},
			wantNum: 1.0 / 2.25, wantMem: 1.0 / 2.25, wantCore: 0.25 / 2.25,
		},
		{
			// rNum=1, rMem=1, rCore=1 (explicit full) -> uniform
			name:    "1 vGPU + 100 cores -> equivalent to whole card",
			specs:   []containerSpec{{num: 1, cores: 100}},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			// rNum=1, rMem=1, rCore=0.01 (very thin slice)
			name:    "1 vGPU + 1 core -> cores barely contribute",
			specs:   []containerSpec{{num: 1, cores: 1}},
			wantNum: 1.0 / 2.01, wantMem: 1.0 / 2.01, wantCore: 0.01 / 2.01,
		},
		{
			// rNum=2, rMem=2, rCore=2*50/100=1 -> sum=5; weights (0.4, 0.4, 0.2)
			name:    "2 vGPUs + 50 cores each",
			specs:   []containerSpec{{num: 2, cores: 50}},
			wantNum: 0.4, wantMem: 0.4, wantCore: 0.2,
		},

		// ---------- explicit memory only (cores stays at 0, memory-only pod) ----------
		{
			// rNum=1, rMem=4, rCore=0 -> sum=5
			name:    "1 vGPU + 4 memory (GB-typed) -> mem dominates 4:1",
			specs:   []containerSpec{{num: 1, mem: 4}},
			wantNum: 0.2, wantMem: 0.8, wantCore: 0,
		},
		{
			// rNum=1, rMem=1, rCore=0 -> sum=2; minimal memory request
			name:    "1 vGPU + 1 memory -> still 1:1 split",
			specs:   []containerSpec{{num: 1, mem: 1}},
			wantNum: 0.5, wantMem: 0.5, wantCore: 0,
		},
		{
			// rNum=2, rMem=8, rCore=0 -> sum=10
			name:    "2 vGPUs + 4 memory each -> total 8 memory units",
			specs:   []containerSpec{{num: 2, mem: 4}},
			wantNum: 0.2, wantMem: 0.8, wantCore: 0,
		},
		{
			// MB-typed huge value: rNum=1, rMem=4096, rCore=0; mem swamps everything
			name:    "1 vGPU + 4096 memory (MB-typed) -> memory nearly 1.0",
			specs:   []containerSpec{{num: 1, mem: 4096}},
			wantNum: 1.0 / 4097, wantMem: 4096.0 / 4097, wantCore: 0,
			delta: 1e-6,
		},

		// ---------- all three explicit ----------
		{
			// rNum=1, rMem=4, rCore=0.5 -> sum=5.5
			name:    "1 vGPU + 50 cores + 4 memory",
			specs:   []containerSpec{{num: 1, cores: 50, mem: 4}},
			wantNum: 1.0 / 5.5, wantMem: 4.0 / 5.5, wantCore: 0.5 / 5.5,
		},
		{
			// rNum=2, rMem=8, rCore=2 -> sum=12; weights (1/6, 4/6, 1/6)
			name:    "2 vGPUs + 100 cores + 4 memory",
			specs:   []containerSpec{{num: 2, cores: 100, mem: 4}},
			wantNum: 1.0 / 6, wantMem: 4.0 / 6, wantCore: 1.0 / 6,
		},
		{
			// rNum=1, rMem=4096, rCore=0.5 -> mem swamps
			name:    "1 vGPU + 50 cores + 4096 memory (MB-typed) -> mem still dominates",
			specs:   []containerSpec{{num: 1, cores: 50, mem: 4096}},
			wantNum: 1.0 / 4097.5, wantMem: 4096.0 / 4097.5, wantCore: 0.5 / 4097.5,
			delta: 1e-6,
		},

		// ---------- multi-container ----------
		{
			// Two identical implicit-full containers: combined still uniform.
			// c1: rNum+=1, rMem+=1, rCore+=1; c2 same; totals (2,2,2)
			name:    "Multi-container: two implicit-full 1 vGPU each",
			specs:   []containerSpec{{num: 1}, {num: 1}},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			// vGPU container + non-vGPU container; non-vGPU skipped entirely.
			// Result identical to just the first container.
			name:    "Multi-container: 1 vGPU + 1 non-vGPU -> non-vGPU skipped",
			specs:   []containerSpec{{num: 1}, nonVGPU},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			// c1=(1,50,0): rNum+=1, rMem+=1, rCore+=0.5
			// c2=(2,0,0):  rNum+=2, rMem+=2, rCore+=2
			// totals (3, 3, 2.5) -> sum=8.5
			name:    "Multi-container: (1 vGPU + 50 cores) + (2 vGPUs implicit-full)",
			specs:   []containerSpec{{num: 1, cores: 50}, {num: 2}},
			wantNum: 3.0 / 8.5, wantMem: 3.0 / 8.5, wantCore: 2.5 / 8.5,
		},
		{
			// c1=(1, mem:4):  rNum+=1, rMem+=4,  rCore+=0    (memory-only)
			// c2=(2, cores:50): rNum+=2, rMem+=2, rCore+=1    (implicit-mem)
			// totals (3, 6, 1) -> sum=10; weights (0.3, 0.6, 0.1)
			name:    "Multi-container: (1 vGPU + 4 memory) + (2 vGPUs + 50 cores)",
			specs:   []containerSpec{{num: 1, mem: 4}, {num: 2, cores: 50}},
			wantNum: 0.3, wantMem: 0.6, wantCore: 0.1,
		},
		{
			// Three containers, mix of shapes:
			// c1=(1,0,0):     (1, 1, 1)
			// c2=(1,0,4):     (1, 4, 0)
			// c3=(2,50,0):    (2, 2, 1)
			// totals (4, 7, 2) -> sum=13
			name:    "Multi-container: implicit-full + memory-only + cores-partial",
			specs:   []containerSpec{{num: 1}, {num: 1, mem: 4}, {num: 2, cores: 50}},
			wantNum: 4.0 / 13, wantMem: 7.0 / 13, wantCore: 2.0 / 13,
		},

		// ---------- edge / fallback cases ----------
		{
			// Pod with no vGPU containers at all -> sum=0 -> uniform fallback
			name:    "No vGPU containers -> uniform fallback",
			specs:   []containerSpec{nonVGPU, nonVGPU},
			wantNum: third, wantMem: third, wantCore: third,
		},
		{
			// Empty pod (no containers) -> sum=0 -> uniform fallback
			name:    "Empty pod -> uniform fallback",
			specs:   nil,
			wantNum: third, wantMem: third, wantCore: third,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			delta := tc.delta
			if delta == 0 {
				delta = 1e-9
			}
			profile := NewRequestProfile(makePod(tc.specs...))
			assert.InDelta(t, tc.wantNum, profile.NumWeight, delta, "NumWeight")
			assert.InDelta(t, tc.wantMem, profile.MemWeight, delta, "MemWeight")
			assert.InDelta(t, tc.wantCore, profile.CoreWeight, delta, "CoreWeight")
			// Weights MUST sum to ~1.0 (modulo float rounding) for every
			// branch, including the uniform fallback — Score relies on this
			// to stay in [0, 1].
			assert.InDelta(t, 1.0, profile.NumWeight+profile.MemWeight+profile.CoreWeight, 1e-9,
				"weights sum to 1.0")
		})
	}
}

// Test_NewRequestProfile_DocTable prints a one-line summary for every
// scenario in Test_NewRequestProfile when run with -v. Skipped by default
// — flip the t.Skip to regenerate the README/design-doc weight table
// when the formula changes.
func Test_NewRequestProfile_DocTable(t *testing.T) {
	t.Skip("Doc-only helper; remove t.Skip and run with -v to regenerate the weight table.")
	rows := []struct {
		label string
		specs []containerSpec
	}{
		{"vgpu-number: 1", []containerSpec{{num: 1}}},
		{"vgpu-number: 2", []containerSpec{{num: 2}}},
		{"vgpu-number: 1 + cores: 50", []containerSpec{{num: 1, cores: 50}}},
		{"vgpu-number: 1 + cores: 100", []containerSpec{{num: 1, cores: 100}}},
		{"vgpu-number: 1 + memory: 4", []containerSpec{{num: 1, mem: 4}}},
		{"vgpu-number: 1 + memory: 4096", []containerSpec{{num: 1, mem: 4096}}},
		{"vgpu-number: 2 + memory: 4 each", []containerSpec{{num: 2, mem: 4}}},
		{"vgpu-number: 1 + cores: 50 + memory: 4", []containerSpec{{num: 1, cores: 50, mem: 4}}},
		{"vgpu-number: 2 + cores: 100 + memory: 4", []containerSpec{{num: 2, cores: 100, mem: 4}}},
	}
	for _, r := range rows {
		p := NewRequestProfile(makePod(r.specs...))
		t.Logf("%-50s -> (Num=%.3f, Mem=%.3f, Core=%.3f)",
			r.label, p.NumWeight, p.MemWeight, p.CoreWeight)
	}
}
