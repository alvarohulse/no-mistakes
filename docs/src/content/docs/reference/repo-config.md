---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Committed per-repo configuration lives in `.no-mistakes.yaml` at the repository root. The global config's [`overrides`](/no-mistakes/reference/global-config/#overrides) map can carry an optional machine-local overlay in this same shape, keyed by the repository's `<owner>/<repo>` identity.

:::caution[Security: gate-control fields are read from the default branch]
`commands.*` and `hooks.{post_worktree,pr_body}` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, and the run-wide `agent`, every `<step>.agent` / `<step>.model` route, and the Review candidate pool select which processes and models launch there (including ordered fallback lists, native Cursor, and `acp:` targets) with the maintainer's credentials.
`prompts` steers those launched agents.
To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands`, `hooks`, `agent`, per-step agent/model routes, the Review candidate pool, and `prompts` from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed).
The daemon also reads `refresh.strategy`, `document.instructions`, `review.path_instructions`, `disable_project_settings`, `no_ci`, `ci.rerun_transient`, and `test.evidence.branch` only from that trusted copy.
If the default branch cannot be fetched and resolved to a readable commit, or its present `.no-mistakes.yaml` cannot be read and parsed, the run aborts before launching an agent.
A readable default-branch tree with no `.no-mistakes.yaml` is valid and uses defaults.
Commit the gate-control settings you want to your default branch.
Non-executing fields (`ignore_patterns`, `auto_fix`, `commit`, intent settings other than its agent/model route, and repository-scoped `test.evidence` fields) are still read from the pushed branch, except `test.evidence.branch`, which names a Git ref the daemon pushes to. `refresh.strategy` is also trusted-only because it controls branch-history mutation.

If you genuinely want per-branch `commands`, `hooks`, `agent`, step routes, and `prompts` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.

The global config's [`overrides`](/no-mistakes/reference/global-config/#overrides) map is a separate, machine-owner-controlled escape hatch. A matching entry overlays the effective committed config after these trust rules, including code-executing fields.
:::

## Machine-local overrides

Repo-specific values that cannot be committed to the default branch (for example canonical commands in a repository whose default branch you do not control) live in the global config's `overrides` map, keyed by the repository's `<owner>/<repo>` identity. Each entry uses this page's repo-config shape and overlays only the fields present in it, after the committed pushed/default trust resolution. The [Global Config Reference](/no-mistakes/reference/global-config/#overrides) owns the key syntax, identity matching, precedence, trust model, and recovery semantics.

`pipeline.skip_steps` is the exception to the shared shape: it is machine-owner policy accepted only inside a matching global override. A committed repository config that declares it is rejected. See the global [`overrides`](/no-mistakes/reference/global-config/#overrides) reference.

```yaml
# .no-mistakes.yaml

agent: codex

review:
  agent: cursor
  model: {name: gpt-5.6-luna-medium, vendor: openai}
  candidates:
    - agent: claude
      model: {name: claude-opus-5, vendor: anthropic}
    - agent: codex
      model: {name: gpt-5.6-sol, vendor: openai}
  # Optional trusted guidance scoped to changed paths.
  path_instructions:
    - path: "internal/scm/**"
      instructions: |
        Any URL or error string that can carry credentials must go through internal/safeurl.
    - path: "docs/**"
      instructions: |
        Prose changes only. Do not request test coverage.

refresh:
  strategy: merge

commands:
  build: "go build ./..."
  lint: "golangci-lint run ./..."
  # Targeted local validation only - not a full-repo CI-parity suite.
  test: "go test ./internal/cli -run '^TestDoctor' -count=1"
  format: "gofmt -w ."

hooks:
  post_worktree: "yarn install --immutable"
  pr_body: "~/scripts/format-pr --auto-linear"

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Optional documentation ownership policy, read only from the trusted default branch.
document:
  instructions: |
    docs/ owns detailed product guidance; README.md owns the introduction.

# For orchestration repos whose project instructions would misidentify gate agents.
# Read only from the trusted default branch. Defaults to false.
disable_project_settings: true

# Positive declaration that this repository intentionally has no CI.
# Read only from the trusted default branch. Defaults to false (CI expected).
# no_ci: true

auto_fix:
  refresh: 3
  review: 3
  build: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

# Read only from the trusted default branch: each rerun is another workflow run.
ci:
  rerun_transient: 0

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  agent: pi
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence

