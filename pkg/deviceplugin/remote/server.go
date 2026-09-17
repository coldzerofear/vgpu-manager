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

// setupConsumerRole publishes this node as a node that runs remote vGPU pods,
// or removes a consumer role left from an earlier configuration. The role is
// removed on shutdown either way.
func setupConsumerRole(reg registrar, enabled bool) {
	// TODO Only enabling remote server may accidentally delete tags maintained by consumer processes
	//reg.AddRegistryFunc(consumerRoleName, removeConsumerRole)
	reg.AddCleanupRegistryFunc(consumerRoleName, removeConsumerRole)
	if !enabled {
		return
	}
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

// setupServerRole publishes this node as a remote GPU server, or removes a
// server role left from an earlier configuration when role is nil. The role is
// removed on shutdown either way.
//
// The node keeps its role while lupine-server is unreachable, publishing
// remotegpu.UnreachableServerEndpointInfo: local pods stay off its GPUs and
// the scheduler sends no remote pods to it.
func setupServerRole(reg registrar, role *serverRole) {
	reg.AddRegistryFunc(serverRoleName, removeServerRole)
	reg.AddCleanupRegistryFunc(serverRoleName, removeServerRole)
	if role == nil {
		return
	}
	reg.AddRegistryFunc(serverRoleName, role.registry)
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
