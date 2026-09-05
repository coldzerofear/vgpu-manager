package endpoint

import (
	"testing"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    *Endpoint
		wantErr bool
	}{
		// ===== 正常用例 =====

		// --- HTTP 完整 URL ---
		{
			name: "http with domain and port",
			raw:  "http://example.com:8080/api/v1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "8080",
				Path:   "/api/v1",
			},
		},
		{
			name: "http with domain no port",
			raw:  "http://example.com/api/v1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "",
				Path:   "/api/v1",
			},
		},
		{
			name: "http with ipv4 and port",
			raw:  "http://192.168.1.1:8080/api/v1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "192.168.1.1",
				Port:   "8080",
				Path:   "/api/v1",
			},
		},
		{
			name: "http with ipv6 and port",
			raw:  "http://[2001:db8::1]:8443/api/v1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "2001:db8::1",
				Port:   "8443",
				Path:   "/api/v1",
			},
		},
		{
			name: "http with ipv6 no port",
			raw:  "http://[2001:db8::1]/api/v1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "2001:db8::1",
				Port:   "",
				Path:   "/api/v1",
			},
		},

		// --- HTTPS 完整 URL ---
		{
			name: "https with domain and port",
			raw:  "https://example.com:443/api/v1",
			want: &Endpoint{
				Scheme: Https,
				Host:   "example.com",
				Port:   "443",
				Path:   "/api/v1",
			},
		},
		{
			name: "https with domain no port",
			raw:  "https://example.com/api/v1",
			want: &Endpoint{
				Scheme: Https,
				Host:   "example.com",
				Port:   "",
				Path:   "/api/v1",
			},
		},
		{
			name: "https with ipv4 and port",
			raw:  "https://10.0.0.1:9090/health",
			want: &Endpoint{
				Scheme: Https,
				Host:   "10.0.0.1",
				Port:   "9090",
				Path:   "/health",
			},
		},
		{
			name: "https with ipv6 and port",
			raw:  "https://[::1]:8443/api/v2",
			want: &Endpoint{
				Scheme: Https,
				Host:   "::1",
				Port:   "8443",
				Path:   "/api/v2",
			},
		},
		{
			name: "only port",
			raw:  ":8443",
			want: &Endpoint{
				Scheme: Http,
				Host:   "",
				Port:   "8443",
				Path:   "",
			},
		},

		// --- 无 scheme 的简写格式（自动补 http://）---
		{
			name: "bare domain:port",
			raw:  "example.com:8080",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "8080",
				Path:   "",
			},
		},
		{
			name: "bare domain only",
			raw:  "example.com",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "",
				Path:   "",
			},
		},
		{
			name: "bare ipv4:port",
			raw:  "192.168.1.1:8080",
			want: &Endpoint{
				Scheme: Http,
				Host:   "192.168.1.1",
				Port:   "8080",
				Path:   "",
			},
		},
		{
			name: "bare ipv4 only",
			raw:  "192.168.1.1",
			want: &Endpoint{
				Scheme: Http,
				Host:   "192.168.1.1",
				Port:   "",
				Path:   "",
			},
		},
		{
			name: "bare ipv6 with brackets and port",
			raw:  "[2001:db8::1]:8080",
			want: &Endpoint{
				Scheme: Http,
				Host:   "2001:db8::1",
				Port:   "8080",
				Path:   "",
			},
		},
		{
			name: "bare ipv6 with brackets no port",
			raw:  "[2001:db8::1]",
			want: &Endpoint{
				Scheme: Http,
				Host:   "2001:db8::1",
				Port:   "",
				Path:   "",
			},
		},

		// --- 边界情况 ---
		{
			name: "path with trailing slash",
			raw:  "http://example.com:8080/api/v1/",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "8080",
				Path:   "/api/v1/",
			},
		},
		{
			name: "path with multiple segments",
			raw:  "http://example.com/a/b/c/d",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "",
				Path:   "/a/b/c/d",
			},
		},
		{
			name: "no path",
			raw:  "http://example.com:8080",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "8080",
				Path:   "",
			},
		},
		{
			name: "whitespace around input",
			raw:  "  http://example.com:8080/api/v1  ",
			want: &Endpoint{
				Scheme: Http,
				Host:   "example.com",
				Port:   "8080",
				Path:   "/api/v1",
			},
		},

		{
			name: "unix scheme",
			raw:  "unix:///var/run/docker.sock",
			want: &Endpoint{
				Scheme: Unix,
				Host:   "",
				Port:   "",
				Path:   "/var/run/docker.sock",
			},
		},

		{
			name: "grpc scheme",
			raw:  "grpc://example.com:9090",
			want: &Endpoint{
				Scheme: Grpc,
				Host:   "example.com",
				Port:   "9090",
				Path:   "",
			},
		},

		// ===== 异常用例 =====

		// --- 空输入 ---
		{
			name:    "empty string",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			raw:     "   ",
			wantErr: true,
		},

		// --- 不支持的 scheme ---
		{
			name:    "ftp scheme",
			raw:     "ftp://example.com:21/files",
			wantErr: true,
		},

		{
			name:    "ws scheme",
			raw:     "ws://example.com:8080/ws",
			wantErr: true,
		},
		{
			name:    "tcp scheme",
			raw:     "tcp://192.168.1.1:6379",
			wantErr: true,
		},

		// --- 非法 URL ---
		{
			name:    "invalid url with spaces in host",
			raw:     "http://exa mple.com:8080",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEndpoint(tt.raw)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseEndpoint(%q) expected error, got nil", tt.raw)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseEndpoint(%q) unexpected error: %v", tt.raw, err)
			}

			if got.Scheme != tt.want.Scheme {
				t.Errorf("Scheme = %q, want %q", got.Scheme, tt.want.Scheme)
			}
			if got.Host != tt.want.Host {
				t.Errorf("Host = %q, want %q", got.Host, tt.want.Host)
			}
			if got.Port != tt.want.Port {
				t.Errorf("Port = %q, want %q", got.Port, tt.want.Port)
			}
			if got.Path != tt.want.Path {
				t.Errorf("Path = %q, want %q", got.Path, tt.want.Path)
			}
		})
	}
}