# Optional prompt additions, read only from the trusted default branch.
# Built-in prompts stay authoritative.
prompts:
  shared: |
    Always included in model prompts.
  test: |
    Test-specific additions.
```

## Fields

### agent

Override the default agent for this repo and its setup-wizard suggestions.

| | |
| --- | --- |
| Type | `string` or `string[]` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves native agents in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `cursor`. If none is available, it probes the registered `acp:cursor` fallback.
`cursor` is the native `cursor-agent` print-mode backend.
`acp:<target>` uses the user-installed `acpx` binary configured in global config; `acp:cursor` keeps the built-in `cursor-agent acp` command.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If the selected explicit agent or `auto` is unavailable, the gate fails before its first pipeline step rather than reporting partial validation as passed.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
`cursor` and `acp:cursor` are distinct native and ACP backends, so both remain in an ordered fallback list.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.
This per-repo `agent` value, including every fallback entry, is still read from the trusted default-branch `.no-mistakes.yaml` unless `allow_repo_commands` is enabled there.

### Per-step agent and model routes

Set `<step>.agent` to route `intent`, `refresh`, `review`, `build`, `test`, `document`, `lint`, `pr`, or `ci` to a different agent. The value accepts the same scalar or ordered fallback-list forms as the run-wide `agent`.

```yaml
agent: claude
review:
  agent: [codex, claude]
ci:
  agent: codex
```

Unconfigured steps inherit the run-wide route unless global `managed: true` requires an explicit one. A step route is resolved once at run startup and applies to every agent invocation in that step, including fixes. Review candidates run cold; only the stable fixer route may keep a durable session. Invocation telemetry records the concrete selected provider.

Set `<step>.model` to a typed model identity with both an exact backend model name and an explicit vendor:

```yaml
review:
  agent: codex
  model:
    name: gpt-5.6-sol
    vendor: openai
```

Supported steps are `intent`, `refresh`, `review`, `build`, `test`, `document`, `lint`, `pr`, and `ci`. `push` is controller-deterministic and accepts neither an agent nor a model. The vendor is required and is never inferred from model naming. Vendor identifiers are lowercase letters, digits, and interior hyphens.

Each supported backend receives the model through its verified interface, with the trusted per-step selection winning over a model default in `agent_args_override` for fresh invocations, fix rounds, and Claude/Codex/Cursor resumed Review sessions. Claude and Codex accept their native model names. Native Cursor accepts Cursor's exact cross-vendor model string, including bracketed parameters. OpenCode requires `name` in `provider/model` form and receives the parsed provider and model IDs in each message request. Pi and Copilot accept their native model names. Rovo Dev model routing is refused because its managed server exposes no verified model-selection interface. `auto` skips incompatible or unsupported backends; if none is runnable, startup fails with the requested model and vendor. Explicit incompatible routes also fail.

ACP targets, including `acp:cursor`, accept bracket-free model families. Any model name containing `[` or `]` is rejected during launch-time config validation before the ACP route is probed, covering parameterized, empty, nested, repeated, and unmatched bracket forms. Native Cursor continues accepting its parameterized model syntax. The controller retains the exact configured name and vendor for telemetry; it never reports the family default as a requested parameterized variant.

To distribute fresh full reviews independently from the stable fixer route, configure a Review candidate pool:

```yaml
review:
  agent: cursor
  model: {name: gpt-5.6-luna-medium, vendor: openai}
  candidates:
    - agent: claude
      model: {name: claude-opus-5, vendor: anthropic}
    - agent: codex
      model: {name: gpt-5.6-sol, vendor: openai}
    - agent: cursor
      model: {name: grok-4.6, vendor: xai}
      optional: true
