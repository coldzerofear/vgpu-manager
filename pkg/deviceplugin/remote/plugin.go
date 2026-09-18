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

// One device plugin serves whichever remote vGPU roles this node is given,
// and each role's publications are switched on by its option:
//
//	                    node devices   vgpu-number   server label   consumer label
//	WithServerRole            yes          yes(*)         yes              no
//	WithConsumerRole           no          yes             no             yes
//	both                      yes          yes            yes             yes
//
// A role that is not configured is actively removed from the node, so a node
// that carried it under an earlier configuration does not keep it.
//
// (*) A server that does not run remote pods itself still offers the slots:
// that is what makes the scheduler see a vGPU node (util.IsVGPUEnabledNode
// reads the allocatable resource). It refuses every Allocate, because no pod
// of this node's kubelet may use GPUs that are served to other nodes. When a
// second process on the node runs the consumer role, that process is the one
// that can allocate, so this one stands down -- see standDown.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/nodedevice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// ConsumerSocketName is where the consumer role serves; ServerSocketName
	// is where a node that only serves its GPUs does. The two must differ: a
	// node may run one process per role, and stopping either one unlinks its
	// socket path -- which would take the other one's socket file with it.
	ConsumerSocketName = "nvidia-vgpu-remote.sock"
	ServerSocketName   = "nvidia-vgpu-remote-server.sock"

	// pluginName names this plugin in logs and in the node registration.
	pluginName = "remote-vgpu-plugin"
	// remoteDeviceIDPrefix prefixes the slot ids offered to kubelet. They
	// stand for "one remote vGPU this node may run", not for a local device.
	remoteDeviceIDPrefix = "remote-vgpu"
	// peerProbeInterval bounds how long the resource stays with the wrong
	// process after a peer consumer appears or dies.
	peerProbeInterval = 10 * time.Second
	// peerProbeTimeout is how long a peer has to answer before it counts as
	// gone (a crashed process leaves its socket file behind).
	peerProbeTimeout = 2 * time.Second
)

// Config is where this plugin lives on the node.
type Config struct {
	NodeName     string
	ResourceName string
	// Socket is this process's device plugin socket. Two vgpu-manager
	// processes on one node must not share it: stopping either one unlinks
	// the path, which would take the other one's socket file with it.
	Socket string
	// PeerConsumerSocket is the socket of another process on this node that
	// may serve the consumer role; empty when none is expected. See standDown.
	PeerConsumerSocket string
}

// Option configures one role of the plugin.
type Option func(*Plugin) error

// Plugin is the device plugin of a node that takes part in remote vGPU.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer
	cfg        Config
	devManager *manager.DeviceManager
	baseServer base.PluginServer
	devices    []*pluginapi.Device
	// reg publishes the role metadata; the device manager, except in tests.
	reg registrar

	// consumer is set by WithConsumerRole: only a consumer runs remote pods
	// here, and only it can answer Allocate.
	consumer *consumerRole
	// publishDevices is set by WithServerRole: this node's own GPUs are what
	// remote pods elsewhere use, so the scheduler needs their registry.
	publishDevices bool

	// registered records whether the last Start registered the resource with
	// kubelet, so Stop knows what to undo and the peer watch what to compare.
	registered bool
	// restart asks the plugin runner to run its start loop again, which is
	// how a change of resource ownership takes effect.
	restart chan struct{}
	stopCh  chan struct{}
	mutex   sync.Mutex
}

var (
	_ base.DevicePlugin    = &Plugin{}
	_ base.RestartNotifier = &Plugin{}
)

