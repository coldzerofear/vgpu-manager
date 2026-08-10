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

package cdi

import "k8s.io/klog/v2"

// Handler generates the node CDI specification and builds the CDI references
// (annotations / CDI devices) that are returned to kubelet during Allocate.
type Handler interface {
	// CreateSpecFile generates the CDI specification file describing the node's
	// devices. It is invoked once at plugin startup.
	CreateSpecFile() error
	// QualifiedName returns the fully-qualified CDI device name for the given
	// class and device id (e.g. "k8s.device-plugin.nvidia.com/gpu=<uuid>").
	QualifiedName(class, id string) string
	// GetDeviceAnnotations builds the CDI container annotations for the given
	// qualified device names, honoring the configured annotation prefix.
	GetDeviceAnnotations(responseID string, qualifiedNames []string) (map[string]string, error)
	AdditionalDevices() []string
}

// null is a no-op Handler used when no CDI strategy is enabled.
type null struct{}

// NewNullHandler returns a Handler that performs no CDI operations.
func NewNullHandler() Handler {
	return &null{}
}

func (n *null) CreateSpecFile() error {
	return nil
}

func (n *null) QualifiedName(_, _ string) string {
	klog.Error("cannot return a qualified CDI device name with the null CDI handler")
	return ""
}

func (n *null) GetDeviceAnnotations(_ string, _ []string) (map[string]string, error) {
	return nil, nil
}

func (n *null) AdditionalDevices() []string {
	return nil
}
