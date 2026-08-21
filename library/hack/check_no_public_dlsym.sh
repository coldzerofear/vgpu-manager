#!/usr/bin/env bash
#
# Forbid calls to the public dlsym() from library/src/.
#
# We export our own dlsym, so a plain dlsym() call inside this library
# resolves back to that hook rather than to libc. Any such call re-enters
# the interceptor, and the paths it reaches from there (load_necessary_data
# -> dlopen under pthread_once) deadlock if re-entered on the same thread.
# The recursion guard turns that into a failed lookup instead of a hang, but
# the call still cannot do what it was written to do.
#
# Call through the cached real_dlsym pointer instead. Bootstrapping that
# pointer is the one case with no pointer to call yet, and it uses dlvsym(),
# which we do not export and which therefore stays allowed here.
#
# Run from repo root or library/. Exit non-zero if any bare dlsym() call
# appears in library/src/.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SRC_DIR="${1:-${LIB_ROOT}/src}"

if [[ ! -d "${SRC_DIR}" ]]; then
  echo "[FAIL] library/src not found at ${SRC_DIR}" >&2
  exit 2
fi

# Bare `dlsym(` only. A leading identifier character rules out real_dlsym(
# (the `_` is excluded by the class), and dlvsym( is a different name that
# this pattern never matches. The definition of our own interceptor is the
# one line that legitimately reads `dlsym(` and is tagged FUNC_ATTR_VISIBLE.
matches=$(grep -rnE '(^|[^_[:alnum:]])dlsym[[:space:]]*\(' "${SRC_DIR}" \
  --include='*.c' --include='*.h' 2>/dev/null \
  | grep -v 'FUNC_ATTR_VISIBLE' \
  | grep -vE ':[0-9]+:[[:space:]]*(\*|//|/\*)' \
  || true)

if [[ -n "${matches}" ]]; then
  echo "[FAIL] public dlsym() call(s) in ${SRC_DIR}:"
  echo "${matches}" | sed 's/^/         /'
  echo
  echo "       Call real_dlsym(...) instead. Assigning the result to"
  echo "       real_dlsym does not make it safe -- the call itself is what"
  echo "       re-enters the interceptor. Use dlvsym() to bootstrap."
  exit 1
fi

echo "[PASS] no public dlsym() calls in ${SRC_DIR}"
