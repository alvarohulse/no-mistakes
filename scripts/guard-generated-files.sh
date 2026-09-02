#!/bin/sh

set -eu

PROVENANCE_BOOTSTRAP_COMMIT=cf50e0a35e0e635d114dbaeedd496374482d2c16
PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT=db354ad276cb8dce961802d7091a5d618b2417b2
PROVENANCE_TAG_PREFIX=refs/tags/no-mistakes/generated-file-provenance
PROVENANCE_REF_PREFIX=refs/no-mistakes/guard-generated-files/authenticated-releases
MAX_PR_COMMITS=512
MAX_PR_COMMIT_PARENTS=32
MAX_PR_GENERATED_CHANGES=128

fail() {
  printf '::error::%s\n' "$*" >&2
  exit 1
}

generated_entries() {
  generated_entries_treeish=$1
  git ls-tree "$generated_entries_treeish" -- CHANGELOG.md .release-please-manifest.json
}

generated_entries_are_regular() {
  generated_entries_are_regular_entries=$1
  generated_entries_are_regular_changelog=false
  generated_entries_are_regular_manifest=false

  set -- $generated_entries_are_regular_entries
  if [ "$#" -ne 8 ]; then
    return 1
  fi
  while [ "$#" -gt 0 ]; do
    if [ "$1" != 100644 ] || [ "$2" != blob ]; then
      return 1
    fi
    case "$4" in
      CHANGELOG.md) generated_entries_are_regular_changelog=true ;;
      .release-please-manifest.json) generated_entries_are_regular_manifest=true ;;
      *) return 1 ;;
    esac
    shift 4
  done
  [ "$generated_entries_are_regular_changelog" = true ] &&
    [ "$generated_entries_are_regular_manifest" = true ]
}

