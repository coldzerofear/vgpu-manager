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

func GetKubeConfig(mutations ...func(*rest.Config)) (*rest.Config, error) {
	err := InitKubeConfig("", "")
	if err != nil {
		return nil, err
	}
	config := rest.CopyConfig(kubeConfig)
	for _, mutation := range mutations {
		mutation(config)
	}
	return config, nil
}

func GetClientSet(mutations ...func(*rest.Config)) (*kubernetes.Clientset, error) {
	config, err := GetKubeConfig(mutations...)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func MutationQPS(qps float32, burst int) func(*rest.Config) {
	return func(config *rest.Config) {
		config.QPS = qps
		config.Burst = burst
	}
}

func MutationUser(userAgent string) func(*rest.Config) {
	return func(config *rest.Config) {
		config.UserAgent = userAgent
	}
}

func MutationContentType(acceptContentTypes, contentType string) func(*rest.Config) {
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
