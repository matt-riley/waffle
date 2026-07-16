#!/usr/bin/env bash

set -euo pipefail

if [[ $# -gt 1 ]]; then
  printf 'usage: %s [repo-root]\n' "${0##*/}" >&2
  exit 2
fi

repo_root=${1:-$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd
)}

source_sha=$(git -C "$repo_root" -c core.fsmonitor=false rev-parse HEAD)
builder_rel=scripts/build-linux-artifact.sh

file_mode() {
  local target=$1
  if stat -c '%a' "$target" >/dev/null 2>&1; then
    stat -c '%a' "$target"
    return
  fi
  if stat -f '%Lp' "$target" >/dev/null 2>&1; then
    stat -f '%Lp' "$target"
    return
  fi
  printf 'error: could not determine mode for %s\n' "$target" >&2
  exit 1
}

tmp_a=$(mktemp -d /private/tmp/waffle-repro-a.XXXXXX)
tmp_b=$(mktemp -d /private/tmp/waffle-repro-b.XXXXXX)
cache_a=$(mktemp -d /private/tmp/waffle-gocache-a.XXXXXX)
cache_b=$(mktemp -d /private/tmp/waffle-gocache-b.XXXXXX)
mod_a=$(mktemp -d /private/tmp/waffle-gomod-a.XXXXXX)
mod_b=$(mktemp -d /private/tmp/waffle-gomod-b.XXXXXX)
trap 'chmod -R -f u+w "$tmp_a" "$tmp_b" "$cache_a" "$cache_b" "$mod_a" "$mod_b" 2>/dev/null || true; rm -rf -- "$tmp_a" "$tmp_b" "$cache_a" "$cache_b" "$mod_a" "$mod_b"' EXIT

git clone --quiet --no-hardlinks --local "$repo_root" "$tmp_a/repo"
git clone --quiet --no-hardlinks --local "$repo_root" "$tmp_b/repo"
git -C "$tmp_a/repo" checkout --quiet "$source_sha"
git -C "$tmp_b/repo" checkout --quiet "$source_sha"
cp "$repo_root/$builder_rel" "$tmp_a/repo/$builder_rel"
cp "$repo_root/$builder_rel" "$tmp_b/repo/$builder_rel"

(
  cd -- "$tmp_a/repo"
  umask 022
  GOFLAGS=-modcacherw GOCACHE=$cache_a GOMODCACHE=$mod_a scripts/build-linux-artifact.sh \
    "$tmp_a/out" \
    123456789 \
    matt-riley/waffle \
    refs/heads/main \
    "$source_sha"
)

(
  cd -- "$tmp_b/repo"
  umask 077
  GOFLAGS=-modcacherw GOCACHE=$cache_b GOMODCACHE=$mod_b scripts/build-linux-artifact.sh \
    "$tmp_b/out" \
    123456789 \
    matt-riley/waffle \
    refs/heads/main \
    "$source_sha"
)

actual_listing=$(
  tar -tzf "$tmp_a/out/waffle-linux-amd64.tar.gz"
)
expected_listing=$'waffle\nwaffle.sha256\nbuild-metadata.json'
if [[ $actual_listing != "$expected_listing" ]]; then
  printf 'unexpected archive listing:\n%s\n' "$actual_listing" >&2
  exit 1
fi

(cd -- "$tmp_a/out" && sha256sum -c waffle.sha256)
(cd -- "$tmp_b/out" && sha256sum -c waffle.sha256)

jq -e '
  .source_repo == "matt-riley/waffle" and
  .source_ref == "refs/heads/main" and
  .source_sha == $sha and
  .workflow_run_id == 123456789 and
  .goos == "linux" and
  .goarch == "amd64"
' --arg sha "$source_sha" "$tmp_a/out/build-metadata.json" >/dev/null

jq -e '
  .source_repo == "matt-riley/waffle" and
  .source_ref == "refs/heads/main" and
  .source_sha == $sha and
  .workflow_run_id == 123456789 and
  .goos == "linux" and
  .goarch == "amd64"
' --arg sha "$source_sha" "$tmp_b/out/build-metadata.json" >/dev/null

for artifact_dir in "$tmp_a/out" "$tmp_b/out"; do
  [[ $(file_mode "$artifact_dir/waffle") == 755 ]] || {
    printf 'unexpected waffle mode in %s\n' "$artifact_dir" >&2
    exit 1
  }
  [[ $(file_mode "$artifact_dir/waffle.sha256") == 644 ]] || {
    printf 'unexpected waffle.sha256 mode in %s\n' "$artifact_dir" >&2
    exit 1
  }
  [[ $(file_mode "$artifact_dir/build-metadata.json") == 644 ]] || {
    printf 'unexpected build-metadata.json mode in %s\n' "$artifact_dir" >&2
    exit 1
  }
done

cmp -s "$tmp_a/out/waffle.sha256" "$tmp_b/out/waffle.sha256"
cmp -s "$tmp_a/out/build-metadata.json" "$tmp_b/out/build-metadata.json"
cmp -s "$tmp_a/out/waffle-linux-amd64.tar.gz" "$tmp_b/out/waffle-linux-amd64.tar.gz"

archive_sha=$(
  sha256sum "$tmp_a/out/waffle-linux-amd64.tar.gz" | awk '{print $1}'
)
printf 'archive_sha256 %s\n' "$archive_sha"
echo REPRO_OK
