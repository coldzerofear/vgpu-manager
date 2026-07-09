package util

import "syscall"

// FcntlRecordLock places or removes a single-byte fcntl (POSIX) record lock at
// the given absolute file offset. It is the shared low-level primitive behind
// the per-device read/write locks used by the vmem and sm-watcher shared-memory
// files, and interoperates with the equivalent fcntl locks taken by the C side
// (library/src/lock.c).
//
//   - lockType: syscall.F_RDLCK, F_WRLCK or F_UNLCK
//   - wait:     true uses F_SETLKW (block until acquired), false uses F_SETLK
//
// offset must be the absolute byte offset of the lock byte within the file,
// i.e. it already includes the per-device stride. Computing that offset with
// unsafe.Offsetof alone is a trap: unsafe.Offsetof(root.arr[i].field) is a
// compile-time constant that ignores i, so callers must add index*stride
// explicitly (see the getXxxLockOffset helpers and their tests).
func FcntlRecordLock(fd uintptr, lockType int16, wait bool, offset int64) error {
	cmd := syscall.F_SETLK
	if wait {
		cmd = syscall.F_SETLKW
	}
	lock := syscall.Flock_t{
		Type:   lockType,
		Whence: 0, // SEEK_SET
		Start:  offset,
		Len:    1,
		Pid:    0,
	}
	return syscall.FcntlFlock(fd, cmd, &lock)
}
