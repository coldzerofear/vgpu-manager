package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
