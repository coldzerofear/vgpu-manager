/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	// Restartable is true for a sidecar (restartPolicy: Always) init container.
	// Kind stays ContainerKindInit for such containers; consumers that care
	// about lifecycle overlap with the app phase must consult this flag.
	Restartable bool
}

func GetAllPodContainerMap(pod *corev1.Pod) map[string]ContainerRef {
	m := make(map[string]ContainerRef, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		m[c.Name] = ContainerRef{
			Name:        c.Name,
			Claims:      c.Resources.Claims,
			Kind:        ContainerKindInit,
			Restartable: IsRestartableInitContainer(c),
		}
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		m[c.Name] = ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindApp,
		}
	}
	return m
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
