package reason

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test_Phrase covers the codePhrases lookup with a known + unknown
// code. The unknown branch is the safety net: if a new Code is added
// without a phrase entry, the raw Code string surfaces instead of an
// empty Event message.
func Test_Phrase(t *testing.T) {
	assert.Equal(t, "Insufficient vGPU memory", Phrase(InsufficientVGPUMemory))
	assert.Equal(t, "Link topology unsatisfied", Phrase(LinkTopologyUnsatisfied))
	// Unknown code → echo the raw string so callers see SOMETHING.
	assert.Equal(t, "MysteryFailure", Phrase(Code("MysteryFailure")))
}

// Test_New covers the node-level constructor (no counts).
func Test_New(t *testing.T) {
	r := New(NodeNotVGPUEnabled)
	assert.Equal(t, NodeNotVGPUEnabled, r.Primary)
	assert.Empty(t, r.Counts)
	assert.Zero(t, r.TotalDevices)
	assert.Equal(t, "node not vGPU-enabled", r.Short())
}

// Test_FromCounts locks down primary-selection: highest count wins,
// ties break by Code lexical order. This is the function that decides
// which reason a node gets bucketed under in the aggregate event, so
// the determinism contract matters for test stability AND for log
// grepping across pod restarts.
func Test_FromCounts(t *testing.T) {
	t.Run("empty counts → nil reason", func(t *testing.T) {
		assert.Nil(t, FromCounts(nil, 0))
		assert.Nil(t, FromCounts(map[Code]int{}, 8))
	})

	t.Run("single code → that code is primary", func(t *testing.T) {
		r := FromCounts(map[Code]int{InsufficientVGPUMemory: 3}, 8)
		assert.Equal(t, InsufficientVGPUMemory, r.Primary)
		assert.Equal(t, 8, r.TotalDevices)
	})

	t.Run("multiple codes → highest count wins", func(t *testing.T) {
		r := FromCounts(map[Code]int{
			InsufficientVGPUMemory: 3,
			DeviceTypeMismatch:     5,
			DeviceUnhealthy:        1,
		}, 9)
		assert.Equal(t, DeviceTypeMismatch, r.Primary)
	})

	t.Run("tie → lexical order on Code", func(t *testing.T) {
		// Two codes with the same count: alphabetically smaller wins.
		// "DeviceTypeMismatch" < "InsufficientVGPUMemory" so the former
		// must be Primary, regardless of map iteration order.
		got := make(map[Code]bool)
		for range 50 {
			r := FromCounts(map[Code]int{
				InsufficientVGPUMemory: 2,
				DeviceTypeMismatch:     2,
			}, 4)
			got[r.Primary] = true
		}
		assert.Len(t, got, 1, "primary should be deterministic across runs")
		assert.True(t, got[DeviceTypeMismatch])
	})
}

// Test_WithDetail covers chainable detail attachment and the nil
// receiver guard (lets callers do reason.New(c).WithDetail(...) even
// when c may produce a nil).
func Test_WithDetail(t *testing.T) {
	r := New(InsufficientVGPUMemory).WithDetail("needs %d MB", 8192)
	assert.Equal(t, "needs 8192 MB", r.Detail)
	assert.Equal(t, "Insufficient vGPU memory", r.Short(), "Short still ignores detail")
	assert.Contains(t, r.Detailed(), "[needs 8192 MB]")

	// nil-safe — WithDetail on a nil reason returns nil rather than
	// panicking. Lets call-site chains like New(c).WithDetail(...) work
	// even if a future refactor makes New return nil for some codes.
	var nilR *FilterReason
	assert.Nil(t, nilR.WithDetail("x"))
}

// Test_Short_Detailed covers what each rendering form looks like.
// Critical contract:
//
//   - Short() is what goes into FailedNodesMap; must be a plain noun
//     phrase, no counts, no node names, no trailing punctuation.
//   - Detailed() is what goes into klog at V(5); MAY include counts,
//     MUST include any Detail.
//   - Counts inside Detailed are sorted by descending count, code
//     lexical for ties — same ordering rule as primary selection.
func Test_Short_Detailed(t *testing.T) {
	t.Run("node-level reason: no counts → Detailed == Short", func(t *testing.T) {
		r := New(NodeNoVGPURegister)
		assert.Equal(t, "node has no vGPU registered", r.Short())
		assert.Equal(t, "node has no vGPU registered", r.Detailed())
	})

	t.Run("device-level reason: counts appear only in Detailed", func(t *testing.T) {
		r := FromCounts(map[Code]int{
			InsufficientVGPUMemory: 3,
			DeviceTypeMismatch:     2,
		}, 8)
		assert.Equal(t, "Insufficient vGPU memory", r.Short(), "Short stays bare")
		// Highest count first; ties not at play here.
		assert.Equal(t,
			"Insufficient vGPU memory (3/8 InsufficientVGPUMemory, 2/8 DeviceTypeMismatch)",
			r.Detailed())
	})

	t.Run("reason with detail appended in [brackets]", func(t *testing.T) {
		r := New(InsufficientVGPUMemory).WithDetail("needs 8192 MB total")
		assert.Equal(t, "Insufficient vGPU memory", r.Short())
		assert.Equal(t, "Insufficient vGPU memory [needs 8192 MB total]", r.Detailed())
	})

	t.Run("nil receiver → empty string everywhere", func(t *testing.T) {
		var r *FilterReason
		assert.Empty(t, r.Short())
		assert.Empty(t, r.Detailed())
	})
}

