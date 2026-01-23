module github.com/coldzerofear/vgpu-manager

go 1.25.3

require (
	github.com/NVIDIA/go-nvlib v0.9.0
	github.com/NVIDIA/go-nvml v0.13.0-1
	github.com/NVIDIA/k8s-dra-driver-gpu v0.0.0-20260108173021-6a1af5e6a4d4
	github.com/NVIDIA/nvidia-container-toolkit v1.18.1
	github.com/blang/semver/v4 v4.0.0
	github.com/evanphx/json-patch v0.5.2
	github.com/fsnotify/fsnotify v1.9.0
	github.com/go-logr/logr v1.4.3
	github.com/google/uuid v1.6.0
	github.com/grepplabs/cert-source v0.0.9
	github.com/julienschmidt/httprouter v1.3.0
	github.com/opencontainers/runc v1.4.0
	github.com/prometheus/client_golang v1.23.2
	github.com/sirupsen/logrus v1.9.3
	github.com/spf13/pflag v1.0.10
	github.com/stretchr/testify v1.11.1
	github.com/urfave/cli/v2 v2.27.7
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56
	golang.org/x/time v0.14.0
	gomodules.xyz/jsonpatch/v2 v2.4.0
	google.golang.org/grpc v1.77.0
	google.golang.org/protobuf v1.36.10
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.34.2
	k8s.io/apimachinery v0.34.2
	k8s.io/apiserver v0.34.2
	k8s.io/client-go v0.34.2
	k8s.io/component-base v0.34.2
	k8s.io/dynamic-resource-allocation v0.34.0
	k8s.io/klog/v2 v2.130.1
	k8s.io/kube-scheduler v0.34.2
	k8s.io/kubectl v0.34.2
	k8s.io/kubelet v0.34.2
	k8s.io/kubernetes v1.34.2
	k8s.io/utils v0.0.0-20250604170112-4c0f3b243397
	sigs.k8s.io/controller-runtime v0.22.0
	tags.cncf.io/container-device-interface v1.0.2-0.20251114135136-1b24d969689f
	tags.cncf.io/container-device-interface/specs-go v1.0.0
)

require (
	cel.dev/expr v0.24.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/cyphar/filepath-securejoin v0.5.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/emicklei/go-restful/v3 v3.12.2 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-logr/zapr v1.3.0 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.20.2 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/cel-go v0.26.0 // indirect
	github.com/google/gnostic-models v0.7.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.26.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/moby/sys/capability v0.4.0 // indirect
	github.com/moby/sys/mountinfo v0.7.2 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runtime-spec v1.3.0 // indirect
	github.com/opencontainers/runtime-tools v0.9.1-0.20251114084447-edf4cb3d2116 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/spf13/cobra v1.9.1 // indirect
	github.com/stoewer/go-strcase v1.3.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xrash/smetrics v0.0.0-20240521201337-686a1a2994c1 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.6.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.60.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.58.0 // indirect
	go.opentelemetry.io/otel v1.38.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.34.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.34.0 // indirect
	go.opentelemetry.io/otel/metric v1.38.0 // indirect
	go.opentelemetry.io/otel/sdk v1.38.0 // indirect
	go.opentelemetry.io/otel/trace v1.38.0 // indirect
	go.opentelemetry.io/proto/otlp v1.5.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.45.0 // indirect
	golang.org/x/mod v0.29.0 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/oauth2 v0.32.0 // indirect
	golang.org/x/sync v0.18.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/term v0.37.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251022142026-3a174f9686a8 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251022142026-3a174f9686a8 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.12.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	k8s.io/apiextensions-apiserver v0.34.2 // indirect
	k8s.io/controller-manager v0.0.0 // indirect
	k8s.io/cri-api v0.34.2 // indirect
	k8s.io/cri-client v0.0.0 // indirect
	k8s.io/kube-openapi v0.0.0-20250710124328-f3f2b991d03b // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.31.2 // indirect
	sigs.k8s.io/json v0.0.0-20241014173422-cfa47c3a1cc8 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.3.0 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
)

replace (
	github.com/NVIDIA/go-nvml => github.com/coldzerofear/go-nvml v0.0.0-20250919193215-e435d1485517
	github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates => ./pkg/kubeletplugin/featuregates
	github.com/opencontainers/runc => github.com/opencontainers/runc v1.2.8
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.34.2
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.34.2
	k8s.io/controller-manager => k8s.io/controller-manager v0.34.2
	k8s.io/cri-client => k8s.io/cri-client v0.34.2
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.34.2
	k8s.io/dynamic-resource-allocation => k8s.io/dynamic-resource-allocation v0.34.2
	k8s.io/endpointslice => k8s.io/endpointslice v0.34.2
	k8s.io/externaljwt => k8s.io/externaljwt v0.34.2
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.34.2
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.34.2
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.34.2
	k8s.io/mount-utils => k8s.io/mount-utils v0.34.2
	k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.34.2
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.34.2
)
