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
			if err := http.ListenAndServe(addr, debugMux); err != nil {
				klog.ErrorS(err, "Debug Server error occurred")
			}
		}()
	})
}
