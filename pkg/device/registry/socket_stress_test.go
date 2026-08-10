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

package registry

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_StartStop_doesNotLeakGoroutines pins the lifecycle of what Start spawns:
// a serve loop per listener plus the socket guard. The guard in particular is a
// ticker goroutine that has to be retired by Stop, and the device-plugin main
// loop restarts plugins on every kubelet reconnect.
func Test_StartStop_doesNotLeakGoroutines(t *testing.T) {
	contPath := t.TempDir()

	// One warm-up cycle first: package-level lazy initialisation inside grpc and
	// klog spawns goroutines that never exit and would otherwise be counted.
	warmup := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, warmup.Start())
	warmup.Stop()

	settle(t)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		s := NewDeviceRegistryServer(contPath, nil, nil)
		require.NoError(t, s.Start())
		s.republishSocketIfMissing()
		s.Stop()
	}

	settle(t)
	assert.LessOrEqual(t, runtime.NumGoroutine(), baseline+2,
		"20 start/stop cycles must not accumulate goroutines")
}

// settle waits for goroutine bookkeeping to quiesce: Stop returns as soon as
// GracefulStop is done, while the serve loops it unblocked still have to be
// scheduled to exit.
func settle(t *testing.T) {
	t.Helper()
	last := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		if now := runtime.NumGoroutine(); now == last {
			return
		} else {
			last = now
		}
	}
}

// Test_concurrentTakeoverChurn hammers the paths that only ever interleave in
// production: overlapping starts, shutdowns racing a successor's takeover, and
// the guard waking up in the middle of both. It asserts the invariant the whole
// design exists for — whoever is left standing owns a socket that clients can
// reach — rather than any particular ordering.
func Test_concurrentTakeoverChurn(t *testing.T) {
	contPath := t.TempDir()
	socket := filepath.Join(contPath, util.Registry, SocketFile)

	// A long-lived server plays the incumbent; the churn happens around it.
	incumbent := NewDeviceRegistryServer(contPath, nil, nil)
	require.NoError(t, incumbent.Start())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewDeviceRegistryServer(contPath, nil, nil)
			if err := s.Start(); err != nil {
				// Only a genuinely broken directory should fail a start.
				assert.NoError(t, err)
				return
			}
			s.republishSocketIfMissing()
			s.Stop()
		}()
	}
	// Concurrent external deletion, which is what the guard is for.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = os.Remove(socket)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	// The incumbent never stopped, so it must still be able to publish itself.
	incumbent.republishSocketIfMissing()
	require.True(t, incumbent.IsRunning())

	info, err := os.Lstat(socket)
	require.NoError(t, err, "a running server must leave a socket behind")
	assert.NotZero(t, info.Mode()&os.ModeSocket)
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err, "the surviving socket must be served")
	_ = conn.Close()

	incumbent.Stop()

	// Nothing may be left over: no socket, and no staged sockets from any of the
	// churn above.
	entries, err := os.ReadDir(filepath.Join(contPath, util.Registry))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), stagedSocketSuffix, "a staged socket leaked")
	}
}
