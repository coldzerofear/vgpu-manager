#!/usr/bin/env python3
"""
Static consistency checks for vgpu-manager's CUDA hook registration chain.

Verifies the three tables that must stay in lock-step:
  1. cuda_library_entry[]   in src/loader.c        -> runtime dlsym targets
  2. cuda_entry_enum_t      in include/cuda-helper.h -> compile-time indices
  3. wrapper function bodies in src/cuda_originals.c / src/cuda_hook.c

Rules enforced:
  R1. Count of entries in (1) and (2) must match.
  R2. Order of entries in (1) and (2) must match pair-wise (indexing uses
      the enum value, so any reorder silently mis-routes every subsequent
      symbol to the wrong real libcuda.so pointer).
  R3. No duplicate names in (1) or (2).
  R4. ABI-conflict families: both old and new ABI versions must be registered.
  R5. The unversioned base name of an ABI-conflict family must NOT appear in
      cuda_hooks_entry[] in src/cuda_hook.c (rule 3 in cuda-helper.h).
  R6. Every (1) entry must have a wrapper function definition exported from
      the .so (unless explicitly allowlisted as forward-only).
  R7. Every base name that has TWO OR MORE _vN siblings in the dispatch
      table must be classified as either:
       - ABI-conflict (added to ABI_CONFLICT_FAMILIES)         => R4/R5 apply
       - signature-stable (added to R7_ALLOWLIST with reason)  => safe to ignore
      Catches regressions where a newly-added versioned hook silently
      introduces a frame-mismatch risk at the cuGetProcAddress
      substitution site (vgpu-manager Issue #1834 class of LD_PRELOAD
      hangs).

Exit 0 if all pass; exit 1 on any R1-R6 violation.

Use --allow-r6 <name> repeatedly to allowlist specific symbols (e.g. entries
that intentionally have no ELF wrapper); the default allowlist is empty so
any regression trips the build.
"""

from __future__ import annotations

import argparse
import re
import sys
from collections import Counter
from pathlib import Path
from typing import Iterable

ROOT = Path(__file__).resolve().parents[1]
LOADER_FILE = HELPER_HEADER = HOOK_FILE = Path()
WRAPPER_FILES: list[Path] = []


def set_root(root: Path) -> None:
    """Point every path at `root`, so one checker serves both library/ and
    library-remote/ (same layout, different hook sets)."""
    global ROOT, LOADER_FILE, HELPER_HEADER, HOOK_FILE, WRAPPER_FILES
    ROOT = root
    LOADER_FILE = root / "src/loader.c"
    HELPER_HEADER = root / "include/cuda-helper.h"
    HOOK_FILE = root / "src/cuda_hook.c"
    WRAPPER_FILES = [root / "src/cuda_originals.c", root / "src/cuda_hook.c"]


set_root(ROOT)

