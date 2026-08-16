#!/bin/sh
set -eu

fail() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

emit_no_release() {
  release_state=$1
  printf '%s\n' \
    "release_state=${release_state}" \
    'release_created=false' \
    'tag_name=' \
    'version=' \
    'release_sha=' >> "$output_path"
}

if [ "$#" -ne 6 ]; then
  fail 'usage: resolve-release-state.sh <repository> <trigger-sha> <release-created> <tag-name> <version> <github-output>'
fi

repository=$1
trigger_sha=$2
action_created=$3
action_tag=$4
action_version=$5
output_path=$6

if ! printf '%s\n' "$repository" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'; then
  fail 'release repository identity is invalid'
fi
if ! printf '%s\n' "$trigger_sha" | grep -Eq '^[0-9a-f]{40}$'; then
  fail 'release trigger SHA is invalid'
fi
if [ -z "$output_path" ]; then
  fail 'release output path is empty'
fi

checked_out_sha=$(git rev-parse --verify 'HEAD^{commit}') ||
  fail 'release checkout does not resolve to a commit'
if [ "$checked_out_sha" != "$trigger_sha" ]; then
  fail 'release checkout does not match the triggering commit'
fi

manifest_version=$(jq -er '
  if type == "object" and (."." | type == "string")
  then ."."
  else error("invalid root version")
  end
' .release-please-manifest.json 2>/dev/null) ||
  fail 'release manifest has an invalid root version'
if ! printf '%s\n' "$manifest_version" |
  grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.+-]+)?$'; then
  fail 'release manifest has an invalid root version'
fi
expected_tag="v${manifest_version}"

case "$action_created" in
  true)
    if [ "$action_tag" != "$expected_tag" ] ||
      [ "$action_version" != "$manifest_version" ]; then
      fail 'release-please outputs are inconsistent with the release manifest'
    fi
    ;;
  false|'')
    if [ -n "$action_tag" ] || [ -n "$action_version" ]; then
      fail 'release-please outputs are inconsistent with the release manifest'
    fi
    ;;
  *)
    fail 'release-please outputs are inconsistent with the release manifest'
    ;;
esac

tag_ref="refs/tags/${expected_tag}"
tag_lookup_status=0
git show-ref --verify --quiet "$tag_ref" || tag_lookup_status=$?
if [ "$tag_lookup_status" -eq 1 ]; then
  if [ "$action_created" = true ]; then
    fail 'release-please reported a release without the expected exact tag'
  fi
  emit_no_release none
  exit 0
fi
if [ "$tag_lookup_status" -ne 0 ]; then
  fail 'could not inspect the expected release tag'
fi

tag_sha=$(git rev-parse --verify "${tag_ref}^{commit}" 2>/dev/null) ||
  fail 'expected release tag does not resolve to a commit'
if [ "$tag_sha" != "$trigger_sha" ]; then
  if [ "$action_created" = true ]; then
    fail 'release-please reported a release without the expected exact tag'
  fi
  emit_no_release none
  exit 0
fi

release_pages=$(mktemp)
release_matches=$(mktemp)
cleanup() {
  rm -f "$release_pages" "$release_matches"
}
trap cleanup 0 1 2 15

if ! gh api --paginate --slurp \
  "repos/${repository}/releases?per_page=100" > "$release_pages"; then
  fail 'could not enumerate hosted releases'
fi

if ! jq -e --arg tag "$expected_tag" '
  if type == "array" and all(.[]; type == "array")
  then [.[][] | select(.tag_name == $tag)]
  else error("invalid release inventory")
  end
' "$release_pages" > "$release_matches" 2>/dev/null; then
  fail 'hosted release inventory is invalid'
fi

release_count=$(jq -er 'length' "$release_matches" 2>/dev/null) ||
  fail 'hosted release inventory is invalid'
case "$release_count" in
  0)
    fail 'exact release tag exists without a readable hosted release'
    ;;
  1) ;;
  *)
    fail 'hosted release state is ambiguous for the exact release tag'
    ;;
esac

release_draft=$(jq -er --arg tag "$expected_tag" '
  .[0] as $release |
  if ($release | type == "object") and
    ($release.id | type == "number") and ($release.id > 0) and
    ($release.tag_name == $tag) and ($release.draft | type == "boolean")
  then ($release.draft | tostring)
  else error("invalid release")
  end
' "$release_matches" 2>/dev/null) ||
  fail 'exact draft release does not match the trusted release-please contract'

if [ "$release_draft" = false ]; then
  if [ "$action_created" = true ]; then
    fail 'release-please outputs are inconsistent with the hosted release'
  fi
  emit_no_release published
  exit 0
fi

if ! jq -e --arg tag "$expected_tag" '
  .[0] as $release |
  ($release | type == "object") and
  ($release.id | type == "number") and ($release.id > 0) and
  ($release.tag_name == $tag) and ($release.draft == true) and
  ($release.prerelease == false) and
  ($release.author | type == "object") and
  ($release.author.login == "github-actions[bot]") and
  ($release.author.type == "Bot")
' "$release_matches" >/dev/null; then
  fail 'exact draft release does not match the trusted release-please contract'
fi

printf '%s\n' \
  'release_state=draft' \
  'release_created=true' \
  "tag_name=${expected_tag}" \
  "version=${manifest_version}" \
  "release_sha=${trigger_sha}" >> "$output_path"
