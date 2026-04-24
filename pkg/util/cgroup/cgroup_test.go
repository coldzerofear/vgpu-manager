package cgroup

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
)

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
