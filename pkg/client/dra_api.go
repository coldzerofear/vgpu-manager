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

package client

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/client-go/discovery"
)

// DRAAPIGroup is the API group every dynamic-resource-allocation object
// lives in.
const DRAAPIGroup = "resource.k8s.io"

// DRAServedVersions returns the resource.k8s.io versions this cluster serves,
// in the API server's own preference order.
//
// An empty result with a nil error is a definite answer, not an unknown: the
// group is not served at all, because the cluster predates dynamic resource
// allocation or the apiserver's DynamicResourceAllocation feature gate is off
// (that gate also gates the API group). A partial discovery failure elsewhere
// in the cluster -- an unreachable aggregated API server, which is common and
// unrelated -- must not hide that answer, so only a failure to discover this
// very group is reported as an error.
func DRAServedVersions(client discovery.DiscoveryInterface) ([]string, error) {
	groups, err := client.ServerGroups()
	if groups != nil {
		for _, group := range groups.Groups {
			if group.Name != DRAAPIGroup {
				continue
			}
			versions := make([]string, 0, len(group.Versions))
			for _, version := range group.Versions {
				versions = append(versions, version.Version)
			}
			return versions, nil
		}
	}
	var failed *discovery.ErrGroupDiscoveryFailed
	if errors.As(err, &failed) {
		for groupVersion, groupErr := range failed.Groups {
			if groupVersion.Group == DRAAPIGroup {
				return nil, fmt.Errorf("discovery of %s failed: %w", groupVersion.String(), groupErr)
			}
		}
		// Another group is broken; ours is simply absent from the list.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// DRAAPIRequirement is what one component needs from the DRA API group.
//
// Check it once at startup: an unavailable API has to fail the process with an
// actionable message, because the alternative is an informer retrying a 404
// forever. That never syncs, so readiness never flips, and for the remote-agent
// it also means the session skeleton's ready file is never written and the
// lupine-server container waits on it indefinitely -- a misconfiguration that
// presents as a hang, which is the hardest kind to diagnose.
type DRAAPIRequirement struct {
	// Subject names what needs the API, as it should read in the error
	// message ("remote-agent", "--enable-dra-monitor").
	Subject string
	// Version is the single group version the component can talk to ("v1").
	// Empty accepts any served version, which is the right setting for a
	// component whose client negotiates the API version itself.
	Version string
	// Remedy completes the error with what the operator can do instead.
	Remedy string
}

// Check reports whether the cluster serves what the requirement asks for.
func (r DRAAPIRequirement) Check(client discovery.DiscoveryInterface) error {
	versions, err := DRAServedVersions(client)
	if err != nil {
		return fmt.Errorf("%s requires the %s API, and checking whether this cluster serves it failed: %w",
			r.Subject, DRAAPIGroup, err)
	}
	if len(versions) == 0 {
		return fmt.Errorf("%s requires the %s API, which this cluster does not serve: either the Kubernetes"+
			" version predates dynamic resource allocation, or the apiserver's DynamicResourceAllocation feature"+
			" gate is off (it gates the API group too). %s", r.Subject, DRAAPIGroup, r.Remedy)
	}
	if r.Version == "" {
		return nil
	}
	for _, version := range versions {
		if version == r.Version {
			return nil
		}
	}
	return fmt.Errorf("%s requires %s/%s, but this cluster serves only %s/{%s}. %s",
		r.Subject, DRAAPIGroup, r.Version, DRAAPIGroup, strings.Join(versions, ", "), r.Remedy)
}
