package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/coldzerofear/vgpu-manager/pkg/api/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/cgroups"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

//#include <stdio.h>
//#include <stdlib.h>
//#include <string.h>
//#include <unistd.h>
//#include <fcntl.h>
//#include <sys/file.h>
//#include <time.h>
//
//int write_to_disk(const char* filename, const char* data) {
//  int fd = 0;
//  int wsize = 0;
//  struct timespec wait = {
//	  .tv_sec = 0, .tv_nsec = 100 * 1000 * 1000,
//  };
//  int ret = 0;
//  size_t data_len = 0;
//  data_len = strlen(data);
//  fd = open(filename, O_CREAT | O_TRUNC | O_WRONLY, 00777);
//  if (fd == -1) {
//    return 1;
//  }
//  while (flock(fd, LOCK_EX)) {
//    nanosleep(&wait, NULL);
//  }
//  wsize = (int)write(fd, (void*)data, data_len);
//  if (wsize != (int)data_len) {
//	  ret = 2;
//    goto DONE;
//  }
//DONE:
//  flock(fd, LOCK_UN);
//  close(fd);
//  return ret;
//}
import "C"

const (
	SocketFile = "socket.sock"
	PidsConfig = "pids.config"

	// resolveBackoff is the per-attempt poll interval used while waiting for
	// the lookup state (informer cache, container status) to become
	// consistent enough to satisfy a register request.
	resolveBackoff = 40 * time.Millisecond

	// resolveTimeout caps how long a single register request may keep polling,
	// independent of the client's context. A legitimate caller resolves within
	// a few hundred ms once its container is Running; this bound stops a
	// malicious caller that never sets an RPC deadline (or never disconnects)
	// from pinning a goroutine + cgroup-read loop forever.
	resolveTimeout = 60 * time.Second

	// maxInFlightResolves caps the total number of register requests resolving
	// concurrently across all callers; excess requests are rejected fast with
	// ResourceExhausted instead of queueing (which would hold connections open).
	maxInFlightResolves = 256
	// maxPerCallerResolves caps the concurrent register requests from a single
	// caller (keyed by its real pod UID, or PID when that can't be derived) so
	// one hijacked container cannot monopolise the global budget.
	maxPerCallerResolves = 8

	// gRPC server hardening: the request body is tiny (a few identifiers), and
	// each container legitimately opens a single short-lived stream.
	maxRecvMsgBytes      = 16 * 1024
	maxConcurrentStreams = 64
	connectionTimeout    = 10 * time.Second
	keepaliveMinTime     = 30 * time.Second
)

// TargetCandidate is one (pod, container) pair the server may try to attribute
// a register request to. The DRA path can return multiple candidates per
// register UUID — the server iterates them and accepts the first one whose
// cgroup yields live PIDs.
type TargetCandidate struct {
	Pod           *corev1.Pod
	ContainerName string
}

// Target carries the resolution result for a register request. Candidates are
// tried in order. ConfigDir is the same for every candidate of the same
// request because Prepare creates one directory per partition annotation.
type Target struct {
	Candidates []TargetCandidate

	// ConfigDir is the absolute path of the directory that will hold
	// pids.config. The directory is created on demand if it doesn't exist.
	ConfigDir string
}

// GetPodByUIDFunc resolves a pod by its UID. Used by the legacy device-plugin
// path where the calling library already knows its own (pod, container) pair.
type GetPodByUIDFunc func(ctx context.Context, uid string) (*corev1.Pod, error)

// GetTargetByUUIDFunc resolves a register UUID minted by the DRA driver into
// the set of (pod, container) candidates that could be the caller plus the
// shared on-disk directory for pids.config. Returning multiple candidates is
// the contract that lets the server tolerate stale informer views and
// fallback partition keys baked into claim annotations at Prepare time.
type GetTargetByUUIDFunc func(ctx context.Context, uuid string) (*Target, error)

func NewDeviceRegistryServer(containerPath string, getPodByUIDFn GetPodByUIDFunc, getTargetByUUIDFn GetTargetByUUIDFunc) *DeviceRegistryServerImpl {
	return &DeviceRegistryServerImpl{
		contPath:          containerPath,
		getPodByUIDFn:     getPodByUIDFn,
		getTargetByUUIDFn: getTargetByUUIDFn,
		inFlight:          make(chan struct{}, maxInFlightResolves),
		perCaller:         make(map[string]int),
	}
}

