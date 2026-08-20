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

## Pipeline fix-commit HEAD continuity is anchored, not a commit-ownership gap

`commitAgentFixes` (`internal/pipeline/steps/common_fix.go`) commits an agent fix,
rereads live HEAD, and admits only a forward descendant of the pipeline's own recorded
head (`sctx.Run.HeadSHA`) through `assertPipelineHeadContinuity`. The recorded head is
the anchor: it lives in the single daemon process's in-memory `Run` struct, is written
only by no-mistakes commit code, and is never derived from the mutable worktree. The
guard runs at every post-review step entry and around every advancing commit, so a
clobbered lineage is caught at the next boundary while the anchor still holds the
pre-clobber reviewed head. The doc comment on `assertPipelineHeadContinuity` owns the
full rationale, including the incident (run `01KXC3SD5NZYMERGDS68Z1C8ER`) it was built
for.

By deliberate scope, this tool does not defend against an adversarial process mutating
the shared worktree between the fix commit and the immediately following HEAD reread;
that writer is outside the stated threat model, and hardening it (a CAS/`commit-tree`
reconciliation layer) is intentionally not built. Do not re-raise the fix-commit
reconciliation, its post-commit HEAD reread, or its forward-descendant continuity check
as an unresolved commit-ownership or "adopts external commits" finding.
