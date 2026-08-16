#!/bin/sh

set -eu

PROVENANCE_BOOTSTRAP_COMMIT=cf50e0a35e0e635d114dbaeedd496374482d2c16
MAX_AUDITED_COMMITS=512
MAX_CANONICAL_CANDIDATES=512
MAX_CANONICAL_COMMITS=4096
MAX_COMMIT_PARENTS=32
MAX_GENERATED_CHANGES=128
MAX_GRAPH_COMMITS=4096

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

generated_entries_regular() {
  generated_entries_regular_treeish=$1
  for generated_entries_regular_path in CHANGELOG.md .release-please-manifest.json; do
    if ! generated_entries_regular_entry=$(git ls-tree "$generated_entries_regular_treeish" -- "$generated_entries_regular_path"); then
      return 1
    fi
    set -- $generated_entries_regular_entry
    if [ "$#" -ne 4 ] || [ "$1" != 100644 ] || [ "$2" != blob ] || [ "$4" != "$generated_entries_regular_path" ]; then
      return 1
    fi
  done
}

is_ancestor() {
  if git merge-base --is-ancestor "$1" "$2"; then
    return 0
  else
    is_ancestor_status=$?
  fi
  if [ "$is_ancestor_status" -eq 1 ]; then
    return 1
  fi
  fail "could not verify canonical upstream ancestry"
}

bounded_rev_list() {
  bounded_rev_list_label=$1
  bounded_rev_list_limit=$2
  shift 2

  if ! bounded_rev_list_commits=$(git rev-list --max-count=$((bounded_rev_list_limit + 1)) "$@"); then
    fail "could not enumerate ${bounded_rev_list_label}"
  fi
  set -- $bounded_rev_list_commits
  if [ "$#" -gt "$bounded_rev_list_limit" ]; then
    fail "${bounded_rev_list_label} exceeds the audit limit"
  fi
  printf '%s\n' "$bounded_rev_list_commits"
}

candidate_ancestors() {
  candidate_ancestors_treeish=$1
  candidate_ancestors_candidates=$2

  if [ -z "$candidate_ancestors_candidates" ]; then
    return 0
  fi
  if ! candidate_ancestors_history=$(bounded_rev_list \
    "provenance ancestry" \
    "$MAX_GRAPH_COMMITS" \
    "$candidate_ancestors_treeish"); then
    exit 1
  fi
  if ! candidate_ancestors_result=$(
    {
      printf '%s\n' $candidate_ancestors_candidates
      printf '%s\n' __CANDIDATE_HISTORY__
      printf '%s\n' $candidate_ancestors_history
    } | awk '
      $0 == "__CANDIDATE_HISTORY__" { in_history = 1; next }
      !in_history { candidates[$1] = 1; next }
      $1 in candidates { print $1 }
    '
  ); then
    fail "could not intersect canonical provenance ancestry"
  fi
  printf '%s\n' "$candidate_ancestors_result"
}

