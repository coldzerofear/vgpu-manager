/*
Copyright 2024-2026 coldzerofear

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

package client

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"
)

func TestPatchMetadataJSONBytes(t *testing.T) {
	// Without a resourceVersion the body is exactly what it always was:
	// callers that do not set it must keep patching unconditionally.
	b, err := PatchMetadata{Annotations: map[string]*string{"a": ptr.To("1"), "b": nil}}.JSONBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"metadata":{"annotations":{"a":"1","b":null}}}` {
		t.Fatalf("unconditional patch = %s", got)
	}
	if strings.Contains(string(b), "resourceVersion") {
		t.Fatal("an unset resourceVersion must not be serialized")
	}
	// With one, it becomes the optimistic-concurrency precondition.
	b, err = PatchMetadata{Labels: map[string]*string{"l": ptr.To("v")}, ResourceVersion: "42"}.JSONBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"metadata":{"labels":{"l":"v"},"resourceVersion":"42"}}` {
		t.Fatalf("conditional patch = %s", got)
	}
}
