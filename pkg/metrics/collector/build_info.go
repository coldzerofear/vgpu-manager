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

package collector

import (
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/prometheus/client_golang/prometheus"
)

func NewBuildInfoCollector(nodeName string) prometheus.Collector {
	info := version.Get()
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "exporter_build_info",
		Help: "Exporter component build version information",
		ConstLabels: map[string]string{
			"node":     nodeName,
			"version":  info.Version,
			"branch":   info.GitBranch,
			"commit":   info.GitCommit,
			"platform": info.Platform,
			"date":     info.BuildDate,
		},
	}, func() float64 {
		return 1
	})
}