# ABI-conflict families: base name -> (old_abi_symbol, new_abi_symbol).
#
# Related to is_abi_conflict_base() in include/cuda-helper.h but not identical
# to it, and conflating the two costs real behaviour. This table is the set of
# families whose two ABIs take different parameters. That list is the subset of
# them whose base name cuGetProcAddress must answer with the driver's own
# pointer - which is only right for a family we do not hook. A family we DO
# hook under its base name, with a wrapper of the matching ABI, belongs here
# and not there, and is recorded in R5_ALLOWLIST with the reason.
#
# Verified against a real <cuda.h> by hack/check_abi_signatures.py.
ABI_CONFLICT_FAMILIES = {
    "cuCtxCreate":                   ("cuCtxCreate_v2",                  "cuCtxCreate_v4"),
    "cuMemAdvise":                   ("cuMemAdvise",                     "cuMemAdvise_v2"),
    "cuMemPrefetchAsync":            ("cuMemPrefetchAsync",              "cuMemPrefetchAsync_v2"),
    "cuGraphGetEdges":               ("cuGraphGetEdges",                 "cuGraphGetEdges_v2"),
    "cuGraphNodeGetDependencies":    ("cuGraphNodeGetDependencies",      "cuGraphNodeGetDependencies_v2"),
    "cuGraphNodeGetDependentNodes":  ("cuGraphNodeGetDependentNodes",    "cuGraphNodeGetDependentNodes_v2"),
    "cuGraphAddDependencies":        ("cuGraphAddDependencies",          "cuGraphAddDependencies_v2"),
    "cuGraphRemoveDependencies":     ("cuGraphRemoveDependencies",       "cuGraphRemoveDependencies_v2"),
    "cuGraphAddNode":                ("cuGraphAddNode",                  "cuGraphAddNode_v2"),
    "cuStreamGetCaptureInfo":        ("cuStreamGetCaptureInfo_v2",       "cuStreamGetCaptureInfo_v3"),
    "cuGraphAddKernelNode":          ("cuGraphAddKernelNode",            "cuGraphAddKernelNode_v2"),
    "cuGraphKernelNodeSetParams":    ("cuGraphKernelNodeSetParams",      "cuGraphKernelNodeSetParams_v2"),
    "cuGraphKernelNodeGetParams":    ("cuGraphKernelNodeGetParams",      "cuGraphKernelNodeGetParams_v2"),
    # (CUgraphExec, CUgraph, CUgraphNode*, CUgraphExecUpdateResult*) against
    # (CUgraphExec, CUgraph, CUgraphExecUpdateResultInfo*): four arguments
    # against three. Both wrappers already bind their own entry.
    "cuGraphExecUpdate":             ("cuGraphExecUpdate",               "cuGraphExecUpdate_v2"),
    # (CUstream, CUcontext*) against (CUstream, CUcontext*, CUgreenCtx*).
    # Both wrappers already call the entry of their own name directly, with
    # no trampoline between them, and the base is not hooked.
    "cuStreamGetCtx":                ("cuStreamGetCtx",                  "cuStreamGetCtx_v2"),
    # Four arguments against five (the fifth being
    # CUdriverProcAddressQueryResult*). It is in this table because its ABIs do
    # differ, and in R5_ALLOWLIST because its base name has to stay hooked:
    # see the note in is_abi_conflict_base(), which keeps it out of THAT list
    # for the opposite reason - handing back the driver's own resolver would
    # let every later lookup bypass cuda_hooks_entry[] entirely.
    "cuGetProcAddress":              ("cuGetProcAddress",                "cuGetProcAddress_v2"),
}

# R5 allowlist: conflict-family base names that MAY appear in
# cuda_hooks_entry[].
#
# R5's blanket rule assumes a conflict family is one we keep our hands off, so
# that cuGetProcAddress can hand back the driver's pointer. A family we need to
# intercept cannot be handled that way: not hooking it would lose the
# interception, and hooking only the _vN form would leave callers of the older
# one unhooked. The requirement that actually matters is that EACH registered
# name carries a wrapper of ITS OWN ABI, and that substitution picks between
# them by the pointer the driver returned (lookup_cuda_hook_ptr) rather than by
# name. An entry here is a claim that both hold.
R5_ALLOWLIST: set = {
    # cuGraphExecUpdate and _v2 are both hooked, both needed: each refreshes
    # the cached graph cost after an update, and a CUDA 11 caller reaching only
    # the four-argument form still has to be seen. The two wrappers take four
    # and three arguments respectively, each calling the entry of that name, so
    # whichever one substitution lands on matches the caller's frame.
    "cuGraphExecUpdate",
    # cuGetProcAddress and _v2 are both hooked and both must be: the pointer
    # this call hands back is what the caller uses to find every other CUDA
    # symbol, so letting it through unhooked would take the whole table out of
    # the picture. The four-argument and five-argument wrappers each call the
    # entry of their own name, and substitution picks between them by the
    # pointer the driver returned.
    "cuGetProcAddress",
}

