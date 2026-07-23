/**
# Copyright 2023 NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package links

import (
	"fmt"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// P2PLinkType defines the link information between two devices.
type P2PLinkType uint

// The following constants define the nature of a link between two devices.
// These include peer-2-peer and NVLink information.
const (
	P2PLinkUnknown P2PLinkType = iota
	P2PLinkCrossCPU
	P2PLinkSameCPU
	P2PLinkHostBridge
	P2PLinkMultiSwitch
	P2PLinkSingleSwitch
	P2PLinkSameBoard
	SingleNVLINKLink
	TwoNVLINKLinks
	ThreeNVLINKLinks
	FourNVLINKLinks
	FiveNVLINKLinks
	SixNVLINKLinks
	SevenNVLINKLinks
	EightNVLINKLinks
	NineNVLINKLinks
	TenNVLINKLinks
	ElevenNVLINKLinks
	TwelveNVLINKLinks
	ThirteenNVLINKLinks
	FourteenNVLINKLinks
	FifteenNVLINKLinks
	SixteenNVLINKLinks
	SeventeenNVLINKLinks
	EighteenNVLINKLinks
)

// String returns the string representation of the P2PLink type.
func (l P2PLinkType) String() string {
	switch l {
	case P2PLinkCrossCPU:
		return "P2PLinkCrossCPU"
	case P2PLinkSameCPU:
		return "P2PLinkSameCPU"
	case P2PLinkHostBridge:
		return "P2PLinkHostBridge"
	case P2PLinkMultiSwitch:
		return "P2PLinkMultiSwitch"
	case P2PLinkSingleSwitch:
		return "P2PLinkSingleSwitch"
	case P2PLinkSameBoard:
		return "P2PLinkSameBoard"
	case SingleNVLINKLink:
		return "SingleNVLINKLink"
	case TwoNVLINKLinks:
		return "TwoNVLINKLinks"
	case ThreeNVLINKLinks:
		return "ThreeNVLINKLinks"
	case FourNVLINKLinks:
		return "FourNVLINKLinks"
	case FiveNVLINKLinks:
		return "FiveNVLINKLinks"
	case SixNVLINKLinks:
		return "SixNVLINKLinks"
	case SevenNVLINKLinks:
		return "SevenNVLINKLinks"
	case EightNVLINKLinks:
		return "EightNVLINKLinks"
	case NineNVLINKLinks:
		return "NineNVLINKLinks"
	case TenNVLINKLinks:
		return "TenNVLINKLinks"
	case ElevenNVLINKLinks:
		return "ElevenNVLINKLinks"
	case TwelveNVLINKLinks:
		return "TwelveNVLINKLinks"
	case ThirteenNVLINKLinks:
		return "ThirteenNVLINKLinks"
	case FourteenNVLINKLinks:
		return "FourteenNVLINKLinks"
	case FifteenNVLINKLinks:
		return "FifteenNVLINKLinks"
	case SixteenNVLINKLinks:
		return "SixteenNVLINKLinks"
	case SeventeenNVLINKLinks:
		return "SeventeenNVLINKLinks"
	case EighteenNVLINKLinks:
		return "EighteenNVLINKLinks"
	default:
		return fmt.Sprintf("UNKNOWN (%v)", uint(l))
	}
}

// GetP2PLink gets the peer-to-peer connectivity between two devices.
func GetP2PLink(dev1 device.Device, dev2 device.Device) (P2PLinkType, error) {
	level, ret := dev1.GetTopologyCommonAncestor(dev2)
	if ret != nvml.SUCCESS {
		return P2PLinkUnknown, fmt.Errorf("failed to get commmon anscestor: %v", ret)
	}

	switch level {
	case nvml.TOPOLOGY_INTERNAL:
		return P2PLinkSameBoard, nil
	case nvml.TOPOLOGY_SINGLE:
		return P2PLinkSingleSwitch, nil
	case nvml.TOPOLOGY_MULTIPLE:
		return P2PLinkMultiSwitch, nil
	case nvml.TOPOLOGY_HOSTBRIDGE:
		return P2PLinkHostBridge, nil
	case nvml.TOPOLOGY_NODE: // NVML_TOPOLOGY_CPU was renamed NVML_TOPOLOGY_NODE
		return P2PLinkSameCPU, nil
	case nvml.TOPOLOGY_SYSTEM:
		return P2PLinkCrossCPU, nil

	}

	return P2PLinkUnknown, fmt.Errorf("unknown topology level: %v", level)
}

// GetNVLink gets the number of NVLinks between the specified devices, covering
// BOTH interconnect topologies:
//
//   - Direct GPU<->GPU NVLink (small boards): a link's remote PCI is the peer
//     GPU, so matching remote BusIDs against dev2 counts the links.
//   - NVSwitch fabric (HGX/DGX): every link terminates at a switch, so no remote
//     BusID ever equals the peer GPU. Matching alone therefore reports "no
//     NVLink" for a fully connected 8-GPU board — which silently degrades every
//     downstream topology decision (NVLink component/island detection,
//     strict-link validation, node fitness ranking, bestEffort pair scoring).
//     We detect the fabric via the remote device TYPE instead.
func GetNVLink(dev1 device.Device, dev2 device.Device) (P2PLinkType, error) {
	pciInfos, err := getAllNvLinkRemotePciInfo(dev1)
	if err != nil {
		return P2PLinkUnknown, fmt.Errorf("failed to get nvlink remote pci info: %v", err)
	}

	dev2PciInfo, ret := dev2.GetPciInfo()
	if ret != nvml.SUCCESS {
		return P2PLinkUnknown, fmt.Errorf("failed to get pci info: %v", ret)
	}
	dev2BusID := PciInfo(dev2PciInfo).BusID()

	// Direct GPU <-> GPU: each link's remote PCI is the peer GPU itself.
	matched := 0
	for _, pciInfo := range pciInfos {
		if pciInfo.BusID() == dev2BusID {
			matched++
		}
	}
	if direct := nvlinkCountToType(matched); direct != P2PLinkUnknown {
		return direct, nil
	}

	// NVSwitch fabric: every link terminates at a switch, so the remote BusID
	// NEVER equals the peer GPU and the direct match above always yields zero —
	// which would report "no NVLink" for a fully connected 8-GPU HGX/DGX board.
	// Both endpoints must be attached to the fabric; the usable width is the
	// weaker side's enabled-link count.
	links1, viaSwitch1, err := countNvSwitchLinks(dev1)
	if err != nil {
		return P2PLinkUnknown, fmt.Errorf("failed to check nvswitch links for dev1: %v", err)
	}
	if !viaSwitch1 {
		return P2PLinkUnknown, nil
	}
	links2, viaSwitch2, err := countNvSwitchLinks(dev2)
	if err != nil {
		return P2PLinkUnknown, fmt.Errorf("failed to check nvswitch links for dev2: %v", err)
	}
	if !viaSwitch2 {
		return P2PLinkUnknown, nil
	}
	return nvlinkCountToType(min(links1, links2)), nil
}

// nvlinkTypes indexes P2PLinkType by (link count - 1).
var nvlinkTypes = [...]P2PLinkType{
	SingleNVLINKLink, TwoNVLINKLinks, ThreeNVLINKLinks, FourNVLINKLinks,
	FiveNVLINKLinks, SixNVLINKLinks, SevenNVLINKLinks, EightNVLINKLinks,
	NineNVLINKLinks, TenNVLINKLinks, ElevenNVLINKLinks, TwelveNVLINKLinks,
	ThirteenNVLINKLinks, FourteenNVLINKLinks, FifteenNVLINKLinks,
	SixteenNVLINKLinks, SeventeenNVLINKLinks, EighteenNVLINKLinks,
}

// nvlinkCountToType maps an NVLink count to its P2PLinkType. Zero or fewer means
// "not NVLink-connected".
//
// Counts above the largest enumerated tier SATURATE to EighteenNVLINKLinks
// rather than falling back to P2PLinkUnknown. This is deliberate: the enum tops
// out at 18 (today's H100/B200 per-GPU maximum) while NVML reports up to
// NVLINK_MAX_LINKS (36) links, so a future GPU — or a switch-attached count
// aggregated across links — can legitimately exceed 18. Mapping that to
// "unknown" would claim the pair has NO NVLink, re-introducing exactly the
// class of failure this file exists to prevent (isolated islands → strict-link
// rejects the node, cross-pod affinity degenerates). Over-reporting link WIDTH
// at the top tier is harmless by comparison: it only affects relative pair
// scoring, never connectivity.
func nvlinkCountToType(n int) P2PLinkType {
	if n <= 0 {
		return P2PLinkUnknown
	}
	if n > len(nvlinkTypes) {
		n = len(nvlinkTypes)
	}
	return nvlinkTypes[n-1]
}

// countNvSwitchLinks returns how many of the device's ENABLED NVLinks terminate
// at an NVSwitch, plus whether it has any such link at all. Per-link probe
// results that merely mean "this link index is not usable" are skipped, matching
// getAllNvLinkRemotePciInfo's tolerance (ERROR_GPU_IS_LOST included, which a
// draining node can legitimately return).
func countNvSwitchLinks(dev device.Device) (int, bool, error) {
	count := 0
	for i := 0; i < nvml.NVLINK_MAX_LINKS; i++ {
		state, ret := dev.GetNvLinkState(i)
		if ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_INVALID_ARGUMENT || ret == nvml.ERROR_GPU_IS_LOST {
			continue
		}
		if ret != nvml.SUCCESS {
			return 0, false, fmt.Errorf("failed to get nvlink state: %v", ret)
		}
		if state != nvml.FEATURE_ENABLED {
			continue
		}
		deviceType, ret := dev.GetNvLinkRemoteDeviceType(i)
		if ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_INVALID_ARGUMENT || ret == nvml.ERROR_GPU_IS_LOST {
			continue
		}
		if ret != nvml.SUCCESS {
			return 0, false, fmt.Errorf("failed to get nvlink remote device type: %v", ret)
		}
		if deviceType == nvml.NVLINK_DEVICE_TYPE_SWITCH {
			count++
		}
	}
	return count, count > 0, nil
}

// getAllNvLinkRemotePciInfo returns the PCI info for all devices attached to the specified device by an NVLink
func getAllNvLinkRemotePciInfo(dev device.Device) ([]PciInfo, error) {
	var pciInfos []PciInfo
	for i := 0; i < nvml.NVLINK_MAX_LINKS; i++ {
		state, ret := dev.GetNvLinkState(i)
		if ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_INVALID_ARGUMENT || ret == nvml.ERROR_GPU_IS_LOST {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get nvlink state: %v", ret)
		}
		if state != nvml.FEATURE_ENABLED {
			continue
		}
		pciInfo, ret := dev.GetNvLinkRemotePciInfo(i)
		if ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_INVALID_ARGUMENT || ret == nvml.ERROR_GPU_IS_LOST {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get remote pci info: %v", ret)
		}
		pciInfos = append(pciInfos, PciInfo(pciInfo))
	}

	return pciInfos, nil
}
