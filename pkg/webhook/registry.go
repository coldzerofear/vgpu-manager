package webhook

import (
	"net/http"
	"sync"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	podmutate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/mutate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type newWebhookFunc func(*runtime.Scheme, *options.Options) (*admission.Webhook, error)

var (
	once           sync.Once
	webhookFuncMap map[string]newWebhookFunc
)

func init() {
	webhookFuncMap = make(map[string]newWebhookFunc)
	webhookFuncMap[podmutate.Path] = podmutate.NewMutateWebhook
}

func RegisterWebhookToServer(server webhook.Server, scheme *runtime.Scheme, opt *options.Options) (err error) {
	once.Do(func() {
		var hook http.Handler
		for path, webhookFunc := range webhookFuncMap {
			hook, err = webhookFunc(scheme, opt)
			if err != nil {
				klog.ErrorS(err, "unable to create webhook", "path", path)
				return
			}
			klog.V(4).InfoS("Register webhook to server", "path", path)
			server.Register(path, hook)
		}
	})
	return err
}
