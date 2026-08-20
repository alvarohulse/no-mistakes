# Greptile review rules for no-mistakes

These standing rules encode design decisions that are already documented and owned
inside this repository. They are not blanket suppressions: each names the code that
owns the rationale so a reviewer can verify it. Do not raise findings that only
restate the concerns below.

## Trusted, opt-in configured commands (build / test / lint) execute branch code by design

`commands.build`, `commands.test`, and `commands.lint` are read only from the pinned
trusted default-branch config, never from a pushed branch, and they run against the
managed worktree only when the maintainer has set `allow_repo_commands: true`. A
contributor cannot self-enable that opt-in. Enabling it is the maintainer's explicit
authorization to execute the branch's own build/test/lint recipes — including
Makefiles and scripts — with the daemon owner's environment. This is the repository's
documented trust model (see the "Repo Config Trust Boundary (security)" section of
`AGENTS.md`), and it is identical for all three commands.

Do not flag executing a trusted, opt-in configured `commands.build` / `commands.test`
/ `commands.lint` against the head worktree as a Build-, Test-, or Lint-specific
security or trust-boundary bypass. It is consistent with how the rest of the pipeline
already works.

## Unconfigured Build evidence is agent-reported by design, exactly like Test and Lint

When `commands.build` is unset, the Build step follows the same contract the Test and
Lint steps already use: the run-wide gate agent discovers and runs the smallest
relevant build or compile commands itself and reports the exact executed commands in
the structured `tested` array (`runAgentBuild` in `internal/pipeline/steps/build.go`,
mirroring `internal/pipeline/steps/test.go`). `hasExecutedBuildEvidence` requires a
non-blank reported command and downgrades a build with no reported command to
"not established", but the evidence is deliberately agent-reported rather than
pipeline-re-executed. The Go-specific command planner, allowlist, and pipeline
re-execution path were removed on purpose and are not being reintroduced, so no
language-specific fallback re-runs the agent's command to independently confirm it.

Do not re-raise the unconfigured Build verdict trusting the agent's `tested` array as
a self-reported-evidence or missing-independent-verification gap. It is identical to
the accepted Test and Lint evidence model, not a Build-specific weakness.

## Fix reconciliation and pipeline-head ownership are pre-existing, owned outside this PR

Placing Build before Review adds one head-continuity guard to the Review step:
`assertReviewHeadContinuity` (`internal/pipeline/steps/review.go`) refuses to run when
the worktree HEAD no longer matches the pipeline's recorded `Run.HeadSHA`. It mirrors
`assertPipelineHeadContinuity` (`internal/pipeline/steps/common_fix.go`) that every
post-Review step already uses; it is a strict addition and removes or weakens nothing.

The mechanics of how a fix round commits and records the pipeline head
(`commitAgentFixes` and the reconciliation reads in
`internal/pipeline/steps/common_fix.go`), including whether a forward, replacement, or
out-of-band descendant commit can become pipeline-owned, are pre-existing code that
this Build-stage change does not modify. Their safety is owned by the
"Post-Review Head Continuity and Push Binding" and
"Rebase Base & Force-Push Safety (data-loss prevention)" sections of `AGENTS.md`.

Do not raise, against this Build-stage change, a finding that the new Review
head-continuity guard fails to also close a reconciliation gap in `common_fix.go`, or
that the fix-reconciliation path can persist a forward/replacement commit before that
guard compares. That behavior is not introduced or altered here; it is owned by the
pre-existing code named above.
