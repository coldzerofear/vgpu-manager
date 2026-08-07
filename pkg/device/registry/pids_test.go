package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func Test_ResetPidsFile(t *testing.T) {
	t.Run("creates the file when missing", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, ResetPidsFile(dir))
		info, err := os.Lstat(filepath.Join(dir, PidsConfig))
		require.NoError(t, err)
		assert.Zero(t, info.Size())
		assert.True(t, info.Mode().IsRegular())
	})

	t.Run("empties in place, keeping the inode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, PidsConfig)
		require.NoError(t, (&DeviceRegistryServerImpl{}).persistPids(dir, []int{7, 8, 9}))
		before, err := os.Lstat(path)
		require.NoError(t, err)

		require.NoError(t, ResetPidsFile(dir))

		after, err := os.Lstat(path)
		require.NoError(t, err)
		assert.Zero(t, after.Size())
		// The inode is the whole point: this file is a bind-mount source, and a
		// replacement would strand the container on the old one.
		assert.True(t, os.SameFile(before, after), "reset must not replace the file")
	})

	t.Run("normalises a mode inherited from an older release", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, PidsConfig)
		require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o777))
		require.NoError(t, os.Chmod(path, 0o777))

		require.NoError(t, ResetPidsFile(dir))

		info, err := os.Lstat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(pidsFileMode), info.Mode().Perm())
	})

	t.Run("refuses to follow a symlink", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "victim")
		require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
		require.NoError(t, os.Symlink(victim, filepath.Join(dir, PidsConfig)))

		require.Error(t, ResetPidsFile(dir))
		content, err := os.ReadFile(victim)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(content))
	})
}

func Test_persistPids(t *testing.T) {
	server := &DeviceRegistryServerImpl{}

	t.Run("refuses an empty list", func(t *testing.T) {
		dir := t.TempDir()
		require.Error(t, server.persistPids(dir, nil))
		_, err := os.Lstat(filepath.Join(dir, PidsConfig))
		assert.True(t, os.IsNotExist(err), "an empty list must not create the file at all")
	})

	t.Run("writes a sorted list and shrinks in place", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, PidsConfig)

		require.NoError(t, server.persistPids(dir, []int{300, 100, 200}))
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "100\n200\n300\n", string(content))

		before, err := os.Lstat(path)
		require.NoError(t, err)

		// A shorter list must leave no tail of the previous, longer one behind.
		require.NoError(t, server.persistPids(dir, []int{42}))
		content, err = os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "42\n", string(content))

		// Rewritten in place, not replaced: pids.config is bind-mounted into the
		// workload container, and swapping the inode would strand that mount on
		// the old one.
		after, err := os.Lstat(path)
		require.NoError(t, err)
		assert.True(t, os.SameFile(before, after), "the file must keep its inode across rewrites")
	})

	t.Run("creates the file without exec or group/other write bits", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, server.persistPids(dir, []int{1}))
		info, err := os.Lstat(filepath.Join(dir, PidsConfig))
		require.NoError(t, err)
		// Asserted as a mask rather than an exact mode because umask applies.
		assert.Zero(t, info.Mode().Perm()&0o133, "want no exec bits and no group/other write, got %s", info.Mode())
	})

	t.Run("refuses to follow a symlink", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "victim")
		require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
		require.NoError(t, os.Symlink(victim, filepath.Join(dir, PidsConfig)))

		require.Error(t, server.persistPids(dir, []int{1}))
		content, err := os.ReadFile(victim)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(content))
	})
}

func Test_ResetPidsFile_doesNotBlockOnAWedgedWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PidsConfig)
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o644))

	held, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer held.Close()
	require.NoError(t, unix.Flock(int(held.Fd()), unix.LOCK_EX))
	defer func() { _ = unix.Flock(int(held.Fd()), unix.LOCK_UN) }()

	// The callers are NodePrepareResources and the NRI CreateContainer hook;
	// both are synchronous from the runtime's point of view, so this has to come
	// back with an error rather than wait for whoever is stuck.
	start := time.Now()
	err = ResetPidsFile(dir)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*lockAcquireBudget, "must give up on the budget, not block")

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "1\n", string(content), "a failed reset must not have truncated anything")
}