// New applies the node's remote roles and returns the plugin serving them.
// The role metadata is published (or removed) from here on, whether or not
// the plugin is ever started: it describes the node, not the kubelet service.
func New(cfg Config, devManager *manager.DeviceManager, opts ...Option) (*Plugin, error) {
	p := &Plugin{
		cfg:        cfg,
		devManager: devManager,
		baseServer: base.NewBasePluginServer(cfg.ResourceName, cfg.Socket, devManager),
		reg:        devManager,
		restart:    make(chan struct{}, 1),
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	if p.consumer == nil && !p.publishDevices {
		return nil, fmt.Errorf("remote plugin needs at least one role")
	}
	p.devices = p.slots()
	return p, nil
}

// WithServerRole makes this node a remote GPU server: it publishes the server
// role label with the endpoints its remote-agent reports (kept fresh until ctx
// is done) and, while it serves them, the registry of its own GPUs.
func WithServerRole(ctx context.Context, kubeClient kubernetes.Interface, agentEndpoint string) Option {
	return func(p *Plugin) error {
		role, err := newServerRole(ctx, p.reg, kubeClient, p.cfg.NodeName, agentEndpoint)
		if err != nil {
			return err
		}
		setupServerRole(p.reg, role)
		p.publishDevices = true
		return nil
	}
}

// WithConsumerRole makes this node run remote vGPU pods: it publishes the
// consumer role label, offers kubelet the slots, and prepares each container
// in Allocate.
func WithConsumerRole(kubeClient kubernetes.Interface, opts ConsumerOptions) Option {
	return func(p *Plugin) error {
		p.consumer = newConsumerRole(p.cfg.NodeName, kubeClient, opts)
		setupConsumerRole(p.reg)
		return nil
	}
}

// slots are what kubelet is offered: as many remote vGPUs as this node runs,
// and never fewer than its own GPUs offer -- a node that serves its GPUs
// remotely must not look smaller than they are (analysis §16.6).
func (p *Plugin) slots() []*pluginapi.Device {
	localSlots := 0
	if p.publishDevices {
		for _, dev := range p.devManager.GetNodeDeviceInfo() {
			if !dev.Mig {
				localSlots += dev.Number
			}
		}
	}
	count := localSlots
	if p.consumer != nil {
		count = max(p.consumer.opts.VGPUNumber, localSlots)
	}
	devices := make([]*pluginapi.Device, 0, count)
	for i := 0; i < count; i++ {
		devices = append(devices, &pluginapi.Device{
			ID:     fmt.Sprintf("%s-%d", remoteDeviceIDPrefix, i),
			Health: pluginapi.Healthy,
		})
	}
	return devices
}

func (p *Plugin) Name() string { return pluginName }

// Devices are slots, not hardware: they never turn unhealthy on this node.
// Whether a remote GPU can serve a pod is the scheduler's decision, made from
// the server node's own registry.
func (p *Plugin) Devices() []*pluginapi.Device { return p.devices }

// Start serves the resource to kubelet, unless another process on this node
// owns it, and publishes this node's devices for as long as it serves them.
func (p *Plugin) Start() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if standDown := p.standDown(); standDown {
		klog.InfoS("Another process on this node serves the remote consumer role; leaving the resource to it",
			"resource", p.cfg.ResourceName, "peer", p.cfg.PeerConsumerSocket)
	} else if err := p.baseServer.Start(p.Name(), p); err != nil {
		return err
	} else {
		p.registered = true
	}
	// The node's devices follow this plugin: published once it serves them,
	// removed when it stops, so the scheduler neither sees devices before the
	// plugin is up nor keeps them after it is gone.
	if p.publishDevices {
		nodedevice.Setup(p.Name(), p.devManager)
	}
	p.stopCh = make(chan struct{})
	if p.cfg.PeerConsumerSocket != "" && p.consumer == nil {
		go p.watchPeer(p.stopCh, p.registered)
	}
	return nil
}

func (p *Plugin) Stop() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
	err := p.baseServer.Stop(p.Name())
	p.registered = false
	nodedevice.Remove(p.Name(), p.devManager)
	return err
}

// RestartCh fires when the plugin's own state calls for another start loop:
// the peer consumer appeared (the resource is no longer ours to serve) or is
// gone (it is ours again).
func (p *Plugin) RestartCh() <-chan struct{} { return p.restart }

// ListAndWatch sends the slot list once: it never changes while the plugin runs.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: p.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", p.Name(), err)
	}
	<-p.baseServer.GetStopCh()
	return nil
}

// GetDevicePluginOptions asks for nothing: everything a remote container needs
// is prepared in Allocate.
func (p *Plugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// GetPreferredAllocation has no preference: the slots are interchangeable.
func (p *Plugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Allocate prepares each container of the pod kubelet is admitting. Only a
// consumer node has such pods: on a server the GPUs go to pods of other
// nodes, and the scheduler keeps local pods off it, so a request here means
// something reached kubelet without being scheduled through us.
func (p *Plugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if p.consumer == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"node %s serves its GPUs to remote pods on other nodes and runs none itself", p.cfg.NodeName)
	}
	return p.consumer.allocate(ctx, req)
}

// standDown reports whether another process on this node serves the consumer
// role and therefore owns the resource. kubelet keeps one endpoint per
// resource name, so registering it here would take it away from the process
// that can actually allocate. A plugin that has the consumer role itself
// never stands down.
func (p *Plugin) standDown() bool {
	if p.consumer != nil || p.cfg.PeerConsumerSocket == "" {
		return false
	}
	return peerAnswers(p.cfg.PeerConsumerSocket)
}

// watchPeer asks for a restart once ownership of the resource should change.
// It probes rather than watching the socket file: a process that crashed
// leaves the file behind, and the node would keep standing down for nothing.
func (p *Plugin) watchPeer(stopCh <-chan struct{}, registered bool) {
	ticker := time.NewTicker(peerProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if peerAnswers(p.cfg.PeerConsumerSocket) == registered {
				continue
			}
			klog.InfoS("Remote consumer peer changed; restarting to hand the resource over",
				"peer", p.cfg.PeerConsumerSocket, "wasRegistered", registered)
			select {
			case p.restart <- struct{}{}:
			default: // a restart is already pending
			}
			return
		}
	}
}

// peerAnswers reports whether a device plugin is serving on socket right now.
func peerAnswers(socket string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), peerProbeTimeout)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_, err = pluginapi.NewDevicePluginClient(conn).GetDevicePluginOptions(ctx, &pluginapi.Empty{})
	if err != nil {
		klog.V(5).InfoS("Remote consumer peer does not answer", "socket", socket, "err", err)
	}
	return err == nil
}
