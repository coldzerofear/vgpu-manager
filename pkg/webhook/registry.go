package webhook

import (
	"sync"

	"github.com/coldzerofear/vgpu-manager/cmd/webhook/options"
	podmutate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/mutate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type newWebhook func(*runtime.Scheme, *options.Options) *admission.Webhook

var (
	once           sync.Once
	webhookFuncMap map[string]newWebhook
)

func init() {
	webhookFuncMap = make(map[string]newWebhook)
	webhookFuncMap[podmutate.Path] = podmutate.NewMutateWebhook
}

func RegistryWebhookToServer(server webhook.Server, scheme *runtime.Scheme, opt *options.Options) (err error) {
	once.Do(func() {
		for path, webhookFunc := range webhookFuncMap {
			hook := webhookFunc(scheme, opt)
			klog.V(4).Infoln("registry webhook to server", "path", path)
			server.Register(path, hook)
		}
	})
	return err
}
