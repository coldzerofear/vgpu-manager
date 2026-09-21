/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package endpoint parses the endpoint strings the remote-GPU components
// exchange: lupine-server (http/https), remote-agent (grpc over TCP, or a
// unix socket for same-node callers), in URL form with every part optional
// except what the caller's defaults cannot supply.
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
	// Unix is a stream socket on the local host: "unix:///abs/path.sock".
	// It has no host or port; Path is the socket path.
	Unix = "unix"
	// Grpc is plaintext gRPC over TCP: "grpc://host:port".
	Grpc = "grpc"
)

type Endpoint struct {
	Scheme Scheme
	Host   string
	Port   string
	Path   string
}

// HostPort returns "host:port" (or just the host when no port is set). Empty
// for unix endpoints, which have neither.
func (e Endpoint) HostPort() string {
	if e.Scheme == Unix {
		return ""
	}
	if e.Port == "" {
		if strings.Contains(e.Host, ":") {
			return "[" + e.Host + "]" // bare IPv6 literal
		}
		return e.Host
	}
	return net.JoinHostPort(e.Host, e.Port)
}

// DefaultPort fills in the port when the parsed endpoint had none. A zero
// port means "no default" and leaves the endpoint alone, as does Unix.
func (e *Endpoint) DefaultPort(port uint) *Endpoint {
	if port != 0 && e.Port == "" && e.Scheme != Unix {
		e.Port = strconv.Itoa(int(port))
	}
	return e
}

// Network is the net.Dial / net.Listen network for this endpoint.
func (e Endpoint) Network() string {
	if e.Scheme == Unix {
		return Unix
	}
	return "tcp"
}

// DialTarget is the address a gRPC client (or net.Dial with Network()) uses
// to reach this endpoint: "host:port" for TCP schemes, "unix:///path" for
// unix sockets (grpc-go resolves that scheme natively). Scheme and path of
// TCP endpoints are dropped: a gateway path prefix needs a protocol-aware
// route, not a dial target.
func (e Endpoint) DialTarget() string {
	if e.Scheme == Unix {
		return Unix + "://" + e.Path
	}
	return e.HostPort()
}

// IsWildcard reports whether the host is unset or the unspecified address
// ("", 0.0.0.0, ::): as a listen address it means every interface, as a dial
// address it means "fill in a host".
func (e Endpoint) IsWildcard() bool {
	if e.Scheme == Unix {
		return false
	}
	if e.Host == "" {
		return true
	}
	ip := net.ParseIP(e.Host)
	return ip != nil && ip.IsUnspecified()
}

// IsLoopback reports whether the host names this machine only: a loopback or
// unspecified IP, "localhost", no host at all, or a unix socket. Such an
// endpoint works for a same-host probe but must never be advertised to
// other nodes.
func (e Endpoint) IsLoopback() bool {
	if e.Scheme == Unix || e.IsWildcard() {
		return true
	}
	if strings.EqualFold(e.Host, "localhost") {
		return true
	}
	ip := net.ParseIP(e.Host)
	return ip != nil && ip.IsLoopback()
}

func (e Endpoint) String() string {
	u := &url.URL{
		Scheme: string(e.Scheme),
		Path:   e.Path,
	}
	if e.Scheme != Unix {
		u.Host = e.HostPort()
	}
	return u.String()
}

type option struct {
	defaultScheme Scheme
	defaultPort   uint
}

// WithDefaultScheme sets the scheme assumed when the input carries none.
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

// ParseEndpoint parses "[scheme://][host][:port][/path]" or
// "unix:///abs/path". Without a scheme the default (http unless overridden)
// applies. Query strings and fragments are rejected rather than silently
// dropped: every consumer re-serialises the endpoint through String().
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
	if u.Opaque != "" {
		return nil, fmt.Errorf("invalid endpoint %q: expected %s://", raw, u.Scheme)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid endpoint %q: query, fragment and userinfo are not supported", raw)
	}

	e := &Endpoint{
		Scheme: Scheme(u.Scheme),
		Host:   u.Hostname(),
		Port:   u.Port(),
		Path:   u.Path,
	}
	if e.Scheme == Unix {
		// url.Parse puts a socket path in Host+Path for "unix://relative/x";
		// only the absolute "unix:///abs/x" form is meaningful.
		if u.Host != "" || !strings.HasPrefix(e.Path, "/") {
			return nil, fmt.Errorf("invalid unix endpoint %q: expected unix:///absolute/path", raw)
		}
		return e, nil
	}
	if e.Port != "" {
		if p, err := strconv.Atoi(e.Port); err != nil || p < 0 || p > 65535 {
			return nil, fmt.Errorf("invalid port %q in endpoint %q", e.Port, raw)
		}
	}
	e.DefaultPort(o.defaultPort)
	return e, nil
}

// ParseEndpoints parses a comma-separated list with the same options for
// every item. Empty items are skipped; an empty list is an error.
func ParseEndpoints(raw string, opts ...func(*option)) ([]*Endpoint, error) {
	var out []*Endpoint
	for _, item := range strings.Split(raw, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		e, err := ParseEndpoint(item, opts...)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty endpoint list")
	}
	return out, nil
}
