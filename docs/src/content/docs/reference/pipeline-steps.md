---
title: Pipeline Steps
description: Reference for each step in the validation pipeline.
---

This is the per-step reference. For the overview and rationale, see [Pipeline](/no-mistakes/concepts/pipeline/). For the fix loop, see [Auto-Fix Loop](/no-mistakes/concepts/auto-fix/).

```
intent → refresh → review → build → test → document → lint → push → pr → ci
```

Each step can produce findings, request approval, trigger auto-fix, or apply safe fixes during its own pass. Steps that encounter fatal errors stop the pipeline. Steps can also be pre-skipped when starting a run, skipped by the user, or skipped automatically by the pipeline.
Each completed or skipped step contributes bounded evidence to PR rendering: primary commands include round, sequence, redacted display text, outcome, and nullable exit code; non-shell evidence and explicit skip/success explanations fill the remaining cases. Git plumbing is not recorded as step evidence.
In the TUI, yolo mode is an explicit override that auto-resolves paused steps: `auto-fix` and `ask-user` findings are fixed once with every finding selected, fix-review gates are approved, and gates with only `no-op` findings are approved as-is.
Every pipeline agent invocation is prompt-steered to keep intentional writes inside the run worktree and avoid mutating system state outside it.
This is a soft boundary, not OS-level sandbox enforcement.
The steering still allows requested test evidence under the managed temporary `no-mistakes-evidence` directory or the configured in-repo evidence directory, plus incidental temp or cache writes from normal development tools.
Configured shell commands and one-shot agent subprocesses are scoped to their step: when the invocation exits, fails, or is cancelled, no-mistakes terminates remaining child processes it spawned so background workers do not outlive the run.
When configured Build, Test, or Lint command output exceeds 64 KiB, the complete output remains in the authoritative step log while findings, IPC responses, and repair prompts receive a valid-UTF-8 head-and-tail projection capped at 64 KiB. The truncation marker reports the exact original and omitted byte counts and points to `no-mistakes axi logs --step <step> --full` for the complete output.
Commits created by the shared Review, Build, Test, Document, and Lint fix path use the configurable [`commit.fix_message`](/no-mistakes/reference/global-config/#commitfix_message) template.

## Intent

Uses agent-supplied intent when a run provides it, otherwise infers the author's intent from recent local Claude Code, Codex, OpenCode, Rovo Dev, Pi, or GitHub Copilot CLI transcripts.
This is best-effort context, and when available it is included in refresh fixes, review checks and fixes, build detection and fixes, test detection, evidence validation, and fixes, documentation checks and fixes, lint detection and fixes, CI auto-fixes, and PR drafting.

**Behavior:**
- Uses run-supplied intent verbatim and skips transcript-based inference, even when `intent.enabled` is false
- Runs transcript-based inference only when `intent.enabled` is true
- Matches local agent transcripts against non-deleted changed files when present, falling back to all changed files for all-deletion diffs, may use the configured pipeline agent to disambiguate plausible matches, and summarizes the likely author intent with that agent
- Stores the selected text and provenance for later prompts, and records the PR-facing result only on the Intent pipeline step as `{text, source, provided}`
- Logs accepted candidate diagnostics, including source, session, CWD, score, confidence, overlap, decision, and acceptance reason
- Logs the matched source, score, and sanitized inferred intent when a transcript matches
- Records a structured absence reason when disabled, skipped, no matching transcript is found, the diff is empty, or extraction fails

This step does not block the pipeline for missing transcripts, summarization that exceeds the five-minute extraction cap, or other extraction failures, which are reported as skipped outcomes.
It can fail the run only if cleanup fails after the disambiguation agent leaves worktree side effects.

## Refresh

Fetches the latest authoritative remote state and configured pushed-branch target, then incorporates them with the run's selected strategy. The canonical step ID is always `refresh`; displays label the step `Rebase` or `Merge`.

**Behavior:**
- Uses `refresh.strategy: rebase|merge`, overridden by `--refresh-strategy`, and defaults to `rebase`
- Uses the default branch as the base unless `--stacked-on <branch>` selects another base; that stack base is fetched freshly and persisted on the run
- Fetches `origin/<base_branch>` from the remote into the worktree, and also fetches the pushed branch for non-base branches unless the push rewrote branch history
- Without fork routing, the pushed-branch target is `origin/<branch>`
- With GitHub fork routing, the pushed-branch target is the fork branch fetched into `refs/remotes/no-mistakes-push/<branch>`
- If the branch is not the base branch, incorporates the pushed-branch target first, then `origin/<base_branch>`
- Rebase strategy rebases onto each target; merge strategy merges each target with `--no-edit`
- If the push rewrote branch history, skips the pushed-branch refresh target so prior remote autofix commits do not get reintroduced
- If the push rewrote the default branch and `origin/<default_branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- If the branch carries commits from the contributor's local default branch that are not on `origin/<default_branch>`, pauses with an `ask-user` finding instead of silently bundling that local work into the PR
- The local-default check is best-effort and only fires when the local default tip is ahead of `origin/<default_branch>` and is an ancestor of the branch `HEAD`
- Skips targets that don't exist or are already ancestors
- If a fast-forward is possible, does a hard-reset instead of rewriting or merging history
- If the diff against the selected base branch is empty after refresh, completes refresh and skips all remaining pipeline steps
- On conflict: records conflicting files, aborts the in-progress rebase or merge, and reports findings

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue` or `git merge --continue` for the selected strategy. Rebase continuation uses a non-interactive Git environment so Git accepts the existing commit message instead of opening an editor. The prompt includes user intent when available. Manual fix rounds also include any per-conflict user notes, any selected user-authored findings from the TUI or AXI interface, and sanitized prior-round history in the prompt. The Refresh step does not synthesize a fix commit subject; Git preserves the rebased subjects or creates the normal merge commit.

**Default auto-fix limit:** `3`.

## Review

AI code review of your diff.

**Behavior:**
- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Includes user intent when the run has supplied intent or transcript matching found a relevant local agent session; the detailed provenance semantics are documented in [Intent extraction](/no-mistakes/guides/agents/#intent-extraction)
- Treats authoritative intent as enforceable for source-verifiable acceptance criteria, but does not report the absence of a remote branch, push, pull request, or CI state that this run's later Push, PR, or CI step owns
- Removes any returned finding whose sole claim is that one of those same-run delivery outcomes is not present yet, while keeping findings about pre-existing or external pull requests, third-party artifacts, and lifecycle state that the current run does not own
- Keeps the later Push, PR, and CI steps responsible for strictly validating their own outcomes after review completes
- For changes that claim a durable bug fix, reconstructs the concrete failing sequence and required invariant, inspects relevant sibling paths and shared state transitions, and reports an inadequate fix only when source evidence proves the same authorized failure remains reachable; the recommendation targets the earliest supported shared boundary
- Does not treat code shape or duplication alone as evidence of a systemic defect, demand speculative redesign, block explicitly authorized short-term containment merely because a later durable fix is possible, expand the user's scope, or promote optional improvements into blockers
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`
- When a [Review adversary](/no-mistakes/reference/repo-config/#per-step-agent-and-model-routes) is configured and the primary review returns `risk_level: high`, runs one independent cross-vendor adversarial review in a separate cold session and merges its namespaced findings into the same Review gate; low- and medium-risk reviews do not invoke it
- With the default `session_reuse: true`, Claude, Codex, and Cursor reuse one reviewer session across the initial review and every full rereview, and a separate fixer session across review-fix turns
- A resume failure retries the same turn in a fresh session for that role, never skips the full rereview, and unsupported agents run cold
- Atomically records the exact commit examined when a full review completes successfully; a parked review retains its candidate only for recovery, while failed, skipped, superseded, and legacy reviews grant no inferred approval authority

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` pause for approval instead of entering the normal auto-fix loop. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. With the default `auto_fix.review: 0`, blocking review findings park for approval even when their action is `auto-fix`; setting repo or global `auto_fix.review` above `0` re-enables the automatic review fix loop for eligible `auto-fix` findings. Findings with `action: no-op` are informational only. The shared [finding-action model](/no-mistakes/concepts/auto-fix/#finding-actions) owns the behavior for a missing `action`.

**Auto-fix:** the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and which findings the user left unselected.
The fixer applies all selected fixes before running one focused verification limited to the changed area, and it is instructed not to run the complete repository test or lint suite during the fix round.
The dedicated Build, Test, and Lint steps after review remain the authoritative gates, although their coverage may be focused when commands are unconfigured.
Follow-up review passes use the history to avoid re-reporting user-ignored findings unless the code now has a materially different problem.

**Default auto-fix limit:** `0`.

### Post-review HEAD continuity

At entry to every remaining step in the fixed pipeline order - Build, Test, Document, Lint, Push, PR, and CI - no-mistakes compares the live worktree `HEAD` with the pipeline-recorded head. An equal head or a pipeline-descendant commit continues. A backward reset, divergent sibling, or unverifiable relationship fails the run before that step performs work, including for steps that would not create a commit.

## Build

Compiles the changed production code before behavioral testing begins. [`commands.build`](/no-mistakes/reference/repo-config/#commandsbuild) owns the deterministic command contract.

**Behavior:**
- If `commands.build` is set, runs it visibly through the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) in the managed process group. A non-zero exit produces an actionable `error` finding with bounded compiler output; the complete output stays in the Build step log.
- If `commands.build` is empty, asks the routed Build agent for one exact command in a read-only planning pass, executes and records that command itself, and parks when the planner returns no usable command. After a failure, the repair agent fixes the cause and the pipeline reruns the same planned command.
- Build agents are explicitly told not to run tests, linters, formatters, static analysis, or documentation work, except when static analysis or formatting is inseparable from the repository's canonical build command.

**Approval:** objective compile failures use `action: auto-fix` and enter the normal fix loop while attempts remain. A build that cannot be established uses `action: ask-user` and parks for a decision. `action: no-op` findings are informational only.

**Auto-fix:** the routed Build agent receives the previous findings, sanitized prior-round context, user intent, and the exact configured build command when one exists. It applies the smallest build root-cause fix, commits through the shared fix machinery, performs only focused build verification, and then the Build step runs again. Tests, linters, and documentation remain owned by their later stages.

**Default auto-fix limit:** `3`.

## Test

Runs **targeted** local validation of the change and requested intent, then gathers evidence for that intent.
Local Test is never a repository-wide regression-suite substitute; broad regression is owned by remote CI and remains mandatory before a PR is ready.
[`commands.test`](/no-mistakes/reference/repo-config/#commandstest) owns the configuration contract for any explicit baseline command.

**Behavior:**
- If `commands.test` is set in repo config: runs it first as a baseline via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) and captures output. Non-zero exit produces `error` findings. Configure a **targeted** command here (see repo-config); do not treat this field as CI-parity complete-suite configuration.
- If `commands.test` is empty, first asks the routed Test agent for one exact targeted command in a read-only planning pass, then executes and records it. After it passes, an evidence agent gathers evidence and artifacts for the intent; it may run further focused checks or write a focused test itself, but never the complete repository suite. A failure enters repair and the pipeline reruns the same planned command.
- When user intent is available after a configured baseline command passes, the same evidence-oriented agent follow-up runs. Evidence agents return structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`). Both evidence and repair agents are instructed not to run the complete repository test suite; a generic driver instruction asking for broad or full-suite confirmation does not override that product boundary. For UI, HTML, CSS, browser, visual layout, or copy-placement changes, the agent attempts reviewer-visible visual evidence and explains in `testing_summary` when screenshots, images, videos, GIFs, or rendered HTML artifacts are not captured.
- "Do not run everything" is not "run nothing": when no targeted check can establish the intent, the agent must write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- The step records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the temporary `no-mistakes-evidence/<runID>` directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR.
- By default, evidence is stored under the temporary `no-mistakes-evidence/<runID>` directory. With `test.evidence.store_in_repo: true`, evidence is stored under `<test.evidence.dir>/<branch-slug>` inside the worktree, staged during push, and published with the branch. Unsafe, symlinked, or Git-ignored evidence directories fall back to temporary storage for that run.
- Before finishing, test agents are instructed to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files under the dedicated evidence directory.
- Missing evidence for user intent can be reported as a warning with `action: ask-user`.
- If the agent creates a new test file, the step records an informational finding with `action: no-op`; it does not require approval. Modifying, deleting, or renaming an existing test file receives no special finding or gate.

**Approval:** test findings with `action: ask-user` pause for approval, including missing-evidence warnings for user intent and a targeted command plan that could not be established. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives the previous test findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles. Repair mode reproduces the specific failure, applies a root-cause fix, and re-runs only focused verification - not a complete-suite confirmation - then the step's baseline command (configured or the same previously planned one) and the evidence path run again.

**Default auto-fix limit:** `3`.

## Document

Updates matching documentation for code changes and reports only unresolved gaps.

**Behavior:**
- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to find every documentation gap, update docs or doc comments for all gaps it can resolve, verify its edits, and commit any documentation changes under the placement policy
- The placement policy gives each fact one authoritative owner, prefers removing stale duplicates or replacing them with pointers, avoids new documentation surfaces for perceived gaps, and keeps durable incident lessons near their owner instead of in `AGENTS.md`
- `document.instructions` can add trusted default-branch ownership rules for the repository
- Documentation and lint remain separate responsibilities; the Document prompt never runs linters or formatters
- Includes user intent when available
- Returns findings only for unresolved documentation gaps or human judgment calls
- Requires approval whenever any unresolved documentation finding is returned, including `info` findings

**Auto-fix:** documentation fixes happen during the initial document pass. Unresolved findings pause for approval instead of starting another automatic document/fix loop. If you manually trigger a fix from the TUI or AXI interface, the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings, and sanitized prior-round history.

**Default auto-fix limit:** not used for automatic document follow-up loops.

## Lint

Runs linters and static analysis.

**Behavior:**
- If `commands.lint` is set: runs it via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows). Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: asks the routed Lint agent for one exact formatter, linter, or static-analysis command in a read-only planning pass, executes and records it, and parks when no meaningful command can be established. After a failure, the repair agent fixes the cause and the pipeline reruns the same planned command.

**Approval:** lint findings with `action: ask-user` pause for approval, including a command plan that could not be established.
`action: auto-fix` findings stay eligible for the fix loop.
`action: no-op` findings are informational only.

**Auto-fix:** the lint step follows the same pattern as test, whether the command was configured or planned - the agent fixes `action: auto-fix` issues using the previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then the same lint command re-runs.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the configured push target.

**Behavior:**
- If `commands.format` is set, runs it first
- Stages in-repo test evidence artifacts when `test.evidence.store_in_repo` is enabled and the evidence directory is not ignored by Git
- Commits any uncommitted agent changes with message `no-mistakes: apply agent fixes`
- Without fork routing, successful run-start validation selects the upstream URL from the working clone; when it matches the gate worktree's `origin`, the worktree URL is used so embedded credentials retained outside the database can authenticate. If validation fails, the run continues with its prior routing.
- With GitHub fork routing, the push target is `repos.fork_url`
- Immediately before remote mutation, reloads the durable review-approved commit and refuses to push when that binding is missing, malformed, or unreachable
- Requires the commit proposed for push to equal or descend from the review-approved commit, allowing commits made by later pipeline steps without authorizing unrelated history
- Re-reads the push target via `git ls-remote` before pushing
- For existing branches, refuses to force-push when the live remote carries commits the pipeline has not incorporated by patch-id
- Fails closed when the remote safety check cannot verify whether the push would discard existing remote work
- Uses `--force-with-lease=<ref>:<sha>` with an explicit SHA anchor for allowed existing-branch rewrites
- Pushes the exact verified commit SHA instead of mutable worktree `HEAD`
- Treats the branch as already pushed when the remote already points at that verified commit
- Uses regular push for new branches
- Updates the run's head SHA in the database to the exact commit delivered

A remote branch can move without being rejected when all remote commits are already represented in the validated head, or when a run is intentionally rewriting history it already knew about.
Any other out-of-band commit stops the push instead of being overwritten.
Pre-skipping or later skipping Review leaves no approval binding, so Push fails closed unless Push is also skipped.

This step never requires approval - it runs automatically after review, build, test, document, and lint pass.

## PR

Creates a pull request or adopts the existing one for the branch.

**Skipped when:**
- The branch is the default branch
- The upstream host is not GitHub, GitLab, Bitbucket Cloud (`bitbucket.org`), or Azure DevOps (`dev.azure.com` / `*.visualstudio.com`)
- The provider CLI (`gh` or `glab`) is not installed for GitHub or GitLab
- The provider CLI is not authenticated for GitHub or GitLab
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)
- The `az` CLI with the `azure-devops` extension is not installed or not authenticated for Azure DevOps
- A legacy or manually edited GitLab, Bitbucket, or Azure DevOps repo record has `fork_url` set, because fork MR/PR routing is currently GitHub-only

**Behavior:**
- Checks for an existing PR on the branch
- If one exists, discovers it and may retarget its base, but never replaces its body. If none exists, creates a new one.
- Targets the run's `--stacked-on` branch when set; otherwise targets the repository default branch
- If an existing PR targets a different base, retargets it with a base-only update that carries no title or body; matching bases are not sent again
- Uses the provider CLI for GitHub/GitLab, the `az` CLI for Azure DevOps, and the Bitbucket API for Bitbucket Cloud
- For GitHub fork routing, keeps `gh --repo` pointed at the parent repository from `origin`, checks existing PRs with the bare branch name, filters matching PRs by head owner, and creates PRs with `--head <fork-owner>:<branch>`
- PR title: agent-generated from the final branch delta with user intent when available, in conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; when a scope is used, it should be the primary affected real module/package from the changed paths and kept broad rather than file-level. If drafting fails, the fallback uses the neutral title `chore: update pull request` rather than inferring scope from earlier commits.
- One PR agent invocation receives the explicit or inferred intent plus the final diff and returns separate heading-free `summary` and `what_changed` GFM fragments with the title. Code identifiers must use backticks; the renderer inserts the section headings exactly once.
- The built-in PR body includes `## Summary`, optional operator-supplied `## Notes`, final-diff `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections. PR-facing intent provenance lives on the Intent pipeline result instead of a duplicate body section. Only `## What Changed` describes the complete final branch scope; deterministic sections remain evidence for the commit each step inspected.
- Pipeline entries label `refresh` as `Rebase` or `Merge`, render stored command/evidence details for successful steps, and omit PR and CI because those steps are incomplete when the body is created. The built-in body opens the section with a compact attribution table listing each recorded invocation's step and round, top-level agent and invocation mode, and wire-observed nested agents; unreported nested attribution renders as `-`, while a supported stream that observed none renders as `none`. Contract v3 supplies a richer per-invocation/round telemetry record instead: agent, model, provider, start time, duration, nullable token/cache meters, nested-agent observations, and nullable CLI-reported USD cost. Formatter layout and API-price estimation remain formatter-owned.
- Generated PR bodies are capped at 63,488 bytes, leaving a 2 KB safety buffer below GitHub's 65,536-character body limit.
- Under body caps, the attribution table is removed as a complete unit before older Pipeline update rounds are omitted at clean boundaries. The newest update is kept when possible, and omission or truncation is marked explicitly.
- `## Summary`, `## Notes`, `## What Changed`, risk, and testing sections are kept ahead of Pipeline history; if the whole body still overruns after Pipeline history has been shed, a final clamp trims it and adds an explicit marker.
- The regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback. Only artifacts inside the repository worktree or inside the run's own evidence directory are linked or embedded; a path naming another run's evidence is dropped.
- For Azure DevOps, the PR description is capped at 4000 characters (UTF-16 code units, matching .NET's measurement): the agent is told about the cap and asked to keep `## What Changed` compact; if the assembled body still overruns, `## Testing` and then the attribution table are dropped, then older Pipeline update rounds are omitted oldest-first at their `<details>` boundaries so the newest evidence survives. A final connector-level clamp adds a visible marker as a last-resort backstop.
- The assembled title and body pass through credential redaction immediately before they are sent, after any `hooks.pr_body` formatter, so a recorded finding, test command, or embedded artifact cannot publish a secret to the host.
- When a PR body formatter is configured, its output replaces the built-in body described above and the section layout becomes its decision; the title stays pipeline-drafted, and the host character cap and byte cap above still clamp what the formatter returns, while the built-in body's section-removal and truncation ordering no longer apply. The formatter's data contract, resolution order, and fallback behavior are owned by [`hooks.pr_body`](/no-mistakes/reference/repo-config/#hookspr_body), and [`no-mistakes pr-body`](/no-mistakes/reference/cli/#no-mistakes-pr-body) renders one without running a gate.

Stores the PR URL in the database and streams it to the TUI.

## CI

Monitors PR health after creation and auto-fixes CI failures. Mergeability polling and merge-conflict handling now apply to GitHub, GitLab, and Azure DevOps.

**Active for GitHub, GitLab, Bitbucket Cloud (`bitbucket.org`), and Azure DevOps (`dev.azure.com` / `*.visualstudio.com`)**.

- GitHub requires `gh` CLI, installed and authenticated.
- GitLab requires `glab` CLI, installed and authenticated.
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.
- Azure DevOps requires the `az` CLI with the `azure-devops` extension, authenticated with a PAT.

**Behavior:**
- Polls provider CI status at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Continues its normal monitoring loop until the PR is merged, closed, declined, or the configured `ci_timeout` idle window elapses, then parks at an approval gate instead of ending the run
- The [`ci_timeout` reference](/no-mistakes/reference/global-config/#ci_timeout) owns idle re-arming, unlimited monitoring, and fail-closed reconciliation while that gate is parked
- On GitHub, GitLab, and Azure DevOps, polls provider mergeability alongside CI checks while the PR remains open
- While the PR stays open, the TUI and terminal title show `Checks passed` once checks are green and known mergeability is clear, and `no-mistakes axi` returns `outcome: checks-passed` with successful-output reporting instructions so agents can summarize the run, ask the user to review and merge, and list any pipeline fixes instead of waiting
- If the default branch moves after `checks-passed`, keeps watching the same PR; a clean behind PR needs no action, while an actual GitHub, GitLab, or Azure DevOps merge conflict is auto-fixed by rebasing onto the base and re-pushing through the force-push safety guard
- The ready signal clears if checks start running again, new failures appear, provider state becomes uncertain, or the PR is merged, closed, or declined
- Waits a 60s grace period before trusting empty results (CI checks may not have registered yet)
- If CI failures or, on GitHub, GitLab, or Azure DevOps, a merge conflict are already known while other checks are still pending: waits for all checks to finish before attempting an auto-fix
- On CI failure: fetches failed job logs (GitHub via `gh run view --log-failed`, GitLab via `glab ci trace`, Bitbucket Cloud via failed pipeline step logs; Azure DevOps has no first-class build-log command, so the agent fixes from the failing-check list without logs), sends them to the agent with user intent when available, and, if the agent produces changes, commits them and uses the same force-push safety guard as the push step
- On GitHub, GitLab, or Azure DevOps merge conflict: asks the agent to rebase onto the latest default-branch tip and make the smallest correct root-cause fix for the conflicts, using user intent when available
- If both CI failures and a GitHub, GitLab, or Azure DevOps merge conflict are present: fixes both in the same attempt
- If a fix attempt produces no changes: automatic mode leaves the failure undeduplicated so it can retry until the auto-fix limit, while manual fix mode returns immediately for manual intervention
- Deduplicates fix attempts only after a fix is actually committed and pushed
- Exits cleanly when the PR is merged, closed, or declined
- If the idle timeout is reached while the PR is still open: pauses for user approval, even when CI checks are currently healthy
- If the idle timeout is reached while CI failures or, on GitHub, GitLab, or Azure DevOps, a merge conflict are still known: pauses for user approval with findings for the remaining issues
- If the idle timeout is reached while GitHub, GitLab, or Azure DevOps PR mergeability is still unresolved: pauses for user approval with a finding describing the unresolved mergeability state
- If CI failures or a GitHub, GitLab, or Azure DevOps merge conflict persist after the auto-fix limit: pauses for user approval with findings listing each failing check and/or the merge conflict

**Default auto-fix limit:** `3` total CI auto-fix attempts.

## Step statuses

Each step progresses through these statuses:

| Status | Meaning |
|---|---|
| `pending` | Not yet started |
| `running` | Currently executing |
| `fixing` | Agent is auto-fixing issues |
| `awaiting_approval` | Paused, waiting for user action |
| `fix_review` | Paused after a fix cycle, showing results for review |
| `completed` | Finished successfully |
| `skipped` | Pre-skipped for the run, skipped by the user, or skipped automatically by the pipeline |
| `failed` | Step failed; the step log includes the returned error message so command stderr and provider errors are visible in the per-step log, not only in the daemon log |

When a non-terminal run has a step in `awaiting_approval` or `fix_review`, AXI run objects also expose `awaiting_agent: parked <duration>` as a run-level observability signal.
The signal clears as soon as the approval wait ends, including `axi respond` and cancellation, and does not change how gates resolve.
When a step is `running` or `fixing`, AXI run objects expose an `active_steps` table with active duration, latest activity, native subprocess PID when present, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
If the latest activity is older than `step_quiet_warning`, AXI prefixes it with `quiet` to make possible wedges visible without changing the run state.
Step logs also record native subprocess start, exit, and retry lifecycle lines plus explicit auto-fix and user-fix round markers.
