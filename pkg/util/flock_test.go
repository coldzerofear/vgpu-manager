package util

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestFcntlRecordLockOFD verifies the two OFD properties the shared-memory
// locks rely on, both of which classic POSIX record locks lack:
//  1. A lock survives the close of an *unrelated* fd on the same inode
//     (classic locks would all be dropped by that close).
//  2. Locks from different fds of the same process still conflict
//     (classic same-process locks never conflict, giving no exclusion).
func TestFcntlRecordLockOFD(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "ofd")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if err = f.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	open := func() *os.File {
		fd, err := os.OpenFile(path, os.O_RDWR, 0644)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}

	fd1 := open()
	fd2 := open()
	defer fd2.Close()

	// Two write locks on two different bytes, via two separate fds.
	if err = FcntlRecordLock(fd1.Fd(), unix.F_WRLCK, false, 0); err != nil {
		t.Fatalf("lock @0 via fd1: %v", err)
	}
	if err = FcntlRecordLock(fd2.Fd(), unix.F_WRLCK, false, 100); err != nil {
		t.Fatalf("lock @100 via fd2: %v", err)
	}

	// Property 1: closing fd1 must NOT drop fd2's lock at offset 100.
	_ = fd1.Close()

	// Property 2: a third fd write-locking offset 100 must still conflict.
	fd3 := open()
	defer fd3.Close()
	if err = FcntlRecordLock(fd3.Fd(), unix.F_WRLCK, false, 100); err == nil {
		t.Fatal("offset 100 acquired after fd1 close: fd2's OFD lock was not preserved")
	}

	// Sanity: a free offset is still lockable, and releasing fd2's lock frees @100.
	if err = FcntlRecordLock(fd3.Fd(), unix.F_WRLCK, false, 200); err != nil {
		t.Fatalf("free offset 200 should lock: %v", err)
	}
	if err = FcntlRecordLock(fd2.Fd(), unix.F_UNLCK, false, 100); err != nil {
		t.Fatalf("unlock @100 via fd2: %v", err)
	}
	if err = FcntlRecordLock(fd3.Fd(), unix.F_WRLCK, false, 100); err != nil {
		t.Fatalf("offset 100 should be free after fd2 unlock: %v", err)
	}
}
