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
