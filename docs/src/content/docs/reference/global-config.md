---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

runner:
  executable: zsh
  args: [-lc]

managed: false

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

acpx_path: acpx

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot

agent_args_override:
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"

pricing:
  profiles:
    cursor: cursor-token-rate

ci_timeout: "168h"

step_quiet_warning: "10m"

daemon_connect_timeout: "3s"

process_termination_grace: "10s"

log_level: info

session_reuse: true

hooks:
  pr_body: "~/scripts/format-pr --auto-linear"

auto_fix:
  refresh: 3
  review: 0
  build: 3
  test: 3
  document: 3
  lint: 3
  ci: 3

ci:
  rerun_transient: 0

commit:
  fix_message: "fix({{.Step}}): {{.Summary}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence
    retention: 336h
    max_runs: 200

prompts:
  shared: |
    Always included in model prompts.
  review: |
    Review-specific additions.

overrides:
  example/project:
    pipeline:
      skip_steps: [ci]
    preflight:
      - test -n "$HOME"
    commands:
      build: "make build"
    prompts:
      test: |
        Repo-specific testing guidance.
```

## Fields

### managed

Opt in to a complete, fail-closed route policy. When `true`, every model-invoking step must declare both an explicit concrete agent route and model identity; `push` remains model-free. The Review step must also declare a non-empty `review.candidates` pool. A newly added pipeline step fails policy resolution until it is explicitly classified. The default `false` preserves inherited run-wide routing.

|         |         |
| ------- | ------- |
| Type    | `bool`  |
| Default | `false` |

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

|         |                                                                                             |
| ------- | ------------------------------------------------------------------------------------------- |
| Type    | `string` or `string[]`                                                                      |
| Values  | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | `auto`                                                                                      |

`auto` resolves native agents in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `cursor`. If none is available, it probes the registered `acp:cursor` fallback.
`cursor` is the native `cursor-agent` print-mode backend and does not require `acpx`.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`. `acp:cursor` has the built-in default command `cursor-agent acp` and remains available as an explicit or ordered fallback.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If an explicit agent is unavailable, `auto` finds no native agent or registered ACP fallback, or no fallback-list entry is available, the gate fails before its first pipeline step rather than reporting a partial command-only validation as passed.
`no-mistakes doctor` checks the global configuration, while every run repeats resolution after applying any trusted repository-level `agent` override.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
`cursor` and `acp:cursor` are distinct native and ACP backends, so a list such as `[cursor, acp:cursor]` keeps both in that order.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.

### runner

Machine-wide default shell runner for prepared commands. The value is argv-shaped: `executable` is a bare supported shell name, `args` is its supported command prefix, and the command string becomes the final argument. POSIX defaults to `sh -c`; Windows defaults to `pwsh -NoLogo -NoProfile -NonInteractive -Command`. Configure `zsh` with `[-lc]` explicitly when a machine policy intentionally requires login-shell initialization. POSIX runners accept only `[-c]` or `[-lc]`; PowerShell accepts only the documented noninteractive argument sequence. Paths and extra arguments are rejected so the persisted policy identity cannot retain machine paths or arbitrary values.

Before execution, no-mistakes resolves the binary, records the canonical configured shell name, supported argument shape, platform, precedence source, and numeric shell version when one can be extracted, then validates command syntax with that shell. An unavailable binary, invalid argv, version-probe failure, or syntax failure is fatal; it never falls back to another shell. Resolved machine paths, full version output, and environment values are not persisted. Combined command diagnostics are bounded and report when truncation occurred.

|         |                                      |
| ------- | ------------------------------------ |
| Type    | `{ executable: string, args: [] }`   |
| Default | `sh -c` (POSIX), `pwsh` (Windows)    |

### Per-step agent and model routes

Set `<step>.agent` to route one pipeline step to a different agent or ordered fallback list. Supported steps are `intent`, `refresh`, `review`, `build`, `test`, `document`, `lint`, `pr`, and `ci`.

```yaml
agent: claude
review:
  agent: [codex, claude]
test:
  agent: pi
```

