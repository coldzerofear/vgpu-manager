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

package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// pidsFileMode is the mode pids.config is created with. Must match the mode
// passed to open() in write_to_disk (see server.go): whichever of the two
// creates the file first decides, and they should not disagree.
//
// Group/other get read only. The file carries host PIDs, and the container is
// meant to consume it, not author it.
const pidsFileMode = 0o644

const (
	// pidsLockBudget caps how long ResetPidsFile waits for the pids.config
	// lock. The only writer holds it for one ftruncate plus one pwrite of a few
	// hundred bytes, so this is orders of magnitude more than a legitimate
	// waiter ever needs; it exists to turn a wedged holder into an error instead
	// of a hang.
	pidsLockBudget = 50 * time.Millisecond
	// pidsLockRetryInterval is the poll interval within that budget.
	pidsLockRetryInterval = 2 * time.Millisecond
)

// flockWithin takes an exclusive flock, giving up after budget rather than
// waiting indefinitely.
func flockWithin(fd int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s: %w", budget, err)
		}
		time.Sleep(pidsLockRetryInterval)
	}
}

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
	//
	// Bounded rather than blocking, and that is the point: the callers are
	// NodePrepareResources and the NRI CreateContainer hook, both of which the
	// runtime waits on synchronously — a blocking flock here would stall
	// container creation for the whole node behind one wedged writer. Nothing
	// should be holding this lock at all, since the container that reads this
	// file does not exist yet, so failing after the budget is the honest answer:
	// the caller fails closed and the runtime retries.
	if err = flockWithin(fd, pidsLockBudget); err != nil {
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
