/*
Copyright The Kubernetes Authors

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

package validate

import (
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Resource definitions for different API versions.
var (
	// v1 resources.
	resourceClaimResourceV1 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1",
		Resource: "resourceclaims",
	}
	resourceClaimTemplateResourceV1 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1",
		Resource: "resourceclaimtemplates",
	}

	// v1beta1 resources.
	resourceClaimResourceV1Beta1 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1beta1",
		Resource: "resourceclaims",
	}
	resourceClaimTemplateResourceV1Beta1 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1beta1",
		Resource: "resourceclaimtemplates",
	}

	// v1beta2 resources.
	resourceClaimResourceV1Beta2 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1beta2",
		Resource: "resourceclaims",
	}
	resourceClaimTemplateResourceV1Beta2 = metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1beta2",
		Resource: "resourceclaimtemplates",
	}
)

// extractResourceClaim extracts and converts a ResourceClaim from an AdmissionReview to v1 format.
func (rw *resourceClaimWebhook) extractResourceClaim(ar admissionv1.AdmissionReview, raw []byte) (*resourcev1.ResourceClaim, error) {
	deserializer := rw.codecs.UniversalDeserializer()

	// Decode to the appropriate version first
	var obj runtime.Object
	var err error

	switch ar.Request.Resource {
	case resourceClaimResourceV1:
		// Decode as v1
		obj = &resourcev1.ResourceClaim{}
	case resourceClaimResourceV1Beta1:
		// Decode as v1beta1
		obj = &resourcev1beta1.ResourceClaim{}
	case resourceClaimResourceV1Beta2:
		// Decode as v1beta2
		obj = &resourcev1beta2.ResourceClaim{}
	default:
		return nil, fmt.Errorf("unsupported resource version: %s", ar.Request.Resource)
	}

	if _, _, err = deserializer.Decode(raw, nil, obj); err != nil {
		return nil, err
	}

	// Convert to v1 using Kubernetes conversion
	var v1Claim resourcev1.ResourceClaim
	if err := rw.scheme.Convert(obj, &v1Claim, nil); err != nil {
		return nil, fmt.Errorf("failed to convert to v1: %w", err)
	}

	return &v1Claim, nil
}

// extractResourceClaimTemplate extracts and converts a ResourceClaimTemplate from an AdmissionReview to v1 format.
func (rw *resourceClaimWebhook) extractResourceClaimTemplate(ar admissionv1.AdmissionReview) (*resourcev1.ResourceClaimTemplate, error) {
	raw := ar.Request.Object.Raw
	deserializer := rw.codecs.UniversalDeserializer()

	// Decode to the appropriate version first
	var obj runtime.Object
	var err error

	switch ar.Request.Resource {
	case resourceClaimTemplateResourceV1:
		// Decode as v1
		obj = &resourcev1.ResourceClaimTemplate{}
	case resourceClaimTemplateResourceV1Beta1:
		// Decode as v1beta1
		obj = &resourcev1beta1.ResourceClaimTemplate{}
	case resourceClaimTemplateResourceV1Beta2:
		// Decode as v1beta2
		obj = &resourcev1beta2.ResourceClaimTemplate{}
	default:
		return nil, fmt.Errorf("unsupported resource version: %s", ar.Request.Resource)
	}

	if _, _, err = deserializer.Decode(raw, nil, obj); err != nil {
		return nil, err
	}

	// Convert to v1 using Kubernetes conversion
	var v1Template resourcev1.ResourceClaimTemplate
	if err := rw.scheme.Convert(obj, &v1Template, nil); err != nil {
		return nil, fmt.Errorf("failed to convert to v1: %w", err)
	}

	return &v1Template, nil
}
