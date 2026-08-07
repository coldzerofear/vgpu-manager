// Package selinux relabels the host directories vgpu-manager bind-mounts into
// workload containers so that a CONFINED container can actually use them.
//
// Why this exists: every path the in-container library touches --
// <manager-root>/<...>/{config,vgpu_lock,vmem_node,sm_node}, the injected
// libvgpu-control.so, /etc/ld.so.preload, the watcher and registry dirs -- is a
// host directory bind-mounted into a pod we do not control. On a node with
// SELinux in enforcing mode the workload runs as container_t while those host
// paths carry a system label (etc_t / default_t), so every open() is denied
// with EACCES no matter how permissive the Unix mode is. The directories are
// already created 0777 (see util.EnsureDir), which settles the UID axis --
// notably OpenShift's arbitrary-UID SCCs -- but the label axis is orthogonal
// and mode bits do nothing for it.
//
// Failure to relabel is deliberately silent. vgpu-manager must keep working
// when it cannot relabel: SELinux may be disabled or absent (the common case,
// where this whole package is a no-op), the daemon may have been deployed
// without privileges, or the filesystem may not support extended attributes.
// None of those are reasons to fail an allocation. The in-container library
// degrades rather than dying when a region is unreachable, so the worst
// outcome of a failed relabel is the same reduced-isolation mode that a node
// without these features already runs in.
package selinux

import (
	"io/fs"
	"os"
	"path/filepath"

	goselinux "github.com/opencontainers/selinux/go-selinux"
	"k8s.io/klog/v2"
)

// DefaultFileLabel is the SELinux context applied to every path handed to a
// workload container.
//
// container_file_t is the type container runtimes give to content a container
// may read and write. The level is bare s0 -- no MCS categories -- on purpose:
// a container is assigned a random category pair (s0:cN,cM) and may only touch
// files whose level it dominates. Since these directories are consumed by a pod
// whose categories we neither know nor control, and in the DRA/partition case
// by several pods at once, an s0 file is the only spelling every one of them
// can reach.
const DefaultFileLabel = "system_u:object_r:container_file_t:s0"

// FileLabelEnv overrides DefaultFileLabel. An escape hatch for clusters with a
// custom policy whose container type is named differently; setting it to an
// empty value disables relabeling altogether.
const FileLabelEnv = "VGPU_SELINUX_FILE_LABEL"

// skipNames are directory entries a recursive relabel must never descend into.
//
// .host_proc is the in-container mount point for the host's /proc. It is
// normally absent on the host side, but if a stale mount point exists,
// following it would walk (and attempt to relabel) the real /proc.
var skipNames = map[string]struct{}{
	".host_proc": {},
}

// label returns the context to apply, or "" when relabeling is disabled either
// because SELinux is not enabled on this node or because the operator turned it
// off through FileLabelEnv.
func label() string {
	value, set := os.LookupEnv(FileLabelEnv)
	if set {
		return value
	}
	if !goselinux.GetEnabled() {
		return ""
	}
	return DefaultFileLabel
}

// Enabled reports whether relabeling will do anything on this node. Callers use
// it to skip the walk in RelabelRecursive; correctness never depends on it.
func Enabled() bool {
	return label() != ""
}

// Relabel best-effort applies the container-shareable label to a single path.
// Errors are swallowed by design -- see the package comment.
func Relabel(path string) {
	fileLabel := label()
	if fileLabel == "" || path == "" {
		return
	}
	relabel(path, fileLabel)
}

// RelabelRecursive best-effort relabels path and everything beneath it.
//
// Used at daemon start-up for the manager root, which picks up the artifacts
// the init container installed (libvgpu-control.so, ld.so.preload) plus any
// directory left over from a previous incarnation. Directories created later go
// through util.EnsureDir, which relabels each one as it appears.
func RelabelRecursive(root string) {
	fileLabel := label()
	if fileLabel == "" || root == "" {
		return
	}
	// Errors from the walk itself are ignored for the same reason as errors
	// from the relabel: a path we cannot even stat is a path we were never
	// going to be able to label.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Keep walking siblings rather than aborting the whole tree.
			return nil //nolint:nilerr // best-effort by design
		}
		if _, skip := skipNames[d.Name()]; skip {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks are labeled, never followed: WalkDir does not descend into
		// them, so a link pointing outside the manager root cannot drag the
		// walk onto host system paths.
		relabel(path, fileLabel)
		return nil
	})
}

func relabel(path, fileLabel string) {
	if err := goselinux.SetFileLabel(path, fileLabel); err != nil {
		// V(5): on a node without SELinux-capable storage, or a daemon running
		// unprivileged, this would otherwise fire for every path on every
		// allocation. The condition is not actionable per-path.
		klog.V(5).ErrorS(err, "set SELinux file label failed (continuing without it)",
			"path", path, "label", fileLabel)
	}
}