# Families upstream gave two ABIs where this library deliberately implements
# only the newer one, under both exported names.
#
# These are the CUDA 3.2 -> 4.0 widening: upstream's plain ELF symbol still
# takes 32-bit sizes and CUdeviceptr_v1, its _v2 takes size_t and the 64-bit
# CUdeviceptr. Both of our wrappers declare the 64-bit form and the shared
# trampoline tries the _v2 entry first, so a caller using the current types
# gets the current behaviour whichever name it came in through. Checked, not
# assumed: all forty were followed through their trampolines and every one
# reaches a versioned entry first.
#
# What this costs is a caller built against CUDA 3.x headers, which would hand
# 32-bit arguments to a 64-bit wrapper. Those headers predate 2011 and this
# library needs far newer ones to build at all.
#
# The trampolines do still keep an `else` that calls the plain v1 entry with
# the modern arguments if _v2 is missing - a rule 2 deviation that can only be
# reached on a pre-CUDA-4.0 driver, i.e. never. Left alone rather than churning
# forty hot-path functions for a branch nothing executes; recorded here so the
# next reader does not have to re-derive it.
#
# Generated and verified by hack/check_abi_signatures.py --check-wrappers
# against CUDA 13.4.2 (2026-10).
MODERN_ONLY_FAMILIES: set = {
    # cuTexRefSetAddress2D was in R7_ALLOWLIST as signature-stable on the
    # strength of _v2 and _v3 agreeing, which they do - but the plain ELF
    # symbol takes CUDA_ARRAY_DESCRIPTOR_v1, CUdeviceptr_v1 and unsigned int.
    # All three wrappers declare the modern form and _cuTexRefSetAddress2D
    # tries _v3, then _v2, then the plain entry, so this is the modern-only
    # pattern and not an exemption. Texture references were removed in CUDA
    # 12, which is why nobody noticed.
    "cuTexRefSetAddress2D",
    # Same shape, from cudaGL.h: the wrappers declare CUdeviceptr and size_t
    # while the plain ELF symbols take CUdeviceptr_v1 and unsigned int, and
    # both trampolines try the _v2 entry first. Read, not assumed.
    "cuGLMapBufferObject",  "cuGLMapBufferObjectAsync",
    "cuArray3DCreate",  "cuArray3DGetDescriptor",  "cuArrayCreate",
    "cuArrayGetDescriptor",  "cuDeviceTotalMem",
    "cuGraphicsResourceGetMappedPointer",  "cuMemAlloc",  "cuMemAllocHost",
    "cuMemAllocPitch",  "cuMemFree",  "cuMemGetAddressRange",
    "cuMemGetInfo",  "cuMemHostGetDevicePointer",  "cuMemcpy2D",
    "cuMemcpy2DAsync",  "cuMemcpy2DUnaligned",  "cuMemcpy3D",
    "cuMemcpy3DAsync",  "cuMemcpyAtoA",  "cuMemcpyAtoD",  "cuMemcpyAtoH",
    "cuMemcpyAtoHAsync",  "cuMemcpyDtoA",  "cuMemcpyDtoD",
    "cuMemcpyDtoDAsync",  "cuMemcpyDtoH",  "cuMemcpyDtoHAsync",
    "cuMemcpyHtoA",  "cuMemcpyHtoAAsync",  "cuMemcpyHtoD",
    "cuMemcpyHtoDAsync",  "cuMemsetD16",  "cuMemsetD2D16",  "cuMemsetD2D32",
    "cuMemsetD2D8",  "cuMemsetD32",  "cuMemsetD8",  "cuModuleGetGlobal",
    "cuTexRefGetAddress",  "cuTexRefSetAddress",
}

# Families no public header declares both forms of, so neither this checker
# nor a reader can say whether their ABIs differ.
#
# cuda.h carries `#define cuLinkCreate cuLinkCreate_v2` and friends, and
# declares the plain names only inside its __CUDA_API_VERSION_INTERNAL block;
# the _v2 names appear nowhere as a prototype. JIT linking left the public API,
# and the signature went with it. cudaGL.h and cudaVDPAU.h do not have them
# either. Settling these needs headers NVIDIA does not ship, so they are
# recorded as undecidable rather than guessed at or left to nag - a family that
# is genuinely new and unclassified has to stay visible, which it cannot if the
# report never empties.
UNDECLARED_FAMILIES: set = {
    "cuLinkCreate",  "cuLinkAddData",  "cuLinkAddFile",
}

