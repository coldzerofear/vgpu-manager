/*
Copyright 2024-2026 coldzerofear

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

package base

import (
	"context"
	"errors"
	"net"
	"os"
	"path"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

var _ PluginServer = &basePluginServerImpl{}

type basePluginServerImpl struct {
	resourceName string
	socket       string
	manager      *manager.DeviceManager

	server *grpc.Server
	health chan *manager.Device
	stop   chan struct{}
}

// NewBasePluginServer returns an initialized basePluginServerImpl.
func NewBasePluginServer(resourceName, socket string, manager *manager.DeviceManager) PluginServer {
	return &basePluginServerImpl{
		resourceName: resourceName,
		socket:       socket,
		manager:      manager,

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

func (b *basePluginServerImpl) initialize(server DevicePlugin) {
	b.server = grpc.NewServer([]grpc.ServerOption{}...)
	b.health = make(chan *manager.Device, len(server.Devices()))
	b.stop = make(chan struct{})
}

func (b *basePluginServerImpl) cleanup() {
	close(b.stop)
	b.server = nil
	b.health = nil
	b.stop = nil
}

func (b *basePluginServerImpl) GetDeviceManager() *manager.DeviceManager {
	return b.manager
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (b *basePluginServerImpl) Start(name string, server DevicePlugin) error {
	b.initialize(server)

	if err := b.serve(server); err != nil {
		klog.Infof("Could not start device plugin for '%s': %s", b.resourceName, err)
		b.cleanup()
		return err
	}

	klog.Infof("Starting to serve '%s' on %s", b.resourceName, b.socket)

	if err := b.register(); err != nil {
		klog.Errorf("Could not register device plugin: %s", err)
		return errors.Join(err, b.Stop(name))
	}

	klog.Infof("Registered device plugin for '%s' with Kubelet", b.resourceName)

	b.manager.AddNotifyChannel(name, b.health)

	return nil
}

// Stop stops the gRPC server.
func (b *basePluginServerImpl) Stop(name string) error {
	if b == nil || b.server == nil {
		return nil
	}
	klog.Infof("Stopping to serve '%s' on %s", b.resourceName, b.socket)

	b.manager.RemoveNotifyChannel(name)

	b.server.Stop()

	if err := os.Remove(b.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	b.cleanup()

	return nil
}

func (b *basePluginServerImpl) GetStopCh() chan struct{} {
	return b.stop
}

func (b *basePluginServerImpl) GetDeviceCh() chan *manager.Device {
	return b.health
}

func (b *basePluginServerImpl) GetResourceName() string {
	return b.resourceName
}

// serve starts the gRPC server of the device plugin.
func (b *basePluginServerImpl) serve(server pluginapi.DevicePluginServer) error {
	_ = os.Remove(b.socket)
	sock, err := net.Listen("unix", b.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(b.server, server)

	// Capture the server this goroutine belongs to. cleanup() sets b.server to
	// nil, so re-reading the field every iteration means any loop that survives
	// a Stop dereferences nil. Capturing it also keeps the goroutine off the
	// `err` above, which serve() keeps writing to after this point.
	srv := b.server

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", b.resourceName)
			}

			klog.Infof("Starting GRPC server for '%s'", b.resourceName)
			// ErrServerStopped is an orderly Stop, not a crash. Retrying it spins
			// on a server that will never serve again — six laps of that and the
			// restartCount guard above takes the whole process down with a
			// Fatalf, which is what used to happen on every plugin restart.
			serveErr := srv.Serve(sock)
			if serveErr == nil || errors.Is(serveErr, grpc.ErrServerStopped) {
				break
			}

			klog.Errorf("GRPC server for '%s' crashed with error: %v", b.resourceName, serveErr)

			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 0
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := b.dial(b.socket, 5*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()

	return nil
}

// register the device plugin for the given resourceName with Kubelet.
func (b *basePluginServerImpl) register() error {
	conn, err := b.dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(b.socket),
		ResourceName: b.resourceName,
		Options:      &pluginapi.DevicePluginOptions{},
	}

	_, err = client.Register(context.Background(), reqt)
	return err
}

// dial establishes the gRPC communication with the registered device plugin.
func (b *basePluginServerImpl) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	//nolint:staticcheck  // TODO: Switch to grpc.NewClient
	return grpc.DialContext(ctx, unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		//nolint:staticcheck  // TODO: WithBlock is deprecated.
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)
}
