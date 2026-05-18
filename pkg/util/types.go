package util

import corev1 "k8s.io/api/core/v1"

type ContainerKind string

const (
	ContainerKindInit ContainerKind = "init"
	ContainerKindApp  ContainerKind = "app"
)

type ContainerRef struct {
	Name   string
	Claims []corev1.ResourceClaim
	Kind   ContainerKind
}

func GetAllPodContainers(pod *corev1.Pod) []ContainerRef {
	all := make([]ContainerRef, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		all = append(all, ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindInit,
		})
	}
	for _, c := range pod.Spec.Containers {
		all = append(all, ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindApp,
		})
	}
	return all
}
