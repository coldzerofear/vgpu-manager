package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
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
	initConfigOnce sync.Once
	initErr        error
	kubeConfig     *rest.Config
)

type configOption struct {
	rest.Config
	masterUrl      string
	kubeConfigPath string
}

func initKubeConfig(opt *configOption) (*rest.Config, error) {
	initConfigOnce.Do(func() {
		switch {
		case opt == nil:
			kubeConfig, initErr = rest.InClusterConfig()
		case opt.kubeConfigPath == "" && opt.masterUrl == "":
			kubeConfig, initErr = rest.InClusterConfig()
		default:
			kubeConfig, initErr = clientcmd.BuildConfigFromFlags(opt.masterUrl, opt.kubeConfigPath)
		}
	})
	return kubeConfig, initErr
}

type Option func(*configOption)

func NewKubeConfig(opts ...Option) (*rest.Config, error) {
	o := &configOption{}
	for _, opt := range opts {
		opt(o)
	}
	config, err := initKubeConfig(o)
	if err != nil {
		return nil, err
	}
	o.Config = *rest.CopyConfig(config)
	for _, opt := range opts {
		opt(o)
	}
	return &o.Config, nil
}

func NewClientSet(opts ...Option) (*kubernetes.Clientset, error) {
	config, err := NewKubeConfig(opts...)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func WithConfigMasterURL(masterUrl string) Option {
	return func(config *configOption) {
		config.masterUrl = masterUrl
	}
}

func WithKubeConfigPath(kubeConfigPath string) Option {
	return func(config *configOption) {
		config.kubeConfigPath = kubeConfigPath
	}
}

func WithTimeoutSecond(seconds uint) Option {
	return func(config *configOption) {
		config.Timeout = time.Duration(seconds) * time.Second
	}
}

func WithQPSBurst(qps float32, burst int) Option {
	return func(config *configOption) {
		config.QPS = qps
		config.Burst = burst
		config.RateLimiter = nil
	}
}

func WithDefaultUserAgent() Option {
	return WithUserAgent(DefaultUserAgent())
}

// adjustCommand returns the last component of the
// OS-specific command path for use in User-Agent.
func adjustCommand(p string) string {
	// Unlikely, but better than returning "".
	if len(p) == 0 {
		return "unknown"
	}
	return filepath.Base(p)
}

// adjustCommit returns sufficient significant figures of the commit's git hash.
func adjustCommit(c string) string {
	if len(c) == 0 {
		return "unknown"
	}
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// adjustVersion strips "alpha", "beta", etc. from version in form
// major.minor.patch-[alpha|beta|etc].
func adjustVersion(v string) string {
	if len(v) == 0 {
		return "unknown"
	}
	seg := strings.SplitN(v, "-", 2)
	return seg[0]
}

// DefaultUserAgent returns a User-Agent string built from static global vars.
func DefaultUserAgent() string {
	return fmt.Sprintf("%s/%s (%s) %s/%s",
		adjustCommand(os.Args[0]),
		adjustVersion(version.Get().Version),
		version.Get().Platform,
		util.ComponentName,
		adjustCommit(version.Get().GitCommit))
}

func WithUserAgent(userAgent string) Option {
	return func(config *configOption) {
		config.UserAgent = userAgent
	}
}

func WithContentType(acceptContentTypes, contentType string) Option {
	return func(config *configOption) {
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
