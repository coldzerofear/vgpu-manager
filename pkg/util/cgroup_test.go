package util

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
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

func Test_CgroupName(t *testing.T) {
	podUID := uuid.NewUUID()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		systemd  string
		cgroupfs string
	}{
		{
			name: "example1: Guaranteed qos",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: podUID,
				},
				Status: corev1.PodStatus{
					QOSClass: corev1.PodQOSGuaranteed,
				},
			},
			systemd: func() string {
				uuid := strings.ReplaceAll(string(podUID), "-", "_")
				return fmt.Sprintf("/kubepods.slice/kubepods-pod%s.slice", uuid)
			}(),
			cgroupfs: fmt.Sprintf("/kubepods/pod%s", string(podUID)),
		},
		{
			name: "example2: Burstable qos",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: podUID,
				},
				Status: corev1.PodStatus{
					QOSClass: corev1.PodQOSBurstable,
				},
			},
			systemd: func() string {
				uuid := strings.ReplaceAll(string(podUID), "-", "_")
				return fmt.Sprintf("/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod%s.slice", uuid)
			}(),
			cgroupfs: fmt.Sprintf("/kubepods/burstable/pod%s", string(podUID)),
		},
		{
			name: "example3: BestEffort qos",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: podUID,
				},
				Status: corev1.PodStatus{
					QOSClass: corev1.PodQOSBestEffort,
				},
			},
			systemd: func() string {
				uuid := strings.ReplaceAll(string(podUID), "-", "_")
				return fmt.Sprintf("/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod%s.slice", uuid)
			}(),
			cgroupfs: fmt.Sprintf("/kubepods/besteffort/pod%s", string(podUID)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cgroupName := NewPodCgroupName(test.pod)
			systemd := cgroupName.ToSystemd()
			assert.Equal(t, test.systemd, systemd)
			cgroupfs := cgroupName.ToCgroupfs()
			assert.Equal(t, test.cgroupfs, cgroupfs)
		})
	}
}