```

`review.candidates` is a closed quality-routing pool, not an ordered availability fallback. Every candidate names exactly one concrete harness and complete model identity. Unavailable required candidates fail policy resolution; unavailable optional candidates are removed, including native Cursor models absent from its reported catalog. Catalog probe errors fail closed, and an empty usable pool is rejected. Every full Review and rereview selects uniformly from the final pool and runs cold under `/review-changes`; `review.agent` and `review.model` remain the stable fixer route. Each review invocation records both the final pool and selected harness/model.

The removed `review.adversary_agent` and `review.adversary_model` fields are rejected with a migration hint.

When `commands.build`, `commands.test`, or `commands.lint` is empty, that step's route plans one exact command in a read-only agent pass. The pipeline executes and records the plan, and the same route owns any repair before the pipeline reruns the command.

Every per-step selector is code-executing configuration. It comes from the pinned trusted default-branch copy unless trusted `allow_repo_commands: true` opts into the pushed copy; a pushed branch cannot self-enable or replace a route under the secure default.

ACP targets accept global `agent_args_override` entries and bare first-class step models when their target spawn command is composable. The first-class model replaces any `-m` or `--model` default from `agent_args_override`.

The legacy top-level `rebase` route is accepted as an alias for `refresh`; setting both sections is rejected as ambiguous. The legacy section accepts agent and model routing but cannot select a strategy.

### refresh.strategy

Choose how the refresh step incorporates its freshly fetched base branch.

| | |
| --- | --- |
| Type | `string` |
| Values | `rebase`, `merge` |
| Default | `rebase` |

The canonical step identity stays `refresh`; user-facing pipeline displays label it `Rebase` or `Merge` for the selected strategy. CLI `--refresh-strategy` overrides this setting for one run. The precedence is explicit CLI selection, then this trusted default-branch value, then `rebase`.

This field is always read from the pinned trusted default-branch config, even when `allow_repo_commands` is enabled, because it controls how the gate rewrites or extends branch history.

### allow_repo_commands

Opt in to honoring the code-executing and agent-steering fields (`commands.{build,test,lint,format}`, `hooks.{post_worktree,pr_body}`, `agent`, every per-step agent/model route, the Review candidate pool, and `prompts`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands`, `hooks`, `agent`, per-step routes, and `prompts` from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell, pick the launched agent, or steer that agent on the daemon host. This opt-in covers those fields only; `refresh.strategy`, `document.instructions`, `review.path_instructions`, `disable_project_settings`, `no_ci`, `ci.rerun_transient`, and `test.evidence.branch` stay trusted-only either way. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### hooks.post_worktree

Deterministic preparation command run once in the newly-created run worktree before the `intent` step. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (no post-worktree preparation) |

Use this for worktree-local setup that later phases need, such as `yarn install`, symlinking an environment file, or warming a cache. It is controller work, not verification: it creates no pipeline step or receipt, and its effects remain in the run worktree for later phases. The command runs in its own process group; after it exits or is cancelled, no-mistakes terminates surviving descendants.

On failure, the run parks before `intent` at `gate.kind: environment`; no step record or auto-fix round is created. Correct the external environment, run `no-mistakes axi abort`, then start a fresh run. `--yes` never auto-resolves this park.

Because the hook executes arbitrary shell with the daemon's credentials, it follows the same trusted-default-branch boundary as `commands.*`. A pushed-branch hook is ignored unless the trusted default branch explicitly enables `allow_repo_commands`.

### hooks.pr_body

External pull request section formatter. Receives the PR body contract as JSON on stdin and returns typed owned-section patches on stdout. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (use the built-in body) |

Use this when your host's pull request template, issue-linking conventions, or section ordering differ from the built-in body. Contract v4 carries separate heading-free GFM `summary` and `what_changed` fragments, opaque optional `metadata`, the Intent step's provenance/absence result, risk fields, pipeline telemetry, and distinct `static_tests`, `review_evidence`, and `user_testing` records. User Testing is an instruction unless `attested` is explicitly true. It also supplies harness-reported, public API-list, and harness-adjusted cost receipts with completeness and provenance. no-mistakes owns those calculations; the formatter only presents supplied facts. Formatters should accept v2, v3, and v4 while a producer rollout is in progress.

Successful stdout is strict JSON. It has no full-body field:

```json
{
  "version": 1,
  "sections": [
    {"id": "summary", "content": "## Summary\n\nGenerated summary"},
    {"id": "static-tests", "content": "## Static Tests\n\n- `go test ./...` passed"},
    {"id": "review-evidence", "content": "## Review Evidence\n\nNo blocking findings."},
    {"id": "user-testing", "content": "## User Testing\n\n1. Exercise the saved flow."}
  ]
}
```

no-mistakes wraps each section in versioned begin/end/hash markers. On later runs it reads the hosted body and replaces only those owned ranges; every byte outside them, including human edits, third-party content, and template checklists, remains unchanged. Missing, duplicate, conflicting, or hash-invalid markers stop publication rather than guessing an insertion point.

