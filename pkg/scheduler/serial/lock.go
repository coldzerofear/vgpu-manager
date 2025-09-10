package serial

import (
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type Lock struct {
	sync.Mutex
	name               string
	timestamp          int64
	minLockingDuration *time.Duration
	enabled            bool
}

func NewLock(name string, lockingDuration *time.Duration, enabled bool) *Lock {
	return &Lock{
		name:               name,
		enabled:            enabled,
		minLockingDuration: lockingDuration,
	}
}

func (l *Lock) Lock() {
	if l.enabled {
		l.Mutex.Lock()
		l.timestamp = time.Now().UnixMicro()
		klog.V(5).Infof("%s lock successfully added", l.name)
	}
}

func (l *Lock) Unlock() {
	if l.enabled {
		since := time.Since(time.UnixMicro(l.timestamp))
		klog.V(5).Infof("%s took %d milliseconds", l.name, since.Milliseconds())
		if l.minLockingDuration != nil && *l.minLockingDuration-since > 0 {
			time.Sleep(*l.minLockingDuration - since)
		}
		l.Mutex.Unlock()
	}
}
