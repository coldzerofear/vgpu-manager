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

// Package remote holds the device plugin's remote vGPU roles.
package remote

import (
	"context"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	// serverRoleName keys the server role in the device manager's node registration.
	serverRoleName = "remote-server"
	// consumerRoleName keys the consumer role there.
	consumerRoleName = "remote-consumer"
	// probeInterval matches how often the remote-agent probes lupine-server.
	probeInterval = 5 * time.Second
)

// Each remote role publishes its own node metadata while its process runs and
// removes it when that process stops -- and never touches the other role's.
// The two roles may run as two processes on one node (a GPU server that also
// runs remote pods), and a process that cleaned up "roles it does not run"
// would keep deleting what the other one publishes. Metadata left behind by a
// remote role that is gone for good is the local plugin's to remove: a node
// runs either the local plugin or remote ones, never both (see RemoveRoles).

// setupConsumerRole publishes this node as one that runs remote vGPU pods, and
// has the label removed when this process stops.
func setupConsumerRole(reg registrar) {
	reg.AddCleanupRegistryFunc(consumerRoleName, removeConsumerRole)
	reg.AddRegistryFunc(consumerRoleName, func(featuregate.FeatureGate) (*client.PatchMetadata, error) {
		return roleMetadata(util.NodeRemoteConsumerLabel, ptr.To("true"), nil, nil), nil
	})
}

func removeConsumerRole(featuregate.FeatureGate) (*client.PatchMetadata, error) {
	return roleMetadata(util.NodeRemoteConsumerLabel, nil, nil, nil), nil
}

// registrar is the part of the device manager that publishes node metadata.
type registrar interface {
	AddRegistryFunc(name string, fn manager.RegistryFunc)
	AddCleanupRegistryFunc(name string, fn manager.RegistryFunc)
	RegisterNotify()
}

// setupServerRole publishes this node as a remote GPU server, and has the
// label and endpoints removed when this process stops.
//
// The node keeps its role while lupine-server is unreachable, publishing
// remotegpu.UnreachableServerEndpointInfo: local pods stay off its GPUs and
// the scheduler sends no remote pods to it.
func setupServerRole(reg registrar, role *serverRole) {
	reg.AddCleanupRegistryFunc(serverRoleName, removeServerRole)
	reg.AddRegistryFunc(serverRoleName, role.registry)
}

// RemoveRoles removes any remote role this node still carries, for a process
// that runs no remote role at all: the local vGPU plugin. A node never runs
// local and remote plugins together -- both register vgpu-number, and a
// remote label beside local devices would send remote pods to a node that
// cannot serve them -- so whatever remote metadata is on the node was left by
// a remote process that is gone (one that died without cleaning up, or a node
// whose role was changed). It is removed on every registration round, not just
// once, so it also clears what such a process leaves behind later.
func RemoveRoles(devManager *manager.DeviceManager) {
	removeRoles(devManager)
}

func removeRoles(reg registrar) {
	reg.AddRegistryFunc(serverRoleName, removeServerRole)
	reg.AddRegistryFunc(consumerRoleName, removeConsumerRole)
}

// newServerRole starts tracking what this node's remote-agent reports about
// its lupine-server, until ctx is done.
func newServerRole(
	ctx context.Context, reg registrar, kubeClient kubernetes.Interface, nodeName, agentEndpoint string,
) (*serverRole, error) {
	agentDial, err := remotegpu.ResolveAgentDial(ctx, kubeClient, nodeName, agentEndpoint)
	if err != nil {
		return nil, err
	}
	role := &serverRole{
		probe: func(ctx context.Context) (*remotegpu.ServerEndpointInfo, error) {
			return remotegpu.ProbeServer(ctx, agentDial)
		},
		notify:    reg.RegisterNotify,
		endpoints: remotegpu.UnreachableServerEndpointInfo,
	}
	klog.InfoS("Remote GPU server role enabled", "agent", agentDial)
	go wait.UntilWithContext(ctx, role.refresh, probeInterval)
	return role, nil
}

// serverRole keeps the published endpoints in step with the remote-agent.
type serverRole struct {
	probe  func(context.Context) (*remotegpu.ServerEndpointInfo, error)
	notify func()

	mu        sync.RWMutex
	endpoints string // value of the endpoints annotation
}

// refresh asks the agent once and republishes right away if the value changed.
func (r *serverRole) refresh(ctx context.Context) {
	info, err := r.probe(ctx)
	value := remotegpu.UnreachableServerEndpointInfo
	if err == nil {
		value, err = info.Encode()
	}
	if err != nil {
		klog.V(4).InfoS("lupine-server is not reachable", "err", err)
		value = remotegpu.UnreachableServerEndpointInfo
	}

	r.mu.Lock()
	changed := r.endpoints != value
	r.endpoints = value
	r.mu.Unlock()
	if changed {
		klog.InfoS("Remote GPU server endpoints changed", "endpoints", value)
		r.notify()
	}
}

// registry publishes the server role label and the current endpoints.
func (r *serverRole) registry(featuregate.FeatureGate) (*client.PatchMetadata, error) {
	r.mu.RLock()
	value := r.endpoints
	r.mu.RUnlock()
	return serverRoleMetadata(ptr.To("true"), &value), nil
}

// removeServerRole removes the server role label and endpoints.
func removeServerRole(featuregate.FeatureGate) (*client.PatchMetadata, error) {
	return serverRoleMetadata(nil, nil), nil
}

// serverRoleMetadata sets the server role label and endpoints; nil removes them.
func serverRoleMetadata(label, endpoints *string) *client.PatchMetadata {
	return roleMetadata(util.NodeRemoteServerLabel, label, ptr.To(util.NodeRemoteEndpointsAnnotation), endpoints)
}

// roleMetadata sets one role label and, when named, one annotation; a nil
// value removes the key.
func roleMetadata(label string, labelValue *string, annotation, annotationValue *string) *client.PatchMetadata {
	metadata := &client.PatchMetadata{Labels: map[string]*string{label: labelValue}}
	if annotation != nil {
		metadata.Annotations = map[string]*string{*annotation: annotationValue}
	}
	return metadata
}