type DeviceRegistryServerImpl struct {
	registry.UnimplementedVDeviceRegistryServer
	mutex             sync.Mutex
	contPath          string
	getPodByUIDFn     GetPodByUIDFunc
	getTargetByUUIDFn GetTargetByUUIDFunc
	server            *grpc.Server
	listener          net.Listener
	running           bool

	// inFlight is a global concurrency budget for resolveTarget; perCaller
	// tracks the in-flight count keyed by caller identity (guarded by
	// perCallerMu). Both reject over-budget requests fast rather than queueing.
	inFlight    chan struct{}
	perCallerMu sync.Mutex
	perCaller   map[string]int
}

// acquireSlot reserves a global + per-caller concurrency slot for a register
// request, returning a release func. ok is false (with nothing reserved) when
// either budget is exhausted, so the caller can reject fast.
func (s *DeviceRegistryServerImpl) acquireSlot(callerKey string) (release func(), ok bool) {
	select {
	case s.inFlight <- struct{}{}:
	default:
		return nil, false
	}
	s.perCallerMu.Lock()
	if s.perCaller[callerKey] >= maxPerCallerResolves {
		s.perCallerMu.Unlock()
		<-s.inFlight
		return nil, false
	}
	s.perCaller[callerKey]++
	s.perCallerMu.Unlock()
	return func() {
		s.perCallerMu.Lock()
		if s.perCaller[callerKey] <= 1 {
			delete(s.perCaller, callerKey)
		} else {
			s.perCaller[callerKey]--
		}
		s.perCallerMu.Unlock()
		<-s.inFlight
	}, true
}

func (s *DeviceRegistryServerImpl) IsRunning() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.running
}

// RegisterContainerDevice is the gRPC entry point invoked by the in-container
// vGPU library. It supports two request shapes:
//
//   - register_uuid set: DRA path. The server hands the UUID to the configured
//     uuid resolver, which returns every (pod, container) the UUID could
//     legitimately refer to plus the partition directory. The server then
//     tries each candidate and writes pids.config under the directory for the
//     first one whose cgroup is alive.
//   - pod_uid + container_name set: legacy device-plugin path. The library
//     tells us its identity directly; we look up the pod and write to the
//     classic per-(pod, container) manager directory.
//
// In either case the call retries transient failures (informer not yet synced,
// container not yet Running, cgroup still ramping up) until the request
// context is canceled.
func (s *DeviceRegistryServerImpl) RegisterContainerDevice(ctx context.Context, req *registry.ContainerDeviceRequest) (resp *registry.ContainerDeviceResponse, err error) {
	klog.V(3).InfoS("RegisterContainerDevice", "podUid", req.GetPodUid(),
		"containerName", req.GetContainerName(), "registerUuid", req.GetRegisterUuid())

	var (
		release      func()
		peerPid      int32
		callerPodUID string
		callerKey    string
		pids         []int
		configDir    string
	)

	defer func() {
		runtime.RecoverFromPanic(&err)
		if release != nil {
			release()
		}
		if err != nil {
			if strings.HasPrefix(err.Error(), "recovered from panic") {
				klog.ErrorS(err, "RegisterContainerDevice panicked", "podUid", req.GetPodUid(),
					"containerName", req.GetContainerName(), "registerUuid", req.GetRegisterUuid(),
					"peerPid", peerPid, "callerPodUID", callerPodUID, "callerKey", callerKey)
				err = fmt.Errorf("an internal error has occurred")
			} else {
				klog.V(3).ErrorS(err, "RegisterContainerDevice failed", "podUid", req.GetPodUid(),
					"containerName", req.GetContainerName(), "registerUuid", req.GetRegisterUuid(),
					"peerPid", peerPid, "callerPodUID", callerPodUID, "callerKey", callerKey)
			}
		} else {
			klog.V(4).InfoS("RegisterContainerDevice success", "podUid", req.GetPodUid(),
				"containerName", req.GetContainerName(), "registerUuid", req.GetRegisterUuid(), "peerPid", peerPid,
				"callerPodUID", callerPodUID, "callerKey", callerKey, "configDir", configDir, "pids", pids)
		}
	}()

	resp = &registry.ContainerDeviceResponse{}

	// Authenticate the caller by its kernel-supplied SO_PEERCRED PID and the
	// pod UID derived from its cgroup. Both may be empty in environments where
	// they cannot be obtained, in which case the checks downstream fail open.
	peerPid = peerPidFromContext(ctx)
	callerPodUID, _ = callerPodUIDFromPid(peerPid)

	// Concurrency budget keyed by caller identity (real pod UID, else PID).
	callerKey = callerPodUID
	if callerKey == "" {
		callerKey = fmt.Sprintf("pid:%d", peerPid)
	}

	var ok bool
	release, ok = s.acquireSlot(callerKey)
	if !ok {
		klog.V(4).InfoS("register request rejected: concurrency budget exhausted",
			"callerKey", callerKey, "peerPid", peerPid)
		return resp, status.Error(codes.ResourceExhausted, "register concurrency budget exhausted, retry later")
	}

	configDir, pids, err = s.resolveTarget(ctx, req, peerPid, callerPodUID)
	if err != nil {
		return resp, err
	}
	if err = s.persistPids(configDir, pids); err != nil {
		return resp, err
	}
	return resp, nil
}

