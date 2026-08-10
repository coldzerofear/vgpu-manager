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
