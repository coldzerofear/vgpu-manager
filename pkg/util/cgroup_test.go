package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_SplitK8sCGroupBasePath(t *testing.T) {
	tests := []struct {
		name     string
		fullPath string
		want     string
	}{
		{
			name:     "cgroupfs example 1",
			fullPath: "/kubepods/pod2fc932ce-fdcc-454b-97bd-aadfdeb4c340/9be25294016e2dc0340dd605ce1f57b492039b267a6a618a7ad2a7a58a740f32",
			want:     "/kubepods",
		},
		{
			name:     "cgroupfs example 2",
			fullPath: "/kubepods/burstable/pod2fc932ce-fdcc-454b-97bd-aadfdeb4c340/9be25294016e2dc0340dd605ce1f57b492039b267a6a618a7ad2a7a58a740f32",
			want:     "/kubepods",
		},
		{
			name:     "systemd example 1",
			fullPath: "/kubepods.slice/kubepods-pod2fc932ce_fdcc_454b_97bd_aadfdeb4c340.slice/cri-containerd-aaefb9d8feed2d453b543f6d928cede7a4dbefa6a0ae7c9b990dd234c56e93b9.scope",
			want:     "/kubepods.slice",
		},
		{
			name:     "systemd example 2",
			fullPath: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2fc932ce_fdcc_454b_97bd_aadfdeb4c340.slice/cri-containerd-aaefb9d8feed2d453b543f6d928cede7a4dbefa6a0ae7c9b990dd234c56e93b9.scope",
			want:     "/kubepods.slice",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			basePath := SplitK8sCGroupBasePath(test.fullPath)
			assert.Equal(t, test.want, basePath)
		})
	}

}