func TestDefaultPortAndHostPort(t *testing.T) {
	e, err := ParseEndpoint("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.HostPort(); got != "10.0.0.1" {
		t.Fatalf("no port: HostPort() = %q, want bare host", got)
	}
	e.DefaultPort(14833)
	if got := e.HostPort(); got != "10.0.0.1:14833" {
		t.Fatalf("after DefaultPort: %q", got)
	}
	// An explicit port is never overwritten.
	e2, _ := ParseEndpoint("https://gpu-a:443/pool")
	e2.DefaultPort(14833)
	if e2.Port != "443" || e2.Path != "/pool" {
		t.Fatalf("explicit port/path must survive: %+v", e2)
	}
}

func TestParseEndpointOptionsAndUnix(t *testing.T) {
	t.Run("default scheme and port apply only when absent", func(t *testing.T) {
		e, err := ParseEndpoint(":14834", WithDefaultScheme(Grpc), WithDefaultPort(1))
		if err != nil || e.Scheme != Grpc || e.Host != "" || e.Port != "14834" {
			t.Fatalf("%+v %v", e, err)
		}
		e, err = ParseEndpoint("gpu-a", WithDefaultScheme(Grpc), WithDefaultPort(14834))
		if err != nil || e.String() != "grpc://gpu-a:14834" {
			t.Fatalf("%+v %v", e, err)
		}
		e, err = ParseEndpoint("https://gpu-a/pool", WithDefaultScheme(Grpc), WithDefaultPort(14834))
		if err != nil || e.String() != "https://gpu-a:14834/pool" {
			t.Fatalf("explicit scheme must survive the default: %+v %v", e, err)
		}
	})
	t.Run("no default port leaves the port empty (never 0)", func(t *testing.T) {
		e, err := ParseEndpoint("example.com")
		if err != nil || e.Port != "" || e.String() != "http://example.com" {
			t.Fatalf("%+v %v", e, err)
		}
		e.DefaultPort(0)
		if e.Port != "" {
			t.Fatalf("DefaultPort(0) must be a no-op, got %q", e.Port)
		}
	})
	t.Run("unix socket", func(t *testing.T) {
		e, err := ParseEndpoint("unix:///etc/vgpu-manager/agent.sock", WithDefaultScheme(Grpc), WithDefaultPort(14834))
		if err != nil {
			t.Fatal(err)
		}
		if e.Scheme != Unix || e.Host != "" || e.Port != "" || e.Path != "/etc/vgpu-manager/agent.sock" {
			t.Fatalf("%+v", e)
		}
		if e.String() != "unix:///etc/vgpu-manager/agent.sock" || e.DialTarget() != "unix:///etc/vgpu-manager/agent.sock" ||
			e.HostPort() != "" || e.Network() != "unix" || !e.IsLoopback() {
			t.Fatalf("%+v: String=%q DialTarget=%q HostPort=%q Network=%q", e, e.String(), e.DialTarget(), e.HostPort(), e.Network())
		}
		for _, bad := range []string{"unix://relative.sock", "unix://host/path.sock", "unix:relative", "unix://"} {
			if e, err := ParseEndpoint(bad); err == nil {
				t.Errorf("%q must be rejected, got %+v", bad, e)
			}
		}
	})
	t.Run("rejected inputs", func(t *testing.T) {
		for _, bad := range []string{"http://x:-1", "http://x:65536", "http://x:abc", "http://x/p?q=1", "http://x/p#f", "http://u:p@x/", "http:x"} {
			if e, err := ParseEndpoint(bad); err == nil {
				t.Errorf("%q must be rejected, got %+v", bad, e)
			}
		}
	})
	t.Run("IsLoopback and DialTarget for TCP", func(t *testing.T) {
		for raw, loop := range map[string]bool{
			":14833": true, "127.0.0.1:14833": true, "localhost": true, "LOCALHOST:1": true, "0.0.0.0:14833": true, "[::1]:1": true, "[::]:1": true,
			"10.0.0.7": false, "gpu-a.corp": false, "[2001:db8::7]:1": false,
		} {
			e, err := ParseEndpoint(raw)
			if err != nil {
				t.Fatalf("%q: %v", raw, err)
			}
			if e.IsLoopback() != loop {
				t.Errorf("IsLoopback(%q) = %v, want %v", raw, e.IsLoopback(), loop)
			}
		}
		e, _ := ParseEndpoint("grpc://[2001:db8::7]:14834/x")
		if e.DialTarget() != "[2001:db8::7]:14834" || e.Network() != "tcp" || e.String() != "grpc://[2001:db8::7]:14834/x" {
			t.Fatalf("%+v: %q %q %q", e, e.DialTarget(), e.Network(), e.String())
		}
		e, _ = ParseEndpoint("[2001:db8::7]")
		if e.HostPort() != "[2001:db8::7]" || e.String() != "http://[2001:db8::7]" {
			t.Fatalf("bare IPv6 must stay bracketed: %q %q", e.HostPort(), e.String())
		}
	})
	t.Run("ParseEndpoints", func(t *testing.T) {
		eps, err := ParseEndpoints(" grpc://:14834, unix:///run/agent.sock ,", WithDefaultScheme(Grpc), WithDefaultPort(14834))
		if err != nil || len(eps) != 2 || eps[0].String() != "grpc://:14834" || eps[1].String() != "unix:///run/agent.sock" {
			t.Fatalf("%v %v", eps, err)
		}
		for _, bad := range []string{"", " , ", "grpc://:14834,ftp://x"} {
			if eps, err := ParseEndpoints(bad); err == nil {
				t.Errorf("%q must be rejected, got %v", bad, eps)
			}
		}
	})
}
