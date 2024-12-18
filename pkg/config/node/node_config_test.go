package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
)

func Test_ParseExcludeDevices(t *testing.T) {
	tests := []struct {
		name   string
		exDevs string
		want   sets.Int
	}{
		{
			name:   "example 1",
			exDevs: "0,0",
			want:   sets.NewInt(0),
		},
		{
			name:   "example 2",
			exDevs: "0,1,3,3",
			want:   sets.NewInt(0, 1, 3),
		},
		{
			name:   "example 3",
			exDevs: "0-3",
			want:   sets.NewInt(0, 1, 2, 3),
		},
		{
			name:   "example 4",
			exDevs: "0-2,4-6",
			want:   sets.NewInt(0, 1, 2, 4, 5, 6),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deviceSet := ParseExcludeDevices(test.exDevs)
			assert.Equal(t, test.want, deviceSet)
		})
	}
}
