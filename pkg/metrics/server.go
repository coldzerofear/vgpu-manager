package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type server struct {
	registry   *prometheus.Registry
	limiter    *rate.Limiter
	port       *int
	httpServer *http.Server
}

func (s *server) Start(stopCh <-chan struct{}) error {
	if s.httpServer != nil {
		return fmt.Errorf("metrics service has been started and cannot be restarted again")
	}
	opts := promhttp.HandlerOpts{Registry: s.registry}
	next := promhttp.HandlerFor(s.registry, opts)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil && !s.limiter.Allow() {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
	routerHandle := httprouter.New()
	route.AddHealthProbe(routerHandle)
	route.AddMetricsHandle(routerHandle, handler)

	s.httpServer = &http.Server{
		Addr:    "0.0.0.0:" + strconv.Itoa(*s.port),
		Handler: routerHandle,
	}

	go func() {
		<-stopCh
		s.Stop()
	}()

	klog.Infof("Metrics server starting on <0.0.0.0:%d>", *s.port)
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

type Option func(*server)

func WithRegistry(registry *prometheus.Registry) Option {
	return func(s *server) {
		s.registry = registry
	}
}

func WithPort(port *int) Option {
	return func(s *server) {
		s.port = port
	}
}

func WithLimiter(limiter *rate.Limiter) Option {
	return func(s *server) {
		s.limiter = limiter
	}
}

func NewServer(opts ...Option) *server {
	s := &server{}
	for _, opt := range opts {
		opt(s)
	}
	// set default value
	if s.registry == nil {
		s.registry = prometheus.DefaultRegisterer.(*prometheus.Registry)
	}
	if s.port == nil {
		s.port = ptr.To[int](8080)
	}
	return s
}
