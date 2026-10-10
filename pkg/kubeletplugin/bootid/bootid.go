/*
Copyright 2024-2026 coldzerofear

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

// Package bootid reads the Linux kernel boot_id used to detect node reboots
// across kubelet plugin restarts.
package bootid

import (
	"fmt"
	"os"
	"strings"
)

const defaultBootIDPath = "/proc/sys/kernel/random/boot_id"

// bootIDPath is mutable for tests.
var bootIDPath = defaultBootIDPath

// GetCurrentBootID returns the trimmed contents of /proc/sys/kernel/random/boot_id.
func GetCurrentBootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("failed to read boot ID from %s: %w", bootIDPath, err)
	}
	return strings.TrimSpace(string(b)), nil
}
