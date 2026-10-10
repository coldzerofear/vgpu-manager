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

package bootid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointBootIDMatchesCurrent(t *testing.T) {
	mockBootID := "beef-beef-beef-beef-beefbeef0001"

	dir := t.TempDir()
	path := filepath.Join(dir, "boot_id")
	if err := os.WriteFile(path, []byte(mockBootID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bootIDPath = path

	bootID, err := GetCurrentBootID()
	if err != nil {
		t.Fatal(err)
	}
	if bootID != mockBootID {
		t.Fatalf("expected boot ID to be '%s', got '%s'", mockBootID, bootID)
	}
}
