package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Scheme string

const (
	Http  = "http"
	Https = "https"
	Unix  = "unix"
	Grpc  = "grpc"
)

type Endpoint struct {
	Scheme Scheme
	Host   string
	Port   string
	Path   string
}

// HostPort returns "host:port" (or just the host when no port is set).
func (e Endpoint) HostPort() string {
	if e.Port == "" {
		return e.Host
	}
	return net.JoinHostPort(e.Host, e.Port)
}

// DefaultPort fills in the port when the parsed endpoint had none.
func (e *Endpoint) DefaultPort(port uint) *Endpoint {
	if e.Port == "" && e.Scheme != Unix {
		e.Port = strconv.Itoa(int(port))
	}
	return e
}

func (e Endpoint) String() string {
	u := &url.URL{
		Scheme: string(e.Scheme),
		Path:   e.Path,
	}
	if u.Scheme != Unix {
		u.Host = e.HostPort()
	}
	return u.String()
}

type option struct {
	defaultScheme Scheme
	defaultPort   uint
}

func WithDefaultScheme(scheme Scheme) func(*option) {
	return func(o *option) {
		o.defaultScheme = scheme
	}
}

// WithDefaultPort fills in the port when the parsed endpoint had none.
func WithDefaultPort(port uint) func(*option) {
	return func(o *option) {
		o.defaultPort = port
	}
}

func ParseEndpoint(raw string, opts ...func(*option)) (*Endpoint, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return nil, fmt.Errorf("empty endpoint")
	}
	o := option{defaultScheme: Http}
	for _, opt := range opts {
		opt(&o)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = fmt.Sprintf("%s://%s", o.defaultScheme, endpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case Https, Http, Unix, Grpc:
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	e := &Endpoint{
		Scheme: Scheme(u.Scheme),
		Host:   u.Hostname(),
		Port:   u.Port(),
		Path:   u.Path,
	}
	e.DefaultPort(o.defaultPort)
	return e, nil
}
