#!/bin/sh
set -eu

minimum="${1:-99.0}"
go test -covermode=atomic -coverprofile=coverage.out ./...
# `go tool cover -func` rounds to one decimal, which can let 98.95% satisfy a
# 99.0% gate. Compute from block statement counts at full precision instead.
actual="$(awk 'NR > 1 { total += $2; if ($3 != 0) covered += $2 } END { if (total == 0) print "100.000000"; else printf "%.6f", 100 * covered / total }' coverage.out)"

awk -v actual="$actual" -v minimum="$minimum" 'BEGIN {
  if ((actual + 0) < (minimum + 0)) {
    printf "coverage %.6f%% is below required %.6f%%\n", actual, minimum > "/dev/stderr"
    exit 1
  }
  printf "coverage %.6f%% meets required %.6f%%\n", actual, minimum
}'
