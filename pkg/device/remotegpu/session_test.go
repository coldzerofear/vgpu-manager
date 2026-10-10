/*
Copyright 2026 coldzerofear

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

package remotegpu

import (
	"regexp"
	"testing"
)

func TestSessionToken(t *testing.T) {
	token := SessionToken("pod-uid", "app")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(token) {
		t.Fatalf("token %q must be a 32 character hex digest", token)
	}
	if token != SessionToken("pod-uid", "app") {
		t.Fatal("the token of a container must be stable")
	}
	for _, other := range []struct{ uid, container string }{
		{"pod-uid", "init"},  // another container of the same pod
		{"other-uid", "app"}, // the same container name in another pod
		{"pod-uid_app", ""},  // the key itself is not a container name
	} {
		if got := SessionToken(other.uid, other.container); got == token {
			t.Errorf("SessionToken(%q, %q) must differ from the app token", other.uid, other.container)
		}
	}
}

func TestSessionKeyAndContainer(t *testing.T) {
	key := SessionKey("pod-uid", "app")
	if key != "pod-uid_app" {
		t.Fatalf("SessionKey = %q", key)
	}
	if name, ok := SessionContainer("pod-uid", key); !ok || name != "app" {
		t.Fatalf("SessionContainer = %q, %v", name, ok)
	}
	// A container name may contain the separator; only the pod prefix is cut.
	if name, ok := SessionContainer("pod-uid", SessionKey("pod-uid", "app_1")); !ok || name != "app_1" {
		t.Fatalf("SessionContainer = %q, %v", name, ok)
	}
	for _, bad := range []string{"other-uid_app", "pod-uid", "pod-uid_", ""} {
		if name, ok := SessionContainer("pod-uid", bad); ok {
			t.Errorf("SessionContainer(%q) = %q, want no match", bad, name)
		}
	}
}
