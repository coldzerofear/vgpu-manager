package registry

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// pidsFileMode is the mode pids.config is created with. Must match the mode
// passed to open() in write_to_disk (see server.go): whichever of the two
// creates the file first decides, and they should not disagree.
//
// Group/other get read only. The file carries host PIDs, and the container is
// meant to consume it, not author it.
const pidsFileMode = 0o644

// ResetPidsFile creates <configDir>/pids.config when it is missing and empties
// it when it is not — deliberately without replacing it.
//
// Unlink-and-recreate (or write-temp-and-rename) would be the more obvious
// implementation and is wrong here: the file is bind-mounted into the workload
// container, a bind mount is pinned to an inode, and swapping the inode leaves
// the container looking at the old one forever. Unlinking is not even possible
// once the mount exists — the kernel returns EBUSY for a mountpoint — so this
// runs before the container is created and keeps the inode either way.
//
// Called at Prepare (DRA) / CreateContainer (NRI) so that the mount source
// exists, and so a partition directory reused by a later incarnation does not
// hand it a previous one's PID list.
func ResetPidsFile(configDir string) error {
	path := filepath.Join(configDir, PidsConfig)
	// O_NOFOLLOW for the same reason write_to_disk uses it: in the DRA path the
	// enclosing directory is writable by the workload container, so the final
	// component is attacker-controllable until the read-only mount is in place.
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, pidsFileMode)
	if err != nil {
		return fmt.Errorf("failed to open pids file %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()

	// Same exclusive lock the registry server takes, so a registration that is
	// somehow already in flight cannot interleave with this truncation.
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("failed to lock pids file %s: %w", path, err)
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()

	// Normalise a mode inherited from an older release, which created this file
	// 0777 in a container-writable directory.
	if err = unix.Fchmod(fd, pidsFileMode); err != nil {
		return fmt.Errorf("failed to set pids file mode on %s: %w", path, err)
	}
	if err = unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("failed to truncate pids file %s: %w", path, err)
	}
	return nil
}
