#!/bin/sh

set -eu

fail() {
  printf '::error::%s\n' "$*" >&2
  exit 1
}

generated_entries() {
  generated_entries_treeish=$1
  for generated_entries_path in CHANGELOG.md .release-please-manifest.json; do
    if ! generated_entries_entry=$(git ls-tree "$generated_entries_treeish" -- "$generated_entries_path"); then
      return 1
    fi
    printf '%s\t%s\n' "$generated_entries_path" "$generated_entries_entry"
  done
}

generated_entries_complete() {
  generated_entries_complete_treeish=$1
  for generated_entries_complete_path in CHANGELOG.md .release-please-manifest.json; do
    if ! generated_entries_complete_entry=$(git ls-tree "$generated_entries_complete_treeish" -- "$generated_entries_complete_path"); then
      return 1
    fi
    if [ -z "$generated_entries_complete_entry" ]; then
      return 1
    fi
  done
}

if [ "$#" -ne 3 ]; then
  fail "usage: guard-generated-files.sh <base-sha> <head-sha> <canonical-upstream-url>"
fi

BASE_SHA=$1
HEAD_SHA=$2
CANONICAL_UPSTREAM_URL=$3
CANONICAL_REF=refs/no-mistakes/guard-generated-files/canonical-main

if ! verified_base=$(git rev-parse --verify "${BASE_SHA}^{commit}"); then
  fail "PR base is not a commit"
fi
if ! verified_head=$(git rev-parse --verify "${HEAD_SHA}^{commit}"); then
  fail "PR head is not a commit"
fi
BASE_SHA=$verified_base
HEAD_SHA=$verified_head

if ! merge_bases=$(git merge-base --all "$BASE_SHA" "$HEAD_SHA"); then
  fail "could not determine the PR merge base"
fi
set -- $merge_bases
if [ "$#" -ne 1 ]; then
  fail "PR base and head must have exactly one merge base"
fi

if ! pr_commits=$(git rev-list "${BASE_SHA}..${HEAD_SHA}"); then
  fail "could not enumerate PR commits"
fi

generated_files_touched=false
for commit in $pr_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect PR commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify PR commit identity"
  fi
  shift
  if [ "$#" -lt 1 ]; then
    fail "PR commit has no parent"
  fi
  if ! commit_entries=$(generated_entries "$commit"); then
    fail "could not inspect generated entries in a PR commit"
  fi

  if [ "$#" -eq 1 ]; then
    if ! parent_entries=$(generated_entries "$1"); then
      fail "could not inspect generated entries in a PR commit parent"
    fi
    if [ "$commit_entries" != "$parent_entries" ]; then
      generated_files_touched=true
    fi
    continue
  fi

  matches_parent=false
  differs_from_parent=false
  for parent in "$@"; do
    if ! parent_entries=$(generated_entries "$parent"); then
      fail "could not inspect generated entries in a merge parent"
    fi
    if [ "$commit_entries" = "$parent_entries" ]; then
      matches_parent=true
    else
      differs_from_parent=true
    fi
  done
  if [ "$matches_parent" = false ]; then
    fail "merge commit synthesizes generated entries not present in any parent"
  fi
  if [ "$differs_from_parent" = true ]; then
    generated_files_touched=true
  fi
done

if [ "$generated_files_touched" = false ]; then
  echo "No release-please-generated files modified. OK."
  exit 0
fi

if ! git fetch --no-tags --force "$CANONICAL_UPSTREAM_URL" \
  "+refs/heads/main:${CANONICAL_REF}"; then
  fail "could not fetch canonical upstream main"
fi
if ! canonical_main=$(git rev-parse --verify "${CANONICAL_REF}^{commit}"); then
  fail "canonical upstream main did not resolve to a commit"
fi

expected_files=$(printf '%s\n' .release-please-manifest.json CHANGELOG.md | LC_ALL=C sort)

