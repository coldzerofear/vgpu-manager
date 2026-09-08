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

package remote

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeBundle builds a client bundle zip the way lupine's bundle_codegen.py
// does (manifest.json with per-file sha256, files flat) and returns it with
// its etag.
func makeBundle(t *testing.T, files map[string]string, tamper func(m *bundleManifest)) ([]byte, string) {
	t.Helper()
	manifest := bundleManifest{Platforms: []string{LocalClientBundlePlatform()}, Schema: 1}
	for name, content := range files {
		sum := sha256.Sum256([]byte(content))
		manifest.Files = append(manifest.Files, struct {
			Mode   string `json:"mode"`
			Path   string `json:"path"`
			Sha256 string `json:"sha256"`
		}{Mode: "0755", Path: name, Sha256: hex.EncodeToString(sum[:])})
	}
	if tamper != nil {
		tamper(&manifest)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create("manifest.json")
	if err := json.NewEncoder(mw).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

var shimFiles = map[string]string{shimLibCuda: "cuda-shim", shimLibNvml: "nvml-shim", shimLibCudart: "cudart-shim"}

func TestInstallClientBundle(t *testing.T) {
	dir := t.TempDir()
	body, etag := makeBundle(t, shimFiles, nil)
	write := func(b []byte) string {
		p := filepath.Join(t.TempDir(), "b.zip")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if err := installClientBundle(dir, "13.3.73", write(body), &remoteagent.ClientBundleInfo{Etag: etag}); err != nil {
		t.Fatal(err)
	}
	for name, content := range shimFiles {
		got, err := os.ReadFile(filepath.Join(dir, "13.3.73", name))
		if err != nil || string(got) != content {
			t.Fatalf("%s: %q %v", name, got, err)
		}
		if st, _ := os.Stat(filepath.Join(dir, "13.3.73", name)); st.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", name, st.Mode())
		}
	}
	if artifactETag(dir, "13.3.73") != etag {
		t.Fatalf("etag file: %q", artifactETag(dir, "13.3.73"))
	}
	if _, err := os.Stat(filepath.Join(dir, "13.3.73", "manifest.json")); err == nil {
		t.Fatal("manifest must not be installed as a shim")
	}
	// Selection sees the version; the temp directories are not versions.
	if sel, err := selectArtifact(dir, "/host", semver.MustParse("13.3.73")); err != nil || sel.Name != "13.3.73" {
		t.Fatalf("select: %+v %v", sel, err)
	}

	// Reinstall moves the previous directory aside instead of deleting it.
	body2, etag2 := makeBundle(t, map[string]string{shimLibCuda: "cuda-shim-2", shimLibNvml: "nvml-shim-2"}, nil)
	if err := installClientBundle(dir, "13.3.73", write(body2), &remoteagent.ClientBundleInfo{Etag: etag2}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	stale := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stale-13.3.73-") {
			stale++
		}
	}
	if stale != 1 || artifactETag(dir, "13.3.73") != etag2 {
		t.Fatalf("stale=%d etag=%q", stale, artifactETag(dir, "13.3.73"))
	}
	if sel, err := selectArtifact(dir, "/host", semver.MustParse("13.3.73")); err != nil || sel.Name != "13.3.73" {
		t.Fatalf("stale directories must not be selectable: %+v %v", sel, err)
	}

	// Rejections leave the installed version untouched.
	bad := []struct {
		name string
		body []byte
		info *remoteagent.ClientBundleInfo
	}{
		{"etag mismatch", body, &remoteagent.ClientBundleInfo{Etag: etag2}},
		{"content-digest mismatch", body, &remoteagent.ClientBundleInfo{Etag: etag, ContentDigest: "sha-256=:AAAA:"}},
		{"not a sha256 etag", body, &remoteagent.ClientBundleInfo{Etag: `"weak"`}},
	}
	for _, tc := range bad {
		if err := installClientBundle(dir, "13.3.73", write(tc.body), tc.info); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
	}
	tampered, tamperedETag := makeBundle(t, shimFiles, func(m *bundleManifest) { m.Files[0].Sha256 = strings.Repeat("0", 64) })
	if err := installClientBundle(dir, "13.3.73", write(tampered), &remoteagent.ClientBundleInfo{Etag: tamperedETag}); err == nil || !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("file sha mismatch: %v", err)
	}
	escaping, escapingETag := makeBundle(t, shimFiles, func(m *bundleManifest) { m.Files[0].Path = "../" + m.Files[0].Path })
	if err := installClientBundle(dir, "13.3.73", write(escaping), &remoteagent.ClientBundleInfo{Etag: escapingETag}); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("path escape: %v", err)
	}
	if artifactETag(dir, "13.3.73") != etag2 {
		t.Fatal("a rejected bundle must not touch the installed version")
	}
	leftovers, _ := os.ReadDir(dir)
	for _, e := range leftovers {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Fatalf("temp directory left behind: %s", e.Name())
		}
	}
}

