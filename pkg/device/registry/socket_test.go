package registry

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func registryDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), util.Registry)
	require.NoError(t, os.MkdirAll(dir, registryDirMode))
	return dir
}

// mustSocketIdentity reads the identity of whatever currently owns socketPath.
// Production code never does this — it records the identity of the inode it
// staged — but a test needs to be able to ask "who owns the path now".
func mustSocketIdentity(t *testing.T, socketPath string) socketIdentity {
	t.Helper()
	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(socketPath, &stat))
	return socketIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
}

func Test_checkSocketPath(t *testing.T) {
	t.Run("absent path is fine", func(t *testing.T) {
		assert.NoError(t, checkSocketPath(filepath.Join(registryDir(t), SocketFile)))
	})

	t.Run("orphaned socket file is fine", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		listener, err := net.Listen("unix", socket)
		require.NoError(t, err)
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, listener.Close())

		assert.NoError(t, checkSocketPath(socket))
	})

	t.Run("live socket is taken over, not refused", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		listener, err := net.Listen("unix", socket)
		require.NoError(t, err)
		defer listener.Close()

		// A rolling upgrade overlaps; refusing here would CrashLoop the incoming
		// pod until the outgoing one finally exits.
		assert.NoError(t, checkSocketPath(socket))
	})

	t.Run("non-socket path is refused", func(t *testing.T) {
		dir := registryDir(t)
		socket := filepath.Join(dir, SocketFile)
		require.NoError(t, os.WriteFile(socket, []byte("not a socket"), 0o600))

		require.Error(t, checkSocketPath(socket))
		_, statErr := os.Lstat(socket)
		assert.NoError(t, statErr, "an unexpected file must not be destroyed")
	})
}

func Test_bindSocket_replacesAtomically(t *testing.T) {
	dir := registryDir(t)
	socket := filepath.Join(dir, SocketFile)

	first, firstID, err := bindSocket(socket)
	require.NoError(t, err)
	defer first.Close()

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(socketFileMode), info.Mode().Perm())
	assert.Equal(t, firstID, mustSocketIdentity(t, socket))

	// The staging path must not survive a successful publish.
	_, err = os.Lstat(stagedSocketPath(socket, os.Getpid()))
	assert.True(t, os.IsNotExist(err), "staged socket should have been renamed away")

	// A takeover replaces the entry; the predecessor's listener keeps its inode.
	second, secondID, err := bindSocket(socket)
	require.NoError(t, err)
	defer second.Close()

	assert.NotEqual(t, firstID, secondID, "takeover must produce a new inode")
	assert.Equal(t, secondID, mustSocketIdentity(t, socket), "the path must resolve to the newcomer")
}

func Test_removeOwnedSocket_onlyOurInode(t *testing.T) {
	dir := registryDir(t)
	socket := filepath.Join(dir, SocketFile)

	first, firstID, err := bindSocket(socket)
	require.NoError(t, err)
	// Held open on purpose: an inode with no links and no openers is freed and
	// its number handed straight back out, so closing here would let the
	// successor land on the very identity we are trying to tell apart.
	defer first.Close()

	second, secondID, err := bindSocket(socket)
	require.NoError(t, err)
	defer second.Close()
	require.NotEqual(t, firstID, secondID)

	removed, err := removeOwnedSocket(socket, firstID)
	require.NoError(t, err)
	assert.False(t, removed, "a successor's socket must not be unlinked")
	_, statErr := os.Lstat(socket)
	assert.NoError(t, statErr, "successor's socket must still be in place")

	// Its owner, on the other hand, may clean it up.
	removed, err = removeOwnedSocket(socket, secondID)
	require.NoError(t, err)
	assert.True(t, removed)
	_, statErr = os.Lstat(socket)
	assert.True(t, os.IsNotExist(statErr))

	// Nothing left to remove is success, not an error.
	removed, err = removeOwnedSocket(socket, secondID)
	assert.NoError(t, err)
	assert.False(t, removed)
}

