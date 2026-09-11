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
	"net"
	"reflect"
	"testing"
)

func ips(s ...string) []net.IP {
	out := make([]net.IP, 0, len(s))
	for _, x := range s {
		out = append(out, net.ParseIP(x))
	}
	return out
}

func TestOrderCandidates(t *testing.T) {
	t.Run("InternalIP first, physical before virtual, v4 before v6, junk dropped", func(t *testing.T) {
		ifaces := []ifaceAddrs{
			{name: "docker0", ips: ips("172.17.0.1")},
			{name: "eth0", ips: ips("10.0.0.7", "fe80::1", "2001:db8::7")},
			{name: "flannel.1", ips: ips("10.244.0.0")},
			{name: "eth1", ips: ips("192.168.1.7", "127.0.0.1", "0.0.0.0", "224.0.0.1")},
		}
		got := orderCandidates([]string{"10.0.0.7", " 10.0.0.7 "}, ifaces)
		want := []string{"10.0.0.7", "192.168.1.7", "2001:db8::7", "172.17.0.1", "10.244.0.0"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("orderCandidates = %v, want %v", got, want)
		}
	})
	t.Run("InternalIP that is not a global unicast is ignored", func(t *testing.T) {
		got := orderCandidates([]string{"127.0.0.1", "not-an-ip", ""}, []ifaceAddrs{{name: "eth0", ips: ips("10.0.0.7")}})
		if !reflect.DeepEqual(got, []string{"10.0.0.7"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("empty inputs give an empty list", func(t *testing.T) {
		if got := orderCandidates(nil, nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("capped at maxCandidates, best kept", func(t *testing.T) {
		var many []net.IP
		for i := 1; i <= maxCandidates+5; i++ {
			many = append(many, net.IPv4(10, 1, 0, byte(i)))
		}
		got := orderCandidates([]string{"192.168.9.9"}, []ifaceAddrs{{name: "eth0", ips: many}})
		if len(got) != maxCandidates || got[0] != "192.168.9.9" {
			t.Fatalf("len=%d first=%q", len(got), got[0])
		}
	})
}

func TestIsVirtualIface(t *testing.T) {
	for name, virtual := range map[string]bool{
		"eth0": false, "ens192": false, "bond0": false, "eno1np0": false,
		"docker0": true, "br-1234": true, "cni0": true, "flannel.1": true,
		"cali123abc": true, "vethabc": true, "tunl0": true, "cilium_host": true,
		"Docker0": true,
	} {
		if got := isVirtualIface(name); got != virtual {
			t.Errorf("isVirtualIface(%q) = %v, want %v", name, got, virtual)
		}
	}
}