// resolveTarget loops until either (a) some candidate resolves to a running
// container whose cgroup yields a non-empty PID set, (b) a hard error
// (terminated pod in legacy mode, vGPU policy violation, uuid not minted by
// us) surfaces, or (c) the context is canceled.
//
// The cgroup PID readback is the authoritative cross-check: the kernel cannot
// lie about whether a container has live processes, so an "informer says
// Running but cgroup is empty" state is treated as a transient stale view and
// we keep polling until a fresher snapshot lands.
//
// On the DRA path the resolver may return multiple candidates per UUID (to
// handle stale partition keys baked into annotations at Prepare time, and
// init+app pairs sharing a partition). The candidate list is tried in order;
// the first viable one wins. If none are viable we wait one backoff tick and
// re-query the resolver — by then either the informer has caught up or the
// runtime has progressed enough for a different candidate to be Running.
func (s *DeviceRegistryServerImpl) resolveTarget(ctx context.Context, req *registry.ContainerDeviceRequest, peerPid int32, callerPodUID string) (string, []int, error) {
	// Server-side deadline: never poll longer than resolveTimeout regardless of
	// the client's context, so a caller that never sets a deadline (or never
	// disconnects) cannot pin this goroutine forever.
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	useUUID := req.GetRegisterUuid() != ""
	cgroupResolver := cgroupFullPathResolver()

	var (
		configDir string
		winPids   []int
		lastErr   error
	)
	pollErr := wait.PollUntilContextCancel(ctx, resolveBackoff, true, func(ctx context.Context) (bool, error) {
		target, err := s.lookupTarget(ctx, req)
		if err != nil {
			// "Not minted by us / never patched" is unrecoverable — bail out
			// instead of looping forever (also forecloses a cheap DoS vector
			// where junk UUIDs would otherwise pin a goroutine indefinitely).
			if apierrors.IsNotFound(err) {
				return false, err
			}
			lastErr = err
			klog.V(5).InfoS("register target lookup failed; retrying", "err", err)
			return false, nil
		}
		if len(target.Candidates) == 0 {
			lastErr = errors.New("resolver returned no candidates")
			return false, nil
		}

		// Caller authentication (fast-fail): when the caller's real pod UID is
		// known (from its SO_PEERCRED PID's cgroup), require it to own at least
		// one candidate. A request whose resolved candidates all belong to
		// OTHER pods is cross-pod impersonation — reject immediately instead of
		// polling, foreclosing a junk-request DoS vector.
		if callerPodUID != "" && !candidatesIncludePod(target.Candidates, callerPodUID) {
			return false, fmt.Errorf("caller pod %q is not authorized for this register request", callerPodUID)
		}

		// Legacy device-plugin path has exactly one candidate; validation
		// failures are hard errors (no other candidate could rescue them).
		// DRA path iterates candidates, accepting the first viable one and
		// continuing on per-candidate soft failures.
		for i, cand := range target.Candidates {
			if !useUUID {
				if util.PodIsTerminated(cand.Pod) {
					return false, fmt.Errorf("pod %s is terminated", klog.KObj(cand.Pod))
				}
			}
			pids, ok := isCandidateAlive(cand, cgroupResolver, peerPid, &lastErr)
			if !ok {
				continue
			}
			configDir = target.ConfigDir
			winPids = pids
			klog.V(5).InfoS("register candidate accepted",
				"pod", klog.KObj(cand.Pod),
				"containerName", cand.ContainerName,
				"candidateIndex", i,
				"pids", len(pids))
			return true, nil
		}
		return false, nil
	})

	if pollErr != nil {
		if errors.Is(pollErr, context.Canceled) || errors.Is(pollErr, context.DeadlineExceeded) {
			if lastErr != nil {
				return "", nil, fmt.Errorf("%w: last attempt: %v", pollErr, lastErr)
			}
		}
		return "", nil, pollErr
	}
	return configDir, winPids, nil
}

