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
