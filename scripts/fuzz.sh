#!/usr/bin/env bash
# Runs every fuzz test in the module, each for the given time.
#
# Usage: scripts/fuzz.sh [time]   (default 30s; also 5m, 1h, or 10000x runs)
#
# A failing input is saved under the package's testdata/fuzz/<FuzzName>/.
# Commit it: from then on `go test` runs it as a regression test.
set -euo pipefail

fuzztime="${1:-30s}"
found=0

# `go test -fuzz` accepts one package and one target at a time.
for pkg in $(go list ./...); do
  # Capture the list first: if the tests don't compile, `set -e` stops the
  # script here instead of quietly fuzzing nothing.
  listing="$(go test -list '^Fuzz' "$pkg")"
  for target in $(grep '^Fuzz' <<<"$listing" || true); do
    found=$((found + 1))
    echo "== fuzzing $target in $pkg for $fuzztime"
    go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime"
  done
done

if [[ "$found" -eq 0 ]]; then
  echo "FAIL: no fuzz tests found" >&2
  exit 1
fi
echo "Fuzzed $found targets for $fuzztime each: no failures."
