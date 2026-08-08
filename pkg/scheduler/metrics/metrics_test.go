package metrics

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
)

// Test_LabelWhitelist_BoundsCardinality is a security regression guard, not a
// formatting test.
//
// The annotation parsers pass unrecognised values through verbatim
// (parseSchedulerPolicy returns SchedulerPolicy(raw); BaseTopology returns the
// mode unchanged), and util's own tests REQUIRE that pass-through. If a label
// is ever fed a parsed value directly instead of going through these helpers,
// a tenant can mint one Prometheus series per distinct annotation string inside
// the scheduler process, and client-side metric maps are never evicted.
//
// So: every input outside the closed set must land in exactly one bucket.
func Test_LabelWhitelist_BoundsCardinality(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		known := map[util.SchedulerPolicy]string{
			util.BinpackPolicy: string(util.BinpackPolicy),
			util.SpreadPolicy:  string(util.SpreadPolicy),
			util.NonePolicy:    string(util.NonePolicy),
			"":                 string(util.NonePolicy), // unset == none
		}
		for in, want := range known {
			assert.Equal(t, want, PolicyLabel(in), "known policy %q must map to itself", in)
		}

		// Anything else — typos, injection attempts, sheer volume — collapses.
		hostile := []util.SchedulerPolicy{
			"bogus", "BINPACK\n", "spread ", "../../etc/passwd",
			util.SchedulerPolicy(make([]byte, 4096)),
		}
		for _, in := range hostile {
			assert.Equal(t, LabelOther, PolicyLabel(in), "unknown policy must bucket")
		}
	})

	t.Run("topology", func(t *testing.T) {
		known := map[util.TopologyMode]string{
			util.NUMATopology: string(util.NUMATopology),
			util.LinkTopology: string(util.LinkTopology),
			util.NoneTopology: string(util.NoneTopology),
			"":                string(util.NoneTopology),
		}
		for in, want := range known {
			assert.Equal(t, want, TopologyLabel(in), "known mode %q must map to itself", in)
		}

		// The STRICT spellings must not appear as labels: callers are required
		// to pass BaseTopology(), and strictness is carried by a separate
		// signal. Seeing them here would mean a caller skipped the base call,
		// which would silently double the mode dimension.
		for _, in := range []util.TopologyMode{util.NUMATopologyStrict, util.LinkTopologyStrict} {
			assert.Equal(t, LabelOther, TopologyLabel(in),
				"strict spelling reaching a label means the caller forgot BaseTopology()")
		}

		for _, in := range []util.TopologyMode{"bogus", "link-ish", "numa\x00"} {
			assert.Equal(t, LabelOther, TopologyLabel(in), "unknown mode must bucket")
		}
	})
}

// Test_LabelWhitelist_IsTotal pins that the helpers never return the empty
// string. An empty label value is legal in Prometheus but renders as an
// absent-looking dimension, which would hide exactly the misconfiguration the
// "other" bucket exists to surface.
func Test_LabelWhitelist_IsTotal(t *testing.T) {
	for _, in := range []util.SchedulerPolicy{"", "x", util.BinpackPolicy} {
		assert.NotEmpty(t, PolicyLabel(in))
	}
	for _, in := range []util.TopologyMode{"", "x", util.LinkTopology} {
		assert.NotEmpty(t, TopologyLabel(in))
	}
}
