package registry

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

const (
	// RegistryLockFile is the sentinel the server flocks for the lifetime of its
	// listener. It is what makes "one registry server per manager directory" an
	// enforced invariant rather than a convention: the device-plugin and the DRA
	// kubelet-plugin resolve the same <manager-dir>/registry/socket.sock path, and
	// nothing else stops both from binding it in turn.
	RegistryLockFile = ".registry.lock"

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

	// registryLockMode is owner-only. Nothing but this server ever opens it.
	registryLockMode = 0o600

	// staleSocketProbeTimeout bounds the "is anyone still serving this socket"
	// check. A live server accepts immediately (the listen backlog answers
	// without the accept loop being scheduled); an orphaned socket file fails
	// instantly with ECONNREFUSED. The budget only covers a pathologically
	// loaded node.
	staleSocketProbeTimeout = 200 * time.Millisecond
)

// errRegistryLocked reports that another live server owns the registry
// directory. Kept distinct so callers can tell "someone else is serving, back
// off and retry" apart from a genuine local failure.
var errRegistryLocked = errors.New("registry directory is locked by another server")

// socketIdentity pins a socket file to the inode this server actually created.
// Paths are not identities: between bind and shutdown another instance can have
// replaced the file, and unlinking by path then deletes *their* socket, leaving
// a live server bound to an unreachable inode and every client failing on a
// path that no longer exists.
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

// acquireDirectoryLock takes the exclusive, non-blocking flock that marks this
// process as the owner of the registry directory. The lock is advisory but
// sufficient: the only contenders are our own two server implementations.
//
// The returned file must be handed to releaseDirectoryLock; closing it (or the
// process dying) releases the lock, so a crashed predecessor never wedges a
// restart.
func acquireDirectoryLock(directory string) (*os.File, error) {
	lockPath := filepath.Join(directory, RegistryLockFile)
	// O_NOFOLLOW: the lock lives beside the socket in a directory we have just
	// tightened, but an older release left it 0777, so a symlink planted before
	// the upgrade could still be sitting there.
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, registryLockMode)
	if err != nil {
		return nil, fmt.Errorf("failed to open registry lock %s: %v", lockPath, err)
	}
	lockFile := os.NewFile(uintptr(fd), lockPath)

	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held, refusing to take over the socket of a running instance",
				errRegistryLocked, lockPath)
		}
		return nil, fmt.Errorf("failed to lock registry directory %s: %v", directory, err)
	}

	// Verify only after locking: before the lock we could be racing whoever is
	// still setting the file up.
	info, err := lockFile.Stat()
	if err != nil {
		releaseDirectoryLock(lockFile)
		return nil, fmt.Errorf("failed to inspect registry lock %s: %v", lockPath, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != os.Geteuid() {
		releaseDirectoryLock(lockFile)
		return nil, fmt.Errorf("registry lock %s is not a regular file owned by this process", lockPath)
	}
	return lockFile, nil
}

// releaseDirectoryLock drops the lock and closes the descriptor. Closing alone
// would release it; the explicit unlock keeps the intent readable and does not
// depend on a descriptor never being duplicated.
func releaseDirectoryLock(lockFile *os.File) {
	if lockFile == nil {
		return
	}
	_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	_ = lockFile.Close()
}

// removeStaleSocket clears the way for a fresh bind, but only for a socket that
// nothing is serving. The previous behaviour — unconditional unlink — is what
// turns two overlapping instances into a node-wide outage: the newcomer steals
// the path from a live server, and the live server's own shutdown then unlinks
// the newcomer's socket.
//
// Reaching here already means we hold the directory lock, so a live peer of
// ours is impossible; the probe covers the leftovers that the lock cannot see
// (a socket bound by a foreign process, or by an instance predating the lock).
func removeStaleSocket(socketPath string) error {
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

	conn, dialErr := net.DialTimeout("unix", socketPath, staleSocketProbeTimeout)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("%w: %s is still accepting connections", errRegistryLocked, socketPath)
	}
	// ECONNREFUSED is the signature of an orphaned socket file: the inode is
	// there, nothing is listening. Anything else (EACCES, ETIMEDOUT, ...) is a
	// state we do not understand well enough to destroy.
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("failed to probe existing registry socket %s: %v", socketPath, dialErr)
	}
	if err = os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove stale registry socket %s: %v", socketPath, err)
	}
	return nil
}

// readSocketIdentity records which inode the bind produced, so shutdown can
// tell our socket apart from a successor's.
func readSocketIdentity(socketPath string) (socketIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(socketPath, &stat); err != nil {
		return socketIdentity{}, fmt.Errorf("failed to stat registry socket %s: %v", socketPath, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return socketIdentity{}, fmt.Errorf("registry socket %s is not a socket after bind", socketPath)
	}
	return socketIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, nil
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
