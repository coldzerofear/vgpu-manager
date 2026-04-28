package common

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type ResourceInfos []ResourceInfo

type ResourceInfo struct {
	Name      string                                    `json:"containerName"`
	Resources map[corev1.ResourceName]resource.Quantity `json:"resources"`
}

func (r ResourceInfos) Encode() (string, error) {
	marshal, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(marshal), nil
}

func (r *ResourceInfos) Decode(val string) error {
	return json.Unmarshal([]byte(val), r)
}

type MainRequestClass string

const (
	MainRequestNonVGPU MainRequestClass = "non-vgpu"
	MainRequestDefVGPU MainRequestClass = "definite-vgpu"
	// FirstAvailable mixed, requires final ruling from the claim webhook
	MainRequestMixedMaybe MainRequestClass = "mixed-maybe-vgpu"
)

type ContainerKind string

const (
	ContainerKindInit ContainerKind = "initContainer"
	ContainerKindApp  ContainerKind = "container"
)

type ContainerRef struct {
	Name   string
	Claims []corev1.ResourceClaim
	Kind   ContainerKind
}