```yaml
hooks:
  pr_body: "~/scripts/format-pr --auto-linear"
```

An execution failure - a non-zero exit, a timeout past 60 seconds, empty output, malformed patch JSON, or output over 1 MiB - falls back to built-in generated content and reports the reason (including the formatter's own stderr) in the run log. The resulting candidate is never truncated into validity: invalid UTF-8, possible secrets, marker conflicts, or host/byte-limit overflow fail closed. Existing bodies can use a fallback only when they already contain the matching owned marker set.

Iterate on a formatter without running a gate:

```bash
no-mistakes pr-body --sample --hook ~/scripts/format-pr
```

See the [`pr-body` command](/no-mistakes/reference/cli/#no-mistakes-pr-body) for the contract shape and the other data sources.

Unlike `post_worktree`, this hook may also be set machine-wide in `~/.no-mistakes/config.yaml`, because one formatter usually serves every repo on a machine. A repo-level value overrides the global one. It executes arbitrary shell with the daemon's credentials, so it follows the same trusted-default-branch boundary as `commands.*`.

### disable_project_settings

Suppress project-level agent settings and instructions for every gate-agent start and resumed session.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This opt-in is intended for agent-orchestration repositories whose `AGENTS.md`, `CLAUDE.md`, or harness-specific project settings would give a validation agent an operator identity and authority that it must not adopt.
When enabled, no-mistakes suppresses the target checkout's project settings for every agent-driven gate step while preserving user-level agent configuration.
Codex, Claude, Pi, and Cursor are verified. Codex receives `project_doc_max_bytes=0` and `--ignore-rules`, Claude loads only its user setting source, and Pi runs with `--no-context-files` (preserving a pinned `--no-context-files` or `-nc` spelling). Native Cursor and `acp:cursor` always launch with an instruction-free primary workspace and the real run worktree as an added root, even when this option is false.
The setting applies to both new and resumed sessions.

The gate fails before launching an agent if any resolved agent or fallback lacks a verified suppression mechanism.
It also fails if `agent_args_override` defeats suppression, such as a nonzero Codex `project_doc_max_bytes` or Claude setting sources that include `project` or `local`.
When this option is `false`, missing, or `null`, other agents retain their existing project-setting behavior; Cursor's mandatory containment remains active.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A pushed branch cannot enable it or disable a trusted opt-in.
If the trusted commit or its present config file cannot be read and parsed, the run aborts rather than guessing that the option is disabled.

### Structured command form

Every `commands.*` value keeps accepting its legacy scalar string. It can also use the shared structured shape:

```yaml
commands:
  build:
    run: make build
    runner: {executable: zsh, args: [-lc]}
    linux:
      run: make build-linux
    macos:
      runner: {executable: zsh, args: [-lc]}
    windows:
      run: npm run build:windows
      runner:
        executable: pwsh
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
```

Platform fields override `run` and `runner` independently. Resolution precedence is the matching `linux`/`macos`/`windows` field, the command's inline `runner`, the global default runner, then the portable product default. Scalar machine overrides still replace or clear the entire command; mapping overrides merge only explicitly present nested fields.

Runner identities use the same secret-free contract as the global default: `sh`, `bash`, or `zsh` with `[-c]` or `[-lc]`, and `pwsh` or `powershell` with `[-NoLogo, -NoProfile, -NonInteractive, -Command]`. Executable paths and extra arguments are rejected, including in inactive platform overrides, because the full structured definition is persisted in the resolved policy.

This schema is active for trusted preflight commands. Build, Test, Lint, and Format retain their existing `sh -c` / `cmd.exe /c` execution path until their dedicated migration; for those four fields, structured runner/platform metadata is preserved and explained but the compatibility `run` value remains the executed command.

Runner selectors are code-executing trusted configuration under the same default-branch and `allow_repo_commands` boundary as the command string. Resolution is fail-closed and does not try another shell after a missing binary, invalid argv, syntax error, launch error, or timeout.

### preflight

An ordered list of deterministic environment checks. Each entry accepts the same scalar or structured command form shown above.

```yaml
preflight:
  - test -n "$HOME"
  - run: command -v git
    runner: {executable: zsh, args: [-lc]}
    windows: Get-Command git | Out-Null
```

no-mistakes resolves and syntax-checks every entry with the effective runner, then executes the list from the registered source checkout after the complete policy is resolved but before it cancels an active run, inserts a run row, creates a worktree, runs hooks, or starts an agent. Commands run once in order with a fixed 30-second limit each. A non-zero exit, timeout, invalid runner, or launch error refuses the run; there is no retry, model repair, or fallback. Failure diagnostics are secret-redacted, control-sanitized, and bounded, and identify the command index plus resolved command/runner source.

A top-level global `preflight` is rejected. Use a trusted default-branch repo config or a matching machine-owned `overrides.<owner>/<repo>.preflight` list. A machine override replaces the full committed list; an explicit empty list clears it.

|         |                        |
| ------- | ---------------------- |
| Type    | `structured command[]` |
| Default | Empty                  |

### commands.build

Explicit build or compile command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` or structured command |
| Default | Empty (agent plans the appropriate build command) |

When set, the Build step runs this exact command visibly and checks its exit code. Non-zero output is bounded in the gate finding and kept in full in the Build step log.
When empty, the routed Build agent selects one exact command in a read-only planning pass. The pipeline executes and records it. If it fails, a repair agent fixes the cause and the pipeline reruns the same plan; no usable plan parks instead of silently passing.
Build is separate from Test: do not put test, lint, or documentation work in `commands.build` unless that work is inseparable from the repository's canonical build command.

### no_ci

Declare that this repository intentionally has no CI.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

When `true` and the forge reports **zero** checks on the PR head, the CI monitor treats that empty result as all-checks-passed and `axi run` may return `outcome: checks-passed`. The monitor log names the declaration (`no_ci: true`) so the positive evidence stays inspectable rather than silently equating every empty forge response with green.

Absence of this field means CI is expected. A zero-length check result then stays not-ready for as long as the forge reports no checks - elapsed time, grace periods, workflow-file presence or absence, prior check history, and branch names are not evidence.

If checks still appear on a declared no-CI repository, their actual states are processed normally. The declaration never waives a registered pending or failing check.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A feature branch cannot self-declare `no_ci: true` to bypass checks, and cannot clear a trusted declaration either.

### commands.test

Explicit **targeted** local test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` or structured command |
| Default | Empty (agent plans the smallest relevant test command) |

`commands.test` is local **targeted validation** of the change and requested intent, not a CI-parity repository-wide regression command.
Broad regression belongs in remote CI and remains mandatory before a PR is ready; do not put a complete-suite walk here just to mirror CI.
no-mistakes does not guess whether an arbitrary shell string is "too broad" - the contract is documented and dogfooded, not enforced with language- or filename-specific heuristics.

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent selects one exact focused test command in a read-only planning pass; the pipeline executes and records it, then an evidence agent gathers evidence and artifacts, running further focused checks itself when the planned command alone does not demonstrate the intent. A repair round reruns the same planned command. When user intent is available, the evidence agent may also run after a configured baseline succeeds, still under the same targeted-validation contract.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` or structured command |
| Default | Empty (agent plans a lint command) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the routed Lint agent selects one exact formatter, linter, or static-analysis command in a read-only planning pass. The pipeline executes and records it. A failure enters repair and reruns the same plan. Document remains a separate documentation-only pass.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
| --- | --- |
| Type | `string` or structured command |
| Default | Empty (no separate push-step formatter) |

This remains separate from the Lint step's planned or configured command.

### document.instructions

Repository-specific documentation ownership policy for the document step.

| | |
| --- | --- |
| Type | `string` (multiline) |
| Default | Empty (built-in placement policy only) |

The document step always applies a built-in placement policy: every fact has exactly one authoritative owner document, stale duplicates are removed or reduced to pointers instead of synchronized, no new documentation surfaces are created merely to close perceived gaps, and incident lessons live as invariants near their owner (with a pointer to the regression test), never as AGENTS.md postmortems.
`document.instructions` states this repository's ownership map or extra placement rules (for example, which file owns which class of facts).
It augments or clarifies the built-in policy; it cannot disable documentation integrity.

Like `commands.*` and `agent`, this field steers gate behavior, so it is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`: a contributor's pushed branch cannot weaken the documentation rules that gate its own review.

### review.path_instructions

Extra review guidance, scoped to the paths a change actually touches.

| | |
|---|---|
| Type | `object[]` with `path` (`string`) and `instructions` (`string`, multiline) |
| Default | Empty (built-in review instructions only) |

Use this for house rules that only apply to part of the tree, for example a redaction rule for the code that builds remote URLs, or a note that a documentation directory needs no test coverage:

```yaml
review:
  path_instructions:
    - path: "internal/scm/**"
      instructions: |
        Any URL or error string that can carry credentials must go through internal/safeurl.
    - path: "docs/**"
      instructions: |
        Prose changes only. Do not request test coverage.
```

Each matched rule reaches the reviewer with the scope it was selected for, so a rule scoped to one directory can never read as a repository-wide instruction:

```
path: docs/**
matched files: docs/notes.md
instructions:
Prose changes only. Do not request test coverage.
```

#### Matching

`path` uses the same matcher and syntax as [`ignore_patterns`](#ignore_patterns), including the rule that `*` never crosses a `/`, so `**/*.go` covers a single directory level rather than every Go file.

The review step appends only the blocks whose `path` matches at least one changed file, in the order they appear in the file.
Two entries with the same `path` **and** the same `instructions` are injected once. The same instruction text under two different `path` values is injected once per path, because each block states its own scope. Two entries with the same `path` and different `instructions` are both injected.
Matching runs against the full changed-file list and is deliberately **not** filtered by `ignore_patterns`: that field is read from the pushed branch, so filtering here would let a contributor drop one of your rules from the review of their own branch.

Blocks augment the built-in review instructions; they cannot disable them, and a finding the reviewer raises from a block goes through the same severity and action model as any other finding.
With nothing configured, or nothing matching the change, the review prompt is exactly what it would be without this setting.
The step log names the rules it applied and the rules that matched nothing, so a rule that never fires is visible in `no-mistakes axi logs --step review`.

#### Limits and validation

`instructions` is prompt text, so merge-conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`) are removed from it and runs of whitespace are collapsed, exactly as for [`document.instructions`](#documentinstructions). Write rules without those tokens; a value that would be left empty once they are removed is rejected rather than silently dropped.

At most 32 entries are allowed, and the assembled prompt section may not exceed 16,384 bytes, because the injected text shares the review prompt's budget and an oversized prompt fails the agent invocation outright.
The size is measured on what is actually injected: the heading, and for every entry its labels, its `path`, its `instructions`, and a 192-byte allowance for its matched-file list. A block whose matched-file list would exceed that allowance is truncated with a `+N more` suffix, so the measured limit holds for any diff.

A missing `path` or `instructions` value, an `instructions` value that renders empty, a `path` that is not a valid glob, or a config over either limit fails when the config is parsed, so the run aborts before an agent starts instead of silently dropping guidance.
These checks run on whichever copy of the file is parsed, including the pushed branch's. A pushed branch's blocks are ignored when the review prompt is built (see [Trust](#trust) below), but an invalid block on that branch still fails its own run, so a broken rule surfaces before it merges and becomes the trusted copy.

#### Trust

Like `document.instructions`, this field steers gate behavior, so it is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of [`allow_repo_commands`](#allow_repo_commands): a value present only on a pushed branch is ignored, so a contributor cannot inject instructions into the review that gates them.

### Command process lifetime

All configured `commands.*` entries are scoped to their step.
After no-mistakes starts one of these commands, it terminates any remaining child processes from that command when the command exits, fails, or the step is cancelled.
Do not rely on a configured command to leave a background server or watcher running after it returns; keep that service inside the command lifetime or start it outside no-mistakes.

### ignore_patterns

Paths to exclude from review and documentation checks.

| | |
| --- | --- |
| Type | `string[]` |
| Default | Empty (no ignores) |

Pattern matching rules. [`review.path_instructions`](#reviewpath_instructions) uses the same matcher, so there is one path syntax to learn:

| Pattern | Rule |
| --- | --- |
| `*.generated.go` | No slash - matches by basename, at any depth |
| `vendor/**` | Ends with `/**` - matches that directory and everything under it |
| `some/path/file.go` | Contains a slash - full path glob against the whole path |
| `**/*.go` | Also a full path glob, so **only one directory level** - `internal/main.go`, not `internal/scm/github/github.go` |

`*` never crosses a `/`, on every platform, so `**/*.go` is not "every Go file"; it behaves as a single-segment wildcard. Use `*.go` to match by extension at any depth, or `internal/**` to cover a subtree.

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
| --- | --- | --- |
| `auto_fix.refresh` | `int` | Inherits from global (default `3`) |
| `auto_fix.review` | `int` | Inherits from global (default `0`) |
| `auto_fix.build` | `int` | Inherits from global (default `3`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.document` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable the follow-up auto-fix loop for a step (findings require manual approval).
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
Unconfigured Build, Test, and Lint still use their own repair loops after the pipeline-run planned command fails.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Review, Build, Test, Document, Lint, and CI use a hard ceiling of three automatic repair attempts; larger values are evaluated as `3`. They stop earlier on a repeated normalized failure or when Git HEAD/worktree content does not change. Timestamp-only log changes are ignored. Refresh retains its separate configured conflict budget.

Legacy aliases: `auto_fix.rebase` for `auto_fix.refresh`, and `auto_fix.babysit` for `auto_fix.ci`. Setting a canonical key together with its legacy alias is rejected as ambiguous.

### ci.rerun_transient

How many times the CI step may re-run a single check the provider reported as cancelled before that check reaches an approval gate.

| | |
|---|---|
| Type | `int` |
| Default | `0` |
| Range | `0` to `5`; values outside it are clamped |
| Trust | Read only from the trusted default branch |

Every rerun this budget authorizes is another provider-side workflow run billed to the repository, so the value is read only from the trusted default-branch copy of this file, exactly like `document.instructions` and `disable_project_settings`.
A pushed branch cannot raise its own rerun budget.
The default is `0` because a cancelled conclusion does not identify its cause: the same value covers the provider aborting its own infrastructure, a maintainer stopping a runaway or unsafe job, and repository concurrency with `cancel-in-progress`.
Rerunning on that ambiguity can restart work someone deliberately stopped, so raise this only for a repository whose cancellations are known to be provider-side.

With no trusted copy of this file, the operator's own [`ci.rerun_transient`](/no-mistakes/reference/global-config/#cirerun_transient) applies, then the built-in default.
A value set here always wins over the global one, so the maintainer of the repository has the last word on how many workflow runs their project is billed for.

A rerun is requested only when the provider itself reported the outcome as `cancelled`, which is the one terminal outcome it attributes to itself rather than to the job:

- `failure`, `error`, `action_required`, and `startup_failure` are the job's own verdict on the commit, so they escalate on the first failure with no added latency.
- `timed_out` means the job exceeded its own `timeout-minutes`, which is usually the branch's own code hanging. Re-running it burns another full timeout window reproducing the same failure, so it is treated as a genuine failure and is not opt-in.
- `stale` is already treated as skipped rather than failed, so it never reaches this decision.
- An outcome no-mistakes recognizes as none of the above never earns a rerun either.

A single non-cancelled failure, or a merge conflict, suppresses the rerun for that poll: the fix agent is needed regardless, and no rerun can clear a merge conflict.

The budget is per check per run and is spent when the rerun is requested, so a provider that refuses the request cannot be retried in a loop.
Check names are not unique on a pull request, so same-named checks share one budget.

A rerun request returns as soon as the provider accepts it, while the new attempt replaces the cancelled check in the status rollup a moment later.
A poll that still reads the exact completion the rerun was requested for has observed nothing new, so the monitor waits for a bounded couple of polls rather than escalating a check it never actually re-ran.
A provider that accepts a rerun and never publishes it cannot stall the run past that.
Once the provider publishes a conclusive replacement, no-mistakes durably stops treating that rerun as outstanding while preserving the spent budget; if the exact watched head is then green, the monitor reports `checks-passed` normally.

A cancelled check that no rerun is going to replace pauses the step for user approval when cancellation is the only remaining issue, so the pull request never looks green.
That is a check that came back cancelled after its rerun, and - at the default budget of `0`, once the budget is spent, or on a provider with no rerun API - the cancellation itself: the provider has already published its conclusion for that check and will not publish another one on its own, so there is nothing left for the monitor to wait for.
It does not enter the `auto_fix.ci` loop and never consumes an auto-fix attempt: a cancellation is the provider reporting itself, so there is nothing for the fix agent to repair and no reason to let it edit code the provider never tested.
Answering that gate with `fix` is still honored, and the fix round you asked for is told about the cancelled check alongside any other issue.

Reruns are skipped when:

- The provider has no rerun API (only GitHub implements one today; GitLab, Bitbucket Cloud, and Azure DevOps reach the approval gate without a rerun).
- The check's details link names nothing the provider can re-run, for example a third-party status pointing at an external dashboard, or a link under a workflow run that names no job the API accepts. A link naming one job re-runs that job; a link naming only the workflow run re-runs that run's failed jobs; an unrecognized link is widened into neither.
- The published branch head no longer equals the commit the run delivered. That case terminates with the expected and observed commits instead: re-running checks against a different head would certify a revision this run never produced. See [pipeline steps: CI](/no-mistakes/reference/pipeline-steps/#ci).

### commit.fix_message

Override the auto-fix commit subject template for this repository.

| | |
| --- | --- |
| Type | `string` |
| Default | Inherits from global config, whose default is `fix({{.Step}}): {{.Summary}}` |

The value follows the [global `commit.fix_message` template syntax and validation rules](/no-mistakes/reference/global-config/#commitfix_message).
That includes the 1,024-byte template limit, 16-placeholder limit, 4,096-byte summary and rendered-subject limits, and rejection of bidi and invisible Unicode format characters.
The setting applies to the Review, Build, Test, Document, and Lint fix path, not commits created by the Refresh, CI, or Push steps.

This non-executing field is read from the pushed branch, so a branch can adopt its own commit convention without enabling `allow_repo_commands`.

### intent

Override transcript-based user-intent extraction settings for this repo.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
| --- | --- | --- |
| `intent.enabled` | `bool` | Inherits from global (default `true`) |
| `intent.threshold` | `float` | Inherits from global (default `0.2`) |
| `intent.slack_days` | `int` | Inherits from global (default `3`) |
| `intent.disabled_readers` | `string[]` | Adds to globally disabled readers |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

### test.evidence

Configure repository publication of evidence artifacts from the test step.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
| --- | --- | --- |
| `test.evidence.store_in_repo` | `bool` | Inherits from global (default `false`) |
| `test.evidence.dir` | `string` | Inherits from global (default `.no-mistakes/evidence`) |
| `test.evidence.branch` | `string` | Inherits from global (default `no-mistakes/evidence`) |

By default, test evidence is written to `<NM_HOME>/evidence/<run-id>` and referenced by local path. Where it is stored locally and how long it is kept are global-only settings; see [`test.evidence`](/no-mistakes/reference/global-config/#testevidence).
For GitHub repositories, set `store_in_repo: true` to publish it to an orphan evidence branch in the code branch's push-target repository and link the artifacts from the PR body; evidence is never committed to the pushed branch, so it never reaches the default branch.
`test.evidence.branch` is read ONLY from the trusted default-branch copy of this file, because it names a Git ref the daemon pushes to; a pushed branch cannot redirect evidence commits.
See [global config](/no-mistakes/reference/global-config/#testevidence) for provider support, limits, validation, and fail-closed behavior.

### prompts

Append repo-specific guidance to no-mistakes' built-in agent prompts.

|      |          |
| ---- | -------- |
| Type | `object` of `string` values |
| Default | Empty (built-in prompts only) |

Built-in prompts remain authoritative: configured prompt text is appended as extra guidance and must not replace output schemas, safety rules, or worktree boundaries.
`prompts.shared` is appended to every pipeline model prompt, then the matching step-specific prompt is appended after it.
Repo prompt config is agent-steering config, so it is read from the trusted default-branch copy unless `allow_repo_commands: true` is set there.
A machine-local [overrides entry](#machine-local-overrides) can overlay individual `prompts.<key>` values after that trust resolution.

Global prompt config and repo prompt config combine in this order:

1. global `prompts.shared`
2. repo `prompts.shared`
3. global `prompts.<step>`
4. repo `prompts.<step>`

| Field | Applies to |
|---|---|
| `prompts.shared` | Every pipeline model prompt |
| `prompts.intent` | Intent summarization and disambiguation |
| `prompts.refresh` | Refresh (rebase or merge) conflict resolution |
| `prompts.review` | Full Review/rereview and review-fix prompts |
| `prompts.build` | Build verification and build-fix prompts |
| `prompts.test` | Test evidence and test-fix prompts |
| `prompts.document` | Documentation update prompt |
| `prompts.lint` | Lint agent and lint-fix prompts |
| `prompts.pr` | PR title/body prompt |
| `prompts.ci` | CI failure and merge-conflict auto-fix prompt |

Push never invokes a model, so there is no `prompts.push`.
An unconfigured Build, Test, or Lint step applies its own `prompts.<step>` guidance to the read-only command-planning pass and any later repair pass.
