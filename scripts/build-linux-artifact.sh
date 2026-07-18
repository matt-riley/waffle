#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 5 ]]; then
  printf 'usage: %s <output-dir> <github-run-id> <source-repo> <source-ref> <source-sha>\n' "${0##*/}" >&2
  exit 2
fi

output_dir=$1
workflow_run_id=$2
source_repo=$3
source_ref=$4
source_sha=$5

if [[ ! $workflow_run_id =~ ^[0-9]+$ ]]; then
  printf 'error: workflow run id must be numeric: %s\n' "$workflow_run_id" >&2
  exit 1
fi

if [[ ! $source_sha =~ ^[0-9a-fA-F]{40}$ ]]; then
  printf 'error: source SHA must be a 40-character hex commit SHA: %s\n' "$source_sha" >&2
  exit 1
fi

repo_root=$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd
)

mkdir -p -- "$output_dir"
chmod 0755 "$output_dir"

version=$(
  cd -- "$repo_root" && git describe --tags --always --dirty
)

(
  cd -- "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-X main.version=$version" \
    -o "$output_dir/waffle" ./cmd/waffle
)

chmod 0755 "$output_dir/waffle"
(cd -- "$output_dir" && sha256sum waffle > waffle.sha256)

jq -n \
  --arg source_repo "$source_repo" \
  --arg source_sha "$source_sha" \
  --arg source_ref "$source_ref" \
  --argjson workflow_run_id "$workflow_run_id" \
  --arg version "$version" \
  --arg goos linux \
  --arg goarch amd64 \
  '{
    source_repo: $source_repo,
    source_sha: $source_sha,
    source_ref: $source_ref,
    workflow_run_id: $workflow_run_id,
    version: $version,
    goos: $goos,
    goarch: $goarch
  }' > "$output_dir/build-metadata.json"

chmod 0644 "$output_dir/waffle.sha256" "$output_dir/build-metadata.json"
TZ=UTC touch -t 197001010000 "$output_dir/waffle" "$output_dir/waffle.sha256" "$output_dir/build-metadata.json"

staging_tar="$output_dir/waffle-linux-amd64.tar"
trap 'rm -f -- "$staging_tar"' EXIT

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  tar_args=(--owner=0 --group=0)
else
  tar_args=(--uid 0 --gid 0 --uname root --gname root)
fi

tar "${tar_args[@]}" -C "$output_dir" -cf "$staging_tar" waffle waffle.sha256 build-metadata.json
gzip -n -c "$staging_tar" > "$output_dir/waffle-linux-amd64.tar.gz"
rm -f -- "$staging_tar"
trap - EXIT
