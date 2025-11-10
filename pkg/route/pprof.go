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
	if port <= 0 {
		return
	}
	runOnce.Do(func() {
		go func() {
			addr := "0.0.0.0:" + strconv.Itoa(port)
			klog.V(4).Infof("Debug Server starting on <%s>", addr)
			if err := http.ListenAndServe(addr, debugMux); err != nil {
				klog.ErrorS(err, "Debug Server error occurred")
			}
		}()
	})
}