// isCandidateAlive runs the per-candidate viability check. Returns (pids,
// true) when the candidate is in Running state and its cgroup has live
// processes; otherwise (nil, false) with *lastErr updated for the eventual
// context-cancel error message.
func isCandidateAlive(cand TargetCandidate, cgroupResolver func(string) string, peerPid int32, lastErr *error) ([]int, bool) {
	if util.PodIsTerminated(cand.Pod) {
		return nil, false
	}
	status, ok := cgroup.GetContainerStatus(cand.Pod, cand.ContainerName)
	if !ok || status.State.Running == nil {
		*lastErr = fmt.Errorf("candidate container %q in pod %s not yet running",
			cand.ContainerName, klog.KObj(cand.Pod))
		return nil, false
	}
	pids := cgroup.GetContainerPidsFunc(cand.Pod, cand.ContainerName, cgroupResolver)
	if len(pids) == 0 {
		*lastErr = fmt.Errorf("candidate container %q in pod %s reports Running but its cgroup has no live PIDs (likely stale informer)",
			cand.ContainerName, klog.KObj(cand.Pod))
		return nil, false
	}
	// Caller authentication: when the kernel-supplied caller PID is known,
	// require it to be one of the container's live PIDs — proving the request
	// originates from inside the very container it registers for, not a
	// neighbour impersonating it. Fail open when peerPid is unavailable.
	if peerPid > 0 && !slices.Contains(pids, int(peerPid)) {
		*lastErr = fmt.Errorf("caller pid %d is not a process of candidate container %q in pod %s (possible impersonation or stale view)",
			peerPid, cand.ContainerName, klog.KObj(cand.Pod))
		return nil, false
	}
	return pids, true
}

// candidatesIncludePod reports whether any candidate belongs to the given pod UID.
func candidatesIncludePod(cands []TargetCandidate, podUID string) bool {
	for i := range cands {
		if cands[i].Pod != nil && strings.EqualFold(string(cands[i].Pod.UID), podUID) {
			return true
		}
	}
	return false
}

// cgroupFullPathResolver picks the right cgroup hierarchy walker based on the
// host's cgroup driver / unified-vs-hybrid mode.
func cgroupFullPathResolver() func(string) string {
	switch {
	case cgroups.IsCgroup2UnifiedMode():
		return cgroup.GetK8sPodCGroupFullPath
	case cgroups.IsCgroup2HybridMode():
		if util.PathIsNotExist(cgroup.CGroupDevicePath) {
			return cgroup.GetK8sPodCGroupFullPath
		}
		return cgroup.GetK8sPodDeviceCGroupFullPath
	default:
		return cgroup.GetK8sPodDeviceCGroupFullPath
	}
}

// lookupTarget performs a single (non-retrying) resolution attempt. It picks
// the right resolver based on the request shape and synthesizes a Target. For
// the legacy path the Target carries exactly one candidate.
func (s *DeviceRegistryServerImpl) lookupTarget(ctx context.Context, req *registry.ContainerDeviceRequest) (*Target, error) {
	if uuid := req.GetRegisterUuid(); uuid != "" {
		if s.getTargetByUUIDFn == nil {
			return nil, errors.New("uuid registration is not supported by this server")
		}
		t, err := s.getTargetByUUIDFn(ctx, uuid)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, fmt.Errorf("uuid %s did not resolve to a target", uuid)
		}
		if len(t.Candidates) == 0 {
			return nil, fmt.Errorf("uuid %s resolver returned no candidates", uuid)
		}
		if t.ConfigDir == "" {
			return nil, fmt.Errorf("uuid %s resolver returned empty ConfigDir", uuid)
		}
		return t, nil
	}

	if s.getPodByUIDFn == nil {
		return nil, errors.New("pod-uid registration is not supported by this server")
	}
	podUID := req.GetPodUid()
	containerName := req.GetContainerName()
	if podUID == "" {
		return nil, errors.New("pod_uid is empty")
	}
	if containerName == "" {
		return nil, errors.New("container_name is empty")
	}
	pod, err := s.getPodByUIDFn(ctx, podUID)
	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, fmt.Errorf("pod %q not found", podUID)
	}
	return &Target{
		Candidates: []TargetCandidate{{
			Pod:           pod,
			ContainerName: containerName,
		}},
		ConfigDir: filepath.Join(
			util.GetPodContainerManagerPath(s.contPath, pod.UID, containerName),
			util.Config,
		),
	}, nil
}

