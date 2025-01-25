package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type server struct {
	registry   *prometheus.Registry
	port       int
	httpServer *http.Server
}

func (s *server) Start(stopCh <-chan struct{}) error {
	if s.httpServer != nil {
		return fmt.Errorf("metrics service has been started and cannot be restarted again")
	}
	opts := promhttp.HandlerOpts{Registry: s.registry}
	handler := promhttp.HandlerFor(s.registry, opts)

	routerHandle := httprouter.New()
	route.AddHealthProbe(routerHandle)
	route.AddMetricsHandle(routerHandle, handler)

	s.httpServer = &http.Server{
		Addr:    "0.0.0.0:" + strconv.Itoa(s.port),
		Handler: routerHandle,
	}

	go func() {
		<-stopCh
		s.Stop()
	}()

	klog.Infof("Metrics server starting on <0.0.0.0:%d>", s.port)
	err := s.httpServer.ListenAndServe()
	s.httpServer = nil
	return err
}

func (s *server) Stop() {
	klog.Infof("Stopping metrics service.")
	if err := s.httpServer.Shutdown(context.Background()); err != nil {
		klog.Errorf("Error while stopping metrics service: %s", err.Error())
	}
	s.httpServer = nil
}

func GetNodeInformer(factory informers.SharedInformerFactory, nodeName string) cache.SharedIndexInformer {
	return factory.InformerFor(&corev1.Node{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "nodes",
			corev1.NamespaceAll, fields.OneTermEqualSelector("metadata.name", nodeName))
		return cache.NewSharedIndexInformer(watcher, &corev1.Node{}, d, cache.Indexers{})
	})
}

func GetPodInformer(factory informers.SharedInformerFactory, nodeName string) cache.SharedIndexInformer {
	return factory.InformerFor(&corev1.Pod{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "pods",
			corev1.NamespaceAll, fields.OneTermEqualSelector("spec.nodeName", nodeName))
		indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
		return cache.NewSharedIndexInformer(watcher, &corev1.Pod{}, d, indexers)
	})
}

func NewServer(registry *prometheus.Registry, port int) *server {
	return &server{
		registry: registry,
		port:     port,
	}
}