# Wrapper parameters that differ from the ELF symbol's in spelling only.
#
# --check-wrappers compares type names, so a pointer written as void* reads as
# a mismatch against a typed pointer even though the two are passed
# identically. An entry here is a claim that the difference cannot change a
# call frame.
BIND_ALLOWLIST: dict = {
    "cuGetProcAddress_v2":
        "Declares void *symbolStatus where the ABI says "
        "CUdriverProcAddressQueryResult *. Both are pointers and pass the "
        "same way; the enum is not vendored in cuda-subset.h, and the wrapper "
        "only forwards the argument.",
}

# R7 allowlist: base names whose versioned ELF exports all take the SAME C
# parameter list, so no caller can be handed a frame it did not build.
#
# To audit, compare each wrapper against the ABI OF THE SYMBOL IT IS NAMED
# AFTER. That is not the same question as whether the wrappers agree with
# each other, and the older wording here asked the second one: the three
# cuTexRefSetAddress2D wrappers do all take the same types, which is what
# that entry below still claims, while the plain ELF symbol takes the
# pre-CUDA-4.0 ones. hack/check_abi_signatures.py --check-wrappers asks the
# first question against a real <cuda.h>; `make check-abi-signatures` runs it.
#
# The parameter lists quoted below were taken from CUDA 13.4.2 by that tool
# rather than typed out, and each family's wrappers were confirmed to declare
# the ABI of their own symbol.
R7_ALLOWLIST: dict = {
    "cuCtxDestroy":
        "cuCtxDestroy and cuCtxDestroy_v2 both take (CUcontext).",
    "cuCtxPopCurrent":
        "cuCtxPopCurrent and cuCtxPopCurrent_v2 both take (CUcontext*).",
    "cuCtxPushCurrent":
        "cuCtxPushCurrent and cuCtxPushCurrent_v2 both take (CUcontext).",
    "cuDeviceGetUuid":
        "cuDeviceGetUuid and cuDeviceGetUuid_v2 both take (CUuuid*, "
        "CUdevice). The newer entry is not merely a rename: cuda.h says "
        "it returns the MIG UUID when the device is in MIG mode. "
        "Preferring it is a behaviour choice, which this entry does not "
        "speak to - only the parameter list.",
    "cuDevicePrimaryCtxRelease":
        "cuDevicePrimaryCtxRelease and cuDevicePrimaryCtxRelease_v2 both "
        "take (CUdevice).",
    "cuDevicePrimaryCtxReset":
        "cuDevicePrimaryCtxReset and cuDevicePrimaryCtxReset_v2 both take "
        "(CUdevice).",
    "cuDevicePrimaryCtxSetFlags":
        "cuDevicePrimaryCtxSetFlags and cuDevicePrimaryCtxSetFlags_v2 "
        "both take (CUdevice, unsigned int).",
    "cuEventDestroy":
        "cuEventDestroy and cuEventDestroy_v2 both take (CUevent).",
    "cuGLCtxCreate":
        "cuGLCtxCreate and cuGLCtxCreate_v2 both take (CUcontext*, unsigned "
        "int, CUdevice). Declared in cudaGL.h, where the body prototype "
        "carries a __CUDA_DEPRECATED prefix.",
    "cuGLGetDevices":
        "cuGLGetDevices and cuGLGetDevices_v2 both take (unsigned int*, "
        "CUdevice*, unsigned int, CUGLDeviceList). From cudaGL.h.",
    "cuVDPAUCtxCreate":
        "cuVDPAUCtxCreate and cuVDPAUCtxCreate_v2 both take (CUcontext*, "
        "unsigned int, CUdevice, VdpDevice, VdpGetProcAddress*). From "
        "cudaVDPAU.h.",
    "cuGraphicsResourceSetMapFlags":
        "cuGraphicsResourceSetMapFlags and "
        "cuGraphicsResourceSetMapFlags_v2 both take (CUgraphicsResource, "
        "unsigned int).",
    "cuIpcOpenMemHandle":
        "cuIpcOpenMemHandle and cuIpcOpenMemHandle_v2 both take "
        "(CUdeviceptr*, CUipcMemHandle, unsigned int).",
    "cuMemHostRegister":
        "cuMemHostRegister and cuMemHostRegister_v2 both take (void*, "
        "size_t, unsigned int).",
    "cuStreamBatchMemOp":
        "cuStreamBatchMemOp and cuStreamBatchMemOp_v2 both take "
        "(CUstream, unsigned int, CUstreamBatchMemOpParams*, unsigned "
        "int).",
    "cuStreamDestroy":
        "cuStreamDestroy and cuStreamDestroy_v2 both take (CUstream).",
    "cuStreamWaitValue32":
        "cuStreamWaitValue32 and cuStreamWaitValue32_v2 both take "
        "(CUstream, CUdeviceptr, cuuint32_t, unsigned int).",
    "cuStreamWaitValue64":
        "cuStreamWaitValue64 and cuStreamWaitValue64_v2 both take "
        "(CUstream, CUdeviceptr, cuuint64_t, unsigned int).",
    "cuStreamWriteValue32":
        "cuStreamWriteValue32 and cuStreamWriteValue32_v2 both take "
        "(CUstream, CUdeviceptr, cuuint32_t, unsigned int).",
    "cuStreamWriteValue64":
        "cuStreamWriteValue64 and cuStreamWriteValue64_v2 both take "
        "(CUstream, CUdeviceptr, cuuint64_t, unsigned int).",
    # cuGraphInstantiate and _v2 both take
    #   (CUgraphExec*, CUgraph, CUgraphNode*, char*, size_t)
    # - cuda.h declares both explicitly in its __CUDA_API_VERSION_INTERNAL
    # block. The `#define cuGraphInstantiate cuGraphInstantiateWithFlags` at
    # the top of cuda.h redirects CUDA 12+ source to a DIFFERENT function,
    # which this library wraps separately; it says nothing about the ABI of
    # the plain ELF symbol. Verified against CUDA 13.4.2 (2026-10).
    "cuGraphInstantiate":
        "Both ELF symbols take (CUgraphExec*, CUgraph, CUgraphNode*, char*, "
        "size_t). The unversioned name is macro-redirected to "
        "cuGraphInstantiateWithFlags in modern headers, which is a separate "
        "function with its own wrapper, not a version of this one.",

}


