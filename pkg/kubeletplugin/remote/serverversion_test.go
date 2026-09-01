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
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver"
)

func TestProbeServerCUDAVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("header is read even from a 404", func(t *testing.T) {
		// lupine answers "/" with 404 but still stamps the version header.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ServerCUDAVersionHeader, "13.3.73")
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		v, err := ProbeServerCUDAVersion(ctx, strings.TrimPrefix(srv.URL, "http://"), time.Second)
		if err != nil || v.String() != "13.3.73" {
			t.Fatalf("expected 13.3.73, got v=%v err=%v", v, err)
		}
	})

	t.Run("endpoint with a scheme is used as is", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ServerCUDAVersionHeader, "12.9.1")
		}))
		defer srv.Close()
		v, err := ProbeServerCUDAVersion(ctx, srv.URL, time.Second)
		if err != nil || v.String() != "12.9.1" {
			t.Fatalf("expected 12.9.1, got v=%v err=%v", v, err)
		}
	})

	t.Run("missing header is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		if _, err := ProbeServerCUDAVersion(ctx, strings.TrimPrefix(srv.URL, "http://"), time.Second); err == nil {
			t.Fatal("expected an error without the version header")
		}
	})

	t.Run("bad version string is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ServerCUDAVersionHeader, "cuda-thirteen")
		}))
		defer srv.Close()
		if _, err := ProbeServerCUDAVersion(ctx, strings.TrimPrefix(srv.URL, "http://"), time.Second); err == nil {
			t.Fatal("expected an error for an unparseable version")
		}
	})

	t.Run("server down is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		addr := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		if _, err := ProbeServerCUDAVersion(ctx, addr, time.Second); err == nil {
			t.Fatal("expected an error when nothing listens")
		}
	})
}

func TestEffectiveCUDACeiling(t *testing.T) {
	v := func(s string) *semver.Version { return semver.MustParse(s) }
	cases := []struct {
		name           string
		driver, server *semver.Version
		want           string
	}{
		{"server lower wins", v("13.3.0"), v("12.9.1"), "12.9.1"},
		{"driver lower wins", v("12.4.0"), v("13.3.73"), "12.4.0"},
		{"equal", v("12.9.1"), v("12.9.1"), "12.9.1"},
		{"server unknown keeps driver", v("13.3.0"), nil, "13.3.0"},
		{"driver unknown keeps server", nil, v("12.9.1"), "12.9.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveCUDACeiling(c.driver, c.server)
			if got == nil || got.String() != c.want {
				t.Fatalf("got %v, want %s", got, c.want)
			}
		})
	}
	if EffectiveCUDACeiling(nil, nil) != nil {
		t.Fatal("both unknown must stay nil")
	}
}
