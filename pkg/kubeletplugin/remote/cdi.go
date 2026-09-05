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
	"sort"

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

// WriteClaimSpec persists the transient spec holding one CDI device per
// allocation result (deviceID -> its container edits; results in the same
// partition carry identical edits) and returns deviceID -> fully qualified
// CDI device name for the kubelet.
func (w *cdiWriter) WriteClaimSpec(claimUID string, devices map[string]*cdiapi.ContainerEdits) (map[string]string, error) {
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	names := make(map[string]string, len(devices))
	dspecs := make([]cdispec.Device, 0, len(devices))
	for _, id := range ids {
		name := claimUID + "-" + id
		dspecs = append(dspecs, cdispec.Device{
			Name:           name,
			ContainerEdits: *devices[id].ContainerEdits,
		})
		names[id] = cdiparser.QualifiedName(cdiVendor, cdiClaimClass, name)
	}
	s, err := spec.New(
		spec.WithVendor(cdiVendor),
		spec.WithClass(cdiClaimClass),
		spec.WithDeviceSpecs(dspecs),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CDI spec: %w", err)
	}

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	path := filepath.Join(w.cdiRoot, specName+".yaml")
	klog.V(6).Infof("Writing remote CDI spec %q for claim %q (%d device(s))", path, claimUID, len(dspecs))
	if err := s.Save(path); err != nil {
		return nil, fmt.Errorf("failed to save CDI spec %q: %w", path, err)
	}
	return names, nil
}

func (w *cdiWriter) DeleteClaimSpec(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	err := os.Remove(filepath.Join(w.cdiRoot, specName+".yaml"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