func TestEnsureArtifact(t *testing.T) {
	ctx := context.Background()
	body, etag := makeBundle(t, shimFiles, nil)
	fa, agent := startSessionAgent(t, true, "http://10.0.0.1:14833")
	fa.bundle, fa.bundleETag = body, etag
	dir := t.TempDir()
	d := &InjectDriver{config: InjectConfig{ArtifactsDir: dir, HostArtifactsDir: "/host/driver"}}
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-1"}}
	devices := []resultDevice{
		{info: &DeviceInfo{AgentEndpoint: agent, CUDAVersion: semver.MustParse("13.3.73")}},
		{info: &DeviceInfo{AgentEndpoint: agent, CUDAVersion: semver.MustParse("13.3.80")}},
	}
	token := func() (string, error) { return "tok", nil }
	noToken := func() (string, error) { t.Fatal("no download expected"); return "", nil }

	// Nothing on the node: fetched from the floor device's agent, named
	// after the floor version, etag recorded and handed on.
	sel, err := d.ensureArtifact(ctx, claim, devices, map[string]string{agent: etag}, token)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Name != "13.3.73" || sel.ETag != etag || sel.HostDir != "/host/driver/13.3.73" || fa.fetches != 1 || fa.infos != 0 {
		t.Fatalf("sel=%+v fetches=%d infos=%d", sel, fa.fetches, fa.infos)
	}
	if _, err := ensureLdPreloadFile(dir, sel); err != nil {
		t.Fatalf("installed artifact must be usable: %v", err)
	}

	// Same build: nothing fetched. Etag not in hand: the agent is asked.
	if sel, err = d.ensureArtifact(ctx, claim, devices, map[string]string{agent: etag}, noToken); err != nil || sel.ETag != etag || fa.fetches != 1 {
		t.Fatalf("%+v %v fetches=%d", sel, err, fa.fetches)
	}
	if sel, err = d.ensureArtifact(ctx, claim, devices, nil, noToken); err != nil || sel.ETag != etag || fa.fetches != 1 || fa.infos != 1 {
		t.Fatalf("%+v %v fetches=%d infos=%d", sel, err, fa.fetches, fa.infos)
	}

	// The server was rebuilt: the artifact is refreshed in place.
	body2, etag2 := makeBundle(t, map[string]string{shimLibCuda: "cuda-2", shimLibNvml: "nvml-2"}, nil)
	fa.bundle, fa.bundleETag = body2, etag2
	if sel, err = d.ensureArtifact(ctx, claim, devices, map[string]string{agent: etag2}, token); err != nil || sel.ETag != etag2 || fa.fetches != 2 {
		t.Fatalf("%+v %v fetches=%d", sel, err, fa.fetches)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "13.3.73", shimLibCuda)); string(got) != "cuda-2" {
		t.Fatalf("refreshed shim: %q", got)
	}

	// A seeded directory (no etag) is used as is, never verified or replaced.
	seededDir := t.TempDir()
	seeded := filepath.Join(seededDir, "13.3.70")
	if err := os.MkdirAll(seeded, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(seeded, shimLibCuda), []byte("seeded"), 0o755)
	ds := &InjectDriver{config: InjectConfig{ArtifactsDir: seededDir, HostArtifactsDir: "/h"}}
	if sel, err = ds.ensureArtifact(ctx, claim, devices, map[string]string{agent: etag2}, noToken); err != nil || sel.Name != "13.3.70" || sel.ETag != "" {
		t.Fatalf("%+v %v", sel, err)
	}

	// Nothing usable and no bundle to fetch: the selection error stands.
	fa.bundle, fa.bundleETag = nil, ""
	empty := &InjectDriver{config: InjectConfig{ArtifactsDir: t.TempDir(), HostArtifactsDir: "/h"}}
	if _, err = empty.ensureArtifact(ctx, claim, devices, map[string]string{agent: ""}, noToken); err == nil || !strings.Contains(err.Error(), "no client bundle") {
		t.Fatalf("expected the selection error, got %v", err)
	}
}