// Test_FormatAggregate is the headline test: it locks the k8s-style
// "0/N nodes are available: ..." output format that the FilteringFailed
// event will use. Property checks:
//
//  1. "0/<total> nodes are available:" prefix
//  2. Comma-separated clauses, trailing period
//  3. Higher-count buckets render first
//  4. Within a bucket node names are sorted alphabetically
//  5. nodesPerBucketLimit truncates long lists with "and K more"
//  6. nil reasons in the map are tolerated (skipped)
func Test_FormatAggregate(t *testing.T) {
	t.Run("single bucket, three nodes", func(t *testing.T) {
		perNode := map[string]*FilterReason{
			"worker-3": New(InsufficientVGPUMemory),
			"worker-1": New(InsufficientVGPUMemory),
			"worker-2": New(InsufficientVGPUMemory),
		}
		got := FormatAggregate(5, perNode, 0)
		assert.Equal(t,
			"0/5 nodes are available: 3 Insufficient vGPU memory (worker-1,worker-2,worker-3).",
			got)
	})

	t.Run("multiple buckets sorted by descending count", func(t *testing.T) {
		perNode := map[string]*FilterReason{
			"a": New(DeviceTypeMismatch),
			"b": New(DeviceTypeMismatch),
			"c": New(InsufficientVGPUMemory),
			"d": New(InsufficientVGPUMemory),
			"e": New(InsufficientVGPUMemory),
			"f": New(InsufficientVGPUMemory),
			"g": New(NodeNoVGPURegister),
		}
		got := FormatAggregate(10, perNode, 0)
		// InsufficientVGPUMemory (4) > DeviceTypeMismatch (2) > NodeNoVGPURegister (1)
		assert.Equal(t,
			"0/10 nodes are available: "+
				"4 Insufficient vGPU memory (c,d,e,f), "+
				"2 GPU type mismatch (a,b), "+
				"1 node has no vGPU registered (g).",
			got)
	})

	t.Run("tie on count → bucket order falls back to Code lexical", func(t *testing.T) {
		perNode := map[string]*FilterReason{
			"a": New(DeviceTypeMismatch),
			"b": New(InsufficientVGPUMemory),
		}
		got := FormatAggregate(5, perNode, 0)
		// Both have 1 node; "DeviceTypeMismatch" < "InsufficientVGPUMemory"
		// in Code-lexical order, so it goes first.
		assert.Contains(t, got,
			"1 GPU type mismatch (a), 1 Insufficient vGPU memory (b)")
	})

	t.Run("nodesPerBucketLimit truncates long lists", func(t *testing.T) {
		perNode := map[string]*FilterReason{
			"n1": New(InsufficientVGPUMemory),
			"n2": New(InsufficientVGPUMemory),
			"n3": New(InsufficientVGPUMemory),
			"n4": New(InsufficientVGPUMemory),
			"n5": New(InsufficientVGPUMemory),
		}
		got := FormatAggregate(20, perNode, 2)
		// Limit=2: first 2 inline, rest collapsed.
		assert.Equal(t,
			"0/20 nodes are available: 5 Insufficient vGPU memory (n1,n2 and 3 more).",
			got)
	})

	t.Run("nil reason entries are skipped", func(t *testing.T) {
		perNode := map[string]*FilterReason{
			"worker-1": New(InsufficientVGPUMemory),
			"worker-2": nil, // shouldn't happen but defended
		}
		got := FormatAggregate(3, perNode, 0)
		// Only worker-1 contributes; the nil is dropped silently.
		assert.Equal(t,
			"0/3 nodes are available: 1 Insufficient vGPU memory (worker-1).",
			got)
	})

	t.Run("empty perNode → degraded fallback (no clauses)", func(t *testing.T) {
		got := FormatAggregate(5, nil, 0)
		assert.Equal(t, "0/5 nodes are available", got)
	})

	t.Run("totalNodes <= 0 → degraded fallback", func(t *testing.T) {
		// Defensive: pathological input shouldn't panic and shouldn't
		// produce a misleading "0/0 nodes are available".
		got := FormatAggregate(0, map[string]*FilterReason{
			"x": New(InsufficientVGPUMemory),
		}, 0)
		assert.True(t, strings.HasPrefix(got, "0/0 nodes are available"),
			"degraded form, no clauses appended")
	})
}
