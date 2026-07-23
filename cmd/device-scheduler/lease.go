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
	"k8s.io/utils/ptr"
)

// LeaseDetector Real time detection of whether the specified Leaf is held by the target Pod and has not expired.
// Thread safe, IsLeader() can be called from any goroutine.
type LeaseDetector struct {
	mu             sync.RWMutex
	current        *coordinationv1.Lease
	namespace      string
	leaseName      string
	identityPrefix string // holderIdentity The prefix that needs to be matched (i.e. Pod name)
	jitter         time.Duration
}

type Option func(*LeaseDetector)

// WithJitter Set an additional grace period for determining expiration (default 0, i.e. strictly judged by renewTime+duration)
func WithJitter(d time.Duration) Option {
	return func(ld *LeaseDetector) { ld.jitter = d }
}

func NewLeaseDetector(factory informers.SharedInformerFactory, namespace, leaseName, identityPrefix string, opts ...Option) (*LeaseDetector, error) {
	ld := &LeaseDetector{
		namespace:      namespace,
		leaseName:      leaseName,
		identityPrefix: identityPrefix,
	}
	for _, opt := range opts {
		opt(ld)
	}

	// ---- Build an Informer that only listens to a single Leaf ----
	informer := factory.InformerFor(&coordinationv1.Lease{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.CoordinationV1().RESTClient(), "leases",
			namespace, fields.OneTermEqualSelector("metadata.name", leaseName))
		indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
		return cache.NewSharedIndexInformer(watcher, &coordinationv1.Lease{}, d, indexers)
	})

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ld.onUpdate(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			ld.onUpdate(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			ld.onDelete(obj)
		},
	})
	if err != nil {
		return nil, err
	}

	return ld, nil
}

// IsLeader Return true if all the following conditions are met:
//  1. Lease Existence
//  2. holderIdentity prefix == podName
//  3. The lease has not expired（renewTime + leaseDurationSeconds + jitter > now）
func (ld *LeaseDetector) IsLeader() bool {
	ld.mu.RLock()
	lease := ld.current
	ld.mu.RUnlock()
	evaluate, _ := ld.evaluate(lease)
	return evaluate
}

// IsLeaderDetailed Return the judgment result and reason (for logging/debugging purposes).
func (ld *LeaseDetector) IsLeaderDetailed() (bool, string) {
	ld.mu.RLock()
	lease := ld.current
	ld.mu.RUnlock()
	return ld.evaluate(lease)
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
	return lease
}

func (ld *LeaseDetector) onUpdate(obj interface{}) {
	lease := ld.convertTargetLease(obj)
	if lease == nil {
		return
	}

	ld.mu.RLock()
	current := ld.current
	ld.mu.RUnlock()

	// Print logs during initial initialization, resource reconstruction, and identity change
	if current == nil || current.UID != lease.UID ||
		!ptr.Equal(current.Spec.HolderIdentity, lease.Spec.HolderIdentity) {
		_, reason := ld.evaluate(lease)
		klog.V(3).Infoln(reason)
	}

	ld.mu.Lock()
	ld.current = lease.DeepCopy()
	ld.mu.Unlock()
}

func (ld *LeaseDetector) onDelete(obj interface{}) {
	lease := ld.convertTargetLease(obj)
	if lease == nil {
		return
	}

	ld.mu.Lock()
	ld.current = nil
	ld.mu.Unlock()
	klog.V(2).Infof("lease-detector: lease %s/%s deleted", ld.namespace, ld.leaseName)
}

func (ld *LeaseDetector) evaluate(lease *coordinationv1.Lease) (bool, string) {
	if lease == nil {
		return false, fmt.Sprintf("lease %s/%s does not exist", ld.namespace, ld.leaseName)
	}

	holder := ""
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
		return false, fmt.Sprintf("lease expired: renewTime=%s, deadline=%s", renewTime.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339))
	}
	return true, fmt.Sprintf("leader=%s, expires at %s", holder, deadline.UTC().Format(time.RFC3339))
}
