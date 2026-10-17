#!/bin/sh
# Deterministic acceptance only; coding is driven by the loop-programming skill.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
mkdir -p .loop/bin
exe=".loop/bin/acceptance.$$"
trap 'rm -f "$exe"' EXIT
GOPROXY="${GOPROXY:-https://goproxy.cn}" go build -o "$exe" ./scripts/acceptance || exit 2
set +e
"$exe" "$@"
code=$?
exit "$code"
