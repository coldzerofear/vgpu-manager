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
