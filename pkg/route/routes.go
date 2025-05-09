package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/julienschmidt/httprouter"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	// version router path
	versionPath = "/version"
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
	metricsPath = "/metrics"
	apiPrefix   = "/scheduler"
	// predication router path
	filterPerfix = apiPrefix + "/filter"
	bindPerfix   = apiPrefix + "/bind"
)

func checkBody(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		http.Error(w, "Please send a request body", 400)
		return
	}
}

// DebugLogging wraps handler for debugging purposes
func DebugLogging(h httprouter.Handle, path string) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		klog.V(5).Infof("%s request body = %s", path, r.Body)
		h(w, r, p)
		klog.V(5).Infof("%s response = %s", path, w)
	}
}

func AddVersion(router *httprouter.Router) {
	router.GET(versionPath, DebugLogging(VersionRoute, versionPath))
}

// VersionRoute returns the version of router in response
func VersionRoute(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	_, _ = fmt.Fprint(w, fmt.Sprint(version.Get()))
}

func AddHealthProbe(router *httprouter.Router) {
	probeHandler := &healthz.Handler{
		Checks: map[string]healthz.Checker{
			"healthz": healthz.Ping,
			"readyz":  healthz.Ping,
		},
	}
	handlerFunc := func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		probeHandler.ServeHTTP(w, r)
	}
	router.GET(healthzPath, handlerFunc)
	router.GET(readyzPath, handlerFunc)
}

func AddMetricsHandle(router *httprouter.Router, metrics http.Handler) {
	handleFunc := func(writer http.ResponseWriter, request *http.Request, _ httprouter.Params) {
		metrics.ServeHTTP(writer, request)
	}
	router.GET(metricsPath, DebugLogging(handleFunc, metricsPath))
}

func AddFilterPredicate(router *httprouter.Router, predicate predicate.FilterPredicate) {
	path := filterPerfix
	router.POST(path, DebugLogging(FilterPredicateRoute(predicate), path))
}

func FilterPredicateRoute(predicate predicate.FilterPredicate) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		checkBody(w, r)

		var buf bytes.Buffer
		body := io.TeeReader(r.Body, &buf)

		var extenderArgs extenderv1.ExtenderArgs
		var extenderFilterResult *extenderv1.ExtenderFilterResult
		if err := json.NewDecoder(body).Decode(&extenderArgs); err != nil {
			klog.Errorf("Decode extender filter args failed: %v", err)
			extenderFilterResult = &extenderv1.ExtenderFilterResult{
				Error: err.Error(),
			}
		} else {
			extenderFilterResult = predicate.Filter(r.Context(), extenderArgs)
		}

		w.Header().Set("Content-Type", "application/json")
		if resultBody, err := json.Marshal(extenderFilterResult); err != nil {
			klog.ErrorS(err, "Failed to marshal extenderFilterResult",
				"extenderFilterResult", extenderFilterResult)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
		} else {
			klog.V(4).InfoS(predicate.Name()+" return extenderFilterResult",
				"extenderFilterResult", string(resultBody))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(resultBody)
		}
	}
}

func AddBindPredicate(router *httprouter.Router, predicate predicate.BindPredicate) {
	path := bindPerfix
	router.POST(path, DebugLogging(BindPredicateRoute(predicate), path))
}

func BindPredicateRoute(predicate predicate.BindPredicate) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		checkBody(w, r)

		var buf bytes.Buffer
		body := io.TeeReader(r.Body, &buf)

		var extenderBindingArgs extenderv1.ExtenderBindingArgs
		var extenderBindingResult *extenderv1.ExtenderBindingResult

		if err := json.NewDecoder(body).Decode(&extenderBindingArgs); err != nil {
			klog.Errorf("Decode extender binding args failed: %v", err)
			extenderBindingResult = &extenderv1.ExtenderBindingResult{
				Error: err.Error(),
			}
		} else {
			extenderBindingResult = predicate.Bind(r.Context(), extenderBindingArgs)
		}
		w.Header().Set("Content-Type", "application/json")
		if resultBody, err := json.Marshal(extenderBindingResult); err != nil {
			klog.ErrorS(err, "Failed to marshal extenderBindingResult",
				"extenderBindingResult", extenderBindingResult)
			w.WriteHeader(http.StatusInternalServerError)
			errMsg := fmt.Sprintf("{'error':'%s'}", err.Error())
			_, _ = w.Write([]byte(errMsg))
		} else {
			klog.V(4).InfoS(predicate.Name()+" return extenderBindingResult",
				"extenderBindingResult", string(resultBody))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(resultBody)
		}
	}
}
