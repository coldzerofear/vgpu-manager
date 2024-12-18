package client

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

const (
	socketPath                 = "/var/lib/kubelet/pod-resources/kubelet.sock"
	defaultPodResourcesMaxSize = 1024 * 1024 * 16
	callTimeout                = 2 * time.Second
)

// start starts the gRPC server, registers the pod resource with the Kubelet
func (pr *PodResource) start() error {
	pr.stop()
	if util.PathIsNotExist(socketPath) {
		return fmt.Errorf("pod resources socket %s not exist", socketPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dial),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaultPodResourcesMaxSize)))
	if err != nil {
		return fmt.Errorf("error dialing socket %s: %v", socketPath, err)
	}
	pr.conn = conn
	pr.client = v1alpha1.NewPodResourcesListerClient(conn)

	klog.V(5).Infoln("pod resource client init success.")
	return nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", addr)
}

// ListPodResource call pod resource List interface, get pod resource info
func (pr *PodResource) ListPodResource() (*v1alpha1.ListPodResourcesResponse, error) {
	if err := pr.start(); err != nil {
		return nil, err
	}
	defer pr.stop()
	if pr == nil {
		return nil, fmt.Errorf("invalid interface receiver")
	}
	if pr.conn == nil || pr.client == nil {
		return nil, fmt.Errorf("client not init")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := pr.client.List(ctx, &v1alpha1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list pod resource failed: %v", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("invalid list response")
	}
	return resp, nil
}

type PodInfo struct {
	PodName       string
	PodNamespace  string
	ContainerName string
}

func (pr *PodResource) GetPodInfoByResourceDeviceIDs(resp *v1alpha1.ListPodResourcesResponse,
	resourceName string, deviceIDs []string) (*PodInfo, error) {
	if resp == nil {
		return nil, fmt.Errorf("ListPodResourcesResponse is empty")
	}
	devSet := sets.NewString(deviceIDs...)
	for _, resources := range resp.GetPodResources() {
		for _, containerResources := range resources.GetContainers() {
			for _, devices := range containerResources.GetDevices() {
				if devices.ResourceName != resourceName {
					continue
				}
				if !devSet.HasAll(devices.GetDeviceIds()...) {
					continue
				}
				return &PodInfo{
					PodName:       resources.Name,
					PodNamespace:  resources.Namespace,
					ContainerName: containerResources.Name,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("pod not found")
}

// stop the connection
func (pr *PodResource) stop() {
	if pr == nil {
		return
	}
	if pr.conn != nil {
		if err := pr.conn.Close(); err != nil {
			klog.Warningf("pod resources stop connect failed: %v", err)
		}
		pr.conn = nil
		pr.client = nil
	}
}

// PodResource implements the get pod resource info
type PodResource struct {
	conn   *grpc.ClientConn
	client v1alpha1.PodResourcesListerClient
}

// NewPodResource returns an initialized PodResource
func NewPodResource() *PodResource {
	return &PodResource{}
}
