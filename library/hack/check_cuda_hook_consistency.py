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
  R6. (WARN) Every (1) entry should have a wrapper function definition.

Exit 0 if all pass; exit 1 on any R1-R5 violation.
"""

from __future__ import annotations

import re
import sys
from collections import Counter
from pathlib import Path
from typing import Iterable

ROOT = Path(__file__).resolve().parents[1]

LOADER_FILE = ROOT / "src/loader.c"
HELPER_HEADER = ROOT / "include/cuda-helper.h"
HOOK_FILE = ROOT / "src/cuda_hook.c"
WRAPPER_FILES = [
    ROOT / "src/cuda_originals.c",
    ROOT / "src/cuda_hook.c",
]

# ABI-conflict families: base name -> (old_abi_symbol, new_abi_symbol).
# Must stay in sync with is_abi_conflict_base() in include/cuda-helper.h.
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
    for base in ABI_CONFLICT_FAMILIES:
        if base in hooks_entries:
            errors.append(
                f"[R5] ABI-conflict base name '{base}' is registered in "
                f"cuda_hooks_entry[] - cuGetProcAddress substitution will "
                f"bind callers to the WRONG ABI. Register the versioned "
                f"names separately instead."
            )

    # R6. (warn) Each loader entry ideally has a wrapper
    wrapper_defs = extract_wrapper_defs(WRAPPER_FILES)
    missing_wrappers = set(loader_entries) - wrapper_defs
    if missing_wrappers:
        warnings.append(
            f"[R6 warn] loader entries with no wrapper function in "
            f"cuda_originals.c / cuda_hook.c: {fmt(missing_wrappers)}"
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
