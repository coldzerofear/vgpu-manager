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

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Masterminds/semver"
)

func TestEffectiveCUDACeiling(t *testing.T) {
	v := func(s string) *semver.Version { return semver.MustParse(s) }
	cases := []struct {
		driver, server, want *semver.Version
	}{
		{v("12.9.0"), nil, v("12.9.0")},         // server unknown: driver ceiling stands
		{v("12.9.0"), v("12.4.0"), v("12.4.0")}, // older server lowers the ceiling
		{v("12.4.0"), v("13.3.0"), v("12.4.0")}, // newer server: driver still caps
		{nil, v("13.3.0"), v("13.3.0")},         // no driver ceiling: server's
	}
	for i, c := range cases {
		got := EffectiveCUDACeiling(c.driver, c.server)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(c.want)) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
	if EffectiveCUDACeiling(nil, nil) != nil {
		t.Error("both nil must stay nil")
	}
}

func TestProbeServerCUDAVersion(t *testing.T) {
	t.Run("header present, any status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ServerCUDAVersionHeader, "13.3.73")
			w.WriteHeader(http.StatusNotFound) // lupine answers 404 on "/": only the header matters
		}))
		defer srv.Close()
		v, err := ProbeServerCUDAVersion(context.Background(), srv.URL, time.Second)
		if err != nil || v.String() != "13.3.73" {
			t.Fatalf("got %v, %v", v, err)
		}
	})

	t.Run("header missing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		if _, err := ProbeServerCUDAVersion(context.Background(), srv.URL, time.Second); err == nil {
			t.Fatal("expected error for a server without the version header")
		}
	})

	t.Run("header unparseable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ServerCUDAVersionHeader, "not-a-version")
		}))
		defer srv.Close()
		if _, err := ProbeServerCUDAVersion(context.Background(), srv.URL, time.Second); err == nil {
			t.Fatal("expected error for an unparseable version")
		}
	})

	t.Run("bad endpoint", func(t *testing.T) {
		if _, err := ProbeServerCUDAVersion(context.Background(), "ftp://x", time.Second); err == nil {
			t.Fatal("expected error for an unsupported scheme")
		}
	})

	t.Run("server down", func(t *testing.T) {
		if _, err := ProbeServerCUDAVersion(context.Background(), "127.0.0.1:1", 200*time.Millisecond); err == nil {
			t.Fatal("expected error for a closed port")
		}
	})
}
