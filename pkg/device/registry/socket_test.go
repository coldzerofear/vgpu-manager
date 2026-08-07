package registry

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func registryDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), util.Registry)
	require.NoError(t, os.MkdirAll(dir, registryDirMode))
	return dir
}

func mustSocketIdentity(t *testing.T, socketPath string) socketIdentity {
	t.Helper()
	identity, err := readSocketIdentity(socketPath)
	require.NoError(t, err)
	return identity
}

func Test_acquireDirectoryLock_exclusive(t *testing.T) {
	dir := registryDir(t)

	first, err := acquireDirectoryLock(dir)
	require.NoError(t, err)
	require.NotNil(t, first)

	// flock conflicts between distinct open file descriptions, including two in
	// the same process — which is what makes this a real guard against the
	// device-plugin and the DRA kubelet-plugin both serving the same directory.
	_, err = acquireDirectoryLock(dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, errRegistryLocked)

	releaseDirectoryLock(first)

	second, err := acquireDirectoryLock(dir)
	require.NoError(t, err, "lock must be reusable once released")
	releaseDirectoryLock(second)
}

func Test_removeStaleSocket(t *testing.T) {
	t.Run("absent path is a no-op", func(t *testing.T) {
		assert.NoError(t, removeStaleSocket(filepath.Join(registryDir(t), SocketFile)))
	})

	t.Run("orphaned socket file is removed", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		listener, err := net.Listen("unix", socket)
		require.NoError(t, err)
		// Close without unlinking: exactly the leftover a killed server leaves.
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, listener.Close())

		require.NoError(t, removeStaleSocket(socket))
		_, err = os.Lstat(socket)
		assert.True(t, os.IsNotExist(err), "stale socket should be gone")
	})

	t.Run("live socket is refused, not stolen", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		listener, err := net.Listen("unix", socket)
		require.NoError(t, err)
		defer listener.Close()

		err = removeStaleSocket(socket)
		require.Error(t, err)
		assert.ErrorIs(t, err, errRegistryLocked)
		_, statErr := os.Lstat(socket)
		assert.NoError(t, statErr, "a live peer's socket must survive")
	})

	t.Run("non-socket path is refused", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		require.NoError(t, os.WriteFile(socket, []byte("not a socket"), 0o600))

		require.Error(t, removeStaleSocket(socket))
		_, statErr := os.Lstat(socket)
		assert.NoError(t, statErr, "an unexpected file must not be destroyed")
	})
}

func Test_removeOwnedSocket_onlyOurInode(t *testing.T) {
	dir := registryDir(t)
	socket := filepath.Join(dir, SocketFile)

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	identity, err := readSocketIdentity(socket)
	require.NoError(t, err)
	// Held open on purpose: an inode with no links and no openers is freed and
	// its number handed straight back out, so closing here would let the
	// successor land on the very identity we are trying to tell apart. Keeping
	// it open also matches the real overlap — the incumbent is still serving
	// when a successor takes the path.
	defer listener.Close()

	// A successor replaces the path with its own socket.
	require.NoError(t, os.Remove(socket))
	successor, err := net.Listen("unix", socket)
	require.NoError(t, err)
	successor.(*net.UnixListener).SetUnlinkOnClose(false)
	defer successor.Close()
	require.NotEqual(t, identity, mustSocketIdentity(t, socket), "test precondition: successor must be a distinct inode")

	removed, err := removeOwnedSocket(socket, identity)
	require.NoError(t, err)
	assert.False(t, removed, "a successor's socket must not be unlinked")
	_, statErr := os.Lstat(socket)
	assert.NoError(t, statErr, "successor's socket must still be in place")

	// Its owner, on the other hand, may clean it up.
	successorIdentity, err := readSocketIdentity(socket)
	require.NoError(t, err)
	removed, err = removeOwnedSocket(socket, successorIdentity)
	require.NoError(t, err)
	assert.True(t, removed)
	_, statErr = os.Lstat(socket)
	assert.True(t, os.IsNotExist(statErr))

	// Nothing left to remove is success, not an error.
	removed, err = removeOwnedSocket(socket, successorIdentity)
	assert.NoError(t, err)
	assert.False(t, removed)
}

// Test_Start_doesNotStealFromLiveInstance is the regression test for the
// outage this change closes: instance B used to unlink A's socket and bind its
// own, and A's shutdown then unlinked B's, leaving nothing at the path and
// every container unable to register.
func Test_Start_doesNotStealFromLiveInstance(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	first := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, first.Start())
	assert.True(t, first.IsRunning())

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(socketFileMode), info.Mode().Perm(), "socket must stay connectable by any container uid")

	dirInfo, err := os.Lstat(filepath.Join(contPath, util.Registry))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(registryDirMode), dirInfo.Mode().Perm(), "registry directory must not be world-writable")

	// A second instance must fail rather than take the socket over.
	second := NewDeviceRegistryServer(contPath, nil, nil)
	err = second.Start()
	require.Error(t, err)
	assert.ErrorIs(t, err, errRegistryLocked)
	assert.False(t, second.IsRunning())

	// The incumbent is untouched and still serving.
	after, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.True(t, os.SameFile(info, after), "the live socket must not have been replaced")
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err, "incumbent must still be accepting connections")
	_ = conn.Close()

	// Once the incumbent stands down, the socket is cleaned up and the path is
	// free for a successor.
	first.Stop()
	_, err = os.Lstat(socket)
	assert.True(t, os.IsNotExist(err), "own socket should be removed on shutdown")

	require.NoError(t, second.Start())
	second.Stop()
}

// Test_Stop_leavesSuccessorSocketAlone covers the other half of the old bug:
// even if something outside our control replaces the socket while we serve,
// shutdown must not delete the replacement.
func Test_Stop_leavesSuccessorSocketAlone(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	server := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, server.Start())

	require.NoError(t, os.Remove(socket))
	successor, err := net.Listen("unix", socket)
	require.NoError(t, err)
	successor.(*net.UnixListener).SetUnlinkOnClose(false)
	defer successor.Close()

	server.Stop()

	_, err = os.Lstat(socket)
	assert.NoError(t, err, "successor's socket must survive our shutdown")
}
