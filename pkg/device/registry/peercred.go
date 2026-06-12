package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// peerCredAuthInfo carries the SO_PEERCRED-derived caller PID for a connection.
type peerCredAuthInfo struct {
	credentials.CommonAuthInfo
	pid int32
}

func (peerCredAuthInfo) AuthType() string { return "peercred" }

// peerCredentials is a server-side gRPC TransportCredentials for a Unix-domain
// socket that captures the connecting process's PID via SO_PEERCRED during the
// handshake. The kernel-supplied PID is authoritative and cannot be forged by
// the caller, so it lets the registry verify that a register request really
// originates from the container it claims (the request body's pod_uid /
// container_name / register_uuid are self-declared and otherwise untrusted).
type peerCredentials struct{}

var _ credentials.TransportCredentials = peerCredentials{}

func (peerCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	info := peerCredAuthInfo{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}
	if uc, ok := conn.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				if cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); e == nil && cred != nil {
					info.pid = cred.Pid
				}
			})
		}
	}
	return conn, info, nil
}

func (peerCredentials) ClientHandshake(_ context.Context, _ string, _ net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("peerCredentials: client handshake not supported")
}

func (peerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (peerCredentials) Clone() credentials.TransportCredentials { return peerCredentials{} }

func (peerCredentials) OverrideServerName(string) error { return nil }

// peerPidFromContext returns the SO_PEERCRED PID captured for the request's
// connection, or 0 if unavailable (non-unix transport, getsockopt failure).
func peerPidFromContext(ctx context.Context) int32 {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return 0
	}
	info, ok := p.AuthInfo.(peerCredAuthInfo)
	if !ok {
		return 0
	}
	return info.pid
}

// podUIDFromCgroupRe extracts a UUID-shaped pod UID from a kubepods cgroup path,
// tolerating both the cgroupfs ("pod<uid>") and systemd ("pod<uid>.slice", with
// dashes replaced by underscores) layouts.
var podUIDFromCgroupRe = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

// callerPodUIDFromPid reads /proc/<pid>/cgroup and extracts the Kubernetes pod
// UID the process belongs to. Returns ("", false) when the PID is unknown, the
// cgroup is unreadable, or the path carries no pod UID (host process, or a pod
// whose UID is not UUID-shaped). The device-plugin runs in the host PID + mount
// namespaces, so the SO_PEERCRED PID resolves against the host /proc.
func callerPodUIDFromPid(pid int32) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", false
	}
	return parsePodUIDFromCgroup(data)
}

// parsePodUIDFromCgroup extracts and normalises a pod UID from the contents of
// a /proc/<pid>/cgroup file. Split out for testability.
func parsePodUIDFromCgroup(data []byte) (string, bool) {
	m := podUIDFromCgroupRe.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(string(m[1]), "_", "-")), true
}
