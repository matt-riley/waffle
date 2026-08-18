#!/usr/bin/env bash
# Runs `go test -race` on one shard of the repository's Go tests.
#
# Three modes:
#   pkg SHARD TOTAL        shard of the package list. The split is a greedy
#                          balanced partition using measured per-package test
#                          weights from scripts/ci-test-shard-weights.tsv, so
#                          the heavy packages (internal/skill, internal/session)
#                          land on different shards and every shard finishes
#                          in roughly the same time. internal/workspace and
#                          cmd/waffle are excluded here because they run via
#                          the named-test modes below.
#   workspace GROUP TOTAL  shard of internal/workspace's tests by name, using
#                          measured per-test weights from
#                          scripts/ci-test-workspace-weights.tsv.
#   cmd GROUP TOTAL        shard ./cmd/waffle by test name using
#                          scripts/ci-test-cmd-weights.tsv. TestServe* lifecycle
#                          and socket tests stay together on the final group so
#                          sharding does not split their process-level context;
#                          the remaining tests are balanced across the other
#                          groups.
#
# Items without a weight entry default to 1s, so newly added packages/tests
# attach to the currently-lightest shard without manual bookkeeping.
#
# Usage: ci-test-shard.sh pkg|workspace|cmd SHARD TOTAL [--list]
#   SHARD/GROUP are 1-based. --list prints the split without running tests.
set -euo pipefail

mode="${1:?usage: ci-test-shard.sh pkg|workspace|cmd SHARD TOTAL [--list]}"
shard="${2:?usage: ci-test-shard.sh pkg|workspace|cmd SHARD TOTAL [--list]}"
total="${3:?usage: ci-test-shard.sh pkg|workspace|cmd SHARD TOTAL [--list]}"
list_only="${4:-}"

if (( shard < 1 || shard > total )); then
  echo "error: shard $shard out of range 1..$total" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
partition_shard="$shard"
partition_total="$total"

case "$mode" in
  pkg)
    module="$(go list -m)"
    weights_file="$script_dir/ci-test-shard-weights.tsv"
    # ./ prefix so go resolves the paths relative to the module rather than
    # treating them as std import paths (go test internal/config fails).
    prefix="./"
    input="$(go list ./... | sed "s|^${module}/||" | grep -v -e '^internal/workspace$' -e '^cmd/waffle$')"
    ;;
  workspace)
    weights_file="$script_dir/ci-test-workspace-weights.tsv"
    prefix=""
    test_pkg="./internal/workspace"
    input="$(go test -list '^Test' "$test_pkg" | grep '^Test')"
    ;;
  cmd)
    weights_file="$script_dir/ci-test-cmd-weights.tsv"
    prefix=""
    test_pkg="./cmd/waffle"
    all_tests="$(go test -list '^Test' "$test_pkg" | grep '^Test')"
    if (( total > 1 )); then
      if (( shard == total )); then
        # Serve exercises daemon lifecycle, Unix sockets, listeners and
        # process-level coordination. Keep those tests in one process rather
        # than making their behaviour depend on which named-test shard they
        # happened to land in.
        input="$(grep '^TestServe' <<< "$all_tests")"
        partition_shard=1
        partition_total=1
      else
        input="$(grep -v '^TestServe' <<< "$all_tests")"
        partition_total=$((total - 1))
      fi
    else
      input="$all_tests"
    fi
    ;;
  *)
    echo "error: unknown mode $mode (expected pkg, workspace, or cmd)" >&2
    exit 1
    ;;
esac

selected="$(awk -v shard="$partition_shard" -v total="$partition_total" -v wf="$weights_file" -v prefix="$prefix" '
BEGIN {
  while ((getline line < wf) > 0) {
    split(line, a, "\t")
    if (a[1] != "" && a[1] !~ /^#/) w[a[1]] = a[2] + 0
  }
  close(wf)
  n = 0
  while ((getline item) > 0) {
    rows[n] = item
    wt[n] = (item in w) ? w[item] : 1
    n++
  }
  # Sort items by weight descending so the greedy fill places the heavy
  # items first, then fills with light ones into the lightest shard.
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
  for (i = 1; i <= count; i++) if (out[i] != "") print prefix out[i]
}' <<< "$input")" || {
  echo "error: failed to partition items for shard $shard" >&2
  exit 1
}

if [[ -z "$selected" ]]; then
  echo "error: shard $shard has no items" >&2
  exit 1
fi

echo "::group::shard ${shard}/${total} ($mode)"
# shellcheck disable=SC2086 # intentional: one item per line
printf '%s\n' $selected
echo "::endgroup::"

if [[ "$list_only" == "--list" ]]; then
  exit 0
fi

if [[ "$mode" == "workspace" || "$mode" == "cmd" ]]; then
  # Join the newline-separated test names into a -run alternation. The regex
  # is anchored so partial name prefixes cannot match extra tests.
  run_regex="^($(printf '%s' "$selected" | tr '\n' '|'))$"
  go test -race "$test_pkg" -run "$run_regex"
else
  # shellcheck disable=SC2086 # intentional word splitting into go test args
  go test -race $selected
fi
