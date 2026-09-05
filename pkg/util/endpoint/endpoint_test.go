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
