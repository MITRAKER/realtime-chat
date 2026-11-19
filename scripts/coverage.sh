#!/usr/bin/env bash
# Runs every test with the race detector while measuring coverage, prints
# the result, and fails if the total is below the minimum.
#
# Usage: scripts/coverage.sh [profile]   (default profile: coverage.out)
#        COVERAGE_MIN=85 scripts/coverage.sh   to demand more
#
# Database tests run only when TEST_DATABASE_URL is set (CI sets it).
set -euo pipefail

profile="${1:-coverage.out}"
min="${COVERAGE_MIN:-80}"

# -coverpkg=./... counts code in every package, whichever package's tests
# run it. Only packages that have tests are listed: for a package without
# tests, `go test -cover` needs the optional covdata tool, which some Go
# installs leave out. Their code still counts through -coverpkg.
tested="$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)"
# $tested is unquoted on purpose: one package per word.
# shellcheck disable=SC2086
go test -race -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$profile" $tested

report="$(go tool cover -func="$profile")"
total="$(awk '/^total:/ { sub("%", "", $3); print $3 }' <<<"$report")"

echo "Total coverage: ${total}% (minimum ${min}%)"

# In GitHub Actions, also show it on the run's summary page.
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Test coverage: ${total}% (minimum ${min}%)"
    echo
    echo '<details><summary>Coverage by function</summary>'
    echo
    echo '```text'
    echo "$report"
    echo '```'
    echo '</details>'
  } >>"$GITHUB_STEP_SUMMARY"
fi

if ! awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 >= m + 0) }'; then
  echo "FAIL: coverage ${total}% is below the minimum ${min}%"
  exit 1
fi
