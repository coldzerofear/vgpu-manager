#!/usr/bin/env bash
# Copyright 2024-2026 coldzerofear
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Verify .golangci.yaml against golangci-lint's JSON schema.
#
# Worth doing, because `golangci-lint run` does not: it ignores a section name
# it does not recognise, so writing `exclusionz` for `exclusions` drops every
# exclusion we rely on and still reports a clean run.
#
# Worth guarding, because the schema is fetched from golangci-lint.run on every
# invocation. A slow or unreachable host would otherwise fail a build for a
# reason that has nothing to do with the code, which is exactly how the lint
# job failed:
#
#   compile schema: failing loading "https://golangci-lint.run/jsonschema/
#   golangci.v2.6.jsonschema.json": context deadline exceeded
#
# So a fetch failure is retried and then downgraded to a warning, while a real
# configuration error stays fatal. The two are told apart by their messages: a
# fetch failure is reported as "compile schema", a configuration error as
# "does not validate"/"invalid elements".

set -uo pipefail

GOLANGCI_LINT="${GOLANGCI_LINT:-golangci-lint}"
if ! command -v "${GOLANGCI_LINT}" >/dev/null 2>&1; then
    gopath_bin="$(go env GOPATH 2>/dev/null || true)/bin"
    if [ -x "${gopath_bin}/golangci-lint" ]; then
        GOLANGCI_LINT="${gopath_bin}/golangci-lint"
    else
        echo "golangci-lint not found; skipping the configuration check" >&2
        exit 0
    fi
fi

for attempt in 1 2 3; do
    if out="$("${GOLANGCI_LINT}" config verify 2>&1)"; then
        echo "[PASS] lint configuration verified against the JSON schema"
        exit 0
    fi
    case "${out}" in
        *"compile schema"* | *"context deadline exceeded"* | *"no such host"* | \
        *"connection refused"* | *"TLS handshake"* | *"i/o timeout"*)
            echo "attempt ${attempt}: could not fetch the JSON schema, retrying" >&2
            sleep 5
            ;;
        *)
            echo "${out}" >&2
            exit 1
            ;;
    esac
done

echo "could not fetch the golangci-lint JSON schema; .golangci.yaml was not verified" >&2
if [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "::warning::golangci-lint JSON schema unreachable; .golangci.yaml was not verified"
fi
exit 0
