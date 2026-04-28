#!/usr/bin/env python3
"""
Compare struct layouts declared in library/include/cuda-subset.h against
the real CUDA headers (<cuda.h>) on the build host.

Goal: catch silent ABI drift where NVIDIA extends a struct in a new CUDA
release but our vendored subset still uses the old layout. Any wrapper
function whose signature mentions that struct would then pass a wrong-sized
struct across the FFI boundary at runtime.

Strategy:
  1. Parse cuda-subset.h for `typedef struct NAME_st { ... } NAME_v1;`
     (plus the `typedef NAME_v1 NAME;` alias) pattern and collect the public
     type names.
  2. For each candidate, run a tiny test-compile against <cuda.h> to see if
     the type exists in the real SDK (some of our structs are internal).
  3. Build two "probe" programs - one using cuda-subset.h, one using
     <cuda.h> - that each print `struct <NAME> size=<N>` for all surviving
     structs.
  4. Run both, diff the lines. Any size mismatch is an error; anything only
     in ours is informational (our subset has something upstream does not).
  5. Also run explicit offsetof checks for the ABI-conflict families, where
     field order matters for calling convention.

Exit 0 if all layouts agree; exit 1 on any size or offset mismatch.

Usage:
  python3 hack/check_struct_layout.py [--cuda-home /usr/local/cuda]

Environment:
  CUDA_HOME   fallback include path for <cuda.h>

If <cuda.h> is not found, the script exits 0 with a skip message so build
hosts without a CUDA toolkit are not blocked.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SUBSET_HEADER = ROOT / "include/cuda-subset.h"

# Hand-curated offsetof audits for ABI-conflict families. These are the
# structs whose field ORDER matters for CUDA 11/12 <-> CUDA 13 calling
# convention, so a size-only check is not enough.
ABI_CONFLICT_OFFSET_CHECKS = [
    ("CUctxCreateParams", ["execAffinityParams", "numExecAffinityParams", "cigParams"]),
    ("CUmemLocation",     ["type", "id"]),
    ("CUgraphEdgeData",   ["from_port", "reserved", "to_port", "type"]),
]


@dataclass
class StructDecl:
    public: str          # e.g. CUctxCreateParams
    tag: str             # e.g. CUctxCreateParams_st


def strip_c_comments(text: str) -> str:
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    text = re.sub(r"//[^\n]*", "", text)
    return text


def collect_subset_structs(subset_text: str) -> list[StructDecl]:
    """
    Find every `typedef struct NAME_st { ... } VNAME;` declaration, then
    resolve the public alias if a subsequent `typedef VNAME PUBNAME;` exists.
    """
    text = strip_c_comments(subset_text)

    # typedef struct <TAG> { ... } <VERSIONED_NAME>;
    struct_pat = re.compile(
        r"typedef\s+struct\s+([A-Za-z_][A-Za-z0-9_]*?)\s*\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}\s*"
        r"([A-Za-z_][A-Za-z0-9_]*)\s*;",
        re.S,
    )
    alias_pat = re.compile(
        r"typedef\s+([A-Za-z_][A-Za-z0-9_]*_v[0-9]+)\s+([A-Za-z_][A-Za-z0-9_]*)\s*;"
    )
    aliases: dict[str, str] = {}
    for m in alias_pat.finditer(text):
        aliases[m.group(1)] = m.group(2)

    out: list[StructDecl] = []
    seen: set[str] = set()
    for m in struct_pat.finditer(text):
        tag = m.group(1)
        versioned = m.group(2)
        public = aliases.get(versioned, versioned)
        if public in seen:
            continue
        seen.add(public)
        out.append(StructDecl(public=public, tag=tag))
    return out


def locate_cuda_h(cuda_home: str | None) -> Path | None:
    candidates: list[Path] = []
    if cuda_home:
        candidates.append(Path(cuda_home) / "include" / "cuda.h")
    env = os.environ.get("CUDA_HOME")
    if env:
        candidates.append(Path(env) / "include" / "cuda.h")
    candidates += [
        Path("/usr/local/cuda/include/cuda.h"),
        Path("/usr/include/cuda.h"),
    ]
    for p in candidates:
        if p.is_file():
            return p
    return None


def run_gcc(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(["gcc"] + args, capture_output=True, text=True)


def build_exe(workdir: Path, src_name: str, src_text: str,
              include_dirs: list[Path]) -> tuple[Path | None, str]:
    src_path = workdir / src_name
    src_path.write_text(src_text, encoding="utf-8")
    exe_path = workdir / (src_name + ".exe")
    args = ["-std=c11", "-w"]
    for inc in include_dirs:
        args += ["-I", str(inc)]
    args += [str(src_path), "-o", str(exe_path)]
    r = run_gcc(args)
    if r.returncode != 0:
        return None, r.stderr
    return exe_path, ""


def filter_real_structs(workdir: Path, cuda_include: Path,
                        candidates: list[StructDecl]) -> list[StructDecl]:
    """Per-struct test-compile to decide which exist in <cuda.h>."""
    survivors: list[StructDecl] = []
    for sd in candidates:
        src = (
            "#include <cuda.h>\n"
            "#include <stddef.h>\n"
            f"static char _probe[sizeof({sd.public})];\n"
            "int main(void) { return (int)sizeof(_probe); }\n"
        )
        exe, _ = build_exe(workdir, f"probe_{sd.public}.c", src, [cuda_include])
        if exe is not None:
            survivors.append(sd)
            try:
                exe.unlink()
            except OSError:
                pass
        try:
            (workdir / f"probe_{sd.public}.c").unlink()
        except OSError:
            pass
    return survivors


def build_size_probe(header_include: str, structs: list[StructDecl]) -> str:
    lines = [
        f"#include {header_include}",
        "#include <stdio.h>",
        "#include <stddef.h>",
        "int main(void) {",
    ]
    for sd in structs:
        lines.append(
            f'  printf("struct {sd.public} size=%zu\\n", sizeof({sd.public}));'
        )
    lines.append("  return 0;")
    lines.append("}")
    return "\n".join(lines)


def build_offset_probe(header_include: str,
                       items: list[tuple[str, list[str]]]) -> str:
    lines = [
        f"#include {header_include}",
        "#include <stdio.h>",
        "#include <stddef.h>",
        "int main(void) {",
    ]
    for name, fields in items:
        for f in fields:
            lines.append(
                f'  printf("offset {name}.{f}=%zu\\n", '
                f'(size_t)offsetof({name}, {f}));'
            )
    lines.append("  return 0;")
    lines.append("}")
    return "\n".join(lines)


def diff_lines(a: list[str], b: list[str]) -> list[tuple[str, str, str]]:
    """Return list of (key, our_line, real_line) for mismatches."""
    def to_dict(lines):
        d: dict[str, str] = {}
        for l in lines:
            # "struct NAME size=N" -> key "struct NAME", val "size=N"
            m = re.match(r"(struct \S+|offset \S+)\s*(.*)$", l.strip())
            if m:
                d[m.group(1)] = m.group(2)
        return d

    our = to_dict(a)
    real = to_dict(b)
    mismatches: list[tuple[str, str, str]] = []
    for k in sorted(set(our) & set(real)):
        if our[k] != real[k]:
            mismatches.append((k, our[k], real[k]))
    return mismatches


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--cuda-home", default=None,
                    help="path to CUDA toolkit (defaults to env CUDA_HOME or "
                         "/usr/local/cuda)")
    args = ap.parse_args()

    cuda_h = locate_cuda_h(args.cuda_home)
    if cuda_h is None:
        print("[SKIP] <cuda.h> not found - set CUDA_HOME or --cuda-home to "
              "enable struct layout validation")
        return 0
    cuda_include = cuda_h.parent

    if shutil.which("gcc") is None:
        print("[SKIP] gcc not found in PATH - layout validation disabled")
        return 0

    subset_text = SUBSET_HEADER.read_text(encoding="utf-8")
    candidates = collect_subset_structs(subset_text)
    if not candidates:
        print("[FAIL] could not extract any struct declaration from "
              f"{SUBSET_HEADER}")
        return 1
    print(f"[INFO] parsed {len(candidates)} struct decls from {SUBSET_HEADER.name}")
    print(f"[INFO] using real <cuda.h> at {cuda_h}")

    errors: list[str] = []

    with tempfile.TemporaryDirectory(prefix="vgpu_layout_") as tmp:
        tmp_path = Path(tmp)

        # Phase 1: filter to structs present on both sides.
        shared = filter_real_structs(tmp_path, cuda_include, candidates)
        only_ours = [c.public for c in candidates if c not in shared]
        print(f"[INFO] {len(shared)} structs present in both headers "
              f"({len(only_ours)} only in our subset)")

        # Phase 2: compare sizes.
        our_probe = build_size_probe('"include/cuda-subset.h"', shared)
        real_probe = build_size_probe("<cuda.h>", shared)

        our_exe, err = build_exe(tmp_path, "our_size.c", our_probe, [ROOT])
        if our_exe is None:
            errors.append(f"[SIZE] our-side probe failed to compile:\n{err}")
        real_exe, err = build_exe(tmp_path, "real_size.c", real_probe, [cuda_include])
        if real_exe is None:
            errors.append(f"[SIZE] real-side probe failed to compile:\n{err}")

        if our_exe and real_exe:
            our_out = subprocess.run([str(our_exe)], capture_output=True, text=True).stdout.splitlines()
            real_out = subprocess.run([str(real_exe)], capture_output=True, text=True).stdout.splitlines()
            size_mismatches = diff_lines(our_out, real_out)
            if size_mismatches:
                for k, a, b in size_mismatches:
                    errors.append(f"[SIZE] {k}: ours={a} real={b}")
            else:
                print(f"[INFO] sizeof check: all {len(shared)} struct sizes match")

        # Phase 3: ABI-conflict offsetof audits.
        offset_items = [(n, fs) for n, fs in ABI_CONFLICT_OFFSET_CHECKS
                        if any(s.public == n for s in shared)]
        if offset_items:
            our_probe = build_offset_probe('"include/cuda-subset.h"', offset_items)
            real_probe = build_offset_probe("<cuda.h>", offset_items)
            our_exe, err = build_exe(tmp_path, "our_off.c", our_probe, [ROOT])
            if our_exe is None:
                errors.append(f"[OFFSET] our-side probe failed:\n{err}")
            real_exe, err = build_exe(tmp_path, "real_off.c", real_probe, [cuda_include])
            if real_exe is None:
                errors.append(f"[OFFSET] real-side probe failed:\n{err}")
            if our_exe and real_exe:
                our_out = subprocess.run([str(our_exe)], capture_output=True, text=True).stdout.splitlines()
                real_out = subprocess.run([str(real_exe)], capture_output=True, text=True).stdout.splitlines()
                offset_mismatches = diff_lines(our_out, real_out)
                if offset_mismatches:
                    for k, a, b in offset_mismatches:
                        errors.append(f"[OFFSET] {k}: ours={a} real={b}")
                else:
                    print(f"[INFO] ABI-conflict offsetof check: all offsets match "
                          f"across {len(offset_items)} structs")

    print()
    if only_ours:
        print(f"[INFO] {len(only_ours)} struct(s) declared only in our subset "
              f"(not validated): {', '.join(sorted(only_ours)[:10])}"
              f"{'...' if len(only_ours) > 10 else ''}")

    if errors:
        print()
        for e in errors:
            print(e)
        print(f"\n[FAIL] struct layout check: {len(errors)} mismatch(es)")
        return 1

    print("[PASS] struct layout check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
