package webhook

import (
	"net/http"
	"sync"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	podmutate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/mutate"
	podvalidate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/validate"
	resvalidate "github.com/coldzerofear/vgpu-manager/pkg/webhook/resourceclaim/validate"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"k8s.io/controller-manager/pkg/healthz"
	"k8s.io/klog/v2"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type NewWebhookHandlerFunc func(rtclient.Client, *options.Options, resourcereader.ResourceAPIReader) (http.Handler, error)

var (
	registerOnce             sync.Once
	registerErr              error
	newWebhookHandlerFuncMap map[string]NewWebhookHandlerFunc
)

func init() {
	newWebhookHandlerFuncMap = make(map[string]NewWebhookHandlerFunc)
	newWebhookHandlerFuncMap[podmutate.Path] = podmutate.NewMutateWebhook
	newWebhookHandlerFuncMap[podvalidate.Path] = podvalidate.NewValidateWebhook
	newWebhookHandlerFuncMap[resvalidate.Path] = resvalidate.NewWebhookHandler
}

func healthCheckMiddleware(healthChecker healthz.UnnamedHealthChecker, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := healthChecker.Check(r); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RegisterWebhookToServer(
	server webhook.Server, checker healthz.UnnamedHealthChecker,
	client rtclient.Client, opt *options.Options,
	reader resourcereader.ResourceAPIReader,
) error {
	registerOnce.Do(func() {
		var webhookHandler http.Handler
		for path, newWebhookFunc := range newWebhookHandlerFuncMap {
			webhookHandler, registerErr = newWebhookFunc(client, opt, reader)
			if registerErr != nil {
				klog.ErrorS(registerErr, "unable to create webhook", "path", path)
				return
			}
			if webhookHandler == nil {
				continue
			}
			if checker != nil {
				webhookHandler = healthCheckMiddleware(checker, webhookHandler)
			}
			klog.V(4).InfoS("Register webhook to server", "path", path)
			server.Register(path, webhookHandler)
		}
	})
	return registerErr
}
