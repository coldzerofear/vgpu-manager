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

package remote

import "testing"

func TestParseServerEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		":14833":                  "http://:14833",
		"10.0.0.7":                "http://10.0.0.7:14833",
		"http://10.0.0.7:15000/p": "http://10.0.0.7:15000/p",
		"https://gw.corp/pool-a":  "https://gw.corp:443/pool-a",
		"https://gw.corp:8443":    "https://gw.corp:8443",
		"[2001:db8::7]":           "http://[2001:db8::7]:14833",
		"127.0.0.1:14833":         "http://127.0.0.1:14833",
	} {
		e, err := ParseServerEndpoint(raw)
		if err != nil || e.String() != want {
			t.Errorf("ParseServerEndpoint(%q) = %v, %v, want %q", raw, e, err, want)
		}
	}
	for _, bad := range []string{"", "grpc://x", "unix:///run/x.sock", "http://x/p?q=1", "ftp://x"} {
		if e, err := ParseServerEndpoint(bad); err == nil {
			t.Errorf("ParseServerEndpoint(%q) = %v, want an error", bad, e)
		}
	}
}

func TestParseAgentEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		":14834":                          "grpc://:14834",
		"10.0.0.7":                        "grpc://10.0.0.7:14834",
		"grpc://10.0.0.7:15000":           "grpc://10.0.0.7:15000",
		"unix:///etc/vgpu-manager/a.sock": "unix:///etc/vgpu-manager/a.sock",
		"http://gpu-a/pool":               "grpc://gpu-a:14834/pool", // older publishers
		"https://gpu-a.example.com":       "grpc://gpu-a.example.com:14834",
		"0.0.0.0:0":                       "grpc://0.0.0.0:0", // listen on any free port
	} {
		e, err := ParseAgentEndpoint(raw)
		if err != nil || e.String() != want {
			t.Errorf("ParseAgentEndpoint(%q) = %v, %v, want %q", raw, e, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "unix://relative.sock", "grpc://x:70000", "tcp://x"} {
		if e, err := ParseAgentEndpoint(bad); err == nil {
			t.Errorf("ParseAgentEndpoint(%q) = %v, want an error", bad, e)
		}
	}
}
