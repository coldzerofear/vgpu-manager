/*
 * Copyright 2023 The Kubernetes Authors.
 * Copyright 2023 NVIDIA CORPORATION.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package flags

import (
	"fmt"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/urfave/cli/v2"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	draclient "k8s.io/dynamic-resource-allocation/client"
)

type KubeClientConfig struct {
	KubeConfig   string
	KubeAPIQPS   float64
	KubeAPIBurst int
}

type ClientSets struct {
	Resource *draclient.Client
	Core     coreclientset.Interface
	//	Nvidia   nvclientset.Interface
}

func (k *KubeClientConfig) Flags() []cli.Flag {
	flags := []cli.Flag{
		&cli.StringFlag{
			Category:    "Kubernetes client:",
			Name:        "kubeconfig",
			Usage:       "Absolute path to the `KUBECONFIG` file. Either this flag or the KUBECONFIG env variable need to be set if the driver is being run out of cluster.",
			Destination: &k.KubeConfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.Float64Flag{
			Category:    "Kubernetes client:",
			Name:        "kube-api-qps",
			Usage:       "`QPS` to use while communicating with the Kubernetes apiserver.",
			Value:       5,
			Destination: &k.KubeAPIQPS,
			EnvVars:     []string{"KUBE_API_QPS"},
		},
		&cli.IntFlag{
			Category:    "Kubernetes client:",
			Name:        "kube-api-burst",
			Usage:       "`Burst` to use while communicating with the Kubernetes apiserver.",
			Value:       10,
			Destination: &k.KubeAPIBurst,
			EnvVars:     []string{"KUBE_API_BURST"},
		},
	}

	return flags
}

func (k *KubeClientConfig) NewClientSetConfig() (*rest.Config, error) {
	if err := client.InitKubeConfig("", k.KubeConfig); err != nil {
		return nil, fmt.Errorf("initialization of kubeConfig failed: %v", err)
	}

	config, err := client.NewKubeConfig(
		client.WithQPSBurst(float32(k.KubeAPIQPS), k.KubeAPIBurst),
		client.WithDefaultUserAgent())
	if err != nil {
		return nil, fmt.Errorf("new kubeConfig failed: %v", err)
	}

	return config, nil
}

func (k *KubeClientConfig) NewClientSets() (ClientSets, error) {
	csconfig, err := k.NewClientSetConfig()
	if err != nil {
		return ClientSets{}, err
	}
	coreclient, err := coreclientset.NewForConfig(csconfig)
	if err != nil {
		return ClientSets{}, fmt.Errorf("create core client: %w", err)
	}

	//nvclient, err := nvclientset.NewForConfig(csconfig)
	//if err != nil {
	//	return ClientSets{}, fmt.Errorf("create nvidia client: %w", err)
	//}

	return ClientSets{
		Resource: draclient.New(coreclient),
		Core:     coreclient,
		//		Nvidia:   nvclient,
	}, nil
}
