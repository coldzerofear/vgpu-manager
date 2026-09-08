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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// The lupine client bundle: every lupine-server binary embeds the client
// shims it was built with, one zip per platform, and serves them over plain
// HTTP on its RPC port (lupine #691). The zip holds manifest.json (path,
// mode and sha256 per file) plus the shims themselves. Its etag is the
// build identity: a client that sends it as LUPINE_CLIENT_ETAG is refused
// (426) by a server built with anything else, which is the version check
// the RPC protocol itself does not have.
const (
	// ClientBundlePathPrefix is followed by the platform ("linux/amd64").
	ClientBundlePathPrefix = "/.well-known/lupine/client/v1/"
	// ClientBundleContentType is the media type lupine-server serves.
	ClientBundleContentType = "application/vnd.lupine.client-bundle.v1+zip"
	// ArtifactETagFile, inside a client artifact version directory, records
	// the etag of the bundle the directory was installed from. Absent for
	// directories seeded by other means (an init container).
	ArtifactETagFile = ".etag"
	// clientBundleTimeout bounds one download at the inject side.
	clientBundleTimeout = 60 * time.Second
	// clientBundleChunk is the stream chunk size, under the default 4 MiB
	// gRPC message limit.
	ClientBundleChunkSize = 1 << 20
	// clientBundleMaxSize refuses runaway downloads.
	clientBundleMaxSize = 512 << 20
)

// ServerHTTPClient talks to lupine-server's HTTP/1.x side (probes, bundle
// downloads). See probeClient for why it is configured as it is.
var ServerHTTPClient = probeClient

var clientBundlePlatforms = sets.New(
	"linux/amd64", "linux/arm64",
	"macos/amd64", "macos/arm64",
	"windows/amd64", "windows/arm64",
)

// LocalClientBundlePlatform is the platform of this process, the one a
// consumer pod on this node needs.
func LocalClientBundlePlatform() string {
	return "linux/" + runtime.GOARCH
}

// ClientBundlePlatform normalises an (os, arch) pair to a lupine platform
// name; empty parts default to linux and this process's architecture.
func ClientBundlePlatform(osName, arch string) (string, error) {
	if osName == "" {
		osName = "linux"
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	platform := strings.ToLower(osName) + "/" + strings.ToLower(arch)
	if !clientBundlePlatforms.Has(platform) {
		return "", fmt.Errorf("unsupported client bundle platform %q (supported: %s)", platform, strings.Join(sets.List(clientBundlePlatforms), ", "))
	}
	return platform, nil
}

// ClientBundleURL is where lupine-server at serverEndpoint serves the
// bundle for platform.
func ClientBundleURL(serverEndpoint, platform string) (string, error) {
	endpoint, err := ParseServerEndpoint(serverEndpoint)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(endpoint.String(), "/") + ClientBundlePathPrefix + platform, nil
}

// ProbeClientBundleETag asks lupine-server for the etag of its bundle for
// platform without downloading it. "" with a nil error means the server
// answers but embeds no bundle for that platform (404).
func ProbeClientBundleETag(ctx context.Context, serverEndpoint, platform string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url, err := ClientBundleURL(serverEndpoint, platform)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := ServerHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lupine-server %s: %w", serverEndpoint, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header.Get("Etag"), nil
	case http.StatusNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("lupine-server %s: HEAD %s: %s", serverEndpoint, url, resp.Status)
	}
}

// FetchClientBundle downloads a client bundle through the agent at
// agentEndpoint into w and returns its metadata. With req.IfNoneMatch equal
// to the agent's current etag nothing is written and info.NotModified is
// set. The bytes are not verified here: installClientBundle does that
// against the etag and the manifest.
func FetchClientBundle(ctx context.Context, agentEndpoint string, req *remoteagent.FetchClientBundleRequest, w io.Writer) (*remoteagent.ClientBundleInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, clientBundleTimeout)
	defer cancel()

	conn, err := dialAgent(agentEndpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	stream, err := remoteagent.NewRemoteAgentClient(conn).FetchClientBundle(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	info := first.GetInfo()
	if info == nil {
		return nil, fmt.Errorf("remote-agent %s: bundle stream did not start with its metadata", agentEndpoint)
	}
	if info.NotModified {
		return info, nil
	}
	if info.Size > clientBundleMaxSize {
		return nil, fmt.Errorf("remote-agent %s: bundle of %d bytes exceeds the %d byte limit", agentEndpoint, info.Size, clientBundleMaxSize)
	}
	var written int64
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
		}
		chunk := msg.GetChunk()
		if written+int64(len(chunk)) > clientBundleMaxSize {
			return nil, fmt.Errorf("remote-agent %s: bundle exceeds the %d byte limit", agentEndpoint, clientBundleMaxSize)
		}
		n, err := w.Write(chunk)
		if err != nil {
			return nil, err
		}
		written += int64(n)
	}
	if info.Size > 0 && written != info.Size {
		return nil, fmt.Errorf("remote-agent %s: bundle truncated (%d of %d bytes)", agentEndpoint, written, info.Size)
	}
	return info, nil
}

// bundleManifest is manifest.json as lupine's bundle_codegen.py writes it.
type bundleManifest struct {
	Files []struct {
		Mode   string `json:"mode"`
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
	} `json:"files"`
	Platforms []string `json:"platforms"`
	Schema    int      `json:"schema"`
}

