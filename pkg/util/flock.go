package util

import "golang.org/x/sys/unix"

// FcntlRecordLock places or removes a single-byte OFD (Open File Description)
// record lock at the given absolute file offset. It is the shared low-level
// primitive behind the per-device read/write locks on the vmem and sm-watcher
// shared-memory files.
//
// OFD locks (F_OFD_SETLK / F_OFD_SETLKW, Linux 3.15+) are used instead of the
// classic process-associated locks (F_SETLK / F_SETLKW) on purpose:
//
//   - Classic POSIX locks are released when the process closes *any* fd on the
//     inode, so opening a fresh fd per lock (as the sm-watcher writer does per
//     device) means one goroutine's Unlock/close silently drops a sibling
//     goroutine's still-held lock on another device byte, and closing the mmap
//     fd would drop live locks too. OFD locks are owned by the open file
//     description, so an unrelated fd close never touches them.
//   - OFD locks also conflict between different fds of the same process, and
//     still conflict cross-process with the classic locks taken by the C
//     library (library/src/lock.c), so reader/writer exclusion is preserved.
//
//   - lockType: unix.F_RDLCK, F_WRLCK or F_UNLCK
//   - wait:     true blocks until acquired (F_OFD_SETLKW), false is non-blocking
//
// offset must be the absolute byte offset of the lock byte within the file,
// i.e. it already includes the per-device stride. Computing that offset with
// unsafe.Offsetof alone is a trap: unsafe.Offsetof(root.arr[i].field) is a
// compile-time constant that ignores i, so callers must add index*stride
// explicitly (see the getXxxLockOffset helpers and their tests).
func FcntlRecordLock(fd uintptr, lockType int16, wait bool, offset int64) error {
	cmd := unix.F_OFD_SETLK
	if wait {
		cmd = unix.F_OFD_SETLKW
	}
	lock := unix.Flock_t{
		Type:   lockType,
		Whence: 0, // SEEK_SET
		Start:  offset,
		Len:    1,
		Pid:    0, // must be 0 for OFD locks
	}
	return unix.FcntlFlock(fd, cmd, &lock)
}
