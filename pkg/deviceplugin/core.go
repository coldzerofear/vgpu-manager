package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type CoreDevicePlugin struct {
	manager      *manager.DeviceManager
	resourceName string
	socket       string

	server *grpc.Server
	health chan *pluginapi.Device
	stop   chan struct{}
}

var _ DevicePlugin = &CoreDevicePlugin{}

// NewCoreDevicePlugin returns an initialized CoreDevicePlugin
func NewCoreDevicePlugin(resourceName string, manager *manager.DeviceManager, socket string) DevicePlugin {
	return &CoreDevicePlugin{
		manager:      manager,
		resourceName: resourceName,
		socket:       socket,

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

func (m *CoreDevicePlugin) Name() string {
	return "core-plugin"
}

func (m *CoreDevicePlugin) initialize() {
	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	m.health = make(chan *pluginapi.Device)
	m.stop = make(chan struct{})
}

func (m *CoreDevicePlugin) cleanup() {
	close(m.stop)
	m.server = nil
	m.health = nil
	m.stop = nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (m *CoreDevicePlugin) Start() error {
	m.initialize()

	if err := m.serve(); err != nil {
		klog.Infof("Could not start device plugin for '%s': %s", m.resourceName, err)
		m.cleanup()
		return err
	}

	klog.Infof("Starting to serve '%s' on %s", m.resourceName, m.socket)

	if err := m.register(); err != nil {
		klog.Infof("Could not register device plugin: %v", err)
		m.Stop()
		return err
	}

	klog.Infof("Registered device plugin for '%s' with Kubelet", m.resourceName)

	m.manager.AddNotifyChannel(m.Name(), m.health)

	return nil
}

// Stop stops the gRPC server.
func (m *CoreDevicePlugin) Stop() error {
	if m == nil || m.server == nil {
		return nil
	}
	klog.Infof("Stopping to serve '%s' on %s", m.resourceName, m.socket)

	m.manager.RemoveNotifyChannel(m.Name())

	m.server.Stop()
	err := os.Remove(m.socket)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	m.cleanup()
	return nil
}

// serve starts the gRPC server of the device plugin.
func (m *CoreDevicePlugin) serve() error {
	os.Remove(m.socket)
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			klog.Infof("Starting GRPC server for '%s'", m.resourceName)
			if err = m.server.Serve(sock); err == nil {
				break
			}
			klog.Errorf("GRPC server for '%s' crashed with error: %v", m.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", m.resourceName)
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
	conn, err := dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// register the device plugin for the given resourceName with Kubelet.
func (m *CoreDevicePlugin) register() error {
	conn, err := dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: m.resourceName,
		Options:      &pluginapi.DevicePluginOptions{},
	}

	_, err = client.Register(context.Background(), reqt)
	return err
}

// GetDevicePluginOptions returns options to be communicated with Device Manager
func (m *CoreDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (m *CoreDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.stop
	for {
		select {
		case d := <-m.health:
			klog.Infof("'%s' device marked unhealthy: %s", m.resourceName, d.ID)
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
				klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
			}
		case <-stopCh:
			return nil
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (m *CoreDevicePlugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (m *CoreDevicePlugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	for i := range req.ContainerRequests {
		responses[i] = &pluginapi.ContainerAllocateResponse{}
	}
	return &pluginapi.AllocateResponse{ContainerResponses: responses}, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container
func (m *CoreDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *CoreDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.manager.GetDevices() {
		if gpuDevice.Mig { // skip mig device
			continue
		}
		for i := 0; i < gpuDevice.Core; i++ {
			devId := fmt.Sprintf("core-%d-%d", gpuDevice.Id, i)
			health := pluginapi.Healthy
			if !gpuDevice.Healthy {
				health = pluginapi.Unhealthy
			}
			devices = append(devices, &pluginapi.Device{
				ID:       devId,
				Health:   health,
				Topology: nil,
			})
		}
	}
	return devices
}
