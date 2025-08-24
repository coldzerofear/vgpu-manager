package watcher

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func Test_BalanceBatches(t *testing.T) {
	testCases := []struct {
		name        string
		totalCards  int
		maxPerBatch int
		want        []BatchConfig
	}{
		{
			name:        "example 1",
			totalCards:  16,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 3, 4},
				{4, 7, 4},
				{8, 11, 4},
				{12, 15, 4},
			},
		}, {
			name:        "example 2",
			totalCards:  5,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 2, 3},
				{3, 4, 2},
			},
		}, {
			name:        "example 3",
			totalCards:  6,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 2, 3},
				{3, 5, 3},
			},
		}, {
			name:        "example 4",
			totalCards:  7,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 3, 4},
				{4, 6, 3},
			},
		}, {
			name:        "example 5",
			totalCards:  8,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 3, 4},
				{4, 7, 4},
			},
		}, {
			name:        "example 6",
			totalCards:  9,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 2, 3},
				{3, 5, 3},
				{6, 8, 3},
			},
		}, {
			name:        "example 7",
			totalCards:  10,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 3, 4},
				{4, 6, 3},
				{7, 9, 3},
			},
		}, {
			name:        "example 8",
			totalCards:  15,
			maxPerBatch: 4,
			want: []BatchConfig{
				{0, 3, 4},
				{4, 7, 4},
				{8, 11, 4},
				{12, 14, 3},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			batches := BalanceBatches(tc.totalCards, tc.maxPerBatch)
			assert.Equal(t, tc.want, batches)
		})
	}
}
