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

package webhook

import (
	"net/http"
	"sync"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	podmutate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/mutate"
	podvalidate "github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/validate"
	resvalidate "github.com/coldzerofear/vgpu-manager/pkg/webhook/resourceclaim/validate"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	vcjobmutate "github.com/coldzerofear/vgpu-manager/pkg/webhook/volcanojob/mutate"
	vcjobvalidate "github.com/coldzerofear/vgpu-manager/pkg/webhook/volcanojob/validate"
	"k8s.io/client-go/tools/events"
	"k8s.io/controller-manager/pkg/healthz"
	"k8s.io/klog/v2"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type NewHandlerFunc func(rtclient.Client, *options.Options, resourcereader.ResourceAPIReader, events.EventRecorderLogger) (http.Handler, error)

var (
	registerOnce    sync.Once
	registerErr     error
	handlerRegistry map[string]NewHandlerFunc
)

func init() {
	handlerRegistry = make(map[string]NewHandlerFunc)
	handlerRegistry[podmutate.Path] = podmutate.NewMutateWebhook
	handlerRegistry[podvalidate.Path] = podvalidate.NewValidateWebhook
	handlerRegistry[resvalidate.Path] = resvalidate.NewValidateWebhook
	handlerRegistry[vcjobmutate.Path] = vcjobmutate.NewMutateWebhook
	handlerRegistry[vcjobvalidate.Path] = vcjobvalidate.NewValidateWebhook
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
	recorder events.EventRecorderLogger,
) error {
	registerOnce.Do(func() {
		var webhookHandler http.Handler
		for path, newHandler := range handlerRegistry {
			webhookHandler, registerErr = newHandler(client, opt, reader, recorder)
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