An unconfigured step inherits the run-wide `agent` unless `managed: true` requires an explicit route. Repo-level step routes override global step routes. A route is resolved once when the run starts and is used for every invocation in that step, including its fix rounds. For Review, `review.agent` and `review.model` are the stable fixer route when a candidate pool is configured.
The legacy top-level `rebase` route is accepted as an alias for `refresh`; setting both sections is rejected as ambiguous. `refresh.strategy` is repository-only because branch-history policy comes from trusted default-branch config.

`<step>.model` is an object with required `name` and explicit lowercase `vendor` fields. Repo model routes override matching global model routes. Each supported backend receives the model through its verified interface on every invocation and fix round; the first-class field wins over a model default in `agent_args_override`. `push` has no agent or model route.

```yaml
review:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
```

Claude and Codex accept their native model names. Native Cursor accepts Cursor's exact cross-vendor model string, including bracketed parameter overrides. OpenCode requires `name` in `provider/model` form and receives the parsed provider and model IDs in each message request. Pi and Copilot accept their native model names. Rovo Dev model routing is refused because its managed server exposes no verified model-selection interface. When the effective agent is `auto`, no-mistakes skips incompatible or unsupported backends. Vendor identity is never derived from the model name. If no compatible backend is runnable, startup fails loudly.

`review.candidates` is a closed full-review pool, not an ordered fallback list. Every entry names one explicit `agent` and complete `model`; `agent: auto` and duplicate pairs are rejected. Set `optional: true` only for a route that may legitimately be absent. Policy resolution removes an optional route whose harness is unavailable or whose exact native Cursor model is absent from `cursor-agent models`; required absence and catalog-probe errors fail before run creation. At least one usable candidate must remain. Every initial full review and rereview selects one usable candidate uniformly at random, runs it cold under the `/review-changes` contract, and records the final pool plus selected route. The fixer route remains stable across rounds.

The removed `review.adversary_agent` and `review.adversary_model` fields now fail config parsing with a migration hint to use `review.candidates`.

ACP targets accept a first-class bare model family such as `claude-opus-5` when no-mistakes can compose a raw target command; `acp:cursor` receives `cursor-agent --model claude-opus-5 acp`. Any name containing `[` or `]` is refused for ACP during launch-time config validation because Cursor ACP normalizes parameterized variants to the family default. Native Cursor continues to accept the exact parameterized model string.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>`, including `agent: acp:cursor`.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | `acpx`   |

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.
The registered `acp:cursor` target uses the `cursor` override key to replace its default `cursor-agent acp` command.
Values are trimmed; a blank or whitespace-only value behaves as no override, so a registered target keeps its default command.
Availability checks always resolve `acpx_path`. They also probe the executable named first in the effective non-blank raw command when it is a bare command name or clean absolute path. Relative, quoted, or escaped raw commands are not pre-probed; `acpx` executes them from the worktree. These checks do not invoke the ACP target or test its credentials.

|         |                     |
| ------- | ------------------- |
| Type    | `map[string]string` |
| Default | Empty               |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents use `acpx_path` for the bridge; use `acp_registry_overrides` to replace a raw target command such as `cursor-agent acp`.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent      | Binary     |
| ---------- | ---------- |
| `claude`   | `claude`   |
| `codex`    | `codex`    |
| `rovodev`  | `acli`     |
| `opencode` | `opencode` |
| `pi`       | `pi`       |
| `copilot`  | `copilot`  |
| `cursor`   | `cursor-agent` |

### agent_args_override

Extra CLI flags to pass to each agent.
Use this to set service tier, reasoning effort, permission mode, model selection where the underlying command supports it, or any other supported flag.
The `cursor` key configures native print mode. Explicit `acp:<target>` keys pass their flags into the ACP target's raw spawn command; `acp:cursor` falls back to the legacy `cursor` key only when no exact key exists. Cursor ACP flags are inserted before its `acp` subcommand. Other ACP raw commands receive configured flags at the end. A registry target with no known raw command fails construction and names the required `acp_registry_overrides.<target>` key instead of silently discarding its flags.

|         |                                                                                     |
| ------- | ----------------------------------------------------------------------------------- |
| Type    | `map[string][]string`                                                               |
| Keys    | `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | Empty (no extra flags)                                                              |

