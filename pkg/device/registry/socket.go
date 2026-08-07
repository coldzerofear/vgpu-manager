package registry

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

const (
	// registryDirMode keeps the registry directory traversable by the workload
	// container (it opens socket.sock and execs device-client by absolute path,
	// neither of which needs the read bit) while denying every non-owner the
	// write bit. A writable directory is what lets an unprivileged host user
	// unlink socket.sock and bind their own in its place, which costs nothing
	// and takes down every GPU container on the node.
	registryDirMode = 0o711

	// socketFileMode has to stay world-writable-ish: the workload container may
	// run as any uid and must be able to connect(2). Connect needs w on the
	// socket inode; the directory mode above is what bounds the blast radius.
	socketFileMode = 0o666

	// stagedSocketSuffix names the path a new listener binds before it is
	// renamed onto socket.sock.
	stagedSocketSuffix = ".staged"

	// livenessProbeTimeout bounds the "is anyone still serving this socket"
	// check. A live server accepts immediately (the listen backlog answers
	// without the accept loop being scheduled); an orphaned socket file fails
	// instantly with ECONNREFUSED. The budget only covers a pathologically
	// loaded node.
	livenessProbeTimeout = 200 * time.Millisecond

	// socketGuardInterval is how often a running server checks that the socket
	// path still exists. It only has to be fast enough to bound the outage
	// caused by the one thing that legitimately deletes our entry — a
	// predecessor running a release that unlinks by path on shutdown — and the
	// check is a single lstat, so this is cheap either way.
	socketGuardInterval = 5 * time.Second
)

// socketIdentity pins a socket file to the inode this server actually created.
// Paths are not identities: a successor may have replaced the file since we
// bound it, and unlinking by path would then delete *their* socket, leaving a
// live server bound to an unreachable inode and every client failing on a path
// that no longer exists.
type socketIdentity struct {
	dev uint64
	ino uint64
}

// warnIfDirectoryWritable reports a registry directory that anyone but its
// owner can write to.
//
// Tightening the mode is best-effort — the server keeps serving when the chmod
// fails, because an unwritable-but-present directory is still a working one.
// The hole it leaves is not obvious from a chmod error alone, though: with the
// write bit open, any local user can unlink socket.sock and bind their own in
// its place, and every GPU container on the node then fails to register. So say
// what the mode actually is rather than leaving it to be inferred.
func warnIfDirectoryWritable(directory string) {
	info, err := os.Lstat(directory)
	if err != nil {
		klog.ErrorS(err, "Failed to inspect the registry directory", "directory", directory)
		return
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		klog.ErrorS(nil, "Registry directory is writable by group or other: any local user can replace "+
			"the registry socket and break device registration on this node",
			"directory", directory, "mode", perm.String(), "wantMode", os.FileMode(registryDirMode).String())
	}
}

// checkSocketPath decides whether the path is ours to take.
//
// Only one thing is refused: a path that exists and is not a socket. That means
// the manager directory is not laid out the way we think it is, and replacing
// an unknown file would destroy something we cannot identify.
//
// A socket that is still being served is *not* refused. Overlapping instances
// are normal — a DaemonSet rolling upgrade can have the outgoing pod still
// serving while the incoming one starts — and refusing would put the incoming
// pod into CrashLoopBackOff until the outgoing one finally exits, which is a
// worse outage than the handover it is trying to avoid. The takeover is safe
// because bindSocket swaps the entry atomically and shutdown only unlinks a
// socket it still owns. What the liveness probe buys is the log line: an
// overlap that never ends is a misconfiguration worth naming.
func checkSocketPath(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to inspect existing registry socket %s: %v", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("registry socket path %s exists but is not a socket (mode %s), refusing to replace it",
			socketPath, info.Mode())
	}

	conn, dialErr := net.DialTimeout("unix", socketPath, livenessProbeTimeout)
	if dialErr != nil {
		// ECONNREFUSED is an orphan left by a killed predecessor. Anything else
		// is unexpected but still not a reason to refuse: the rename below
		// replaces whatever is there without needing to understand it.
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			klog.V(3).InfoS("Existing registry socket did not answer a liveness probe; taking it over",
				"socket", socketPath, "err", dialErr)
		}
		return nil
	}
	_ = conn.Close()
	klog.InfoS("Taking over a registry socket that another instance is still serving. "+
		"This is expected while a rolling upgrade overlaps; if it repeats outside an upgrade, two "+
		"components (device-plugin and DRA kubelet-plugin) are both serving this node",
		"socket", socketPath)
	return nil
}

