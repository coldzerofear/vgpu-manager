package main

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	registerUuid   string
	timeoutSeconds = 12
	version        bool
)

func runApp() (exitCode int) {
	exitCode = 1

	if len(address) == 0 {
		klog.Errorf("The rpc address cannot be empty")
		return exitCode
	}

	registerUuid = strings.TrimSpace(registerUuid)
	if len(registerUuid) == 0 {
		if len(podUid) == 0 {
			klog.Errorf("The pod uid cannot be empty")
			return exitCode
		}
		if len(containerName) == 0 {
			klog.Errorf("The container name cannot be empty")
			return exitCode
		}
	}
	if timeoutSeconds <= 0 {
		klog.Errorf("The timeout must be greater than 0 seconds")
		return exitCode
	}
	if util.PathIsNotExist(address) {
		klog.Errorf("The rpc address does not exist")
		return exitCode
	}

	timeout := time.Duration(timeoutSeconds) * time.Second
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, d time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, d)
		}))
	if err != nil {
		klog.Errorf("can't dial %s: %v", address, err)
		return exitCode
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer func() {
		cancel()
		_ = conn.Close()
	}()

	client := registry.NewVDeviceRegistryClient(conn)
	request := &registry.ContainerDeviceRequest{
		PodUid:        podUid,
		ContainerName: containerName,
		RegisterUuid:  registerUuid,
	}

	if _, err = client.RegisterContainerDevice(ctx, request); err != nil {
		klog.Errorf("fail to get response from manager: %v", err)
		return exitCode
	}

	exitCode = 0
	return exitCode
}

func main() {
	cmdFlags := pflag.CommandLine
	cmdFlags.SortFlags = false
	cmdFlags.StringVar(&address, "address", "", "RPC address location for dial.")
	cmdFlags.StringVar(&podUid, "pod-uid", "", "Pod UID of caller.")
	cmdFlags.StringVar(&containerName, "container-name", "", "Pod container name of caller.")
	cmdFlags.StringVar(&registerUuid, "register-uuid", "", "UUID registered on the manager client.")
	cmdFlags.IntVar(&timeoutSeconds, "timeout", timeoutSeconds, "RPC connection timeout in seconds.")
	cmdFlags.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.Parse()

	defer klog.Flush()
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		return
	}

	if exitCode := runApp(); exitCode != 0 {
		klog.FlushAndExit(klog.ExitFlushTimeout, exitCode)
	}
}
