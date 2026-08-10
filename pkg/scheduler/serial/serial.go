/*
Copyright 2025-2026 coldzerofear

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

package serial

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

type Locker struct {
	mutex              KeyRWMutex
	name               string
	timestamps         []int64
	minLockingDuration *time.Duration
	enabled            bool
}

type Option func(*Locker)

func WithName(name string) Option {
	return func(locker *Locker) {
		locker.name = name
	}
}

func WithEnabled(enabled bool) Option {
	return func(locker *Locker) {
		locker.enabled = enabled
	}
}

func WithLockDuration(d *time.Duration) Option {
	return func(locker *Locker) {
		locker.minLockingDuration = d
	}
}

func NewLocker(options ...Option) *Locker {
	locker := Locker{}
	for _, opt := range options {
		opt(&locker)
	}
	if locker.enabled {
		n := runtime.GOMAXPROCS(0) * 4
		if n < 4 {
			n = 4
		}
		locker.mutex = NewHashed(n)
		locker.timestamps = make([]int64, n)
	}
	return &locker
}

func getLockId(id ...string) string {
	var lockId string
	switch len(id) {
	case 0:
		lockId = "<default>"
	case 1:
		lockId = id[0]
	default:
		sort.Strings(id)
		lockId = strings.Join(id, ",")
	}
	return lockId
}

func (l *Locker) RLock(id ...string) {
	if l.enabled {
		lockId := getLockId(id...)
		l.mutex.RLockKey(lockId)
		l.timestamps[HashCode(lockId)%uint32(len(l.timestamps))] = time.Now().UnixMicro()
		klog.V(5).InfoS(fmt.Sprintf("%s read lock successfully added", l.name), "lockId", lockId)
	}
}

func (l *Locker) Lock(id ...string) {
	if l.enabled {
		lockId := getLockId(id...)
		l.mutex.LockKey(lockId)
		l.timestamps[HashCode(lockId)%uint32(len(l.timestamps))] = time.Now().UnixMicro()
		klog.V(5).InfoS(fmt.Sprintf("%s lock successfully added", l.name), "lockId", lockId)
	}
}

func (l *Locker) delay(id string) {
	since := time.Since(time.UnixMicro(l.timestamps[HashCode(id)%uint32(len(l.timestamps))]))
	klog.V(5).InfoS(fmt.Sprintf("%s took %d milliseconds", l.name, since.Milliseconds()), "lockId", id)
	if l.minLockingDuration != nil && *l.minLockingDuration-since > 0 {
		time.Sleep(*l.minLockingDuration - since)
	}
}

func (l *Locker) RUnlock(id ...string) {
	if l.enabled {
		lockId := getLockId(id...)
		l.delay(lockId)
		_ = l.mutex.RUnlockKey(lockId)
	}
}

func (l *Locker) Unlock(id ...string) {
	if l.enabled {
		lockId := getLockId(id...)
		l.delay(lockId)
		_ = l.mutex.UnlockKey(lockId)
	}
}
