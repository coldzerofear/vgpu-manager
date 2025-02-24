package deviceplugin

import (
	"context"
	"net"
	"os"
	"path"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"google.golang.org/grpc"
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

// newBaseDevicePlugin returns an initialized baseDevicePlugin
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
		klog.Infof("Could not register device plugin: %v", err)
		_ = b.Stop(name)
		return err
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
	err := os.Remove(b.socket)
	if err != nil && !os.IsNotExist(err) {
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
			klog.Infof("Starting GRPC server for '%s'", b.resourceName)
			if err = b.server.Serve(sock); err == nil {
				break
			}
			klog.Errorf("GRPC server for '%s' crashed with error: %v", b.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", b.resourceName)
			}

			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := dial(b.socket, 5*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()

	return nil
}

// register the device plugin for the given resourceName with Kubelet.
func (b *baseDevicePlugin) register() error {
	conn, err := dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

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
func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	if c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	); err != nil {
		return nil, err
	} else {
		return c, nil
	}
}
