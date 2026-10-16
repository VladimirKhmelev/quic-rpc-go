#!/usr/bin/env bash
# Usage: fuzz.sh FUZZTIME MODULE...
# Runs every Fuzz* target of every package in the given modules. FUZZTIME is
# passed to -fuzztime: an execution count (1000000x) or a duration (30s).
# FUZZ_OVERRIDES, such as "FuzzServerStream=100000x", gives single targets
# their own budget.
set -euo pipefail

fuzztime=$1
shift

declare -A budget
for o in ${FUZZ_OVERRIDES:-}; do
  budget[${o%%=*}]=${o#*=}
done

for mod in "$@"; do
  (
    cd "$mod"
    for pkg in $(go list ./...); do
      for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
        t=${budget[$target]:-$fuzztime}
        echo "==> fuzz $pkg $target ($t)"
        go test -run '^$' -fuzz "^${target}\$" -fuzztime "$t" "$pkg"
      done
    done
  )
done