def strip_c_comments(content: str) -> str:
    content = re.sub(r"/\*.*?\*/", "", content, flags=re.S)
    content = re.sub(r"//[^\n]*", "", content)
    return content


def read_text(path: Path) -> str:
    if not path.exists():
        raise FileNotFoundError(f"required file missing: {path}")
    return path.read_text(encoding="utf-8")


def extract_library_entry(loader_text: str) -> list[str]:
    """
    Extract the ordered list of names from cuda_library_entry[] {...}.
    Only the cuda_library_entry block is consumed (loader.c also contains
    nvml_library_entry, cuda_hooks_entry parallels, etc.).
    """
    text = strip_c_comments(loader_text)
    m = re.search(r"entry_t\s+cuda_library_entry\s*\[\s*\]\s*=\s*\{(.*?)\n\};", text, re.S)
    if not m:
        raise RuntimeError("cuda_library_entry[] block not found in loader.c")
    body = m.group(1)
    return re.findall(r'\{\s*\.name\s*=\s*"([^"]+)"\s*\}', body)


def extract_enum_entries(helper_text: str) -> list[str]:
    """
    Extract the ordered list of CUDA_ENTRY_ENUM(...) symbols from the
    cuda_entry_enum_t definition. Macros definitions/comment references at
    the top of the file are filtered out by scoping to the enum body.
    """
    text = strip_c_comments(helper_text)
    m = re.search(r"typedef\s+enum\s*\{(.*?)CUDA_ENTRY_END\s*\}\s*cuda_entry_enum_t\s*;", text, re.S)
    if not m:
        raise RuntimeError("cuda_entry_enum_t block not found in cuda-helper.h")
    return re.findall(r"CUDA_ENTRY_ENUM\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)", m.group(1))


