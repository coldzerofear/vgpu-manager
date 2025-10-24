package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

const (
	defaultSocketPath          = "/var/lib/kubelet/pod-resources/kubelet.sock"
	defaultPodResourcesMaxSize = 1024 * 1024 * 16
	defaultCallTimeout         = 2 * time.Second
)

// PodResource implements the get pod resource info
type PodResource struct {
	sync.Mutex
	socketPath  string
	callTimeout time.Duration
	resMaxSize  int
	conn        *grpc.ClientConn
	client      v1alpha1.PodResourcesListerClient
}

type PodResourceOption func(*PodResource)

func WithCallTimeoutSecond(seconds uint) PodResourceOption {
	return func(resource *PodResource) {
		resource.callTimeout = time.Duration(seconds) * time.Second
	}
}

func WithResMaxSize(maxSize uint) PodResourceOption {
	return func(resource *PodResource) {
		resource.resMaxSize = int(maxSize)
	}
}

func WithSocketPath(socketPath string) PodResourceOption {
	return func(resource *PodResource) {
		resource.socketPath = socketPath
	}
}

// NewPodResource returns an initialized PodResource
func NewPodResource(opts ...PodResourceOption) *PodResource {
	podResource := &PodResource{}
	for _, opt := range opts {
		opt(podResource)
	}
	if podResource.socketPath == "" {
		podResource.socketPath = defaultSocketPath
	}
	if podResource.callTimeout == 0 {
		podResource.callTimeout = defaultCallTimeout
	}
	if podResource.resMaxSize == 0 {
		podResource.resMaxSize = defaultPodResourcesMaxSize
	}
	return podResource
}

// start starts the gRPC server, registers the pod resource with the Kubelet
func (pr *PodResource) start() error {
	pr.stop()
	if util.PathIsNotExist(pr.socketPath) {
		return fmt.Errorf("pod resources socket %s not exist", pr.socketPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pr.callTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, pr.socketPath, grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(pr.resMaxSize)))
	if err != nil {
		return fmt.Errorf("error dialing socket %s: %v", pr.socketPath, err)
	}
	pr.conn = conn
	pr.client = v1alpha1.NewPodResourcesListerClient(conn)

	klog.V(5).Infoln("pod resource client init success.")
	return nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", addr)
}

type DeviceKeepFunc func(devices *v1alpha1.ContainerDevices) bool

// ListPodResource call pod resource List interface, get pod resource info
func (pr *PodResource) ListPodResource(ctx context.Context, filterFuncs ...DeviceKeepFunc) (*v1alpha1.ListPodResourcesResponse, error) {
	pr.Lock()
	defer pr.Unlock()

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
	ctx, cancel := context.WithTimeout(ctx, pr.callTimeout)
	defer cancel()
	resp, err := pr.client.List(ctx, &v1alpha1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list pod resource failed: %v", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("invalid list response")
	}
	if len(filterFuncs) == 0 {
		return resp, nil
	}
	var podResources []*v1alpha1.PodResources
	for _, podResource := range resp.GetPodResources() {
		var contRess []*v1alpha1.ContainerResources
		for _, contResources := range podResource.GetContainers() {
			var contDevs []*v1alpha1.ContainerDevices
			for _, contDevices := range contResources.GetDevices() {
				keep := true
				for _, filterFunc := range filterFuncs {
					if !filterFunc(contDevices) {
						keep = false
						break
					}
				}
				if keep {
					contDevs = append(contDevs, contDevices)
				}
			}
			if len(contDevs) > 0 {
				contRess = append(contRess, &v1alpha1.ContainerResources{
					Name:    contResources.GetName(),
					Devices: contDevs,
				})
			}
		}
		if len(contRess) > 0 {
			podResources = append(podResources, &v1alpha1.PodResources{
				Name:       podResource.GetName(),
				Namespace:  podResource.GetNamespace(),
				Containers: contRess,
			})
		}
	}
	return &v1alpha1.ListPodResourcesResponse{PodResources: podResources}, nil
}

type PodInfo struct {
	PodName       string
	PodNamespace  string
	ContainerName string
}

type MatchFunc func(*v1alpha1.ContainerDevices) bool

func (pr *PodResource) GetPodInfoByMatchFunc(resp *v1alpha1.ListPodResourcesResponse, matchFunc MatchFunc) (*PodInfo, error) {
	if resp == nil {
		return nil, fmt.Errorf("ListPodResourcesResponse is empty")
	}
	for _, podResource := range resp.GetPodResources() {
		for _, contResources := range podResource.GetContainers() {
			for _, contDevices := range contResources.GetDevices() {
				if !matchFunc(contDevices) {
					continue
				}
				return &PodInfo{
					PodName:       podResource.GetName(),
					PodNamespace:  podResource.GetNamespace(),
					ContainerName: contResources.GetName(),
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("no matching podInfo found")
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
