/*
Copyright 2025-2026 coldzerofear

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

package route

import (
	"net/http"
	"net/http/pprof"
	"strconv"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

var (
	runOnce  sync.Once
	debugMux *http.ServeMux
)

func init() {
	debugMux = http.NewServeMux()
	debugMux.HandleFunc("/debug/pprof/", pprof.Index)
	debugMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	debugMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	debugMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	debugMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

func StartDebugServer(port int) {
	runOnce.Do(func() {
		if port <= 0 {
			return
		}
		go func() {
			addr := "0.0.0.0:" + strconv.Itoa(port)
			klog.V(4).Infof("Debug Server starting on <%s>", addr)
			// ReadHeaderTimeout and nothing else. It bounds the one phase an
			// attacker controls - a client that opens a connection and dribbles
			// out its request headers holds a goroutine for as long as it likes
			// otherwise. ReadTimeout and WriteTimeout are deliberately unset:
			// /debug/pprof/profile and /trace stream for as long as the caller
			// asks (?seconds=), so a write deadline would truncate exactly the
			// profiles this server exists to serve.
			srv := &http.Server{
				Addr:              addr,
				Handler:           debugMux,
				ReadHeaderTimeout: 10 * time.Second,
			}
			if err := srv.ListenAndServe(); err != nil {
				klog.ErrorS(err, "Debug Server error occurred")
			}
		}()
	})
}