User-supplied flags are normally inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. Security suppression selected by trusted [`disable_project_settings`](/no-mistakes/reference/repo-config/#disable_project_settings) may be placed first while preserving a compatible operator pin. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent      | Reserved flags                                                                                              |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `claude`   | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema`, `-r`, `--resume`, `--session-id`, `-c`, `--continue`, `--fork-session` |
| `codex`    | `exec`, `resume`, `--resume`, `--session`, `--session-id`, `--thread`, `--thread-id`, `--last`, `--json`, `--color` |
| `rovodev`  | `rovodev`, `serve`, `--disable-session-token`                                                               |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs`, `--model`                                                  |
| `pi`       | `--mode`, `--no-session`                                                                                    |
| `copilot`  | `-p`, `--prompt`, `--output-format`, `--no-color`                                                          |
| `cursor`, `acp:cursor` | `-p`, `--print`, `--output-format`, `resume`, `--resume`, `--continue`, `--workspace`, `--add-dir`, `--trust` |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.
The Claude, Codex, and Cursor session-control forms are reserved so no-mistakes can keep review-loop conversations deterministic: review turns stay session-free while the fixer keeps its own isolated durable session. Cursor workspace flags are reserved because no-mistakes owns the clean-primary-workspace containment boundary.

Smart defaults:

- For `claude`, supplying `--permission-mode` (or `--dangerously-skip-permissions`) suppresses the default `--dangerously-skip-permissions`.
- For `codex`, supplying `--ask-for-approval`, `--sandbox`, or `--dangerously-bypass-approvals-and-sandbox` suppresses the default `--dangerously-bypass-approvals-and-sandbox`.
- For `cursor`, supplying `-f`, `--force`, `--yolo`, or `--auto-review` suppresses the default `--force`.

Permission and sandbox flags affect the underlying agent, but they do not disable no-mistakes' pipeline prompt steering.
Pipeline agents are still told to keep intentional writes inside the worktree and avoid mutating system state outside it.

Example:

```yaml
agent_args_override:
  claude:
    - --model
    - sonnet
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"
  rovodev:
    - --profile
    - work
  pi:
    - --provider
    - google
```

For Codex, `service_tier` and `model_reasoning_effort` tune different things: `service_tier` selects the speed or priority lane, while `model_reasoning_effort` selects reasoning depth. no-mistakes reloads global config while setting up each run, so edits made before `no-mistakes axi run` apply to that run. For repeatable profiles, use separately initialized `NM_HOME` directories; each has its own `config.yaml` and no-mistakes state.

### pricing.profiles

Explicit harness billing profiles used for the harness-adjusted cost class. Keys are normalized harness names and values must identify an embedded profile valid for that harness. Profiles are global-only and are persisted in the resolved run policy; no harness identity activates an adjustment implicitly.

```yaml
pricing:
  profiles:
    cursor: cursor-token-rate
```

`cursor-token-rate` applies Cursor's documented third-party-model token surcharge during its effective window and records its source, catalog/profile versions, hashes, exclusions, and adjustment kind in each estimate. Embedded Claude Code and Codex/Azure profiles remain inactive because no authoritative exact private adjustment is configured. Reported harness cost, public API-list estimate, and harness-adjusted estimate remain independent nullable facts.

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and on GitHub, GitLab, or Azure DevOps PR mergeability, before giving up.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open.
If it later develops an actual GitHub, GitLab, or Azure DevOps merge conflict, the CI auto-fix path rebases and re-pushes it, while a clean behind PR needs no command.
A genuinely idle/abandoned PR still parks at an approval gate after the timeout elapses.
While that CI gate is parked, the daemon continues bounded read-only PR-state checks.
If the PR is merged or closed externally, the stale gate completes automatically; an open, unknown, or temporarily unreachable PR remains parked for a user decision.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

Legacy alias: `babysit_timeout`.

### step_quiet_warning

How long a running or fixing step can go without recorded step-log or native-agent lifecycle activity before AXI status marks the step as quiet.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10m`                  |

Accepts any positive Go `time.ParseDuration` string: `30s`, `5m`, `1h`, etc.
Non-positive values are ignored and keep the default.

This is observability only.
It does not cancel the step, change auto-fix behavior, or mark the run failed.
AXI renders the quiet signal in the `active_steps` table as part of `last_activity`, for example `quiet 12m3s ago: codex started pid=4242`.
For older active runs that do not yet have activity rows, AXI falls back to the step log file's modification time.

### daemon_connect_timeout

Maximum time a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging. Guards against a daemon process that is alive but stuck or unresponsive.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `3s`                   |

Accepts any positive Go `time.ParseDuration` string. Overridable per-invocation with the `NM_DAEMON_CONNECT_TIMEOUT` environment variable; see [Environment Variables](/no-mistakes/reference/environment/#nm_daemon_connect_timeout).

### process_termination_grace

Maximum time a Unix process group gets to exit after no-mistakes sends `SIGTERM`. If any processes remain when the ceiling expires, cleanup escalates to `SIGKILL`.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10s`                  |

Accepts any positive Go `time.ParseDuration` string. Invalid, zero, and negative values are rejected when the global config is loaded.

This is a ceiling, not an unconditional delay. When the agent, test runner, build watcher, or other descendant exits promptly after `SIGTERM`, no-mistakes continues immediately. Windows process trees use job-object termination and do not wait on this Unix signal window.

The same ceiling bounds cleanup of descendants that left the process group. Agent CLIs spawn each tool-call shell detached, so anything an agent backgrounds leads its own Unix session and cannot be reached by a process-group signal. On Unix, no-mistakes tracks those escaped descendants and terminates them when the step tears down, using the same `SIGTERM`-then-`SIGKILL` escalation. Discovery is driven by the kernel and never by a timer: macOS learns of each fork through kqueue process events while the agent is still running, and Linux makes the daemon a child subreaper so orphaned descendants reparent onto it instead of vanishing to init, and teardown enumerates what landed there.

Discovery narrows the window rather than closing it, and the residual gap differs by platform:

- **macOS** reports *that* a process forked but not which pid, so no-mistakes reads the process table on each wakeup. A descendant that both escapes and is orphaned inside that gap is missed.
- **Linux** collects orphans at teardown, and identifies which invocation each one came from by an environment marker inherited through the process tree. Because runs execute concurrently and orphans from all of them reparent onto the daemon together, an orphan that cannot be identified is left running rather than terminated on a guess. A descendant that escaped by changing only its process group, without `setsid`, is not collected on this path either, and a process-group signal cannot reach it, so the sentinel below is what reports it.
- **Windows** has no such window at all: the job object owns the whole tree structurally.

Because the window is real, every agent subprocess also inherits a sentinel descriptor that reaches end-of-file only once the last descendant has exited. After the sweep no-mistakes checks it, and logs `processes survived step teardown` with the agent pid if anything still holds it. Treat that warning as a report of stray processes left behind by that step — it is the difference between a leak you can see and a silent one.

### log_level

Daemon log verbosity.

|         |                                  |
| ------- | -------------------------------- |
| Type    | `string`                         |
| Values  | `debug`, `info`, `warn`, `error` |
| Default | `info`                           |

### session_reuse

Per-run agent session reuse for the review loop's fixer role.

|         |        |
| ------- | ------ |
| Type    | `bool` |
| Default | `true` |

When enabled and the pipeline agent supports native session resume (Claude via `--resume`, Codex via `exec resume`, Cursor via `--resume`), each run keeps one durable fixer session across its review-fix turns.
Review turns - the initial full review and every full rereview - always run as fresh, session-free invocations regardless of this setting: a rereview certifies fixes that implement the previous review turn's findings, so it must never resume the session that prescribed them; cross-round review context travels only in the explicit sanitized round history.
The fixer session is never lent to review turns, other pipeline steps stay session-isolated in their own cold invocations, and different runs never reuse identities.
When resume is unavailable or fails, the fix turn falls back to a cold run or a fresh fixer session and the fallback is recorded in the local `agent_invocations` performance record.
Session identities are persisted only as minimum local resume metadata, never as prompts or transcripts.
The [daemon crash-recovery reference](/no-mistakes/concepts/daemon/#crash-recovery) owns which parked gates can resume or reconcile after a restart.
Set `false` to force every agent invocation cold.

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For unconfigured Build, Test, and Lint, the agent plans one exact command read-only, the pipeline executes it, and repair rounds rerun that same plan after applying a fix.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field               | Type  | Default | Description                                                                                 |
| ------------------- | ----- | ------- | ------------------------------------------------------------------------------------------- |
| `auto_fix.refresh`  | `int` | `3`     | Refresh conflict auto-fix attempts                                                          |
| `auto_fix.review`   | `int` | `0`     | Review finding auto-fix attempts                                                            |
| `auto_fix.build`    | `int` | `3`     | Build or compile failure auto-fix attempts                                                  |
| `auto_fix.test`     | `int` | `3`     | Test failure auto-fix attempts                                                              |
| `auto_fix.document` | `int` | `3`     | Not used by the automatic document pass                                                     |
| `auto_fix.lint`     | `int` | `3`     | Lint issue auto-fix attempts                                                                |
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus GitHub, GitLab, and Azure DevOps merge conflicts |

Legacy aliases: `auto_fix.rebase` for `auto_fix.refresh`, and `auto_fix.babysit` for `auto_fix.ci`. Setting a canonical key together with its legacy alias is rejected as ambiguous.

These are global defaults. Per-repo config can override individual steps.

Review, Build, Test, Document, Lint, and CI use a hard ceiling of three automatic repair attempts; larger configured values are evaluated as `3`. They stop sooner when the normalized failure repeats or Git HEAD/worktree content makes no progress. File modification times, including timestamp-only log touches, do not count as progress. Refresh conflict handling retains its separate configured budget.

### ci.rerun_transient

How many times the CI step may re-run a single check the provider reported as cancelled before that check reaches an approval gate.

| | |
|---|---|
| Type | `int` |
| Default | `0` |
| Range | `0` to `5`; values outside it are clamped |

```yaml
ci:
  rerun_transient: 0
```

Each rerun is another provider-side workflow run billed to the repository being contributed to.
Set `0` here to never spend someone else's CI minutes; this is the only place to make that choice for a repository whose default branch you do not control.

The per-repo [`ci.rerun_transient`](/no-mistakes/reference/repo-config/#cirerun_transient) overrides this value and owns the classification, the trust boundary, and every case that skips the rerun.

### commit.fix_message

Template for the subject of commits created by the shared Review, Build, Test, Document, Lint, and CI fix path.

| | |
| --- | --- |
| Type | `string` |
| Default | `fix({{.Step}}): {{.Summary}}` |

The template supports literal text and two Go-style placeholders:

| Variable | Value |
| --- | --- |
| `{{.Step}}` | Pipeline step name, such as `review`, `build`, `test`, `document`, `lint`, or `ci` |
| `{{.Summary}}` | Sanitized one-line summary returned by the fix agent, or the step's deterministic fallback summary |

The value must be a valid UTF-8 template that renders to a non-empty, single-line commit subject.
The template source is limited to 1,024 bytes and 16 placeholders.
The fix-agent summary and final rendered subject are each limited to 4,096 bytes.
Before rendering, no-mistakes predicts the subject size from the validated literal text and placeholders, then rejects oversized output without allocating the expanded message.
Template functions, control actions, named templates, unknown placeholders, malformed syntax, control characters, unsafe Unicode format characters, and Unicode line or paragraph separators cause configuration loading to fail.
The blocked format set includes every Unicode `Bidi_Control` code point plus `U+00AD`, `U+180E`, `U+200B`, `U+2060` through `U+2064`, the deprecated bidi controls `U+206A` through `U+206F`, `U+FEFF`, `U+FFF9` through `U+FFFB`, and Unicode tag characters in `U+E0000` through `U+E007F`.
Legitimate `U+200C` zero-width non-joiner and `U+200D` zero-width joiner text shaping remains allowed.

The commit body records the actual authoring harness when it has a defined identity (`claude`, `codex`, native `cursor`, or `acp:cursor`) and always records `No-Mistakes-Model`. The adapter-observed model wins; the configured model is the fallback, and unavailable or unsafe metadata is recorded as `unknown`.
The final rendered subject is validated again, so unsafe characters in an agent-provided summary are also rejected.
The setting does not change commit subjects created by the Refresh or Push steps.
A per-repo [`commit.fix_message`](/no-mistakes/reference/repo-config/#commitfix_message) value overrides this global setting.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, and pass that summary to refresh, review, build, test, document, lint, CI auto-fix, and PR drafting prompts. The generated description keeps intent provenance on the Intent pipeline result instead of duplicating it in the body.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts are written to `<NM_HOME>/evidence/<run-id>` and referenced by local path.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                         | Type     | Default                  | Description                                                                |
| ----------------------------- | -------- | ------------------------ | -------------------------------------------------------------------------- |
| `test.evidence.store_in_repo` | `bool`   | `false`                  | Publish test evidence artifacts to the repository's orphan evidence branch |
| `test.evidence.dir`           | `string` | `.no-mistakes/evidence`  | Directory prefix inside the evidence branch                                |
| `test.evidence.branch`        | `string` | `no-mistakes/evidence`   | Name of the orphan evidence branch                                         |
| `test.evidence.local_root`    | `string` | `<NM_HOME>/evidence`     | Absolute directory where run evidence is written on local disk             |
| `test.evidence.retention`     | `string` | `336h` (14 days)         | Minimum run age from creation before rich terminal data is eligible for pruning; positive values below 14 days use the 14-day floor, while `unlimited`/`none`/`off`/`never` or a non-positive duration disables rich pruning |
| `test.evidence.max_runs`      | `int`    | `200`                    | Minimum newest terminal-unpinned set retained regardless of age; values below 50 retain the required floor |

The test step always collects evidence outside the worktree, so artifacts never enter the branch under validation.
When `store_in_repo` is true for a GitHub repository, the PR step copies that directory onto `branch` under `<dir>/<branch-slug>` in the code branch's push-target repository (the fork when fork routing is configured), pushes it, and links the artifacts from the pull request body.
The branch is an orphan: it shares no history with your code branches, so evidence never reaches the default branch. Links use the evidence commit rather than the branch, so they keep resolving after later runs.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
`branch` must be a valid Git branch name; an invalid value fails the config with the offending key and value.
The publisher never force-pushes. It appends to the fetched evidence-branch tip with a fast-forward push, retries one lost race, and refuses to use the run branch, default branch, or an existing branch whose tip lacks the `.no-mistakes-evidence` marker.
Publication is also refused when the remote cannot be read or pushed, an artifact exceeds 64 MiB, a run exceeds 500 files or 256 MiB, or another writer wins the retry. The PR body then keeps its local rendering instead of adding links that would not resolve.
Evidence-branch publication currently supports GitHub links only. On other providers, no evidence branch is pushed and the PR body keeps its local rendering.
Enabling this pushes a branch to your remote, so pick a `branch` name your CI workflows do not build.

#### Local storage and cleanup

Evidence lives under the app root rather than the system temp directory. On Linux the daemon runs from a service unit that does not export `TMPDIR`, so the old temp-directory default resolved to the shared `/tmp`, which current Ubuntu mounts as a RAM-backed `tmpfs`. The app root is disk backed on macOS, Linux, and Windows alike.

no-mistakes reaps its recorded run directories itself rather than relying on an operating-system temp cleaner. Unrecognized directories under a custom `local_root` are left untouched.

- A finished run that produced no artifacts leaves nothing behind.
- Pending, running, and explicitly pinned runs are never touched.
- Every run created inside the configured `retention` window or the mandatory 14-day floor is retained.
- At least the newest 50 terminal unpinned runs, or the configured larger `max_runs` set, are retained regardless of age.
- Older unpinned evidence and run logs are removed before their rich database rows are pruned.

Reaping runs after each finished run and again at daemon startup. Use `no-mistakes runs pin <run-id>` and `unpin` to change explicit retention. Before rich rows cascade away their steps, rounds, and invocations, no-mistakes atomically stores an immutable, no-foreign-key metric receipt so historical stats survive. The [local/remote data boundary](/no-mistakes/reference/environment/#what-stays-local-and-what-leaves-the-machine) owns that receipt's content-free fields and privacy exclusions. An upgraded daemon also drains the pre-relocation directory in the system temp directory under the same rules; nothing is migrated, because absolute paths recorded in older pull request bodies name the old location.

`local_root` must be an absolute path outside `<NM_HOME>/worktrees`; a relative or managed-worktree path fails daemon startup and prevents new or recovered runs from starting. Because `retention` sets the guaranteed age window for a PR body's local artifact links, raise it rather than lowering it if your reviews run long.

The publication fields are global defaults. Repo config can override `store_in_repo` and `dir`; it can override `branch` only through the trusted default-branch copy. `local_root`, `retention`, and `max_runs` are global-only: a repository does not get to name a filesystem path this machine's daemon writes to, or set the retention budget for a directory every repository on the machine shares.

### eval

Local review-evaluation corpus settings for [`no-mistakes eval`](/no-mistakes/reference/eval/).

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                      | Type   | Default | Description                                                            |
| -------------------------- | ------ | ------- | ---------------------------------------------------------------------- |
| `eval.capture_provenance`  | `bool` | `true`  | Record the exact commit and configuration inputs a replay needs        |
| `eval.auto_capture`        | `bool` | `true`  | Freeze eligible finished runs' review passes into the local corpus     |
| `eval.max_cases`           | `int`  | `200`   | Retention target for automatic collection; `0` keeps every case        |
| `eval.diversified_size`    | `int`  | `32`    | Cap on the official gold-only `diversified` set; `0` is one gold case per stratum |

`capture_provenance` is what makes a review pass replayable at all. It is recorded when the round is written and cannot be added afterwards, because the pinned configuration is a point-in-time snapshot, so a run reviewed with it off can never be captured later.

`auto_capture` collects those passes without any command: when an eligible run finishes, its decided review rounds become cases. It does nothing while `capture_provenance` is off. Collection runs after the pipeline has already reported its outcome and can never change it; a failure is logged and nothing else.

`max_cases` sets the retention target enforced after automatic collection. When it is exceeded the oldest unprotected cases are dropped first. A case with a replay in progress or recorded candidate replays is protected, so the corpus can remain above the target rather than invalidate a comparison you have spent tokens on. Cases from the same repository share one local object pool, so a case costs its own records plus the objects its commits introduced rather than a copy of the repository.

`diversified_size` caps the official gold-only eval set used by `eval run --cases diversified`. Selection is stratified and pinned; unlabeled cases never fill it. `0` keeps one gold case per stratum with no Hamilton bound. Corpus retention (`max_cases`) and this official-set cap are different knobs.

These are operator settings for this machine's local disk, so they are global-only: an `eval` block in a repository's `.no-mistakes.yaml` is ignored. Corpus storage stays under `<NM_HOME>/eval` and no-mistakes never uploads it; replay still sends code to the selected agent's configured model provider as described in the [Evaluation toolkit](/no-mistakes/reference/eval/).

### hooks.pr_body

Machine-wide default pull request body formatter.

|      |          |
| ---- | -------- |
| Type | `string` |
| Default | Empty (use the built-in body) |

This is the only hook accepted here. One formatter usually serves every repo on a machine, whereas `hooks.post_worktree` is a repo's own install command - setting that globally would run the wrong setup in every other repo, so it is rejected with an error rather than silently ignored.

A repo's own `hooks.pr_body` overrides this value. The field's behavior, contract, and failure handling are owned by [the repo config reference](/no-mistakes/reference/repo-config/#hookspr_body); iterate on a formatter with [`no-mistakes pr-body`](/no-mistakes/reference/cli/#no-mistakes-pr-body).

### prompts

Global prompt additions appended to no-mistakes' built-in pipeline prompts.

|      |          |
| ---- | -------- |
| Type | `object` of `string` values |
| Default | Empty (built-in prompts only) |

Built-in prompts remain authoritative: configured prompt text is appended as extra guidance and must not replace output schemas, safety rules, or worktree boundaries.
Values set here apply to every repository gated by this machine.

A repo's own `prompts` never replaces these values; the two append, global guidance first. The supported keys, the step each one reaches, and the full merge order are owned by [the repo config reference](/no-mistakes/reference/repo-config/#prompts).

### overrides

Machine-local per-repository configuration, keyed by repository identity.

|      |          |
| ---- | -------- |
| Type | `map` of `<owner>/<repo>` keys to [repo-config](/no-mistakes/reference/repo-config/)-shaped objects |
| Default | Empty (no repository is overridden) |

Use an entry here for repo-specific values that cannot be committed to the repository's default branch - for example canonical commands in a repository whose default branch you do not control. This is machine-owner-trusted configuration with the same standing the retired machine-local config file had: it can set code-executing fields (`commands`, `hooks`), the run-wide `agent`, per-step agent/model routes, and per-key `prompts`.

```yaml
overrides:
  example/project:
    agent: codex
    pipeline:
      skip_steps: [ci]
    commands:
      build: "go build ./..."
      lint: "make lint"
    prompts:
      test: |
        Repo-specific testing guidance.
```

**Keys and identity matching.** A key must be exactly `<owner>/<repo>` - two path segments, no scheme, host, URL syntax, or a clone URL's trailing `.git` - and is matched case-insensitively. At run start the daemon normalizes the registered upstream URL through the same identity rules the gate uses, so equivalent SSH and HTTPS remotes for the same repository match the same key; the match is host-agnostic, comparing only the owner/repo path. A malformed key or unparseable entry fails config loading loudly. A repository no key matches - including one whose registered remote has no normalizable identity, such as a local-path remote, and one whose identity path is deeper than two segments, such as a GitLab subgroup project - behaves exactly as if nothing was configured. An entry must not declare the legacy `repo:` field; the key is the binding. `no-mistakes doctor` reports the configured keys and whether the current repository matches one, and flags the retired `NM_REPO_CONFIG` environment variable if a machine still exports it, since its contents belong here now.

**Precedence.** A matching entry overlays the effective committed config after the [default-branch trust rules](/no-mistakes/reference/repo-config/) are applied: only fields explicitly present in the entry apply, and explicitly present empty values clear the committed value (for example `commands.test: ""` disables a committed test command). Fields absent from the entry keep the committed/trusted resolution, and the result then merges over this file's global defaults as usual.

**Trust model.** Entries live in this machine-local file, which only the machine owner edits, so they sit on the trusted side of the pushed-branch boundary - a contributor's branch can neither add nor disturb them. Runs with a matching entry record their contributing config sources: the PR Pipeline section shows only generic source labels (including `global-override`) with 12-character digest prefixes, while the full digest, the matched key, and this file's path stay in the local state database. Recovery requires the launch-time global config digest and re-reads committed inputs from their launch-time Git refs, refusing drift instead of silently changing a run's config.

#### `overrides.<owner>/<repo>.pipeline.skip_steps`

Machine-owner policy for steps that should not run in one matching repository. Values use canonical pipeline names; the legacy `rebase` and `babysit` names normalize to `refresh` and `ci`, and duplicates are removed. This field is rejected in committed `.no-mistakes.yaml` files and as a global default: it is accepted only inside a matching `overrides` entry.

Configured skips are defaults. An explicit run `--skip` list replaces them rather than merging with them, while `--skip=none` (or the Git push option `no-mistakes.skip=none`) clears them for that run. Every pre-run skipped step is persisted with either `global-override` or `run-request` as its source and appears that way in policy, status, and formatter contracts. Skipping Review is refused while Push remains enabled.

Skipping `ci` disables only no-mistakes' internal forge watcher. The run completes when its remaining enabled steps pass; no CircleCI/GitHub/GitLab result is synthesized, and external required checks remain authoritative.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, Bitbucket Cloud credentials, and update-check suppression.
