package util

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"syscall"
	"unsafe"
)

type MmapFile struct {
	Path     string
	Data     []byte
	File     *os.File
	FileInfo fs.FileInfo
	Inode    uint64
	DevID    uint64
	option   MmapOption
}

func getPathInodeKey(path string) (dev uint64, inode uint64, err error) {
	var stat syscall.Stat_t
	if err = syscall.Stat(path, &stat); err != nil {
		return 0, 0, err
	}
	return stat.Dev, stat.Ino, nil
}

func getFdInodeKey(fd uintptr) (dev uint64, inode uint64, err error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(fd), &stat); err != nil {
		return 0, 0, err
	}
	return stat.Dev, stat.Ino, nil
}

// MmapOption Used for customizing mmap behavior
type MmapOption struct {
	Prot  int // syscall.PROT_READ, PROT_WRITE, PROT_EXEC
	Flags int // syscall.MAP_SHARED, MAP_PRIVATE, MAP_ANONYMOUS
}

var DefaultReadOnlyMmap = MmapOption{
	Prot:  syscall.PROT_READ,
	Flags: syscall.MAP_SHARED,
}

var DefaultReadWriteMmap = MmapOption{
	Prot:  syscall.PROT_READ | syscall.PROT_WRITE,
	Flags: syscall.MAP_SHARED,
}

func OpenMmap(path string, opt MmapOption) (*MmapFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		// Return the *fs.PathError unwrapped: callers branch on os.IsNotExist
		// (e.g. a container config/vmem file that has not been written yet),
		// and os.IsNotExist does not see through a fmt.Errorf %w wrap. The
		// PathError already carries the path in its message.
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("FStat %q failed: %w", path, err)
	}
	size := int(fi.Size())
	if size == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("FileSize of %q is 0", path)
	}
	if size > math.MaxInt {
		_ = f.Close()
		return nil, fmt.Errorf("file %q too large for mmap: %d bytes", path, size)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, size, opt.Prot, opt.Flags)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("MmapFile %q failed: %w", path, err)
	}
	dev, inode, _ := getFdInodeKey(f.Fd())
	return &MmapFile{
		Path:     path,
		Data:     data,
		File:     f,
		FileInfo: fi,
		Inode:    inode,
		DevID:    dev,
		option:   opt,
	}, nil
}

func (mf *MmapFile) NeedsReload() (reload bool, err error) {
	dev, inode, err := getPathInodeKey(mf.Path)
	if err != nil {
		return false, err
	}
	return dev != mf.DevID || inode != mf.Inode, nil
}

//func (mf *MmapFile) Reload() error {
//	newMf, err := OpenMmap(mf.Path, mf.option)
//	if err != nil {
//		return err
//	}
//	oldData := mf.Data
//	oldFile := mf.File
//	*mf = *newMf
//
//	if oldData != nil {
//		_ = syscall.Munmap(oldData)
//	}
//	if oldFile != nil {
//		_ = oldFile.Close()
//	}
//	return nil
//}

// Sync flushes dirty pages of a writable mapping back to the backing file.
// It is a no-op for an already-closed mapping.
func (mf *MmapFile) Sync() error {
	if len(mf.Data) == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_MSYNC,
		uintptr(unsafe.Pointer(&mf.Data[0])),
		uintptr(len(mf.Data)),
		uintptr(syscall.MS_SYNC),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func (mf *MmapFile) Close() error {
	if mf.Data != nil {
		_ = syscall.Munmap(mf.Data)
		mf.Data = nil
	}
	if mf.File != nil {
		mf.File.Close()
		mf.File = nil
	}
	mf.FileInfo = nil
	return nil
}
