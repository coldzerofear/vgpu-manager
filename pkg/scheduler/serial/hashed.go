package serial

import (
	"hash/fnv"
	"runtime"
	"sync"
)

// NewHashed returns a new instance of KeyRWMutex which hashes arbitrary keys to
// a fixed set of locks. `n` specifies number of locks, if n <= 0, we use
// number of cpus.
// Note that because it uses fixed set of locks, different keys may share same
// lock, so it's possible to wait on same lock.
func NewHashed(n int) KeyRWMutex {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	return &hashedKeyRWMutex{
		mutexes: make([]sync.RWMutex, n),
	}
}

type hashedKeyRWMutex struct {
	mutexes []sync.RWMutex
}

// RLockKey Acquires a read lock associated with the specified ID.
func (km *hashedKeyRWMutex) RLockKey(id string) {
	km.mutexes[HashCode(id)%uint32(len(km.mutexes))].RLock()
}

// RUnlockKey Releases the read lock associated with the specified ID.
func (km *hashedKeyRWMutex) RUnlockKey(id string) error {
	km.mutexes[HashCode(id)%uint32(len(km.mutexes))].RUnlock()
	return nil
}

// LockKey Acquires a lock associated with the specified ID.
func (km *hashedKeyRWMutex) LockKey(id string) {
	km.mutexes[HashCode(id)%uint32(len(km.mutexes))].Lock()
}

// UnlockKey Releases the lock associated with the specified ID.
func (km *hashedKeyRWMutex) UnlockKey(id string) error {
	km.mutexes[HashCode(id)%uint32(len(km.mutexes))].Unlock()
	return nil
}

func HashCode(id string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(id))
	return h.Sum32()
}
