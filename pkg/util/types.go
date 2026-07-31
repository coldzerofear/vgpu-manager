package util

import corev1 "k8s.io/api/core/v1"

type ContainerKind string

const (
	ContainerKindInit ContainerKind = "init"
	ContainerKindApp  ContainerKind = "app"
)

// IsRestartableInitContainer reports whether an init container is a sidecar,
// i.e. it declares restartPolicy: Always. Unlike a regular init container
// (which runs to completion before the app containers start), a sidecar keeps
// running alongside the app containers for the rest of the pod's life, so for
// resource and lifecycle purposes it overlaps the app phase.
func IsRestartableInitContainer(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

type ContainerRef struct {
	Name   string
	Claims []corev1.ResourceClaim
	Kind   ContainerKind
	// Restartable is true for a sidecar (restartPolicy: Always) init container.
	// Kind stays ContainerKindInit for such containers; consumers that care
	// about lifecycle overlap with the app phase must consult this flag.
	Restartable bool
}

func GetAllPodContainers(pod *corev1.Pod) []ContainerRef {
	all := make([]ContainerRef, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		all = append(all, ContainerRef{
			Name:        c.Name,
			Claims:      c.Resources.Claims,
			Kind:        ContainerKindInit,
			Restartable: IsRestartableInitContainer(c),
		})
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		all = append(all, ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindApp,
		})
	}
	return all
}
