package server

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
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/exp/maps"
	"golang.org/x/time/rate"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type Middleware func(handler http.Handler) (http.Handler, error)

type Server struct {
	mutex        sync.Mutex
	collectors   []prometheus.Collector
	labels       prometheus.Labels
	limiter      *rate.Limiter
	timeout      time.Duration
	port         *int
	debugMetrics bool
	httpServer   *http.Server
	middleware   Middleware
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

func (s *Server) Start(ctx context.Context) (err error) {
	if !s.mutex.TryLock() {
		return fmt.Errorf("metrics service has been started and cannot be restarted again")
	}
	defer s.mutex.Unlock()

	var (
		lastResp                         = &lastResponse{}
		registry                         = prometheus.NewRegistry()
		registerer prometheus.Registerer = registry
		gatherer   prometheus.Gatherer   = registry
	)
	if len(s.labels) > 0 {
		registerer = prometheus.WrapRegistererWith(s.labels, registerer)
	}
	if s.debugMetrics {
		for _, collector := range []prometheus.Collector{
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		} {
			if err = registerer.Register(collector); err != nil {
				return err
			}
		}
	}
	for _, collector := range s.collectors {
		if err = registerer.Register(collector); err != nil {
			return err
		}
	}

	metricHandler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		Registry:      registerer,
		ErrorLog:      klogErrLog{},
		Timeout:       s.timeout,
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
	metricHandler = promhttp.InstrumentMetricHandler(registerer, metricHandler)
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			metricHandler.ServeHTTP(recorder, r)
			lastResp.SetResponse(recorder.GetResponse())
		}
	})
	if s.middleware != nil {
		handler, err = s.middleware(handler)
		if err != nil {
			return err
		}
	}
	routerHandle := httprouter.New()
	route.AddHealthProbe(routerHandle)
	route.AddMetricsHandle(routerHandle, handler)

	s.httpServer = &http.Server{
		Addr:    "0.0.0.0:" + strconv.Itoa(*s.port),
		Handler: routerHandle,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()
			if err := s.stop(ctx); err != nil {
				klog.ErrorS(err, "error while stopping metrics service")
			}
			close(idleConnsClosed)
		case <-idleConnsClosed:
			_ = s.stop(ctx)
		}
	}()

	klog.Infof("Metrics server starting on <0.0.0.0:%d>", *s.port)
	// Block here until the service exits.
	if err = s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		close(idleConnsClosed)
		return err
	}

	<-idleConnsClosed
	return nil
}

func (s *Server) stop(ctx context.Context) error {
	klog.Infof("Stopping metrics service")
	if server := s.httpServer; server != nil {
		if err := server.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

type Option func(*Server)

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

func WithMiddleware(fn Middleware) Option {
	return func(s *Server) {
		s.middleware = fn
	}
}

func WithLabels(labels prometheus.Labels) Option {
	return func(s *Server) {
		if s.labels == nil {
			s.labels = prometheus.Labels{}
		}
		maps.Copy(s.labels, labels)
	}
}

func WithCollectors(cs ...prometheus.Collector) Option {
	return func(s *Server) {
		s.collectors = append(s.collectors, cs...)
	}
}

func WithDebugMetrics(b bool) Option {
	return func(s *Server) {
		s.debugMetrics = b
	}
}

func NewServer(opts ...Option) *Server {
	s := &Server{}
	for _, opt := range opts {
		opt(s)
	}

	if s.port == nil {
		s.port = ptr.To[int](8080)
	}
	return s
}
