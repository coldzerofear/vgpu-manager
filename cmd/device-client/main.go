package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

var (
	address        string
	podUid         string
	containerName  string
	timeoutSeconds = 10
	version        bool
)

func main() {
	cmdFlags := pflag.CommandLine
	cmdFlags.SortFlags = false
	cmdFlags.StringVar(&address, "address", "", "RPC address location for dial.")
	cmdFlags.StringVar(&podUid, "pod-uid", "", "Pod UID of caller.")
	cmdFlags.StringVar(&containerName, "container-name", "", "Container name of caller.")
	cmdFlags.IntVar(&timeoutSeconds, "timeout", timeoutSeconds, "Set RPC connection timeout seconds.")
	cmdFlags.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.Parse()
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
	defer klog.Flush()
	if len(address) == 0 {
		klog.Fatal("The rpc address cannot be empty")
	}
	if len(podUid) == 0 {
		klog.Fatal("The pod uid cannot be empty")
	}
	if len(containerName) == 0 {
		klog.Fatal("The container name cannot be empty")
	}
	if timeoutSeconds <= 0 {
		klog.Fatal("The timeout must be greater than 0 seconds")
	}
	if util.PathIsNotExist(address) {
		klog.Fatal("The rpc address does not exist")
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	conn, err := grpc.Dial(address,
		grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, d time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, d)
		}))
	if err != nil {
		klog.Fatalf("can't dial %s, error %v", address, err)
	}
	defer func() {
		_ = conn.Close()
	}()

	client := registry.NewVDeviceRegistryClient(conn)
	req := &registry.ContainerDeviceRequest{
		PodUid:        podUid,
		ContainerName: containerName,
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err = client.RegisterContainerDevice(ctx, req); err != nil {
		klog.Fatalf("fail to get response from manager, error %v", err)
	}
}