// etagDigest extracts the hex sha256 from a bundle etag ("sha256:<hex>",
// optionally quoted as an HTTP entity tag).
func etagDigest(etag string) (string, bool) {
	value := strings.Trim(strings.TrimSpace(etag), `"`)
	if !strings.HasPrefix(value, "sha256:") {
		return "", false
	}
	digest := strings.ToLower(strings.TrimPrefix(value, "sha256:"))
	if len(digest) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return digest, true
}

// verifyBundleDigest checks the zip against the etag and, when present,
// the content-digest ("sha-256=:<base64>:") the server sent with it.
func verifyBundleDigest(zipPath string, info *remoteagent.ClientBundleInfo) error {
	want, ok := etagDigest(info.Etag)
	if !ok {
		return fmt.Errorf("bundle etag %q is not a sha256 etag", info.Etag)
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sum := h.Sum(nil)
	if got := hex.EncodeToString(sum); got != want {
		return fmt.Errorf("bundle sha256 %s does not match its etag %s", got, want)
	}
	if cd := strings.TrimSpace(info.ContentDigest); cd != "" {
		encoded := strings.TrimSuffix(strings.TrimPrefix(cd, "sha-256=:"), ":")
		if encoded == cd {
			return fmt.Errorf("unsupported content-digest %q", cd)
		}
		if got := base64.StdEncoding.EncodeToString(sum); got != encoded {
			return fmt.Errorf("bundle sha256 does not match its content-digest %q", cd)
		}
	}
	return nil
}

// installClientBundle turns a verified-by-etag bundle zip into the client
// artifact version directory <artifactsDir>/<name>: files are extracted to
// a private directory, each checked against the manifest, the etag is
// recorded, and the directory is swapped into place. A directory already
// there is moved aside (".stale-<name>-<time>"), not deleted: pods bind
// mounted it and keep it alive; it is invisible to artifact selection
// (not a version name) and can be removed once no such pod is left.
func installClientBundle(artifactsDir, name, zipPath string, info *remoteagent.ClientBundleInfo) error {
	if err := verifyBundleDigest(zipPath, info); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer zr.Close()
	entries := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	manifestFile, ok := entries["manifest.json"]
	if !ok {
		return errors.New("bundle has no manifest.json")
	}
	var manifest bundleManifest
	if err := readZipJSON(manifestFile, &manifest); err != nil {
		return fmt.Errorf("bundle manifest: %w", err)
	}
	if manifest.Schema != 1 {
		return fmt.Errorf("bundle manifest schema %d is not supported", manifest.Schema)
	}
	if len(manifest.Files) == 0 {
		return errors.New("bundle manifest lists no files")
	}

	tmp, err := os.MkdirTemp(artifactsDir, ".install-"+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // no-op once renamed into place
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	for _, entry := range manifest.Files {
		// Flat archive by construction: a path that is not a plain file
		// name is a malformed (or hostile) bundle.
		if entry.Path == "" || entry.Path != filepath.Base(entry.Path) || entry.Path == "." || entry.Path == ".." {
			return fmt.Errorf("bundle manifest names an invalid path %q", entry.Path)
		}
		zf, ok := entries[entry.Path]
		if !ok {
			return fmt.Errorf("bundle manifest lists %s but the archive has no such file", entry.Path)
		}
		mode := os.FileMode(0o755)
		if entry.Mode != "" {
			parsed, err := strconv.ParseUint(entry.Mode, 8, 32)
			if err != nil {
				return fmt.Errorf("bundle manifest: mode %q of %s: %w", entry.Mode, entry.Path, err)
			}
			mode = os.FileMode(parsed) & os.ModePerm
		}
		if err := extractZipFile(zf, filepath.Join(tmp, entry.Path), mode, entry.Sha256); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ArtifactETagFile), []byte(info.Etag+"\n"), 0o644); err != nil {
		return err
	}

	final := filepath.Join(artifactsDir, name)
	if _, err := os.Stat(final); err == nil {
		aside := filepath.Join(artifactsDir, fmt.Sprintf(".stale-%s-%d", name, time.Now().UnixNano()))
		if err := os.Rename(final, aside); err != nil {
			return fmt.Errorf("move previous artifact %s aside: %w", name, err)
		}
		klog.Infof("Client artifact %s replaced (previous copy kept at %s for pods still using it)", name, aside)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("install artifact %s: %w", name, err)
	}
	return nil
}

func readZipJSON(f *zip.File, out interface{}) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(out)
}

// extractZipFile writes one archive member to dst with mode, verifying its
// sha256 (hex) on the way.
func extractZipFile(f *zip.File, dst string, mode os.FileMode, wantSha256 string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, clientBundleMaxSize))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSha256) {
		return fmt.Errorf("bundle file %s: sha256 %s does not match the manifest's %s", f.Name, got, wantSha256)
	}
	return nil
}

// artifactETag is the etag recorded for an installed artifact version, ""
// when the directory was not installed from a bundle.
func artifactETag(artifactsDir, name string) string {
	data, err := os.ReadFile(filepath.Join(artifactsDir, name, ArtifactETagFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