// bindSocket publishes a new listener at socketPath, replacing whatever entry
// is there, and returns the identity of the inode it created.
//
// The bind happens on a staging path and the result is rename(2)d into place
// rather than unlinking socket.sock and binding it directly. Unlink-then-bind
// leaves a window in which the path does not exist, and a client that connects
// during it gets ENOENT — which for the in-container library means a failed
// registration and a killed workload process. rename is atomic: a client sees
// the old socket or the new one, never nothing.
//
// A predecessor's inode survives the rename with its listener intact, so its
// established connections finish normally; only new connections land here.
func bindSocket(socketPath string) (*net.UnixListener, socketIdentity, error) {
	stagedPath := stagedSocketPath(socketPath, os.Getpid(), stageCounter.Add(1))
	// A staged path can only be left behind by a process killed between bind
	// and rename; see stagedSocketPath for why this reclaims it.
	if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, socketIdentity{}, fmt.Errorf("failed to clear staged registry socket %s: %v", stagedPath, err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: stagedPath, Net: "unix"})
	if err != nil {
		return nil, socketIdentity{}, fmt.Errorf("failed to listen unix: %v", err)
	}
	// Go unlinks the socket path on Close by default, without checking that the
	// file is still the one it bound — and after the rename below, the path it
	// would unlink is socket.sock, which by shutdown time may belong to a
	// successor. Ownership is tracked explicitly instead; see removeOwnedSocket.
	listener.SetUnlinkOnClose(false)

	identity, err := publish(stagedPath, socketPath)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(stagedPath)
		return nil, socketIdentity{}, err
	}
	return listener, identity, nil
}

// publish chmods the staged socket, records its inode, and renames it into
// place. Split out so the identity is read from the staging path: reading it
// from the final path would race a concurrent takeover and could record an
// inode that is not ours, which is exactly the confusion this type exists to
// prevent.
func publish(stagedPath, socketPath string) (socketIdentity, error) {
	if err := os.Chmod(stagedPath, socketFileMode); err != nil {
		return socketIdentity{}, fmt.Errorf("failed to set socket permissions: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(stagedPath, &stat); err != nil {
		return socketIdentity{}, fmt.Errorf("failed to stat registry socket %s: %v", stagedPath, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return socketIdentity{}, fmt.Errorf("registry socket %s is not a socket after bind", stagedPath)
	}
	if err := os.Rename(stagedPath, socketPath); err != nil {
		return socketIdentity{}, fmt.Errorf("failed to publish registry socket at %s: %v", socketPath, err)
	}
	return socketIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, nil
}

// socketMissing reports whether the socket path has no directory entry at all.
//
// A path that resolves to someone else's inode is deliberately not "missing":
// that is a successor who has taken over, and restoring our own entry on top of
// theirs would start a rename war between two live servers.
func socketMissing(socketPath string) (bool, error) {
	_, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to inspect registry socket %s: %v", socketPath, err)
	}
	return false, nil
}

// removeOwnedSocket unlinks the socket file only when the path still resolves
// to the inode this server created. removed is false when the path is gone or
// now belongs to someone else — both are "leave it alone", not failures.
func removeOwnedSocket(socketPath string, identity socketIdentity) (removed bool, err error) {
	var stat unix.Stat_t
	if err = unix.Lstat(socketPath, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("failed to inspect registry socket %s during shutdown: %v", socketPath, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		uint64(stat.Dev) != identity.dev || uint64(stat.Ino) != identity.ino {
		return false, nil
	}
	if err = unix.Unlink(socketPath); err != nil && !errors.Is(err, unix.ENOENT) {
		return false, fmt.Errorf("failed to remove registry socket %s: %v", socketPath, err)
	}
	return true, nil
}

// stageCounter makes every staging path in this process distinct.
var stageCounter atomic.Uint64

// stagedSocketPath names the path one bind attempt stages its listener on.
//
// Both parts matter. The pid separates processes; the counter separates
// attempts within one, because nothing serialises two servers in the same
// process — the guard republishing for one while another starts, or simply two
// instances built from NewDeviceRegistryServer. Sharing a staging path there is
// not a rare interleaving but a routine one: whoever calls os.Remove second
// deletes the other's freshly bound socket, and the loser fails with either
// EADDRINUSE on bind or ENOENT on the chmod that follows.
//
// Reusing pid+counter across a restart is deliberate: a process killed in the
// microseconds between bind and rename leaves a 0-byte socket behind, and a
// later run following the same sequence reclaims it in the os.Remove below.
func stagedSocketPath(socketPath string, pid int, attempt uint64) string {
	return fmt.Sprintf("%s.%d.%d%s", socketPath, pid, attempt, stagedSocketSuffix)
}