def extract_hooks_entry(hook_text: str) -> list[str]:
    """Extract ordered base names registered in cuda_hooks_entry[]."""
    text = strip_c_comments(hook_text)
    m = re.search(r"entry_t\s+cuda_hooks_entry\s*\[\s*\]\s*=\s*\{(.*?)\n\};", text, re.S)
    if not m:
        return []
    return re.findall(r'\{\s*\.name\s*=\s*"([^"]+)"', m.group(1))


def extract_wrapper_defs(paths: Iterable[Path]) -> set[str]:
    """
    Collect names of functions whose return type is CUresult, CUDBGResult,
    or void (the three shapes used by CUDA driver API entry points this
    library wraps).
    """
    pattern = re.compile(
        r"^\s*(?:static\s+)?(?:CUresult|CUDBGResult|void)\s+"
        r"(?:\*\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(",
        re.M,
    )
    defs: set[str] = set()
    for p in paths:
        for m in pattern.finditer(strip_c_comments(read_text(p))):
            name = m.group(1)
            if name.startswith("cu"):
                defs.add(name)
    return defs


def fmt(items: Iterable[str], cap: int = 20) -> str:
    items = sorted(set(items))
    if len(items) <= cap:
        return ", ".join(items) or "(none)"
    return f"{', '.join(items[:cap])}, ... (+{len(items) - cap} more)"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--allow-r6", action="append", default=[],
        help="allowlist a symbol from R6 missing-wrapper check (repeatable); "
             "use when a dispatch-table entry is intentionally forward-only "
             "with no corresponding ELF export")
    ap.add_argument("--root", default=None,
                    help="library tree to check (defaults to this script's "
                         "parent; pass ../library-remote to check that one)")
    args = ap.parse_args()
    if args.root:
        set_root(Path(args.root).resolve())
    r6_allowlist = set(args.allow_r6)

    loader_entries = extract_library_entry(read_text(LOADER_FILE))
    enum_entries = extract_enum_entries(read_text(HELPER_HEADER))
    hooks_entries = extract_hooks_entry(read_text(HOOK_FILE))

    errors: list[str] = []
    warnings: list[str] = []

    # R1. Count match
    if len(loader_entries) != len(enum_entries):
        errors.append(
            f"[R1] count mismatch: loader={len(loader_entries)}, enum={len(enum_entries)}"
        )

    # R2. Order match pair-wise (dispatch via enum is by position)
    first_mismatch = None
    for i, (ld, en) in enumerate(zip(loader_entries, enum_entries)):
        if ld != en:
            first_mismatch = (i, ld, en)
            break
    if first_mismatch:
        i, ld, en = first_mismatch
        errors.append(
            f"[R2] order mismatch at index {i}: loader={ld!r} enum={en!r} "
            f"(dispatch table is indexed by enum position - any reorder "
            f"mis-routes every subsequent symbol)"
        )

    # R3. Duplicates
    for label, items in (("loader", loader_entries), ("enum", enum_entries),
                        ("hooks", hooks_entries)):
        dups = [n for n, c in Counter(items).items() if c > 1]
        if dups:
            errors.append(f"[R3] duplicate names in {label}[]: {fmt(dups)}")

    # R4. ABI-conflict families: both ABIs registered in dispatch table
    loader_set = set(loader_entries)
    for base, (old_sym, new_sym) in ABI_CONFLICT_FAMILIES.items():
        missing = [s for s in (old_sym, new_sym) if s not in loader_set]
        if missing:
            errors.append(
                f"[R4] ABI-conflict family '{base}' missing versioned symbol(s) "
                f"in cuda_library_entry[]: {fmt(missing)}"
            )

    # R5. Conflict-family BASE names must not appear in cuda_hooks_entry[]
    # except for explicitly allowlisted ones (see R5_ALLOWLIST comment).
    for base in ABI_CONFLICT_FAMILIES:
        if base in hooks_entries and base not in R5_ALLOWLIST:
            errors.append(
                f"[R5] ABI-conflict base name '{base}' is registered in "
                f"cuda_hooks_entry[] - cuGetProcAddress substitution will "
                f"bind callers to the WRONG ABI. Register the versioned "
                f"names separately instead."
            )

    # R6. Every loader entry must have a wrapper function (or be explicitly
    # allowlisted via --allow-r6 for forward-only entries).
    wrapper_defs = extract_wrapper_defs(WRAPPER_FILES)
    missing_wrappers = (set(loader_entries) - wrapper_defs) - r6_allowlist
    if missing_wrappers:
        errors.append(
            f"[R6] loader entries with no wrapper function in "
            f"cuda_originals.c / cuda_hook.c: {fmt(missing_wrappers)}. "
            f"Add a wrapper or pass --allow-r6 <symbol> if the entry is "
            f"intentionally forward-only."
        )

    # R7. Multi-version base audit. Every base name with 2+ versioned
    # siblings in the dispatch table must be explicitly classified as
    # either an ABI-conflict family (handled by R4/R5) or a
    # signature-stable family (in R7_ALLOWLIST). Catches the case where
    # a future commit adds a new cuFooBar_v3 hook without the maintainer
    # noticing that the C parameter list changed between _v2 and _v3,
    # which would let cuGetProcAddress substitute the wrong-ABI wrapper
    # and produce the LD_PRELOAD frame-mismatch hang documented in
    # Issue #1834.
    #
    # The unversioned name counts as a version of its own. It is an ELF symbol
    # with an ABI like any other, and requiring two _vN siblings before asking
    # about a family left the commonest shape - plain plus _v2 - unexamined
    # entirely. cuGraphKernelNodeGetParams was that shape, and both of its
    # wrappers declared the newer struct while one of them was the plain
    # symbol, so the driver wrote past a caller's frame. R7 never saw it.
    multi_ver_bases: dict[str, list[int]] = {}
    ver_re = re.compile(r"^(cu[A-Z][A-Za-z0-9_]*)_v(\d+)$")
    loader_set_all = set(loader_entries)
    for entry in loader_entries:
        m = ver_re.match(entry)
        if m:
            base = m.group(1)
            ver = int(m.group(2))
            multi_ver_bases.setdefault(base, []).append(ver)
    for base, vers in multi_ver_bases.items():
        if base in loader_set_all:
            vers.append(1)
    multi_ver = {b: sorted(v) for b, v in multi_ver_bases.items() if len(v) >= 2}
    classified = (set(ABI_CONFLICT_FAMILIES) | set(R7_ALLOWLIST)
                  | set(MODERN_ONLY_FAMILIES) | set(UNDECLARED_FAMILIES))
    unclassified = {b: vs for b, vs in multi_ver.items() if b not in classified}
    if unclassified:
        lines = []
        for base, vers in sorted(unclassified.items()):
            ver_str = ", ".join(f"_v{v}" for v in vers)
            lines.append(f"           - {base}  ({ver_str})")
        errors.append(
            f"[R7] {len(unclassified)} base name(s) have multiple _vN "
            + "\n".join(lines)
            + "\n         Each must go into ONE of:\n"
            "           ABI_CONFLICT_FAMILIES  (parameter lists DIFFER between\n"
            "                                   versions, and each exported\n"
            "                                   wrapper binds its own entry)\n"
            "           R7_ALLOWLIST           (every version takes the SAME\n"
            "                                   parameter list)\n"
            "           MODERN_ONLY_FAMILIES   (they differ upstream, and this\n"
            "                                   library implements only the\n"
            "                                   newer ABI under both names)\n"
            "           UNDECLARED_FAMILIES    (no public header declares both\n"
            "                                   forms, so it cannot be said)\n"
            "         hack/check_abi_signatures.py decides which, from a real\n"
            "         <cuda.h>. Do not classify one from the comments."
        )

    # Report
    for w in warnings:
        print(w)
    for e in errors:
        print(e)

    print()
    if errors:
        print(f"[FAIL] CUDA hook consistency check: {len(errors)} error(s), "
              f"{len(warnings)} warning(s).")
        return 1
    print(f"[PASS] CUDA hook consistency check: "
          f"{len(loader_entries)} library entries, {len(enum_entries)} enum entries, "
          f"{len(hooks_entries)} hook entries, {len(wrapper_defs)} wrappers, "
          f"{len(warnings)} warning(s).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
