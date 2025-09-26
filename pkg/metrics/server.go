package metrics

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type Server struct {
	registry   *prometheus.Registry
	limiter    *rate.Limiter
	timeout    time.Duration
	port       *int
	httpServer *http.Server
}

type klogErrLog struct{}

func (k klogErrLog) Println(v ...interface{}) {
	klog.Errorln(v...)
}

type response struct {
	header http.Header
	status int
	body   []byte
}

func (r *response) Clone() response {
	return response{
		header: r.header.Clone(),
		status: r.status,
		body:   bytes.Clone(r.body),
	}
}

type lastResponse struct {
	mutex    sync.RWMutex
	response response
}

func (r *lastResponse) GetResponse() response {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.response.Clone()
}

func (r *lastResponse) SetResponse(resp response) {
	resp = resp.Clone()
	r.mutex.Lock()
	r.response = resp
	r.mutex.Unlock()
}

type responseRecorder struct {
	writer http.ResponseWriter
	buffer *bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header {
	return r.writer.Header()
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.buffer.Write(b)
	return r.writer.Write(b)
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.writer.WriteHeader(status)
}

func (r *responseRecorder) GetResponse() response {
	return response{
		body:   r.buffer.Bytes(),
		status: r.status,
		header: r.Header(),
	}
}

func (s *Server) Start(stopCh <-chan struct{}) error {
	if s.httpServer != nil {
		return fmt.Errorf("metrics service has been started and cannot be restarted again")
	}

	var (
		opts = promhttp.HandlerOpts{
			Registry: s.registry,
			ErrorLog: klogErrLog{},
			Timeout:  s.timeout,
		}
		lastResp = &lastResponse{}
		next     = promhttp.InstrumentMetricHandler(s.registry, promhttp.HandlerFor(s.registry, opts))
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil && !s.limiter.Allow() {
			resp := lastResp.GetResponse()
			if resp.status != http.StatusOK {
				http.Error(w, "Too many requests", http.StatusTooManyRequests)
				return
			}
			w.Header().Set("X-Rate-Limited", "true")
			for k, v := range resp.header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.status)
			_, _ = w.Write(resp.body)
		} else {
			recorder := &responseRecorder{
				writer: w,
				buffer: bytes.NewBuffer(nil),
				status: http.StatusOK,
			}
			next.ServeHTTP(recorder, r)
			lastResp.SetResponse(recorder.GetResponse())
		}
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

func (s *Server) Stop() {
	klog.Infof("Stopping metrics service")
	if err := s.httpServer.Shutdown(context.Background()); err != nil {
		klog.Errorf("Error while stopping metrics service: %s", err.Error())
	}
	s.httpServer = nil
}

type Option func(*Server)

func WithRegistry(registry *prometheus.Registry) Option {
	return func(s *Server) {
		s.registry = registry
	}
}

func WithPort(port *int) Option {
	return func(s *Server) {
		s.port = port
	}
}

func WithLimiter(limiter *rate.Limiter) Option {
	return func(s *Server) {
		s.limiter = limiter
	}
}

func WithTimeoutSecond(seconds uint) Option {
	return func(s *Server) {
		s.timeout = time.Duration(seconds) * time.Second
	}
}

func NewServer(opts ...Option) *Server {
	s := &Server{}
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