generated_entries_tuple() {
  generated_entries_tuple_entries=$1
  set -- $generated_entries_tuple_entries
  if [ "$#" -ne 8 ]; then
    fail "could not form the generated-file entry tuple"
  fi
  printf '%s:%s:%s:%s:%s:%s:%s:%s\n' "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8"
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

provenance_precedes() {
  provenance_precedes_older=$1
  provenance_precedes_newer=$2

  if is_ancestor "$provenance_precedes_older" "$provenance_precedes_newer"; then
    return 0
  fi

  provenance_precedes_current=$provenance_precedes_newer
  provenance_precedes_seen=
  while :; do
    case " $provenance_precedes_seen " in
      *" $provenance_precedes_current "*) fail "canonical generated-file provenance lineage is cyclic" ;;
    esac
    provenance_precedes_seen="${provenance_precedes_seen} ${provenance_precedes_current}"
    if ! provenance_precedes_parent_matches=$(printf '%s\n' "$canonical_candidate_parent_records" | awk -v candidate="$provenance_precedes_current" '
      NF == 2 && $1 == candidate { print $2 }
    '); then
      fail "could not inspect canonical generated-file provenance lineage"
    fi
    set -- $provenance_precedes_parent_matches
    if [ "$#" -eq 0 ]; then
      return 1
    fi
    if [ "$#" -ne 1 ]; then
      fail "canonical generated-file provenance lineage is ambiguous"
    fi
    provenance_precedes_parent_tuple=$1
    if [ "$provenance_precedes_parent_tuple" = - ]; then
      return 1
    fi
    if ! provenance_precedes_candidates=$(printf '%s\n' "$canonical_candidate_records" | awk -v tuple="$provenance_precedes_parent_tuple" '
      NF == 2 && $2 == tuple { print $1 }
    '); then
      fail "could not inspect canonical generated-file provenance lineage"
    fi
    set -- $provenance_precedes_candidates
    if [ "$#" -eq 0 ]; then
      return 1
    fi
    if [ "$#" -ne 1 ]; then
      fail "canonical generated-file provenance lineage is ambiguous"
    fi
    provenance_precedes_current=$1
    if [ "$provenance_precedes_current" = "$provenance_precedes_older" ]; then
      return 0
    fi
  done
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

commits_touching_generated_files() {
  commits_touching_generated_files_list=$1

  if [ -z "$commits_touching_generated_files_list" ]; then
    return 0
  fi
  if ! commits_touching_generated_files_result=$(
    printf '%s\n' "$commits_touching_generated_files_list" |
      git diff-tree --stdin --root -r -m --name-only --format='%H' -- \
        CHANGELOG.md \
        .release-please-manifest.json |
      awk '
        /^[0-9a-f]{40}$/ && !seen[$0]++ { printf "%s ", $0 }
      '
  ); then
    fail "could not identify commits that change generated files"
  fi
  printf '%s\n' "$commits_touching_generated_files_result"
}

candidate_ancestors() {
  candidate_ancestors_treeish=$1
  candidate_ancestors_candidates=$2

  if [ -z "$candidate_ancestors_candidates" ]; then
    return 0
  fi
  if ! candidate_ancestors_history=$(git rev-list "$candidate_ancestors_treeish"); then
    fail "could not enumerate provenance ancestry"
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
  if [ -n "$pending_release_candidate" ] && [ "$1" = "$pending_release_candidate" ]; then
    return 0
  fi
  case " $canonical_candidates " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

preserves_base_provenance() {
  preserves_base_provenance_commit=$1

  if ! preserves_base_provenance_entries=$(generated_entries "$preserves_base_provenance_commit"); then
    fail "could not inspect generated entries while preserving PR base provenance"
  fi
  if [ "$preserves_base_provenance_entries" != "$base_entries" ]; then
    return 1
  fi
  if ! preserves_base_provenance_ancestors=$(candidate_ancestors \
    "$preserves_base_provenance_commit" \
    "$canonical_candidates"); then
    exit 1
  fi
  for preserves_base_provenance_ancestor in $preserves_base_provenance_ancestors; do
    if ! provenance_precedes "$preserves_base_provenance_ancestor" "$base_floor"; then
      return 1
    fi
  done
  return 0
}

canonical_candidate_for_tuple() {
  canonical_candidate_for_tuple_value=$1
  canonical_candidate_for_tuple_label=$2

  if ! canonical_candidate_for_tuple_matches=$(printf '%s\n' "$canonical_candidate_records" | awk -v tuple="$canonical_candidate_for_tuple_value" '
    NF == 2 && $2 == tuple { print $1 }
  '); then
    fail "could not match ${canonical_candidate_for_tuple_label} to canonical provenance"
  fi
  set -- $canonical_candidate_for_tuple_matches
  if [ "$#" -ne 1 ]; then
    fail "generated files changed in a noncanonical commit: ${canonical_candidate_for_tuple_label} must match exactly one canonical generated-file tuple"
  fi
  canonical_tuple_candidate=$1
}

trusted_floor_for_entries() {
  trusted_floor_for_entries_value=$1
  trusted_floor_for_entries_label=$2

  if [ "$trusted_floor_for_entries_value" = "$bootstrap_entries" ]; then
    trusted_entries_floor=$bootstrap_floor
    return 0
  fi
  if ! trusted_floor_for_entries_tuple=$(generated_entries_tuple "$trusted_floor_for_entries_value"); then
    exit 1
  fi
  canonical_candidate_for_tuple \
    "$trusted_floor_for_entries_tuple" \
    "$trusted_floor_for_entries_label"
  trusted_entries_floor=$canonical_tuple_candidate
}

is_trusted_base_first_parent_commit() {
  case "${trusted_base_first_parent_commit_set} " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

derive_provenance() {
  derive_provenance_treeish=$1
  derive_provenance_label=$2
  derive_provenance_require_candidate=${3-false}

  if ! derive_provenance_entries=$(generated_entries "$derive_provenance_treeish"); then
    fail "could not inspect generated entries for ${derive_provenance_label}"
  fi

  if ! derive_provenance_ancestors=$(candidate_ancestors \
    "$derive_provenance_treeish" \
    "$canonical_candidates"); then
    exit 1
  fi

  if ! derive_provenance_tuple=$(generated_entries_tuple "$derive_provenance_entries"); then
    exit 1
  fi
  if ! derive_provenance_tuple_state=$(
    {
      printf '%s\n' "$canonical_candidate_records"
      printf '%s\n' __CANDIDATE_ANCESTORS__
      printf '%s\n' "$derive_provenance_ancestors"
    } | awk -v target="$derive_provenance_tuple" '
      $0 == "__CANDIDATE_ANCESTORS__" { in_ancestors = 1; next }
      !in_ancestors && NF == 2 { tuples[$1] = $2; next }
      in_ancestors && ($1 in tuples) {
        carried[tuples[$1]]++
        if (tuples[$1] == target) matching++
      }
      END {
        duplicate = "-"
        for (tuple in carried) {
          if (carried[tuple] > 1) {
            duplicate = tuple
            break
          }
        }
        print matching + 0, duplicate
      }
    '
  ); then
    fail "could not validate canonical generated-file tuple uniqueness"
  fi
  set -- $derive_provenance_tuple_state
  derive_provenance_matching_count=$1
  derive_provenance_duplicate_tuple=$2
  if [ "$derive_provenance_duplicate_tuple" != - ]; then
    fail "${derive_provenance_label} carries more than one canonical release candidate for a generated-file tuple"
  fi
  if [ "$derive_provenance_require_candidate" = true ] && [ "$derive_provenance_matching_count" -ne 1 ]; then
    fail "${derive_provenance_label} must carry exactly one canonical release candidate for its generated-file tuple"
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

audit_trusted_base_first_parent() {
  audit_trusted_base_first_parent_list=$1
  trusted_base_floor=$2
  trusted_base_transition_tuples=

  if [ -z "$audit_trusted_base_first_parent_list" ]; then
    return 0
  fi
  if ! trusted_base_first_parent_graph=$(
    printf '%s\n' "$audit_trusted_base_first_parent_list" |
      git rev-list --parents --no-walk=unsorted --stdin
  ); then
    fail "could not inspect trusted base first-parent history"
  fi
  if ! trusted_base_first_parent_graph_commits=$(awk '{ print $1 }' <<EOF
$trusted_base_first_parent_graph
EOF
  ); then
    fail "could not verify trusted base first-parent history"
  fi
  if [ "$trusted_base_first_parent_graph_commits" != "$audit_trusted_base_first_parent_list" ]; then
    fail "could not verify trusted base first-parent history"
  fi

  while IFS= read -r commit_and_parents; do
    set -- $commit_and_parents
    commit=$1
    shift
    if [ "$#" -lt 1 ]; then
      fail "trusted base first-parent commit has no parent"
    fi
    first_parent=$1
    if ! commit_entries=$(generated_entries "$commit"); then
      fail "could not inspect generated entries in trusted base first-parent history"
    fi
    if ! first_parent_entries=$(generated_entries "$first_parent"); then
      fail "could not inspect generated entries in a trusted base first parent"
    fi

    if [ "$#" -eq 1 ]; then
      if [ "$commit_entries" = "$first_parent_entries" ]; then
        continue
      fi
      if ! generated_entries_are_regular "$commit_entries"; then
        fail "trusted base rewrite entries must be regular non-executable blobs"
      fi
      if ! transition_tuple=$(generated_entries_tuple "$commit_entries"); then
        exit 1
      fi
      canonical_candidate_for_tuple "$transition_tuple" "trusted base rewrite"
      transition_candidate=$canonical_tuple_candidate
    else
      matches_parent=false
      for parent in "$@"; do
        if ! parent_entries=$(generated_entries "$parent"); then
          fail "could not inspect generated entries in a trusted base merge parent"
        fi
        if [ "$commit_entries" = "$parent_entries" ]; then
          matches_parent=true
        fi
      done
      if [ "$matches_parent" = false ]; then
        fail "merge commit synthesizes generated entries not present in any parent"
      fi

      transition_changed=true
      if [ "$commit_entries" = "$first_parent_entries" ]; then
        transition_candidate=$trusted_base_floor
        transition_changed=false
      else
        trusted_floor_for_entries "$commit_entries" "trusted base generated-changing merge commit"
        transition_candidate=$trusted_entries_floor
        if ! transition_tuple=$(generated_entries_tuple "$commit_entries"); then
          exit 1
        fi
      fi
      for parent in "$@"; do
        if ! parent_entries=$(generated_entries "$parent"); then
          fail "could not inspect generated entries in a trusted base merge parent"
        fi
        trusted_floor_for_entries "$parent_entries" "trusted base merge parent"
        if ! provenance_precedes "$trusted_entries_floor" "$transition_candidate"; then
          fail "trusted base generated files roll back canonical provenance"
        fi
      done
      if [ "$transition_changed" = false ]; then
        continue
      fi
    fi

    if ! provenance_precedes "$trusted_base_floor" "$transition_candidate"; then
      fail "trusted base generated files roll back canonical provenance"
    fi
    case " $trusted_base_transition_tuples " in
      *" $transition_tuple "*) fail "trusted base reuses a canonical generated-file tuple" ;;
    esac
    trusted_base_transition_tuples="${trusted_base_transition_tuples} ${transition_tuple}"
    trusted_base_floor=$transition_candidate
  done <<EOF
$trusted_base_first_parent_graph
EOF
}

audit_commits() {
  audit_commits_list=$1
  audit_commits_label=$2
  audit_commits_trust=$3
  audited_generated_files_touched=false
  audited_generated_change_count=0

  if [ -z "$audit_commits_list" ]; then
    return 0
  fi
  if ! audit_commit_graph=$(
    printf '%s\n' "$audit_commits_list" |
      git rev-list --parents --no-walk=unsorted --stdin
  ); then
    fail "could not inspect ${audit_commits_label} commit parents"
  fi
  if ! audit_commit_graph_commits=$(awk '{ print $1 }' <<EOF
$audit_commit_graph
EOF
  ); then
    fail "could not verify ${audit_commits_label} commit identities"
  fi
  if [ "$audit_commit_graph_commits" != "$audit_commits_list" ]; then
    fail "could not verify ${audit_commits_label} commit identities"
  fi

  if ! audit_commits_generated_changes=$(commits_touching_generated_files "$audit_commits_list"); then
    exit 1
  fi

  while IFS= read -r commit_and_parents; do
    set -- $commit_and_parents
    commit=$1
    shift
    if [ "$#" -lt 1 ]; then
      fail "${audit_commits_label} commit has no parent"
    fi
    if [ "$audit_commits_trust" = untrusted ] && [ "$#" -gt "$MAX_PR_COMMIT_PARENTS" ]; then
      fail "${audit_commits_label} commit has too many parents"
    fi
    if [ "$audit_commits_trust" = trusted ] && is_trusted_base_first_parent_commit "$commit"; then
      continue
    fi
    case " $audit_commits_generated_changes " in
      *" $commit "*) ;;
      *) continue ;;
    esac
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
      if [ "$audit_commits_trust" = untrusted ] && [ "$audited_generated_change_count" -gt "$MAX_PR_GENERATED_CHANGES" ]; then
        fail "${audit_commits_label} has too many generated-file changes"
      fi
      if [ "$audit_commits_trust" = trusted ]; then
        trusted_floor_for_entries "$parent_entries" "${audit_commits_label} commit parent"
        trusted_parent_floor=$trusted_entries_floor
        trusted_floor_for_entries "$commit_entries" "${audit_commits_label} rewrite"
        trusted_commit_floor=$trusted_entries_floor
        if ! provenance_precedes "$trusted_parent_floor" "$trusted_commit_floor"; then
          fail "trusted base generated files roll back canonical provenance"
        fi
        continue
      fi
      if ! is_canonical_candidate "$commit"; then
        fail "generated files changed in a noncanonical commit"
      fi
      if [ -n "$pending_release_candidate" ] && [ "$commit" = "$pending_release_candidate" ]; then
        continue
      fi
      derive_provenance "$commit" "${audit_commits_label} generated-changing commit" true
      if [ "$audit_commits_trust" = untrusted ] && ! provenance_precedes "$base_floor" "$provenance_floor"; then
        fail "PR generated files roll back canonical provenance"
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
    if [ "$audit_commits_trust" = untrusted ] && [ "$audited_generated_change_count" -gt "$MAX_PR_GENERATED_CHANGES" ]; then
      fail "${audit_commits_label} has too many generated-file changes"
    fi
    if [ "$audit_commits_trust" = trusted ]; then
      trusted_floor_for_entries "$commit_entries" "${audit_commits_label} merge commit"
      trusted_merge_floor=$trusted_entries_floor
      for parent in "$@"; do
        if ! parent_entries=$(generated_entries "$parent"); then
          fail "could not inspect generated entries in a ${audit_commits_label} merge parent"
        fi
        trusted_floor_for_entries "$parent_entries" "${audit_commits_label} merge parent"
        if ! provenance_precedes "$trusted_entries_floor" "$trusted_merge_floor"; then
          fail "trusted base generated files roll back canonical provenance"
        fi
      done
      continue
    fi
    if [ "$audit_commits_trust" = untrusted ] && preserves_base_provenance "$commit"; then
      continue
    fi
    derive_provenance "$commit" "${audit_commits_label} generated-changing merge commit" true
    if [ "$audit_commits_trust" = untrusted ] && ! provenance_precedes "$base_floor" "$provenance_floor"; then
      fail "PR generated files roll back canonical provenance"
    fi
  done <<EOF
$audit_commit_graph
EOF
}

if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  fail "usage: guard-generated-files.sh <base-sha> <head-sha> <canonical-upstream-url> <release-attestation-url> [authenticated-pending-release-sha]"
fi

BASE_SHA=$1
HEAD_SHA=$2
CANONICAL_UPSTREAM_URL=$3
RELEASE_ATTESTATION_URL=$4
PENDING_RELEASE_SHA=${5-}
CANONICAL_REF=refs/no-mistakes/guard-generated-files/canonical-main
pending_release_candidate=
expected_files=$(printf '%s\n' .release-please-manifest.json CHANGELOG.md | LC_ALL=C sort)

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
  "$MAX_PR_COMMITS" \
  "${BASE_SHA}..${HEAD_SHA}"); then
  exit 1
fi

if ! base_entries=$(generated_entries "$BASE_SHA"); then
  fail "could not inspect generated entries at the PR base"
fi
if ! head_entries=$(generated_entries "$HEAD_SHA"); then
  fail "could not inspect generated entries at the PR head"
fi
if ! generated_entries_are_regular "$base_entries" || ! generated_entries_are_regular "$head_entries"; then
  fail "the PR base and head must contain both generated files as regular non-executable blobs"
fi

if ! git fetch --no-tags --force "$CANONICAL_UPSTREAM_URL" \
  "+refs/heads/main:${CANONICAL_REF}"; then
  fail "could not fetch canonical upstream main"
fi
if ! canonical_main=$(git rev-parse --verify "${CANONICAL_REF}^{commit}"); then
  fail "canonical upstream main did not resolve to a commit"
fi

if ! git fetch --no-tags --force --prune "$RELEASE_ATTESTATION_URL" \
  "+${PROVENANCE_TAG_PREFIX}/*:${PROVENANCE_REF_PREFIX}/*"; then
  fail "could not fetch release attestations"
fi

if ! authenticated_release_refs=$(git for-each-ref \
  --format='%(refname) %(objecttype) %(objectname)' \
  "${PROVENANCE_REF_PREFIX}/"); then
  fail "could not enumerate authenticated release provenance"
fi
authenticated_candidate_records=
while IFS=' ' read -r authenticated_ref authenticated_type authenticated_sha; do
  if [ -z "$authenticated_ref" ]; then
    continue
  fi
  case "$authenticated_ref" in
    "${PROVENANCE_REF_PREFIX}/"*) ;;
    *) fail "authenticated release provenance has an invalid ref" ;;
  esac
  authenticated_ref_sha=${authenticated_ref#"${PROVENANCE_REF_PREFIX}/"}
  if ! printf '%s\n' "$authenticated_ref_sha" | grep -Eq '^[0-9a-f]{40}$' ||
    [ "$authenticated_type" != commit ] ||
    [ "$authenticated_sha" != "$authenticated_ref_sha" ]; then
    fail "authenticated release provenance does not name its exact commit"
  fi
  if ! authenticated_commit_and_parent=$(git rev-list --parents -n 1 "$authenticated_sha"); then
    fail "could not inspect authenticated release provenance"
  fi
  set -- $authenticated_commit_and_parent
  if [ "$#" -ne 2 ] || [ "$1" != "$authenticated_sha" ]; then
    fail "authenticated release provenance must identify a single-parent commit"
  fi
  authenticated_parent=$2
  if ! authenticated_parent_entries=$(generated_entries "$authenticated_parent"); then
    fail "could not inspect authenticated release provenance parent entries"
  fi
  if ! generated_entries_are_regular "$authenticated_parent_entries"; then
    fail "authenticated release provenance parent entries must be regular non-executable blobs"
  fi
  if ! authenticated_parent_tuple=$(generated_entries_tuple "$authenticated_parent_entries"); then
    exit 1
  fi
  if ! authenticated_files=$(git diff --no-renames --name-only \
    "$authenticated_parent" "$authenticated_sha"); then
    fail "could not inspect authenticated release provenance files"
  fi
  authenticated_files=$(printf '%s\n' "$authenticated_files" | LC_ALL=C sort)
  if [ "$authenticated_files" != "$expected_files" ]; then
    fail "authenticated release provenance must change only the generated files"
  fi
  if ! authenticated_entries=$(generated_entries "$authenticated_sha"); then
    fail "could not inspect authenticated release provenance entries"
  fi
  if ! generated_entries_are_regular "$authenticated_entries"; then
    fail "authenticated release provenance entries must be regular non-executable blobs"
  fi
  if ! authenticated_tuple=$(generated_entries_tuple "$authenticated_entries"); then
    exit 1
  fi
  if [ -z "$authenticated_candidate_records" ]; then
    authenticated_candidate_records="${authenticated_sha} ${authenticated_tuple} ${authenticated_parent_tuple}"
  else
    authenticated_candidate_records="${authenticated_candidate_records}
${authenticated_sha} ${authenticated_tuple} ${authenticated_parent_tuple}"
  fi
done <<EOF
$authenticated_release_refs
EOF

if ! canonical_generated_commits=$(git rev-list \
  --full-history \
  "$canonical_main" \
  -- \
  CHANGELOG.md \
  .release-please-manifest.json); then
  fail "could not enumerate canonical generated-file commits"
fi
if ! canonical_commit_graph=$(
  printf '%s\n' "$canonical_generated_commits" |
    git rev-list --parents --no-walk=unsorted --stdin
); then
  fail "could not inspect canonical upstream commit parents"
fi
if ! canonical_commit_graph_commits=$(awk '{ print $1 }' <<EOF
$canonical_commit_graph
EOF
); then
  fail "could not verify canonical upstream commit identities"
fi
if [ "$canonical_commit_graph_commits" != "$canonical_generated_commits" ]; then
  fail "could not verify canonical upstream commit identities"
fi
if canonical_candidate_pairs=$(awk '
  $0 == "__CANONICAL_COMMIT_GRAPH__" { in_graph = 1; next }
  !in_graph { generated[$1] = 1; next }
  NF == 2 && ($1 in generated) { print $1, $2 }
' <<EOF
$canonical_generated_commits
__CANONICAL_COMMIT_GRAPH__
$canonical_commit_graph
EOF
); then
  :
else
  fail "could not select canonical upstream release candidates"
fi

canonical_candidates=
canonical_candidate_records=
canonical_candidate_parent_records=

set -- $canonical_candidate_pairs
while [ "$#" -gt 0 ]; do
  commit=$1
  parent=$2
  shift 2
  if [ "$commit" = "$PROVENANCE_BOOTSTRAP_COMMIT" ]; then
    continue
  fi
  if ! canonical_files=$(git diff --no-renames --name-only "$parent" "$commit"); then
    fail "could not inspect canonical upstream commit files"
  fi
  canonical_files=$(printf '%s\n' "$canonical_files" | LC_ALL=C sort)
  if [ "$canonical_files" != "$expected_files" ]; then
    continue
  fi
  if ! candidate_entries=$(generated_entries "$commit"); then
    fail "could not inspect canonical release entries"
  fi
  if ! generated_entries_are_regular "$candidate_entries"; then
    continue
  fi
  canonical_candidates="${canonical_candidates} ${commit}"
  if ! candidate_tuple=$(generated_entries_tuple "$candidate_entries"); then
    exit 1
  fi
  if ! candidate_parent_entries=$(generated_entries "$parent"); then
    fail "could not inspect canonical release parent entries"
  fi
  candidate_parent_tuple=-
  if generated_entries_are_regular "$candidate_parent_entries"; then
    if ! candidate_parent_tuple=$(generated_entries_tuple "$candidate_parent_entries"); then
      exit 1
    fi
  fi
  if [ -z "$canonical_candidate_records" ]; then
    canonical_candidate_records="${commit} ${candidate_tuple}"
    canonical_candidate_parent_records="${commit} ${candidate_parent_tuple}"
  else
    canonical_candidate_records="${canonical_candidate_records}
${commit} ${candidate_tuple}"
    canonical_candidate_parent_records="${canonical_candidate_parent_records}
${commit} ${candidate_parent_tuple}"
  fi
done

while IFS=' ' read -r authenticated_sha authenticated_tuple authenticated_parent_tuple; do
  if [ -z "$authenticated_sha" ]; then
    continue
  fi
  if printf '%s\n' "$canonical_candidate_records" | awk -v tuple="$authenticated_tuple" '
    NF == 2 && $2 == tuple { found = 1 }
    END { exit found ? 0 : 1 }
  '; then
    continue
  fi
  canonical_candidates="${canonical_candidates} ${authenticated_sha}"
  if [ -z "$canonical_candidate_records" ]; then
    canonical_candidate_records="${authenticated_sha} ${authenticated_tuple}"
    canonical_candidate_parent_records="${authenticated_sha} ${authenticated_parent_tuple}"
  else
    canonical_candidate_records="${canonical_candidate_records}
${authenticated_sha} ${authenticated_tuple}"
    canonical_candidate_parent_records="${canonical_candidate_parent_records}
${authenticated_sha} ${authenticated_parent_tuple}"
  fi
done <<EOF
$authenticated_candidate_records
EOF

if ! duplicate_candidate_tuple=$(printf '%s\n' "$canonical_candidate_records" | awk '
  NF == 2 { count[$2]++ }
  END {
    for (tuple in count) {
      if (count[tuple] > 1) {
        print tuple
        exit
      }
    }
  }
'); then
  fail "could not validate canonical generated-file tuple uniqueness"
fi
if [ -n "$duplicate_candidate_tuple" ]; then
  fail "canonical upstream reuses a generated-file tuple"
fi

if ! invalid_candidate_lineage=$(
  {
    printf '%s\n' "$canonical_candidate_records"
    printf '%s\n' __CANDIDATE_PARENTS__
    printf '%s\n' "$canonical_candidate_parent_records"
  } | awk '
    function visit(candidate, predecessor) {
      state[candidate] = 1
      predecessor = candidate_by_tuple[parent_tuple[candidate]]
      if (predecessor != "") {
        if (state[predecessor] == 1) {
          print predecessor
          exit
        }
        if (state[predecessor] == 0) visit(predecessor)
      }
      state[candidate] = 2
    }
    $0 == "__CANDIDATE_PARENTS__" { in_parents = 1; next }
    !in_parents && NF == 2 {
      candidates[$1] = 1
      candidate_by_tuple[$2] = $1
      next
    }
    in_parents && NF == 2 {
      parent_tuple[$1] = $2
      parent_count[$1]++
    }
    END {
      for (candidate in candidates) {
        if (parent_count[candidate] != 1) {
          print candidate
          exit
        }
      }
      for (candidate in candidates) {
        if (state[candidate] == 0) visit(candidate)
      }
    }
  '
); then
  fail "could not validate canonical generated-file provenance lineage"
fi
if [ -n "$invalid_candidate_lineage" ]; then
  fail "canonical generated-file provenance lineage is cyclic or incomplete"
fi

if [ -n "$PENDING_RELEASE_SHA" ]; then
  if ! verified_pending_release=$(git rev-parse --verify "${PENDING_RELEASE_SHA}^{commit}"); then
    fail "authenticated pending release is not a commit"
  fi
  if [ "$verified_pending_release" != "$HEAD_SHA" ]; then
    fail "authenticated pending release must be the exact PR head"
  fi
  if ! pending_release_commit_and_parent=$(git rev-list --parents -n 1 "$verified_pending_release"); then
    fail "could not inspect the authenticated pending release"
  fi
  set -- $pending_release_commit_and_parent
  if [ "$#" -ne 2 ] || [ "$1" != "$verified_pending_release" ] || [ "$2" != "$BASE_SHA" ]; then
    fail "authenticated pending release must be a single commit on the exact PR base"
  fi
  if ! pending_release_files=$(git diff --no-renames --name-only "$BASE_SHA" "$verified_pending_release"); then
    fail "could not inspect authenticated pending release files"
  fi
  pending_release_files=$(printf '%s\n' "$pending_release_files" | LC_ALL=C sort)
  if [ "$pending_release_files" != "$expected_files" ]; then
    fail "authenticated pending release must change only the release-please-generated files"
  fi
  if ! pending_release_entries=$(generated_entries "$verified_pending_release"); then
    fail "could not inspect authenticated pending release entries"
  fi
  if ! generated_entries_are_regular "$pending_release_entries"; then
    fail "authenticated pending release entries must be regular non-executable blobs"
  fi
  if [ "$pending_release_entries" = "$base_entries" ]; then
    fail "authenticated pending release must advance the generated files"
  fi
  if ! pending_release_tuple=$(generated_entries_tuple "$pending_release_entries"); then
    exit 1
  fi
  if printf '%s\n' "$canonical_candidate_records" | awk -v tuple="$pending_release_tuple" '
    NF == 2 && $2 == tuple { found = 1 }
    END { exit found ? 0 : 1 }
  '; then
    fail "authenticated pending release reuses a canonical generated-file tuple"
  fi
  pending_release_candidate=$verified_pending_release
fi

if ! bootstrap_commit_and_parent=$(git rev-list --parents -n 1 "$PROVENANCE_BOOTSTRAP_COMMIT"); then
  fail "could not inspect the pinned provenance bootstrap"
fi
set -- $bootstrap_commit_and_parent
if [ "$#" -ne 2 ] || [ "$1" != "$PROVENANCE_BOOTSTRAP_COMMIT" ]; then
  fail "pinned provenance bootstrap must be a single-parent commit"
fi
bootstrap_parent=$2
if ! bootstrap_entries=$(generated_entries "$PROVENANCE_BOOTSTRAP_COMMIT"); then
  fail "could not inspect pinned provenance bootstrap entries"
fi
if ! generated_entries_are_regular "$bootstrap_entries"; then
  fail "pinned provenance bootstrap entries must be regular non-executable blobs"
fi
if ! bootstrap_adoption_entries=$(generated_entries "$PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT"); then
  fail "could not inspect the pinned provenance mainline adoption"
fi
if [ "$bootstrap_adoption_entries" != "$bootstrap_entries" ]; then
  fail "pinned provenance mainline adoption does not preserve the bootstrap entries"
fi
if ! is_ancestor "$PROVENANCE_BOOTSTRAP_COMMIT" "$PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT"; then
  fail "pinned provenance mainline adoption does not carry the bootstrap"
fi
if ! base_first_parent_history=$(git rev-list --first-parent "$BASE_SHA"); then
  fail "could not enumerate trusted base first-parent history"
fi
base_carries_pinned_adoption=false
for base_first_parent_commit in $base_first_parent_history; do
  if [ "$base_first_parent_commit" = "$PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT" ]; then
    base_carries_pinned_adoption=true
    break
  fi
done
if [ "$base_carries_pinned_adoption" = false ]; then
  fail "trusted base does not carry the exact pinned provenance adoption on its first-parent history"
fi

derive_provenance "$bootstrap_parent" "pinned provenance bootstrap parent" false
bootstrap_floor=$provenance_floor

if ! trusted_base_first_parent_after_anchor=$(git rev-list \
  --first-parent \
  --reverse \
  "${PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT}..${BASE_SHA}"); then
  fail "could not enumerate post-adoption base first-parent history"
fi
trusted_base_first_parent_commit_set=
for trusted_base_first_parent_commit in $trusted_base_first_parent_after_anchor; do
  trusted_base_first_parent_commit_set="${trusted_base_first_parent_commit_set} ${trusted_base_first_parent_commit}"
done

audit_trusted_base_first_parent "$trusted_base_first_parent_after_anchor" "$bootstrap_floor"
base_floor=$trusted_base_floor

if ! base_history_commits=$(git rev-list "${PROVENANCE_BOOTSTRAP_ADOPTION_COMMIT}..${BASE_SHA}"); then
  fail "could not enumerate post-adoption base commits"
fi
audit_commits "$base_history_commits" "post-adoption base" trusted

audit_commits "$pr_commits" "PR" untrusted
generated_files_touched=$audited_generated_files_touched

if [ -n "$pending_release_candidate" ]; then
  echo "Release-please-generated files match authenticated pending release ${pending_release_candidate}. OK."
  exit 0
fi

if [ "$generated_files_touched" = false ] && [ "$head_entries" = "$base_entries" ]; then
  echo "No release-please-generated files modified. OK."
  exit 0
fi

if [ "$head_entries" = "$base_entries" ]; then
  echo "Release-please-generated files exactly preserve the validated PR base. OK."
  exit 0
fi

derive_provenance "$HEAD_SHA" "PR head" true
matching_head_commit=$provenance_floor

if ! provenance_precedes "$base_floor" "$matching_head_commit"; then
  fail "generated files roll back the canonical release carried by the PR base"
fi

echo "Release-please-generated files match canonical upstream commit ${matching_head_commit}. OK."
