#!/usr/bin/env bash
#
# Catch paths that bypass session_path().
#
# Every per-session file has two locations -- inside the session directory when
# VGPU_CONFIG_SESSION_PATH is set, and its historical place when it is not --
# and session.c is the only file allowed to know both. Anything that builds a
# path from the component macros instead goes to the historical place always,
# which in a session means a file nobody created.
#
# That is not hypothetical: LOCK_PATH_FORMAT was
#
#     #define LOCK_PATH_FORMAT (TMP_DIR VGPU_LOCK_DIR "/vgpu_%d.lock")
#
# so lock_gpu_device() opened /tmp/.vgpu_lock/vgpu_N.lock while the directory
# it had just created was <session>/.vgpu_lock. open() failed, the function
# returned -1, and every caller carried on -- the memory budget check ran with
# no cross-process exclusion in exactly the mode that needs it most.
#
# Grepping the sources cannot find this: the concatenation is spelled with
# macro names, not with the path it produces. So this runs the preprocessor
# first and looks at what the concatenation actually yields. String literals
# are joined in translation phase 6, i.e. AFTER preprocessing, so a compile-
# time path appears as adjacent literals -- that adjacency is the signature.
#
# Usage:  check_session_paths.sh [library-root]

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_ROOT="$(cd "${1:-${SCRIPT_DIR}/..}" && pwd)"

if ! command -v gcc >/dev/null 2>&1 && ! command -v cc >/dev/null 2>&1; then
  echo "[SKIP] no C preprocessor in PATH - cannot validate session path routing"
  exit 0
fi
CC="$(command -v gcc || command -v cc)"

# Files permitted to name a historical path. session.c holds the fallback
# table (that IS its job); the test asserts those fallbacks are unchanged.
ALLOWED_FILES=(session.c test_session_paths.c)

# Paths that are node-level by nature, so they have no per-session form:
#   .host_proc  the host's /proc bind mount, for cgroup PID attribution
#   /driver/    where the .so itself is installed
#   /registry/  the manager's registration socket (CLIENT mode only)
ALLOWED_PATHS='/\.host_proc|/driver/|/registry/'

violations=0
report=""

for src in "${LIB_ROOT}"/src/*.c "${LIB_ROOT}"/tools/*.c "${LIB_ROOT}"/test/nogpu/*.c; do
  [[ -f "${src}" ]] || continue
  base="$(basename "${src}")"
  skip=0
  for allowed in "${ALLOWED_FILES[@]}"; do
    [[ "${base}" == "${allowed}" ]] && skip=1
  done
  (( skip )) && continue

  # Adjacent literals starting at a root the session moves.
  hits=$("${CC}" -E -I"${LIB_ROOT}" -I"${LIB_ROOT}/include" -D_GNU_SOURCE "${src}" 2>/dev/null \
         | grep -oE '"(/tmp|/etc/vgpu-manager)"[[:space:]]*"[^"]*"([[:space:]]*"[^"]*")*' \
         | grep -vE "${ALLOWED_PATHS}" || true)

  if [[ -n "${hits}" ]]; then
    while IFS= read -r hit; do
      [[ -z "${hit}" ]] && continue
      report+="         ${base}: ${hit}"$'\n'
      violations=$((violations + 1))
    done <<< "${hits}"
  fi
done

if (( violations > 0 )); then
  echo "[FAIL] ${violations} compile-time path(s) bypass session_path():"
  printf '%s' "${report}"
  echo "       These resolve to the historical location even inside a session,"
  echo "       where nothing creates it -- the open() then fails silently."
  echo "       Build the path at runtime instead:"
  echo "         snprintf(buf, sizeof(buf), \"%s/name\", session_path(SESSION_...));"
  echo "       If the path is genuinely node-level, add it to ALLOWED_PATHS here"
  echo "       with a note saying why it has no per-session form."
  exit 1
fi

echo "[PASS] session path routing: no compile-time path bypasses session_path()"