// persistPids writes the sorted PID list to <configDir>/pids.config atomically
// (flock + truncate+write) via the in-tree cgo helper.
func (s *DeviceRegistryServerImpl) persistPids(configDir string, pids []int) error {
	_ = util.EnsureDir(configDir, 0o777)
	var buf bytes.Buffer
	sort.Ints(pids)
	for _, pid := range pids {
		buf.WriteString(strconv.Itoa(pid))
		buf.WriteByte('\n')
	}

	cFileName := C.CString(filepath.Join(configDir, PidsConfig))
	cData := C.CString(buf.String())
	defer func() {
		C.free(unsafe.Pointer(cFileName))
		C.free(unsafe.Pointer(cData))
	}()
	if C.write_to_disk(cFileName, cData) != 0 {
		return fmt.Errorf("failed to write pids file at %s", filepath.Join(configDir, PidsConfig))
	}
	return nil
}

func (s *DeviceRegistryServerImpl) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.running {
		return fmt.Errorf("DeviceRegistry server is already running")
	}
	registryPath := filepath.Join(s.contPath, util.Registry)
	_ = os.MkdirAll(registryPath, 0777)
	_ = os.Chmod(registryPath, 0777)
	socketFile := filepath.Join(registryPath, SocketFile)
	if err := syscall.Unlink(socketFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %v", err)
	}

	addr, err := net.ResolveUnixAddr("unix", socketFile)
	if err != nil {
		return fmt.Errorf("failed to resolve unix addr: %v", err)
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("failed to listen unix: %v", err)
	}

	if err = os.Chmod(socketFile, 0777); err != nil {
		_ = listener.Close()
		return fmt.Errorf("failed to set socket permissions: %v", err)
	}
	s.listener = listener
	// Hardening against a hijacked container flooding the shared socket:
	//   - Creds captures the caller's SO_PEERCRED PID for authentication.
	//   - small message / stream / connection limits bound per-connection cost
	//     (the request body is a few identifiers; one short stream per caller).
	//   - keepalive enforcement rejects clients that ping too aggressively.
	// The per-request deadline + concurrency budget live in resolveTarget /
	// acquireSlot.
	s.server = grpc.NewServer(
		grpc.Creds(peerCredentials{}),
		grpc.MaxRecvMsgSize(maxRecvMsgBytes),
		grpc.MaxConcurrentStreams(maxConcurrentStreams),
		grpc.ConnectionTimeout(connectionTimeout),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinTime,
			PermitWithoutStream: false,
		}),
	)

	registry.RegisterVDeviceRegistryServer(s.server, s)
	s.running = true

	go func() {
		if err = s.server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			klog.Errorf("DeviceRegistry gRPC server serve error: %v", err)
		}
		s.mutex.Lock()
		s.running = false
		s.mutex.Unlock()
	}()

	klog.V(3).Info("DeviceRegistry gRPC server started successfully")
	return nil
}

func (s *DeviceRegistryServerImpl) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Immediately close the listener and reject new connections
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}

	// Elegantly stop existing requests
	if s.server != nil {
		stopped := make(chan struct{})
		go func() {
			s.server.GracefulStop()
			close(stopped)
		}()

		select {
		case <-stopped:
			klog.Info("DeviceRegistry gRPC server stopped gracefully")
		case <-time.After(10 * time.Second):
			klog.Warning("Force stopping gRPC server after timeout")
			s.server.Stop()
		}
		s.server = nil
	}

	s.running = false
}
