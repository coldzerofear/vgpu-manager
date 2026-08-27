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

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const defaultSessionBase = "/etc/vgpu-manager/remote-sessions"

func main() {
	var (
		kube pkgflags.KubeClientConfig
		cfg  remoteagent.Config
	)

	app := &cli.App{
		Name:  "remote-agent",
		Usage: "GPU-node agent for remote GPU sessions (runs alongside lupine-server)",
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "node-name", Usage: "Node this agent runs on (= the driver's pool name).", Destination: &cfg.NodeName, EnvVars: []string{"NODE_NAME"}, Required: true},
			&cli.StringFlag{Name: "driver-name", Usage: "DRA driver name whose slices/claims are consumed.", Value: util.DRADriverName, Destination: &cfg.DriverName, EnvVars: []string{"DRIVER_NAME"}},
			&cli.StringFlag{Name: "session-base", Usage: "Session directory root shared with lupine-server (VGPU_CONFIG_SESSION_BASE).", Value: defaultSessionBase, Destination: &cfg.SessionBase, EnvVars: []string{"VGPU_CONFIG_SESSION_BASE"}},
			&cli.StringFlag{Name: "ready-file", Usage: "File written after preflight; the server container waits for it. Defaults to <session-base>/.agent-ready.", Destination: &cfg.ReadyFile, EnvVars: []string{"READY_FILE"}},
			&cli.StringFlag{Name: "lupine-server-addr", Usage: "lupine-server address to probe.", Value: fmt.Sprintf("127.0.0.1:%d", remote.DefaultServerPort), Destination: &cfg.ServerAddr, EnvVars: []string{"LUPINE_SERVER_ADDR"}},
			&cli.StringFlag{Name: "listen-server-port", Usage: "gRPC listen address.", Value: fmt.Sprintf(":%d", remote.DefaultAgentPort), Destination: &cfg.ListenAddr, EnvVars: []string{"LISTEN_SERVER_PORT"}},
			&cli.StringFlag{Name: "endpoint", Usage: "Endpoint value reported by ServerInfo (informational).", Destination: &cfg.Endpoint, EnvVars: []string{"REMOTE_ENDPOINT"}},
			&cli.BoolFlag{Name: "sm-watcher", Usage: "Mark sessions as using the node-wide external SM watcher.", Destination: &cfg.SMWatcher, EnvVars: []string{"SM_WATCHER"}},
			&cli.DurationFlag{Name: "gc-interval", Usage: "Orphaned session sweep interval.", Value: time.Minute, Destination: &cfg.GCInterval, EnvVars: []string{"GC_INTERVAL"}},
		}, kube.Flags()...),
		Action: func(c *cli.Context) error {
			if cfg.ReadyFile == "" {
				cfg.ReadyFile = cfg.SessionBase + "/.agent-ready"
			}
			clientSets, err := kube.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client sets: %w", err)
			}
			cfg.ClientSets = clientSets

			ctx, cancel := signal.NotifyContext(c.Context, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
			defer cancel()

			return remoteagent.New(cfg).Run(ctx)
		},
	}

	if err := app.RunContext(context.Background(), os.Args); err != nil {
		klog.Errorf("%v", err)
		os.Exit(1)
	}
}
