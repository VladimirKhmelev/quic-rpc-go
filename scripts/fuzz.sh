#!/usr/bin/env bash
# Usage: fuzz.sh FUZZTIME MODULE...
# Runs every Fuzz* target of every package in the given modules. FUZZTIME is
# passed to -fuzztime: an execution count (1000000x) or a duration (30s).
set -euo pipefail

fuzztime=$1
shift

for mod in "$@"; do
  (
    cd "$mod"
    for pkg in $(go list ./...); do
      for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
        echo "==> fuzz $pkg $target ($fuzztime)"
        go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg"
      done
    done
  )
done