is_canonical_candidate() {
  case " $canonical_candidates " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

derive_provenance() {
  derive_provenance_treeish=$1
  derive_provenance_label=$2

  if ! derive_provenance_entries=$(generated_entries "$derive_provenance_treeish"); then
    fail "could not inspect generated entries for ${derive_provenance_label}"
  fi

  if ! derive_provenance_ancestors=$(candidate_ancestors \
    "$derive_provenance_treeish" \
    "$canonical_candidates"); then
    exit 1
  fi

  derive_provenance_latest=
  if [ -n "$derive_provenance_ancestors" ]; then
    if ! derive_provenance_latest=$(git merge-base --independent $derive_provenance_ancestors); then
      fail "could not determine latest canonical provenance for ${derive_provenance_label}"
    fi
  fi
  set -- $derive_provenance_latest
  derive_provenance_latest_count=$#

  if [ "$derive_provenance_latest_count" -gt 1 ]; then
    fail "${derive_provenance_label} has ambiguous canonical release provenance"
  fi
  if [ "$derive_provenance_latest_count" -eq 1 ]; then
    provenance_floor=$derive_provenance_latest
  else
    if ! derive_provenance_carriers=$(git merge-base --all "$derive_provenance_treeish" "$canonical_main"); then
      fail "could not determine the canonical carrier for ${derive_provenance_label}"
    fi
    set -- $derive_provenance_carriers
    if [ "$#" -ne 1 ]; then
      fail "${derive_provenance_label} must have exactly one canonical carrier"
    fi
    provenance_floor=$1
  fi

  if ! derive_provenance_floor_entries=$(generated_entries "$provenance_floor"); then
    fail "could not inspect canonical provenance for ${derive_provenance_label}"
  fi
  if [ "$derive_provenance_entries" != "$derive_provenance_floor_entries" ]; then
    fail "${derive_provenance_label} generated files do not match its latest canonical provenance"
  fi
}

audit_commits() {
  audit_commits_list=$1
  audit_commits_label=$2
  audited_generated_files_touched=false
  audited_generated_change_count=0

  for commit in $audit_commits_list; do
    if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
      fail "could not inspect ${audit_commits_label} commit parents"
    fi
    set -- $commit_and_parents
    if [ "$1" != "$commit" ]; then
      fail "could not verify ${audit_commits_label} commit identity"
    fi
    shift
    if [ "$#" -lt 1 ]; then
      fail "${audit_commits_label} commit has no parent"
    fi
    if [ "$#" -gt "$MAX_COMMIT_PARENTS" ]; then
      fail "${audit_commits_label} commit has too many parents"
    fi
    if ! commit_entries=$(generated_entries "$commit"); then
      fail "could not inspect generated entries in a ${audit_commits_label} commit"
    fi

    if [ "$#" -eq 1 ]; then
      if ! parent_entries=$(generated_entries "$1"); then
        fail "could not inspect generated entries in a ${audit_commits_label} commit parent"
      fi
      if [ "$commit_entries" = "$parent_entries" ]; then
        continue
      fi
      audited_generated_files_touched=true
      audited_generated_change_count=$((audited_generated_change_count + 1))
      if [ "$audited_generated_change_count" -gt "$MAX_GENERATED_CHANGES" ]; then
        fail "${audit_commits_label} has too many generated-file changes"
      fi
      if ! is_canonical_candidate "$commit"; then
        fail "generated files changed in a noncanonical commit"
      fi
      continue
    fi

    matches_parent=false
    differs_from_parent=false
    for parent in "$@"; do
      if ! parent_entries=$(generated_entries "$parent"); then
        fail "could not inspect generated entries in a ${audit_commits_label} merge parent"
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
    if [ "$differs_from_parent" = false ]; then
      continue
    fi
    audited_generated_files_touched=true
    audited_generated_change_count=$((audited_generated_change_count + 1))
    if [ "$audited_generated_change_count" -gt "$MAX_GENERATED_CHANGES" ]; then
      fail "${audit_commits_label} has too many generated-file changes"
    fi
    derive_provenance "$commit" "${audit_commits_label} generated-changing merge commit"
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

if ! pr_commits=$(bounded_rev_list \
  "PR commits" \
  "$MAX_AUDITED_COMMITS" \
  "${BASE_SHA}..${HEAD_SHA}"); then
  exit 1
fi

if ! base_entries=$(generated_entries "$BASE_SHA"); then
  fail "could not inspect generated entries at the PR base"
fi
if ! head_entries=$(generated_entries "$HEAD_SHA"); then
  fail "could not inspect generated entries at the PR head"
fi
if ! generated_entries_regular "$BASE_SHA" || ! generated_entries_regular "$HEAD_SHA"; then
  fail "the PR base and head must contain both generated files as regular non-executable blobs"
fi

if ! git fetch --no-tags --force "$CANONICAL_UPSTREAM_URL" \
  "+refs/heads/main:${CANONICAL_REF}"; then
  fail "could not fetch canonical upstream main"
fi
if ! canonical_main=$(git rev-parse --verify "${CANONICAL_REF}^{commit}"); then
  fail "canonical upstream main did not resolve to a commit"
fi

expected_files=$(printf '%s\n' .release-please-manifest.json CHANGELOG.md | LC_ALL=C sort)

if ! canonical_commits=$(bounded_rev_list \
  "canonical upstream commits" \
  "$MAX_CANONICAL_COMMITS" \
  "$canonical_main"); then
  exit 1
fi

canonical_candidates=
canonical_candidate_count=0
matching_head_candidates=

for commit in $canonical_commits; do
  if ! commit_and_parents=$(git rev-list --parents -n 1 "$commit"); then
    fail "could not inspect canonical upstream commit parents"
  fi
  set -- $commit_and_parents
  if [ "$1" != "$commit" ]; then
    fail "could not verify canonical upstream commit identity"
  fi
  if [ "$#" -gt $((MAX_COMMIT_PARENTS + 1)) ]; then
    fail "canonical upstream commit has too many parents"
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
  if ! generated_entries_regular "$commit"; then
    continue
  fi
  if ! candidate_entries=$(generated_entries "$commit"); then
    fail "could not inspect canonical release entries"
  fi
  canonical_candidates="${canonical_candidates} ${commit}"
  canonical_candidate_count=$((canonical_candidate_count + 1))
  if [ "$canonical_candidate_count" -gt "$MAX_CANONICAL_CANDIDATES" ]; then
    fail "canonical upstream has too many release candidates"
  fi

  if [ "$candidate_entries" = "$head_entries" ]; then
    matching_head_candidates="${matching_head_candidates} ${commit}"
  fi
done

if ! matching_head_commits=$(candidate_ancestors "$HEAD_SHA" "$matching_head_candidates"); then
  exit 1
fi
set -- $matching_head_commits
matching_head_commit_count=$#
matching_head_commit=${1-}

bootstrap_carried=false
if git cat-file -e "${PROVENANCE_BOOTSTRAP_COMMIT}^{commit}" 2>/dev/null; then
  if is_ancestor "$PROVENANCE_BOOTSTRAP_COMMIT" "$BASE_SHA"; then
    bootstrap_carried=true
  fi
fi

if [ "$bootstrap_carried" = true ]; then
  if ! bootstrap_commit_and_parent=$(git rev-list --parents -n 1 "$PROVENANCE_BOOTSTRAP_COMMIT"); then
    fail "could not inspect the trusted provenance bootstrap"
  fi
  set -- $bootstrap_commit_and_parent
  if [ "$#" -ne 2 ] || [ "$1" != "$PROVENANCE_BOOTSTRAP_COMMIT" ]; then
    fail "trusted provenance bootstrap must be a single-parent commit"
  fi
  bootstrap_parent=$2
  if ! bootstrap_entries=$(generated_entries "$PROVENANCE_BOOTSTRAP_COMMIT"); then
    fail "could not inspect trusted provenance bootstrap entries"
  fi
  if ! generated_entries_regular "$PROVENANCE_BOOTSTRAP_COMMIT"; then
    fail "trusted provenance bootstrap entries must be regular non-executable blobs"
  fi
  if ! base_history_commits=$(bounded_rev_list \
    "post-bootstrap base commits" \
    "$MAX_AUDITED_COMMITS" \
    "${PROVENANCE_BOOTSTRAP_COMMIT}..${BASE_SHA}"); then
    exit 1
  fi
  audit_commits "$base_history_commits" "post-bootstrap base"
  base_history_generated_files_touched=$audited_generated_files_touched

  if [ "$base_history_generated_files_touched" = false ] && [ "$base_entries" = "$bootstrap_entries" ]; then
    derive_provenance "$bootstrap_parent" "trusted provenance bootstrap parent"
    base_floor=$provenance_floor
  else
    derive_provenance "$BASE_SHA" "PR base"
    base_floor=$provenance_floor
  fi
else
  derive_provenance "$BASE_SHA" "PR base"
  base_floor=$provenance_floor
fi

audit_commits "$pr_commits" "PR"
generated_files_touched=$audited_generated_files_touched

if [ "$generated_files_touched" = false ] && [ "$head_entries" = "$base_entries" ]; then
  echo "No release-please-generated files modified. OK."
  exit 0
fi

if [ "$head_entries" = "$base_entries" ]; then
  echo "Release-please-generated files exactly preserve the validated PR base. OK."
  exit 0
fi

if [ "$matching_head_commit_count" -ne 1 ]; then
  fail "generated files must exactly match one release-only commit carried from canonical upstream main"
fi

if ! is_ancestor "$base_floor" "$matching_head_commit"; then
  fail "generated files roll back the canonical release carried by the PR base"
fi

echo "Release-please-generated files match canonical upstream commit ${matching_head_commit}. OK."
