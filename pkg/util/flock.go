package util

import (
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// ofdUnsupported is set once F_OFD_* returns EINVAL (kernel < 3.15). After that
// we go straight to classic locks and skip the wasted OFD syscall. It only ever
// transitions false->true: a successful OFD call means the kernel supports OFD,
// so it can never flip afterwards, which keeps acquire and release on the same
// lock family (a classic F_UNLCK does not release an OFD lock and vice versa).
var ofdUnsupported atomic.Bool

// FcntlRecordLock places or removes a single-byte record lock at the given
// absolute file offset. It is the shared low-level primitive behind the
// per-device read/write locks on the vmem and sm-watcher shared-memory files.
//
// It prefers OFD (Open File Description) locks (F_OFD_SETLK / F_OFD_SETLKW,
// Linux 3.15+) and transparently falls back to classic process-associated locks
// (F_SETLK / F_SETLKW) on kernels that reject them with EINVAL.
//
// OFD locks are used on purpose. Classic POSIX locks are (a) released when the
// process closes ANY fd on the inode, and (b) never conflict between fds of the
// same process. So callers that open a fresh fd per lock, or hold several device
// locks concurrently in one process, would have one Unlock/close silently drop a
// sibling lock. OFD locks are owned by the open file description: an unrelated
// close never touches them, they conflict across fds within a process, and they
// still conflict cross-process with the classic locks taken by the C library
// (library/src/lock.c), so reader/writer exclusion is preserved — including
// during a mixed-version rollout.
//
//   - lockType: unix.F_RDLCK, F_WRLCK or F_UNLCK
//   - wait:     true blocks until acquired (F_SETLKW variants), false is non-blocking
//
// offset must be the absolute byte offset of the lock byte within the file, i.e.
// it already includes the per-device stride. unsafe.Offsetof(root.arr[i].field)
// is a compile-time constant that ignores i, so callers must add index*stride
// explicitly (see the getXxxLockOffset helpers and their tests).
func FcntlRecordLock(fd uintptr, lockType int16, wait bool, offset int64) error {
	lock := unix.Flock_t{
		Type:   lockType,
		Whence: 0, // SEEK_SET
		Start:  offset,
		Len:    1,
		Pid:    0, // must be 0 for OFD locks; ignored by classic locks
	}
	if !ofdUnsupported.Load() {
		cmd := unix.F_OFD_SETLK
		if wait {
			cmd = unix.F_OFD_SETLKW
		}
		err := unix.FcntlFlock(fd, cmd, &lock)
		if err != unix.EINVAL {
			return err
		}
		// Kernel does not support OFD locks; fall back for good.
		ofdUnsupported.Store(true)
	}
	cmd := unix.F_SETLK
	if wait {
		cmd = unix.F_SETLKW
	}
	return unix.FcntlFlock(fd, cmd, &lock)
}
