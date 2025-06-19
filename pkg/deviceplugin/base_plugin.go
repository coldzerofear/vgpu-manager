package deviceplugin

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

type baseDevicePlugin struct {
	resourceName string
	socket       string
	manager      *manager.DeviceManager

	server *grpc.Server
	health chan *manager.Device
	stop   chan struct{}
}

// newBaseDevicePlugin returns an initialized baseDevicePlugin.
func newBaseDevicePlugin(resourceName, socket string, manager *manager.DeviceManager) *baseDevicePlugin {
	return &baseDevicePlugin{
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

func (b *baseDevicePlugin) initialize() {
	b.server = grpc.NewServer([]grpc.ServerOption{}...)
	b.health = make(chan *manager.Device)
	b.stop = make(chan struct{})
}

func (b *baseDevicePlugin) cleanup() {
	close(b.stop)
	b.server = nil
	b.health = nil
	b.stop = nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (b *baseDevicePlugin) Start(name string, server pluginapi.DevicePluginServer) error {
	b.initialize()

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
func (b *baseDevicePlugin) Stop(name string) error {
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

// serve starts the gRPC server of the device plugin.
func (b *baseDevicePlugin) serve(server pluginapi.DevicePluginServer) error {
	_ = os.Remove(b.socket)
	sock, err := net.Listen("unix", b.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(b.server, server)

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
			if err = b.server.Serve(sock); err == nil {
				break
			}

			klog.Errorf("GRPC server for '%s' crashed with error: %v", b.resourceName, err)

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
func (b *baseDevicePlugin) register() error {
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
func (b *baseDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
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
