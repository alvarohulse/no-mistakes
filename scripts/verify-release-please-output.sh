#!/bin/sh

set -eu

fail() {
  printf '::error::%s\n' "$*" >&2
  exit 1
}

if [ "$#" -ne 2 ]; then
  fail "usage: verify-release-please-output.sh <head-sha> <expected-output-dir>"
fi

HEAD_SHA=$1
EXPECTED_OUTPUT_DIR=$2

if ! verified_head=$(git rev-parse --verify "${HEAD_SHA}^{commit}"); then
  fail "release-please head is not a commit"
fi
if [ "$verified_head" != "$HEAD_SHA" ]; then
  fail "release-please head must be a full commit SHA"
fi

actual_dir=$(mktemp -d "${TMPDIR:-/tmp}/release-please-output.XXXXXX")
trap 'rm -rf "$actual_dir"' 0 HUP INT TERM

for path in CHANGELOG.md .release-please-manifest.json; do
  expected_path="${EXPECTED_OUTPUT_DIR}/${path}"
  actual_path="${actual_dir}/${path}"
  if [ ! -f "$expected_path" ] || [ -L "$expected_path" ]; then
    fail "captured release-please producer output is missing ${path}"
  fi
  if ! git show "${HEAD_SHA}:${path}" > "$actual_path"; then
    fail "release-please head is missing ${path}"
  fi
  if ! cmp -s "$expected_path" "$actual_path"; then
    fail "${path} does not match captured release-please producer output"
  fi
done

printf 'Release-please head %s matches the captured producer output. OK.\n' "$HEAD_SHA"
