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

package remote

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// cdiWriter writes per-claim transient CDI specs for remote claims. Unlike
// the local CDIHandler (pkg/kubeletplugin/cdi.go) it has no NVML/nvcdi
// dependency: a remote claim injects only env and bind mounts — the consumer
// node has no GPU driver and must not receive device nodes.
type cdiWriter struct {
	cdiRoot string
}

func newCDIWriter(cdiRoot string) *cdiWriter {
	return &cdiWriter{cdiRoot: cdiRoot}
}

// remoteDeviceName is the single CDI device a remote claim maps to. All
// allocation results of the claim reference it: the injected env/mounts are
// per-claim (one LUPINE_SERVER list, one session), not per-device.
func remoteDeviceName(claimUID string) string {
	return claimUID + "-remote"
}

// WriteClaimSpec persists the transient spec and returns the fully qualified
// CDI device name to report back to the kubelet.
func (w *cdiWriter) WriteClaimSpec(claimUID string, edits *cdiapi.ContainerEdits) (string, error) {
	dspec := cdispec.Device{
		Name:           remoteDeviceName(claimUID),
		ContainerEdits: *edits.ContainerEdits,
	}
	s, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiClaimClass),
		spec.WithDeviceSpecs([]cdispec.Device{dspec}),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create CDI spec: %w", err)
	}

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	path := filepath.Join(w.cdiRoot, specName+".yaml")
	klog.V(6).Infof("Writing remote CDI spec %q for claim %q", path, claimUID)
	if err := s.Save(path); err != nil {
		return "", fmt.Errorf("failed to save CDI spec %q: %w", path, err)
	}
	return cdiparser.QualifiedName(cdiVendor, cdiClaimClass, dspec.Name), nil
}

func (w *cdiWriter) DeleteClaimSpec(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	err := os.Remove(filepath.Join(w.cdiRoot, specName+".yaml"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