for commit in $pr_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect PR commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify PR commit identity"
  fi
  shift
  if [ "$#" -ne 1 ]; then
    continue
  fi
  parent=$1
  if ! commit_entries=$(generated_entries "$commit"); then
    fail "could not inspect generated entries in a PR commit"
  fi
  if ! parent_entries=$(generated_entries "$parent"); then
    fail "could not inspect generated entries in a PR commit parent"
  fi
  if [ "$commit_entries" = "$parent_entries" ]; then
    continue
  fi

  if git merge-base --is-ancestor "$commit" "$canonical_main"; then
    :
  else
    ancestor_status=$?
    if [ "$ancestor_status" -eq 1 ]; then
      fail "generated files changed in a noncanonical commit"
    fi
    fail "could not verify canonical upstream ancestry"
  fi

  if ! canonical_files=$(git diff --no-renames --name-only "$parent" "$commit"); then
    fail "could not inspect canonical upstream commit files"
  fi
  canonical_files=$(printf '%s\n' "$canonical_files" | LC_ALL=C sort)
  if [ "$canonical_files" != "$expected_files" ]; then
    fail "canonical generated-file change is not a single-parent release-only commit"
  fi
  if ! generated_entries_complete "$commit"; then
    fail "canonical release commit does not contain both generated files"
  fi
done

if ! base_entries=$(generated_entries "$BASE_SHA"); then
  fail "could not inspect generated entries at the PR base"
fi
if ! head_entries=$(generated_entries "$HEAD_SHA"); then
  fail "could not inspect generated entries at the PR head"
fi
if ! generated_entries_complete "$BASE_SHA" || ! generated_entries_complete "$HEAD_SHA"; then
  fail "the PR base and head must contain both generated files"
fi

if [ "$head_entries" = "$base_entries" ]; then
  echo "Release-please-generated files exactly preserve the validated PR base. OK."
  exit 0
fi

if ! canonical_commits=$(git rev-list "$canonical_main"); then
  fail "could not enumerate canonical upstream commits"
fi

matching_base_commit=
matching_base_commit_count=0
matching_head_commit=
matching_head_commit_count=0

for commit in $canonical_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect canonical upstream commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify canonical upstream commit identity"
  fi
  if [ "$#" -ne 2 ]; then
    continue
  fi
  parent=$2
  if ! canonical_files=$(git diff --no-renames --name-only "$parent" "$commit"); then
    fail "could not inspect canonical upstream commit files"
  fi
  canonical_files=$(printf '%s\n' "$canonical_files" | LC_ALL=C sort)
  if [ "$canonical_files" != "$expected_files" ]; then
    continue
  fi
  if ! generated_entries_complete "$commit"; then
    continue
  fi
  if ! candidate_entries=$(generated_entries "$commit"); then
    fail "could not inspect canonical release entries"
  fi

  if [ "$candidate_entries" = "$base_entries" ]; then
    if git merge-base --is-ancestor "$commit" "$BASE_SHA"; then
      matching_base_commit=$commit
      matching_base_commit_count=$((matching_base_commit_count + 1))
    else
      ancestor_status=$?
      if [ "$ancestor_status" -ne 1 ]; then
        fail "could not verify base release ancestry"
      fi
    fi
  fi
  if [ "$candidate_entries" = "$head_entries" ]; then
    if git merge-base --is-ancestor "$commit" "$HEAD_SHA"; then
      matching_head_commit=$commit
      matching_head_commit_count=$((matching_head_commit_count + 1))
    else
      ancestor_status=$?
      if [ "$ancestor_status" -ne 1 ]; then
        fail "could not verify head release ancestry"
      fi
    fi
  fi
done

if [ "$matching_head_commit_count" -ne 1 ]; then
  fail "generated files must exactly match one release-only commit carried from canonical upstream main"
fi

if [ "$matching_base_commit_count" -gt 1 ]; then
  fail "PR base generated files have ambiguous canonical release provenance"
fi
if [ "$matching_base_commit_count" -eq 1 ]; then
  base_floor=$matching_base_commit
else
  if ! base_carriers=$(git merge-base --all "$BASE_SHA" "$canonical_main"); then
    fail "could not determine the canonical PR base carrier"
  fi
  set -- $base_carriers
  if [ "$#" -ne 1 ]; then
    fail "PR base must have exactly one canonical carrier"
  fi
  base_floor=$1
  if ! base_floor_entries=$(generated_entries "$base_floor"); then
    fail "could not inspect the canonical PR base carrier"
  fi
  if [ "$base_floor_entries" != "$base_entries" ]; then
    fail "PR base generated files do not have canonical provenance"
  fi
fi

if git merge-base --is-ancestor "$base_floor" "$matching_head_commit"; then
  :
else
  ancestor_status=$?
  if [ "$ancestor_status" -eq 1 ]; then
    fail "generated files roll back the canonical release carried by the PR base"
  fi
  fail "could not compare PR base and head release provenance"
fi

echo "Release-please-generated files match canonical upstream commit ${matching_head_commit}. OK."
