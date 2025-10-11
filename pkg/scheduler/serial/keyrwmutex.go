package serial

import "k8s.io/utils/keymutex"

// KeyRWMutex is a thread-safe interface for acquiring locks on arbitrary strings.
type KeyRWMutex interface {
	keymutex.KeyMutex
	// RLockKey Acquires a read lock associated with the specified ID, creates the lock if one doesn't already exist.
	RLockKey(id string)
	// RUnlockKey Releases the read lock associated with the specified ID.
	// Returns an error if the specified ID doesn't exist.
	RUnlockKey(id string) error
}
