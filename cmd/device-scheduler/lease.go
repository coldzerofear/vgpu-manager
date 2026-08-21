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

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// LeaseDetector Real time detection of whether the specified Leaf is held by the target Pod and has not expired.
// Thread safe, IsLeader() can be called from any goroutine.
type LeaseDetector struct {
	mu              sync.RWMutex
	current         *coordinationv1.Lease
	namespace       string
	leaseName       string
	identityPrefix  string // holderIdentity The prefix that needs to be matched (i.e. Pod name)
	leaderCallback  func()
	releaseCallback func()
	startOnce       sync.Once
	startCallback   func()
	jitter          time.Duration
	isLeader        bool
}

type Option func(*LeaseDetector)

// WithJitter Set an additional grace period for determining expiration (default 0, i.e. strictly judged by renewTime+duration)
func WithJitter(d time.Duration) Option {
	return func(ld *LeaseDetector) { ld.jitter = d }
}

func WithLeaderCallback(c func()) Option {
	return func(ld *LeaseDetector) { ld.leaderCallback = c }
}

func WithReleaseCallback(c func()) Option {
	return func(ld *LeaseDetector) { ld.releaseCallback = c }
}

func WithStartCallback(c func()) Option {
	return func(ld *LeaseDetector) { ld.startCallback = c }
}

func (ld *LeaseDetector) LeaderCallback() {
	if ld.leaderCallback != nil {
		ld.leaderCallback()
	}
}

func (ld *LeaseDetector) ReleaseCallback() {
	if ld.releaseCallback != nil {
		ld.releaseCallback()
	}
}

func (ld *LeaseDetector) StartCallbackOnce() {
	if ld.startCallback != nil {
		ld.startOnce.Do(ld.startCallback)
	}
}

func NewLeaseDetector(
	factory informers.SharedInformerFactory, namespace,
	leaseName, identityPrefix string, opts ...Option,
) (*LeaseDetector, error) {
	ld := &LeaseDetector{
		namespace:      namespace,
		leaseName:      leaseName,
		identityPrefix: identityPrefix,
	}
	for _, opt := range opts {
		opt(ld)
	}

	// ---- Build an Informer that only listens to a single Leaf ----
	informer := factory.InformerFor(&coordinationv1.Lease{},
		func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
			watcher := cache.NewListWatchFromClient(k.CoordinationV1().RESTClient(),
				"leases", namespace, fields.OneTermEqualSelector("metadata.name", leaseName))
			return cache.NewSharedIndexInformer(watcher, &coordinationv1.Lease{}, d,
				cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		},
	)

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { ld.onUpdate(obj) },
		UpdateFunc: func(_, newObj interface{}) { ld.onUpdate(newObj) },
		DeleteFunc: func(obj interface{}) { ld.onDelete(obj) },
	})
	if err != nil {
		return nil, err
	}

	go func() {
		checker := informer.HasSyncedChecker()
		<-checker.Done()
		ld.StartCallbackOnce()
	}()

	return ld, nil
}

// IsLeader Return true if all the following conditions are met:
//  1. Lease Existence
//  2. holderIdentity prefix == podName
//  3. The lease has not expired（renewTime + leaseDurationSeconds + jitter > now）
func (ld *LeaseDetector) IsLeader() bool {
	evaluate, _ := ld.IsLeaderDetailed()
	return evaluate
}

// IsLeaderDetailed Return the judgment result and reason (for logging/debugging purposes).
func (ld *LeaseDetector) IsLeaderDetailed() (bool, string) {
	return ld.evaluate(ld.GetLeaseSnapshot())
}

func (ld *LeaseDetector) GetLeaseSnapshot() *coordinationv1.Lease {
	var lease *coordinationv1.Lease
	ld.mu.RLock()
	if ld.current != nil {
		lease = ld.current.DeepCopy()
	}
	ld.mu.RUnlock()
	return lease
}

func (ld *LeaseDetector) convertTargetLease(obj interface{}) *coordinationv1.Lease {
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return nil
		}
		lease, ok = tombstone.Obj.(*coordinationv1.Lease)
		if !ok {
			return nil
		}
	}
	if lease.Namespace != ld.namespace || lease.Name != ld.leaseName {
		return nil
	}
	return lease.DeepCopy()
}

// onUpdate handles both Add and Update events.
// It evaluates the new lease state, detects identity transitions, and fires
// callbacks only on state changes (not on every update).
func (ld *LeaseDetector) onUpdate(obj interface{}) {
	ld.StartCallbackOnce()

	current := ld.convertTargetLease(obj)
	if current == nil {
		return
	}

	// Evaluate the new lease to determine if we are the leader
	newIsLeader, reason := ld.evaluate(current)

	// Capture previous leader state before updating
	ld.mu.Lock()
	wasLeader := ld.isLeader
	ld.current = current
	ld.isLeader = newIsLeader
	ld.mu.Unlock()

	// Fire callbacks only on state transitions
	if wasLeader && !newIsLeader {
		// Transition: leader -> not leader (released)
		klog.V(2).Infof("lease-detector: lost leadership, reason: %s", reason)
		ld.ReleaseCallback()
	} else if !wasLeader && newIsLeader {
		// Transition: not leader -> leader (acquired)
		klog.V(3).Infof("lease-detector: acquired leadership, reason: %s", reason)
		ld.LeaderCallback()
	} else {
		// No state change — only log at verbose level for debugging
		klog.V(5).Infof("lease-detector: no leadership change, %s", reason)
	}
}

// onDelete handles lease deletion events.
// When the lease is deleted, we immediately transition out of leader state
// and fire the release callback if we were the leader.
func (ld *LeaseDetector) onDelete(obj interface{}) {
	lease := ld.convertTargetLease(obj)
	if lease == nil {
		return
	}

	// Capture previous leader state before clearing
	ld.mu.Lock()
	wasLeader := ld.isLeader
	ld.current = nil
	ld.isLeader = false
	ld.mu.Unlock()

	if wasLeader {
		// Transition: leader -> not leader (lease deleted)
		klog.V(2).Infof("lease-detector: lease %s/%s deleted, lost leadership", ld.namespace, ld.leaseName)
		ld.ReleaseCallback()
	} else {
		klog.V(2).Infof("lease-detector: lease %s/%s deleted", ld.namespace, ld.leaseName)
	}
}

func (ld *LeaseDetector) evaluate(lease *coordinationv1.Lease) (bool, string) {
	if lease == nil {
		return false, fmt.Sprintf("lease %s/%s does not exist", ld.namespace, ld.leaseName)
	}

	var holder string
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if !strings.HasPrefix(holder, ld.identityPrefix) {
		return false, fmt.Sprintf("holder mismatch: got %q, want prefix %q", holder, ld.identityPrefix)
	}

	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false, fmt.Sprintf("renewTime or leaseDurationSeconds is nil")
	}

	renewTime := lease.Spec.RenewTime.Time
	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	deadline := renewTime.Add(duration).Add(ld.jitter)
	if time.Now().After(deadline) {
		return false, fmt.Sprintf("lease expired: renewTime=%s, deadline=%s",
			renewTime.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339))
	}
	return true, fmt.Sprintf("leader=%s, expires at %s", holder, deadline.UTC().Format(time.RFC3339))
}
