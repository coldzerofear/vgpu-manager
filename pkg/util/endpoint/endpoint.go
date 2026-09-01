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
func (e *Endpoint) DefaultPort(port int) *Endpoint {
	if e.Port == "" {
		e.Port = strconv.Itoa(port)
	}
	return e
}

func (e Endpoint) String() string {
	u := &url.URL{
		Scheme: string(e.Scheme),
		Host:   e.HostPort(),
		Path:   e.Path,
	}
	return u.String()
}

func ParseEndpoint(raw string) (*Endpoint, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return nil, fmt.Errorf("empty endpoint")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = fmt.Sprintf("%s://%s", Http, endpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case Https:
	case Http:
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return &Endpoint{
		Scheme: Scheme(u.Scheme),
		Host:   u.Hostname(),
		Port:   u.Port(),
		Path:   u.Path,
	}, nil
}
