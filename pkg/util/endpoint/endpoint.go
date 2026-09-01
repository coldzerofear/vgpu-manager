package endpoint

import (
	"fmt"
	"net"
	"net/url"
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

func (e Endpoint) HostPort() string {
	return net.JoinHostPort(e.Host, e.Port)
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