// Test_Start_takesOverFromLiveInstance is the regression test for the outage
// this design closes: the incoming server has to be able to publish itself
// while the outgoing one is still serving, and the outgoing one's shutdown must
// not take the newcomer's socket down with it.
func Test_Start_takesOverFromLiveInstance(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	outgoing := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, outgoing.Start())
	assert.True(t, outgoing.IsRunning())

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(socketFileMode), info.Mode().Perm(),
		"socket must stay connectable by any container uid")

	dirInfo, err := os.Lstat(filepath.Join(contPath, util.Registry))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(registryDirMode), dirInfo.Mode().Perm(),
		"registry directory must not be world-writable")

	// An overlapping start must succeed rather than back off: a DaemonSet
	// rolling upgrade routinely has both pods alive for a few seconds.
	incoming := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, incoming.Start())
	assert.True(t, incoming.IsRunning())

	after, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.False(t, os.SameFile(info, after), "the newcomer should own the path")
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err, "the path must be connectable throughout the handover")
	_ = conn.Close()

	// The outgoing server stands down and must leave the newcomer alone.
	outgoing.Stop()
	still, err := os.Lstat(socket)
	require.NoError(t, err, "the newcomer's socket must survive the predecessor's shutdown")
	assert.True(t, os.SameFile(after, still))
	conn, err = net.Dial("unix", socket)
	require.NoError(t, err, "the newcomer must still be serving")
	_ = conn.Close()

	incoming.Stop()
	_, err = os.Lstat(socket)
	assert.True(t, os.IsNotExist(err), "the last owner removes the socket on shutdown")
}

// Test_guardSocket_republishesDeletedSocket covers the predecessor that runs a
// release which unlinks by path on shutdown: it deletes our entry seconds after
// we took over, and nothing but this guard notices.
func Test_guardSocket_republishesDeletedSocket(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	server := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, server.Start())
	defer server.Stop()

	before := mustSocketIdentity(t, socket)
	require.NoError(t, os.Remove(socket))

	// Drive the guard directly rather than waiting out socketGuardInterval.
	server.republishSocketIfMissing()

	after, err := os.Lstat(socket)
	require.NoError(t, err, "the guard must put the socket back")
	assert.NotEqual(t, before, mustSocketIdentity(t, socket), "republishing binds a new inode")
	assert.Equal(t, os.FileMode(socketFileMode), after.Mode().Perm())

	conn, err := net.Dial("unix", socket)
	require.NoError(t, err, "the republished socket must be served")
	_ = conn.Close()
}

// A successor's socket is not "missing", and republishing over it would start a
// rename war between two live servers.
func Test_guardSocket_leavesSuccessorAlone(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	outgoing := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, outgoing.Start())
	defer outgoing.Stop()

	successor, successorID, err := bindSocket(socket)
	require.NoError(t, err)
	defer successor.Close()

	outgoing.republishSocketIfMissing()

	assert.Equal(t, successorID, mustSocketIdentity(t, socket),
		"the guard must not take the path back from a successor")
}

// Test_rollingUpgradeFromLegacyRelease walks the whole handover against a
// predecessor that behaves like a release from before the ownership check:
// it unlinks socket.sock by path when it shuts down, some seconds after we
// have already taken the path over.
func Test_rollingUpgradeFromLegacyRelease(t *testing.T) {
	contPath := t.TempDir()
	registryPath := filepath.Join(contPath, util.Registry)
	require.NoError(t, os.MkdirAll(registryPath, registryDirMode))
	socket := filepath.Join(registryPath, SocketFile)

	// The outgoing pod: a plain listener with Go's unlink-on-close left enabled,
	// which is exactly what the old Stop() did.
	legacy, err := net.Listen("unix", socket)
	require.NoError(t, err)

	// The incoming pod starts while the outgoing one is still serving.
	incoming := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, incoming.Start(), "an overlapping start must not fail")
	defer incoming.Stop()

	conn, err := net.Dial("unix", socket)
	require.NoError(t, err)
	_ = conn.Close()

	// The outgoing pod exits and takes our directory entry with it.
	require.NoError(t, legacy.Close())
	_, err = os.Lstat(socket)
	require.True(t, os.IsNotExist(err), "precondition: the legacy shutdown removed the socket")

	// One guard tick later we are reachable again.
	incoming.republishSocketIfMissing()
	conn, err = net.Dial("unix", socket)
	require.NoError(t, err, "the guard must restore reachability after a legacy predecessor exits")
	_ = conn.Close()
}

func Test_Stop_retiresTheGuard(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	server := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, server.Start())
	server.Stop()

	// Give a guard that ignored the stop signal a chance to misbehave.
	time.Sleep(50 * time.Millisecond)
	server.republishSocketIfMissing()

	_, err := os.Lstat(socket)
	assert.True(t, os.IsNotExist(err), "a stopped server must not republish its socket")
}
