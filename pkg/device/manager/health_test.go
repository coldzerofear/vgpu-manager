package manager

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetAdditionalXids(t *testing.T) {
	testCases := []struct {
		description string
		input       string
		expected    []uint64
	}{
		{
			description: "Empty input",
		},
		{
			description: "Only comma",
			input:       ",",
		},
		{
			description: "Non-integer input",
			input:       "not-an-int",
		},
		{
			description: "Single integer",
			input:       "68",
			expected:    []uint64{68},
		},
		{
			description: "Negative integer",
			input:       "-68",
		},
		{
			description: "Single integer with trailing spaces",
			input:       "68  ",
			expected:    []uint64{68},
		},
		{
			description: "Single integer followed by comma without trailing number",
			input:       "68,",
			expected:    []uint64{68},
		},
		{
			description: "Comma without preceding number followed by single integer",
			input:       ",68",
			expected:    []uint64{68},
		},
		{
			description: "Two comma-separated integers",
			input:       "68,67",
			expected:    []uint64{68, 67},
		},
		{
			description: "Two integers separated by non-integer",
			input:       "68,not-an-int,67",
			expected:    []uint64{68, 67},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			xids := getAdditionalXids(tc.input)
			require.EqualValues(t, tc.expected, xids)
		})
	}
}
