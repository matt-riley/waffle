#!/usr/bin/env bash
# Runs `go test -race` on one shard of the repository's Go packages.
#
# The split is a greedy balanced partition using measured per-package test
# weights from scripts/ci-test-shard-weights.tsv, so the heavy packages
# (cmd/waffle, internal/workspace) land on different shards and every shard
# finishes in roughly the same time. Packages without an entry default to a
# weight of 1s, so newly added packages attach to the currently-lightest
# shard without manual bookkeeping.
#
# Usage: ci-test-shard.sh SHARD TOTAL_SHARDS [--list]
#   SHARD is 1-based. --list prints the package split without running tests.
set -euo pipefail

shard="${1:?usage: ci-test-shard.sh SHARD TOTAL_SHARDS [--list]}"
total="${2:?usage: ci-test-shard.sh SHARD TOTAL_SHARDS [--list]}"
list_only="${3:-}"

if (( shard < 1 || shard > total )); then
  echo "error: shard $shard out of range 1..$total" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module="$(go list -m)"
weights_file="$script_dir/ci-test-shard-weights.tsv"
packages="$(go list ./... | sed "s|^${module}/||")"

selected="$(awk -v shard="$shard" -v total="$total" -v wf="$weights_file" '
BEGIN {
  while ((getline line < wf) > 0) {
    split(line, a, "\t")
    if (a[1] != "" && a[1] !~ /^#/) w[a[1]] = a[2] + 0
  }
  close(wf)
  n = 0
  while ((getline pkg) > 0) {
    rows[n] = pkg
    wt[n] = (pkg in w) ? w[pkg] : 1
    n++
  }
  # Sort packages by weight descending so the greedy fill places the heavy
  # packages first, then fills with light ones into the lightest shard.
  for (i = 0; i < n; i++) {
    best = i
    for (j = i + 1; j < n; j++) if (wt[j] > wt[best]) best = j
    t = rows[i]; rows[i] = rows[best]; rows[best] = t
    t = wt[i]; wt[i] = wt[best]; wt[best] = t
  }
  for (i = 0; i < total; i++) sums[i] = 0
  for (i = 0; i < n; i++) {
    lightest = 0
    for (k = 1; k < total; k++) if (sums[k] < sums[lightest]) lightest = k
    shards[lightest] = shards[lightest] " " rows[i]
    sums[lightest] += wt[i]
  }
  count = split(shards[shard - 1], out, " ")
  if (count == 0 || (count == 1 && out[1] == "")) exit 2
  # ./ prefix so go resolves the paths relative to the module rather than
  # treating them as std import paths (go test internal/config fails).
  for (i = 1; i <= count; i++) if (out[i] != "") print "./" out[i]
}' <<< "$packages")" || {
  echo "error: failed to partition packages for shard $shard" >&2
  exit 1
}

if [[ -z "$selected" ]]; then
  echo "error: shard $shard has no packages" >&2
  exit 1
fi

echo "::group::shard ${shard}/${total} packages"
# shellcheck disable=SC2086 # intentional: one package per line
printf '%s\n' $selected
echo "::endgroup::"

if [[ "$list_only" == "--list" ]]; then
  exit 0
fi

# shellcheck disable=SC2086 # intentional word splitting into go test args
go test -race $selected
