package client

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

var (
	initKubeConfig sync.Once
	kubeConfig     *rest.Config
)

func InitKubeConfig(masterUrl, kubeconfig string) error {
	var err error
	initKubeConfig.Do(func() {
		kubeConfig, err = clientcmd.BuildConfigFromFlags(masterUrl, kubeconfig)
	})
	return err
}

type Option func(*rest.Config)

func NewKubeConfig(opts ...Option) (*rest.Config, error) {
	err := InitKubeConfig("", "")
	if err != nil {
		return nil, err
	}
	config := rest.CopyConfig(kubeConfig)
	for _, opt := range opts {
		opt(config)
	}
	return config, nil
}

func NewClientSet(opts ...Option) (*kubernetes.Clientset, error) {
	config, err := NewKubeConfig(opts...)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func WithTimeoutSecond(seconds uint) Option {
	return func(config *rest.Config) {
		config.Timeout = time.Duration(seconds) * time.Second
	}
}

func WithQPSBurst(qps float32, burst int) Option {
	return func(config *rest.Config) {
		config.QPS = qps
		config.Burst = burst
		config.RateLimiter = nil
	}
}

func WithDefaultUserAgent() Option {
	return WithUserAgent(rest.DefaultKubernetesUserAgent())
}

func WithUserAgent(userAgent string) Option {
	return func(config *rest.Config) {
		config.UserAgent = userAgent
	}
}

func WithDefaultContentType() Option {
	return WithContentType(
		strings.Join([]string{runtime.ContentTypeProtobuf, runtime.ContentTypeJSON}, ","),
		runtime.ContentTypeJSON)
}

func WithContentType(acceptContentTypes, contentType string) Option {
	return func(config *rest.Config) {
		config.AcceptContentTypes = acceptContentTypes
		config.ContentType = contentType
	}
}

func GetActivePodsOnNode(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) ([]corev1.Pod, error) {
	var (
		podList       *corev1.PodList
		err           error
		fieldSelector = fields.AndSelectors(
			fields.OneTermEqualSelector("spec.nodeName", nodeName),
			fields.OneTermNotEqualSelector("status.phase", string(corev1.PodSucceeded)),
			fields.OneTermNotEqualSelector("status.phase", string(corev1.PodFailed))).String()
		labelSelector = labels.FormatLabels(map[string]string{
			util.PodAssignedPhaseLabel: string(util.AssignPhaseAllocating),
		})
		listOptions = metav1.ListOptions{
			FieldSelector: fieldSelector,
			LabelSelector: labelSelector,
		}
	)
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		podList, err = kubeClient.CoreV1().Pods(corev1.NamespaceAll).List(ctx, listOptions)
		return err
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}
