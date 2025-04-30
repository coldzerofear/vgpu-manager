package client

import (
	"context"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
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

func WithQPS(qps float32, burst int) Option {
	return func(config *rest.Config) {
		config.QPS = qps
		config.Burst = burst
	}
}

func WithUser(userAgent string) Option {
	return func(config *rest.Config) {
		config.UserAgent = userAgent
	}
}

func WithDefaultContentType() Option {
	return WithContentType(
		"application/vnd.kubernetes.protobuf,application/json",
		"application/json")
}

func WithContentType(acceptContentTypes, contentType string) Option {
	return func(config *rest.Config) {
		config.AcceptContentTypes = acceptContentTypes
		config.ContentType = contentType
	}
}

func GetActivePodsOnNode(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) ([]corev1.Pod, error) {
	fieldSelector, err := fields.ParseSelector("spec.nodeName=" + nodeName + "," +
		"status.phase!=" + string(corev1.PodSucceeded) + ",status.phase!=" + string(corev1.PodFailed))
	if err != nil {
		return nil, err
	}
	var podList *corev1.PodList
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		podList, err = kubeClient.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fieldSelector.String(),
			LabelSelector: labels.FormatLabels(map[string]string{
				util.PodAssignedPhaseLabel: string(util.AssignPhaseAllocating),
			}),
			//ResourceVersion: "0",
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}
