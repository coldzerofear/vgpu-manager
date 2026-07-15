/*
 * Copyright 2024 NVIDIA CORPORATION.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nri

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// Entry is the per-container partition target the NRI plugin resolves at
// CreateContainer time. It is what the register server's pod-uid resolver reads
// (design §12.8): ClaimUID identifies the owning claim; ConfigDir is the
// absolute directory (in the plugin/library filesystem view) that holds
// pids.config for this container.
type Entry struct {
	ClaimUID  string
	ConfigDir string
}

// Cache holds (podUID, containerName) -> Entry. It is the authoritative source
// for the register server's pod-uid path in NRI mode, rebuilt from container env
// on every Synchronize (design §12.9, §12.13.4). The synced flag gates the
// register fallback until the first Synchronize completes (§12.13.5): before
// that, a miss is "not yet known" (retryable), not "not an NRI container".
type Cache struct {
	mu     sync.RWMutex
	byKey  map[string]Entry
	synced bool
}

// NewCache returns an empty, not-yet-synced cache.
func NewCache() *Cache {
	return &Cache{byKey: make(map[string]Entry)}
}

// Key is the cache key for a container. Exported so callers (e.g. the register
// resolver) build it the same way.
func Key(podUID, containerName string) string {
	return podUID + "/" + containerName
}

// ConfigDirFor computes the per-container partition config directory in the
// plugin/library filesystem view, matching the register server's pod-uid path
// and the NRI CreateContainer mount target (design §12.3):
//
//	<ManagerRootPath>/claims/<claimUID>/<podUID>_<containerName>/config
func ConfigDirFor(claimUID, podUID, containerName string) string {
	containerDir := fmt.Sprintf("%s_%s", podUID, containerName)
	return filepath.Join(util.ManagerRootPath, util.Claims, claimUID, containerDir, util.Config)
}

// Set records or overwrites a single container's entry.
func (c *Cache) Set(podUID, containerName string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[Key(podUID, containerName)] = e
}

// Get returns the entry for a container and whether it was present.
func (c *Cache) Get(podUID, containerName string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byKey[Key(podUID, containerName)]
	return e, ok
}

// Delete drops a container's entry (RemoveContainer).
func (c *Cache) Delete(podUID, containerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byKey, Key(podUID, containerName))
}

// Replace atomically swaps the whole map and marks the cache synced. Called by
// Synchronize once it has rebuilt every entry from the replayed container set.
func (c *Cache) Replace(entries map[string]Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey = entries
	c.synced = true
}

// Synced reports whether the first Synchronize has completed.
func (c *Cache) Synced() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.synced
}

// Len returns the number of cached entries (for logging/metrics).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byKey)
}
