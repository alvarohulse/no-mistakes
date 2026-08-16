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

generated_files_changed=false
for commit in $pr_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect PR commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify PR commit identity"
  fi
  if [ "$#" -lt 2 ]; then
    fail "PR commit has no parent"
  fi
  first_parent=$2
  if ! changed_generated_files=$(git diff --no-renames --name-only \
    "$first_parent" "$commit" -- CHANGELOG.md .release-please-manifest.json); then
    fail "could not inspect generated entries in PR history"
  fi
  if [ -n "$changed_generated_files" ]; then
    generated_files_changed=true
    break
  fi
done

if [ "$generated_files_changed" = false ]; then
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
matching_commit=
matching_commit_count=0

for commit in $pr_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect PR commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify PR commit identity"
  fi

  if git merge-base --is-ancestor "$commit" "$canonical_main"; then
    canonical_commit=true
  else
    ancestor_status=$?
    if [ "$ancestor_status" -eq 1 ]; then
      canonical_commit=false
    else
      fail "could not verify canonical upstream ancestry"
    fi
  fi

  if [ "$canonical_commit" = false ]; then
    if [ "$#" -lt 2 ]; then
      fail "noncanonical PR commit has no parent"
    fi
    first_parent=$2
    if ! commit_entries=$(generated_entries "$commit"); then
      fail "could not inspect generated entries in a PR commit"
    fi
    if ! first_parent_entries=$(generated_entries "$first_parent"); then
      fail "could not inspect generated entries in a PR commit parent"
    fi
    if [ "$commit_entries" = "$first_parent_entries" ]; then
      continue
    fi
    if [ "$#" -lt 3 ]; then
      fail "generated files changed in a noncanonical commit"
    fi

    shift 2
    carried_from_canonical_parent=false
    for parent in "$@"; do
      if git merge-base --is-ancestor "$parent" "$canonical_main"; then
        :
      else
        ancestor_status=$?
        if [ "$ancestor_status" -eq 1 ]; then
          continue
        fi
        fail "could not verify merge-parent ancestry"
      fi

      if git merge-base --is-ancestor "$parent" "$first_parent"; then
        continue
      else
        ancestor_status=$?
        if [ "$ancestor_status" -ne 1 ]; then
          fail "could not verify merge-parent novelty"
        fi
      fi

      if ! parent_entries=$(generated_entries "$parent"); then
        fail "could not inspect generated entries in a merge parent"
      fi
      if [ "$commit_entries" = "$parent_entries" ]; then
        carried_from_canonical_parent=true
        break
      fi
    done

    if [ "$carried_from_canonical_parent" = false ]; then
      fail "generated files changed outside a canonical upstream merge"
    fi
    continue
  fi

  if [ "$#" -ne 2 ]; then
    continue
  fi
  canonical_parent=$2

  if ! canonical_files=$(git diff --no-renames --name-only "$canonical_parent" "$commit"); then
    fail "could not inspect canonical upstream commit files"
  fi
  canonical_files=$(printf '%s\n' "$canonical_files" | LC_ALL=C sort)
  if [ "$canonical_files" != "$expected_files" ]; then
    continue
  fi

  entries_match=true
  for path in CHANGELOG.md .release-please-manifest.json; do
    if ! head_entry=$(git ls-tree "$HEAD_SHA" -- "$path"); then
      fail "could not inspect ${path} at the PR head"
    fi
    if ! canonical_entry=$(git ls-tree "$commit" -- "$path"); then
      fail "could not inspect ${path} in canonical upstream"
    fi
    if [ -z "$head_entry" ] || [ -z "$canonical_entry" ] || \
      [ "$head_entry" != "$canonical_entry" ]; then
      entries_match=false
      break
    fi
  done

  if [ "$entries_match" = true ]; then
    matching_commit=$commit
    matching_commit_count=$((matching_commit_count + 1))
  fi
done

if [ "$matching_commit_count" -ne 1 ]; then
  fail "generated files must exactly match one release-only commit carried from canonical upstream main"
fi

echo "Release-please-generated files match canonical upstream commit ${matching_commit}. OK."
