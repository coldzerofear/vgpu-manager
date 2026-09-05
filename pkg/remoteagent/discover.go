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

package remoteagent

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// serverState is one immutable observation of lupine-server, replaced as a
// whole by every probe so readers never see a version from one probe and a
// reachability bit from another.
type serverState struct {
	// Up: the last probe got an HTTP answer with the CUDA version header.
	Up bool
	// CudaVersion is the server build version as it reports it ("13.3.73").
	// Kept from the last successful probe while the server is down: a restart
	// from the same image is the common case, and the publisher must not
	// drop the attribute over a blip.
	CudaVersion string
	// Endpoint is the address other nodes can reach the server at, in URL
	// form, or "" when not known. Set from --advertise-server-endpoint, from
	// --remote-server-endpoint when its host is routable, or discovered by
	// probing this host's addresses when it is a loopback (see discover).
	Endpoint string
	// RoutableHost is the host of this machine that answered for the
	// server (or the routable probe host); the agent's own endpoint is built
	// on it. "" when unknown. Kept separately from Endpoint because an
	// advertised server endpoint (DNS, gateway) says nothing about how to
	// reach the agent.
	RoutableHost string
	// AgentEndpoint is this agent's own gRPC address other nodes should use
	// (grpc://host:port), or "" when the host is unknown or the agent has no
	// TCP listener.
	AgentEndpoint string
	// LastProbe is when this observation was made.
	LastProbe time.Time
}

const (
	// One probe of one candidate; a listening server answers in milliseconds,
	// so this is generous, and it bounds a scan of unroutable candidates.
	candidateProbeTimeout = 2 * time.Second
	// The whole discovery pass: enough for a handful of dead candidates, short
	// enough that the probe loop (5s period) is not starved for long.
	discoverTimeout = 20 * time.Second
	// A node rarely changes its InternalIP; do not ask the API on every scan.
	nodeAddrsTTL = time.Minute
	// Interfaces on a fat node can carry hundreds of addresses (pod veths on
	// hostNetwork do not, but SR-IOV VFs and IPv6 temporaries can); the
	// preferred ones sort first, so the tail is not worth probing.
	maxCandidates = 16
)

// virtualIfacePrefixes name interfaces that carry overlay, bridge or
// container-side addresses. They are not excluded, only probed last: a
// pod on another node cannot normally reach them, and when a cluster does
// route them (host-gw flannel, calico without encapsulation) the node
// InternalIP is a better advertisement anyway.
var virtualIfacePrefixes = []string{
	"docker", "br-", "cni", "flannel", "cali", "veth", "lxc", "virbr", "tunl",
	"vxlan", "kube-ipvs", "dummy", "nodelocaldns", "tap", "wg", "ovs", "antrea",
	"cilium", "lxd", "vnet", "podman", "ip6tnl", "sit", "gre", "erspan", "ipip",
}

// ifaceAddrs is one interface as seen by candidate ordering: its name and
// the IPs it carries. Split out so the ordering is testable without a NIC.
type ifaceAddrs struct {
	name string
	ips  []net.IP
}

func isVirtualIface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// hostIfaceAddrs lists the up, non-loopback interfaces and their addresses.
func hostIfaceAddrs() ([]ifaceAddrs, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceAddrs, 0, len(ifaces))
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			klog.V(4).Infof("interface %s: addrs: %v", ifc.Name, err)
			continue
		}
		ia := ifaceAddrs{name: ifc.Name}
		for _, addr := range addrs {
			var ip net.IP
			switch a := addr.(type) {
			case *net.IPNet:
				ip = a.IP
			case *net.IPAddr:
				ip = a.IP
			}
			if ip != nil {
				ia.ips = append(ia.ips, ip)
			}
		}
		if len(ia.ips) > 0 {
			out = append(out, ia)
		}
	}
	return out, nil
}

// orderCandidates ranks the hosts worth advertising, best first:
//
//  1. the node's InternalIP addresses, in API order -- what the kubelet
//     itself is reached at, so it is routable from every other node;
//  2. global-unicast addresses of physical-looking interfaces, IPv4 before
//     IPv6, in interface order;
//  3. the same for virtual-looking interfaces (see virtualIfacePrefixes).
//
// Loopback, link-local, multicast and unspecified addresses are dropped; so
// are duplicates, keeping the first (best) position. The list is capped at
// maxCandidates.
func orderCandidates(internalIPs []string, ifaces []ifaceAddrs) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ip net.IP) {
		if ip == nil || !ip.IsGlobalUnicast() {
			return
		}
		s := ip.String()
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range internalIPs {
		add(net.ParseIP(strings.TrimSpace(s)))
	}
	type bucket struct {
		virtual bool
		v6      bool
		order   int
		ip      net.IP
	}
	var rest []bucket
	for i, ifc := range ifaces {
		virtual := isVirtualIface(ifc.name)
		for _, ip := range ifc.ips {
			if !ip.IsGlobalUnicast() {
				continue
			}
			rest = append(rest, bucket{virtual: virtual, v6: ip.To4() == nil, order: i, ip: ip})
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].virtual != rest[j].virtual {
			return !rest[i].virtual
		}
		if rest[i].v6 != rest[j].v6 {
			return !rest[i].v6
		}
		return rest[i].order < rest[j].order
	})
	for _, b := range rest {
		add(b.ip)
	}
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// nodeInternalIPs returns the node's InternalIP addresses, cached for
// nodeAddrsTTL. Failures are logged and yield the cached (possibly empty)
// list: discovery still has the interface addresses to fall back on.
func (a *Agent) nodeInternalIPs(ctx context.Context) []string {
	a.nodeAddrsMu.Lock()
	defer a.nodeAddrsMu.Unlock()
	if time.Since(a.nodeAddrsAt) < nodeAddrsTTL {
		return a.nodeAddrs
	}
	if a.cfg.ClientSets.Core == nil {
		return a.nodeAddrs
	}
	node, err := a.cfg.ClientSets.Core.CoreV1().Nodes().Get(ctx, a.cfg.NodeName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		klog.V(2).Infof("node %s InternalIP lookup failed (using interface addresses only): %v", a.cfg.NodeName, err)
		return a.nodeAddrs
	}
	var ips []string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			ips = append(ips, addr.Address)
		}
	}
	a.nodeAddrs, a.nodeAddrsAt = ips, time.Now()
	return ips
}

// serverAnswersAt probes lupine-server at the probe endpoint with its host
// replaced by host.
func serverAnswersAt(ctx context.Context, probe *endpointutil.Endpoint, host string) bool {
	candidate := *probe
	candidate.Host = host
	_, err := remote.ProbeServerCUDAVersion(ctx, candidate.String(), candidateProbeTimeout)
	if err != nil {
		klog.V(4).Infof("candidate %s: %v", candidate.String(), err)
	}
	return err == nil
}

// discoverRoutableHost finds an address of this host at which the
// lupine-server answering on the loopback probe endpoint also answers.
// Returns "" when no candidate answers within discoverTimeout.
func (a *Agent) discoverRoutableHost(ctx context.Context, probe *endpointutil.Endpoint) string {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	ifaces, err := hostIfaceAddrs()
	if err != nil {
		klog.Warningf("list host interfaces: %v", err)
	}
	candidates := orderCandidates(a.nodeInternalIPs(ctx), ifaces)
	for _, host := range candidates {
		if ctx.Err() != nil {
			break
		}
		if serverAnswersAt(ctx, probe, host) {
			return host
		}
	}
	klog.V(2).Infof("no routable address answers for lupine-server (tried %d candidate(s): %v)", len(candidates), candidates)
	return ""
}
