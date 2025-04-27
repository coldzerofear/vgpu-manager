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

func Test_MatchNodeName(t *testing.T) {
	tests := []struct {
		name       string
		cmNodeName string
		cuNodeName string
		want       bool
	}{
		{
			name:       "example 1, Equal names",
			cmNodeName: "testNode",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 2",
			cmNodeName: "test.*",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 3",
			cmNodeName: "^test_",
			cuNodeName: "testNode",
			want:       false,
		}, {
			name:       "example 4",
			cmNodeName: "\\.Node$",
			cuNodeName: "test.Node",
			want:       true,
		}, {
			name:       "example 5",
			cmNodeName: `Node$`,
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 6",
			cmNodeName: `(?i)node$`,
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 7",
			cmNodeName: `(?i)node(1|2)$`,
			cuNodeName: "testNode2",
			want:       true,
		}, {
			name:       "example 8",
			cmNodeName: `(?i)node(1|2)$`,
			cuNodeName: "testNode3",
			want:       false,
		}, {
			name:       "example 9",
			cmNodeName: "^test\\.",
			cuNodeName: "test.Node",
			want:       true,
		}, {
			name:       "example 10",
			cmNodeName: "test",
			cuNodeName: "testNode",
			want:       false,
		}, {
			name:       "example 11",
			cmNodeName: "*Node",
			cuNodeName: "testNode",
			want:       true,
		}, {
			name:       "example 12",
			cmNodeName: "^test",
			cuNodeName: "testNode",
			want:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := matchNodeName(test.cmNodeName, test.cuNodeName)
			assert.Equal(t, test.want, got)
		})
	}
}
