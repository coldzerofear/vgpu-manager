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

// Package gpuallocator is a trimmed fork of NVIDIA/go-gpuallocator, carrying
// the GPU device model and the P2P link-type table the scheduler's topology
// modes are built on.
//
// The surface splits in two, and the split is not obvious from the files:
//
// LIVE — consumed by the scheduler on every topology-mode placement:
//
//	Device, DeviceList, NewDevice, NewDevices, WithNvmlLib, WithDeviceLib
//	PairScore  (the per-pair link score every tier decision is ranked by)
//
// RETAINED, NOT CALLED IN PRODUCTION — Allocator, Policy, NewAllocator,
// NewBestEffortAllocator, NewBestEffortPolicy, DeviceSet:
//
// bestEffortPolicy WAS the link-allocation algorithm until the tiered selector
// replaced it (see docs/link_topology_tiered_allocation_design.md). It is kept
// deliberately, for two reasons:
//
//  1. It is the BASELINE in pkg/device/allocator/comparison_test.go, which is
//     the evidence that the replacement is never worse on real topologies.
//     Deleting the policy deletes the proof along with it.
//  2. These files are byte-compatible with upstream, which keeps a future
//     re-sync a diff rather than a merge.
//
// So: if you are looking for the code that actually chooses GPUs today, it is
// pkg/device/allocator/tiered.go, not here. Do not wire anything new to
// Allocator — it initialises NVML, which the scheduler must never do.
package gpuallocator
