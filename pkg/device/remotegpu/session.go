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

package remotegpu

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// On the device-plugin path every container of a remote pod gets its own
// session on the GPU server: its devices and limits are per container, and
// containers that run at the same time must not share one quota.

// sessionTokenLength keeps a derived token the same shape as the random ones
// the DRA path mints (16 bytes, hex).
const sessionTokenLength = 32

// SessionKey identifies the session of one container of a pod.
func SessionKey(podUID, containerName string) string {
	return util.NRIPartitionKey(podUID, containerName)
}

// SessionContainer returns the container name in a session key of podUID.
func SessionContainer(podUID, sessionKey string) (string, bool) {
	name, ok := strings.CutPrefix(sessionKey, podUID+"_")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// SessionToken is the session token of one container: the hash of its session
// key, so the device plugin, the agent and the monitor all derive the same
// value and none of them has to publish it. It is not a secret — the agent
// authorizes a request by checking the pod, not by knowing the token.
func SessionToken(podUID, containerName string) string {
	sum := sha256.Sum256([]byte(SessionKey(podUID, containerName)))
	return hex.EncodeToString(sum[:])[:sessionTokenLength]
}
