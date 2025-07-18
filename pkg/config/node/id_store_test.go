package node

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func Test_parseDeviceIDs(t *testing.T) {
	gpuUUID1 := "GPU-" + uuid.New().String()
	gpuUUID2 := "GPU-" + uuid.New().String()
	tests := []struct {
		name       string
		idStr      string
		intIDSlice []int
		strIDSlice []string
		want       IDStore
	}{
		{
			name:  "example 1",
			idStr: "0",
			want:  NewIntIDStore(0),
		},
		{
			name:  "example 2",
			idStr: "0,1,3,3",
			want:  NewIntIDStore(0, 1, 3),
		},
		{
			name:  "example 3",
			idStr: "0..3",
			want:  NewIntIDStore(0, 1, 2, 3),
		},
		{
			name:  "example 4",
			idStr: "0..2,4..6",
			want:  NewIntIDStore(0, 1, 2, 4, 5, 6),
		},
		{
			name:  "example 5",
			idStr: fmt.Sprintf("%s,%s", gpuUUID1, gpuUUID2),
			want:  NewIDStore(gpuUUID1, gpuUUID2),
		},
		{
			name:  "example 6",
			idStr: fmt.Sprintf("\"%s\"", gpuUUID1),
			want:  NewIDStore(gpuUUID1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deviceSet := parseDeviceIDs(test.idStr)
			assert.Equal(t, test.want, deviceSet)
		})
	}
}

func Test_MarshalJSON(t *testing.T) {
	gpuUUID1 := "GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef1"
	gpuUUID2 := "GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef2"
	tests := []struct {
		name  string
		store IDStore
		want  string
	}{
		{
			name:  "example 1",
			store: NewIntIDStore(0, 1, 2, 3),
			want:  "[\"0\",\"1\",\"2\",\"3\"]",
		},
		{
			name:  "example 2",
			store: NewIntIDStore(0, 1, 2, 3, 3),
			want:  "[\"0\",\"1\",\"2\",\"3\"]",
		},
		{
			name:  "example 3",
			store: NewIDStore(gpuUUID1, gpuUUID2),
			want:  fmt.Sprintf("[\"%s\",\"%s\"]", gpuUUID1, gpuUUID2),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marshal, err := json.Marshal(test.store)
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, test.want, string(marshal))
		})
	}
}

func Test_MarshalYaml(t *testing.T) {
	gpuUUID1 := "GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef1"
	gpuUUID2 := "GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef2"
	tests := []struct {
		name  string
		store IDStore
		want  string
	}{
		{
			name:  "example 1",
			store: NewIntIDStore(0, 1, 2, 3),
			want: `- "0"
- "1"
- "2"
- "3"
`,
		},
		{
			name:  "example 2",
			store: NewIntIDStore(0, 1, 2, 3, 3),
			want: `- "0"
- "1"
- "2"
- "3"
`,
		},
		{
			name:  "example 3",
			store: NewIDStore(gpuUUID1, gpuUUID2),
			want: `- GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef1
- GPU-ce4433fa-ec0a-4d6b-9525-ac3ff00d5ef2
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marshal, err := yaml.Marshal(test.store)
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, test.want, string(marshal))
		})
	}
}
