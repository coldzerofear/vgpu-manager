#!/usr/bin/env python3
"""
Classify every versioned CUDA API family against a real <cuda.h>, and check
that classification against the tables this repository keeps by hand.

The problem it answers: for a base name that exists in cuda_library_entry[]
both plain and as _vN, does the unversioned ELF SYMBOL take the same C
parameter list as the versioned one, or a different one? Everything about how
a wrapper may be written follows from that answer - a family whose two ABIs
differ must never share a dispatcher, must never fall back across versions,
and must never be hooked under its base name (rules 1-3 in cuda-helper.h).

ELF symbols, not what source code means by a name: those differ. cuda.h has
a block of `#define cuFoo cuFoo_vN` at the top that redirects calls, and the
body then declares the prototype under the PLAIN name - so that prototype
describes whatever the macro points at, not the plain ELF symbol. The
original signatures live in the trailing `#if defined(__CUDA_API_VERSION_
INTERNAL)` block, which is authoritative for the ELF symbols because it is
written after the macros are #undef'd.

The redirection is not always to a _vN sibling either: cuGraphInstantiate
redirects to cuGraphInstantiateWithFlags, a different function, while the
ELF cuGraphInstantiate and cuGraphInstantiate_v2 take the same five
arguments as each other. Reading the body prototype as "the modern form of
the base symbol" calls that family a conflict when it is not.

  SAME       both ABIs take the same parameter list. Sharing one wrapper is
             a semantic choice, not an ABI hazard. Belongs in R7_ALLOWLIST.
  DIFFERS    the ABIs are incompatible. Belongs in ABI_CONFLICT_FAMILIES,
             and rules 1-3 apply.
  UNRESOLVED cuda.h alone does not declare both forms - the GL and VDPAU
             entry points live in cudaGL.h / cudaVDPAU.h. Reported, never
             guessed.

Usage:
  python3 hack/check_abi_signatures.py [--cuda-home /usr/local/cuda] [--strict]

A table classification the header contradicts is always an error: one of the
two is wrong and only a person can say which. A family the header says has
two ABIs but that no table classifies is a warning, and --strict turns those
into errors too - which is what a CI job wants once the backlog is empty.

--check-wrappers additionally compares what each exported wrapper DECLARES
against the ABI of the ELF symbol it is named after. That is the check which
would have caught the kernel-node bug on its own, without anyone having to
classify the family first.

Reading its output on this library today: most of what it reports is the
CUDA 3.2 -> 4.0 widening, where a wrapper for the unversioned name declares
the modern 64-bit form while the plain ELF symbol is still the 32-bit one
(cuMemAlloc, the cuMemcpy and cuMemset families, cuArray*, cuTexRef*). That
is the same shape as the kernel-node bug and it is not a false report - but
the only callers it could affect build against CUDA 3.x headers, and the
wrappers were written for the callers that exist. Changing them is a
judgement about who may dlsym a plain name, not a mechanical fix, so they are
reported and left alone.

If <cuda.h> is not found the script exits 0 with a skip message, so hosts
without a CUDA toolkit are not blocked.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

sys.path.insert(0, str(Path(__file__).resolve().parent))
from check_cuda_hook_consistency import (  # noqa: E402
    ABI_CONFLICT_FAMILIES,
    BIND_ALLOWLIST,
    MODERN_ONLY_FAMILIES,
    R7_ALLOWLIST,
    UNDECLARED_FAMILIES,
)

# Declarations may carry an attribute macro first: cudaGL.h writes the
# deprecated GL entry points as `__CUDA_DEPRECATED CUresult CUDAAPI cuGL...`.
# Requiring CUresult at the start of the line silently drops those, which left
# cuGLCtxCreate looking unclassifiable when the header states it plainly.
_DECL_PREFIX = r"(?:__CUDA_DEPRECATED\s+|extern\s+|CUDAAPI\s+)*"
PROTO_RE = re.compile(r"^\s*" + _DECL_PREFIX + r"CUresult\s+CUDAAPI\s+(\w+)\((.*?)\)\s*;", re.S)
DECL_START_RE = re.compile(r"^" + _DECL_PREFIX + r"CUresult\s+CUDAAPI\s+\w+\(")
REDIRECT_RE = re.compile(r"^\s*#define\s+(cu[A-Za-z0-9_]+)\s+(cu[A-Za-z0-9_]+)\s*$")


# cuda.h does not declare the OpenGL or VDPAU entry points; those live beside
# it in their own headers, with the same shape (a redirect block at the top, a
# trailing __CUDA_API_VERSION_INTERNAL block with the original prototypes). The
# library exports wrappers for them, so they are read too when present.
COMPANION_HEADERS = ("cudaGL.h", "cudaVDPAU.h")


def locate_cuda_h(cuda_home: str | None) -> Path | None:
    candidates: list[Path] = []
    if cuda_home:
        candidates.append(Path(cuda_home) / "include" / "cuda.h")
    env = os.environ.get("CUDA_HOME")
    if env:
        candidates.append(Path(env) / "include" / "cuda.h")
    candidates += [Path("/usr/local/cuda/include/cuda.h"), Path("/usr/include/cuda.h")]
    for p in candidates:
        if p.is_file():
            return p
    return None


TYPEDEF_ALIAS_RE = re.compile(r"^\s*typedef\s+(\w+)\s+(\w+)\s*;")


def alias_roots(*texts: str) -> dict[str, str]:
    """Canonical name for each `typedef A B;` alias.

    Needed because a textual comparison reads an alias as a difference:
    CUDA_KERNEL_NODE_PARAMS *is* CUDA_KERNEL_NODE_PARAMS_v2, and a wrapper
    naming one while the header names the other is the same ABI, not a
    mismatch. CUdeviceptr_v1 is a different type from CUdeviceptr and stays
    different - that one is a real 32-bit against 64-bit break.
    """
    direct: dict[str, str] = {}
    for text in texts:
        for line in text.splitlines():
            m = TYPEDEF_ALIAS_RE.match(line)
            if m:
                direct[m.group(2)] = m.group(1)
    roots: dict[str, str] = {}
    for name in direct:
        seen, cur = {name}, name
        while cur in direct and direct[cur] not in seen:
            cur = direct[cur]
            seen.add(cur)
        roots[name] = cur
    return roots


def canonical(types: list[str], roots: dict[str, str]) -> list[str]:
    """Rewrite each type to its alias root, keeping const and * as written."""
    out = []
    for t in types:
        out.append(re.sub(r"\b(\w+)\b", lambda m: roots.get(m.group(1), m.group(1)), t))
    return out


def normalise_params(params: str) -> list[str]:
    """Parameter TYPES only, with the names and spacing taken out, so two
    declarations of the same ABI compare equal."""
    out: list[str] = []
    for p in params.split(","):
        p = p.strip()
        if not p or p == "void":
            continue
        p = re.sub(r"\b\w+\s*(\[\s*\])?$", "", p).strip()  # drop the parameter name
        out.append(re.sub(r"\s+", " ", p).replace(" *", "*"))
    return out


def collect_prototypes(lines: list[str]) -> dict[str, list[str]]:
    """name -> parameter type list, joining declarations that wrap."""
    out: dict[str, list[str]] = {}
    buf = ""
    for line in lines:
        s = line.strip()
        if not buf and not DECL_START_RE.match(s):
            continue
        buf = f"{buf} {s}" if buf else s
        if ";" not in buf:
            continue
        m = PROTO_RE.match(buf)
        buf = ""
        if m:
            out[m.group(1)] = normalise_params(m.group(2))
    return out


def internal_block_start(lines: list[str]) -> int:
    """Index of the trailing __CUDA_API_VERSION_INTERNAL block.

    The token also appears near the top, guarding the renaming macros, so take
    the last occurrence that opens a plain `#if defined(...)` line.
    """
    found = -1
    for i, line in enumerate(lines):
        if line.startswith("#if defined(__CUDA_API_VERSION_INTERNAL)"):
            found = i
    return found


def strip_c_comments(text: str) -> str:
    """Drop /* */ and // so a commented-out table entry is not read as live.

    cuda_library_entry[] keeps retired symbols commented in place
    (cuStreamBeginCapture and friends). Matching the raw text counts them as
    registered, which invents families the library does not export.
    """
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    return re.sub(r"//[^\n]*", "", text)


def versioned_families(loader_text: str) -> dict[str, list[str]]:
    """base -> its _vN siblings, for bases cuda_library_entry[] registers both
    ways. The table is the right source: these are the ELF symbols the library
    actually exports, and most _vN names are never declared in cuda.h at all -
    they exist only as the target of a `#define`."""
    names = re.findall(r'\{\.name = "(\w+)"\}', strip_c_comments(loader_text))
    present = set(names)
    fams: dict[str, list[str]] = {}
    for n in names:
        m = re.match(r"^(cu[A-Z]\w*?)_v\d+$", n)
        if m and m.group(1) in present:
            fams.setdefault(m.group(1), []).append(n)
    return {b: sorted(v) for b, v in sorted(fams.items())}


def elf_signature(name: str, base: str, body: dict, legacy: dict,
                  redirects: dict[str, str]) -> list[str] | None:
    """Parameter list of the ELF symbol `name`, or None if the header does not
    pin it down.

    An explicit declaration always wins. Failing that, the body's prototype
    under the base name describes whatever the macro redirects the base to -
    useful for exactly one name, the redirect target.
    """
    if name in legacy:
        return legacy[name]
    if name in body and redirects.get(name) in (None, name):
        return body[name]
    if redirects.get(base) == name and base in body:
        return body[base]
    return None


def classify(base: str, siblings: list[str], body: dict, legacy: dict,
             redirects: dict[str, str]) -> tuple[str, str]:
    """(verdict, detail) for one family: the base ELF symbol against each of
    its _vN siblings. One differing pair makes the family a conflict, since
    that is enough for a shared wrapper to misread a frame."""
    base_sig = elf_signature(base, base, body, legacy, redirects)
    if base_sig is None:
        return "UNRESOLVED", "no prototype pins down the plain ELF symbol"

    for v in siblings:
        vsig = elf_signature(v, base, body, legacy, redirects)
        if vsig is None:
            return "UNRESOLVED", f"no prototype pins down {v}"
        if vsig != base_sig:
            return "DIFFERS", (f"{base} ({', '.join(base_sig)}) vs "
                               f"{v} ({', '.join(vsig)})")
    return "SAME", f"({', '.join(base_sig)}) for {base} and {', '.join(siblings)}"



WRAPPER_FILES = ("src/cuda_originals.c", "src/cuda_hook.c")


def collect_wrapper_signatures() -> dict[str, list[str]]:
    """Parameter types of every CUresult wrapper this library DEFINES.

    Scanned per top-level function block, because a regex that spans the file
    happily pairs one function's name with another's parameter list.
    """
    out: dict[str, list[str]] = {}
    for rel in WRAPPER_FILES:
        path = ROOT / rel
        if not path.is_file():
            continue
        lines = path.read_text(errors="replace").splitlines()
        i = 0
        while i < len(lines):
            m = re.match(r"^CUresult (\w+)\(", lines[i])
            if not m:
                i += 1
                continue
            name, buf, j = m.group(1), "", i
            while j < len(lines) and "{" not in buf:
                buf += " " + lines[j].strip()
                j += 1
            sig = re.match(r"^\s*CUresult +\w+\((.*?)\)\s*\{", buf)
            if sig:
                out[name] = normalise_params(sig.group(1))
            while j < len(lines) and lines[j] != "}":
                j += 1
            i = j + 1
    return out


def check_wrapper_bindings(families: dict[str, list[str]], body: dict, legacy: dict,
                           redirects: dict[str, str],
                           roots: dict[str, str]) -> tuple[list[str], list[str]]:
    """Every exported wrapper must declare the parameters of the ELF symbol it
    is named after.

    This is the check that would have caught the kernel-node bug directly: both
    wrappers there declared the _v2 struct while one of them was the plain
    symbol, so a caller of the older ABI was handed a frame 16 bytes longer
    than the one it had built.
    """
    wrappers = collect_wrapper_signatures()
    errors: list[str] = []
    expected: list[str] = []
    skipped: list[str] = []
    for base, siblings in families.items():
        for name in (base, *siblings):
            have = wrappers.get(name)
            want = elf_signature(name, base, body, legacy, redirects)
            if have is None or want is None:
                skipped.append(name)
                continue
            if canonical(have, roots) == canonical(want, roots):
                continue
            line = (f"wrapper {name} declares ({', '.join(have)}) but the "
                    f"ELF symbol takes ({', '.join(want)})")
            # A modern-only family is the deliberate version of this: the plain
            # wrapper carries the newer ABI on purpose. Only the plain name is
            # expected to differ - a _vN wrapper that drifts is still a bug.
            if name in BIND_ALLOWLIST:
                expected.append(line)
            elif base in MODERN_ONLY_FAMILIES and name == base:
                expected.append(line)
            else:
                errors.append(f"[BIND] {line}")
    return errors, expected, skipped



# Seeing through __CUDA_API_PTDS/PTSZ(), which wrap the symbol name in the
# call rather than being part of it.
ENTRY_CALL_RE = re.compile(
    r"CUDA_(?:FIND_ENTRY|ENTRY_CHECK\w*|INTERNAL_\w+)\(\s*cuda_library_entry,\s*"
    r"(?:__CUDA_API_PT[DS][SZ]?\(\s*)?(\w+)")
VERSION_SUFFIX_RE = re.compile(r"_v(\d+)(?:_pt[ds][sz])?$")


def symbol_version(name: str) -> int:
    """1 for an unversioned name, N for _vN, ignoring any _ptsz/_ptds tail."""
    m = VERSION_SUFFIX_RE.search(name)
    return int(m.group(1)) if m else 1


def wrapper_body(name: str, source: str) -> str | None:
    m = re.search(rf"^CUresult {re.escape(name)}\([^;{{]*\)\s*\{{(.*?)^\}}",
                  source, re.S | re.M)
    return m.group(1) if m else None


def check_dispatch_targets(families: dict[str, list[str]], body: dict, legacy: dict,
                           redirects: dict[str, str],
                           roots: dict[str, str]) -> tuple[list[str], list[str]]:
    """In an ABI-conflict family, a wrapper must reach the entry of its own name.

    Comparing declarations does not cover this. Both wrappers can declare
    exactly the right parameters and still call each other's entry, which is
    what cuStreamBeginCapture did: the one-argument wrapper called the
    two-argument entry and vice versa, so one caller's capture mode was
    dropped and the other's was read from a register nobody set. Every
    signature check passed.

    Only ABI-conflict families are held to this, and within one only a target
    whose own parameters DIFFER from the wrapper's counts. cuCtxCreate calling
    cuCtxCreate_v2 is not a crossing worth reporting: cuda.h gives those two
    the same three arguments, and the family is a conflict because of _v3 and
    _v4. What matters is reaching an entry that would read the frame
    differently, not reaching one with another number in its name.
    """
    source = "\n".join((ROOT / rel).read_text(errors="replace")
                        for rel in WRAPPER_FILES if (ROOT / rel).is_file())
    errors: list[str] = []
    skipped: list[str] = []
    for base, siblings in families.items():
        if base not in ABI_CONFLICT_FAMILIES:
            continue
        for name in (base, *siblings):
            body = wrapper_body(name, source)
            if body is None:
                skipped.append(name)
                continue
            trampoline = re.search(r"return (_cu\w+)\(", body)
            if trampoline:
                inner = wrapper_body(trampoline.group(1), source)
                if inner is not None:
                    body = inner
            targets = ENTRY_CALL_RE.findall(body)
            if not targets:
                skipped.append(name)
                continue
            target = targets[0]
            if symbol_version(target) == symbol_version(name):
                continue
            mine = elf_signature(name, base, body, legacy, redirects)
            theirs = elf_signature(target, base, body, legacy, redirects)
            if mine is None or theirs is None:
                skipped.append(name)
                continue
            if canonical(mine, roots) != canonical(theirs, roots):
                errors.append(
                    f"[DISPATCH] wrapper {name} ({', '.join(mine)}) calls "
                    f"{target} ({', '.join(theirs)})")
    return errors, skipped


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--cuda-home", default=None)
    ap.add_argument("--check-wrappers", action="store_true",
                    help="also check that each exported wrapper declares the "
                         "parameters of the ELF symbol it is named after")
    ap.add_argument("--strict", action="store_true",
                    help="fail on a classification that contradicts the header, "
                         "or a DIFFERS family that is not classified at all")
    args = ap.parse_args()

    cuda_h = locate_cuda_h(args.cuda_home)
    if cuda_h is None:
        print("[SKIP] <cuda.h> not found - set CUDA_HOME or --cuda-home to "
              "enable ABI signature classification")
        return 0

    body: dict[str, list[str]] = {}
    legacy: dict[str, list[str]] = {}
    redirects: dict[str, str] = {}
    header_texts: list[str] = []
    read: list[str] = []
    for header in (cuda_h, *(cuda_h.parent / n for n in COMPANION_HEADERS)):
        if not header.is_file():
            continue
        text = header.read_text(errors="replace")
        lines = text.splitlines()
        split_at = internal_block_start(lines)
        if split_at < 0:
            if header == cuda_h:
                print(f"[FAIL] no __CUDA_API_VERSION_INTERNAL block in {cuda_h}")
                return 1
            continue
        header_texts.append(text)
        read.append(header.name)
        body.update(collect_prototypes(lines[:split_at]))
        legacy.update(collect_prototypes(lines[split_at:]))
        redirects.update({m.group(1): m.group(2)
                          for line in lines[:split_at] if (m := REDIRECT_RE.match(line))})
    print(f"[INFO] using {', '.join(read)} from {cuda_h.parent} ({len(body)} current, "
          f"{len(legacy)} historical prototypes, {len(redirects)} redirects)")

    families = versioned_families((ROOT / "src/loader.c").read_text())
    buckets: dict[str, list[tuple[str, str]]] = {"SAME": [], "DIFFERS": [], "UNRESOLVED": []}
    for base, siblings in families.items():
        verdict, detail = classify(base, siblings, body, legacy, redirects)
        buckets[verdict].append((base, detail))

    for verdict in ("SAME", "DIFFERS", "UNRESOLVED"):
        print(f"[INFO] {verdict}: {len(buckets[verdict])} family(ies)")

    errors: list[str] = []
    warnings: list[str] = []

    for base, detail in buckets["SAME"]:
        if base in ABI_CONFLICT_FAMILIES:
            errors.append(f"[ABI] {base} is in ABI_CONFLICT_FAMILIES, but both ABIs "
                          f"take the same parameters {detail}")
        elif base not in R7_ALLOWLIST:
            # Still unclassified as far as R7 is concerned: the point of that
            # rule is that every multi-version base is classified either way,
            # so that a later _vN addition cannot slip in unexamined.
            warnings.append(f"[ABI] {base} takes the same parameters across its "
                            f"versions but is in neither table: {detail}")
    for base, detail in buckets["DIFFERS"]:
        if base in R7_ALLOWLIST:
            errors.append(f"[ABI] {base} is in R7_ALLOWLIST as signature-stable, but "
                          f"the ABIs differ: {detail}")
        elif base not in ABI_CONFLICT_FAMILIES and base not in MODERN_ONLY_FAMILIES:
            warnings.append(f"[ABI] {base} has two incompatible ABIs and is in no "
                            f"table: {detail}")
    for base, detail in buckets["UNRESOLVED"]:
        if (base not in ABI_CONFLICT_FAMILIES and base not in R7_ALLOWLIST
                and base not in MODERN_ONLY_FAMILIES
                and base not in UNDECLARED_FAMILIES):
            warnings.append(f"[ABI] {base} could not be classified from this header "
                            f"and is in no table: {detail}")

    if args.check_wrappers:
        roots = alias_roots(*header_texts,
                            (ROOT / "include/cuda-subset.h").read_text())
        bind_errors, bind_expected, bind_skipped = check_wrapper_bindings(
            families, body, legacy, redirects, roots)
        print(f"[INFO] wrapper binding: {len(bind_errors)} mismatch(es), "
              f"{len(bind_expected)} deliberate (MODERN_ONLY_FAMILIES), "
              f"{len(bind_skipped)} name(s) not comparable")
        errors.extend(bind_errors)

        disp_errors, disp_skipped = check_dispatch_targets(
            families, body, legacy, redirects, roots)
        print(f"[INFO] dispatch targets: {len(disp_errors)} crossed, "
              f"{len(disp_skipped)} not followed")
        errors.extend(disp_errors)

    print()
    for w in warnings:
        print(w)
    for e in errors:
        print(e)

    if errors or (args.strict and warnings):
        print(f"\n[FAIL] ABI signature check: {len(errors)} contradiction(s), "
              f"{len(warnings)} unclassified.")
        return 1
    print(f"\n[PASS] ABI signature check: {len(families)} versioned families, "
          f"{len(errors)} contradiction(s), {len(warnings)} unclassified"
          f"{' (reported, not fatal without --strict)' if warnings else ''}.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
