---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Committed per-repo configuration lives in `.no-mistakes.yaml` at the repository root. An optional machine-local file can use the same shape with the additional required `repo:` binding described below.

:::caution[Security: gate-control fields are read from the default branch]
`commands.*` and `hooks.post_worktree` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, and the run-wide `agent`, every `<step>.agent` / `<step>.model` route, and the Review adversary route select which processes and models launch there (including ordered fallback lists, ACP aliases such as `cursor`, and `acp:` targets) with the maintainer's credentials.
To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands`, `hooks`, `agent`, per-step agent/model routes, and the Review adversary route from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed).
The daemon also reads `refresh.strategy`, `document.instructions`, and `disable_project_settings` only from that trusted copy.
If the default branch cannot be fetched and resolved to a readable commit, or its present `.no-mistakes.yaml` cannot be read and parsed, the run aborts before launching an agent.
A readable default-branch tree with no `.no-mistakes.yaml` is valid and uses defaults.
Commit the gate-control settings you want to your default branch.
Non-executing fields (`ignore_patterns`, `auto_fix`, `commit`, intent settings other than its agent/model route, and `test.evidence`) are still read from the pushed branch. `refresh.strategy` is the exception because it controls branch-history mutation.

If you genuinely want per-branch `commands`, `hooks`, `agent`, and step routes (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.

`NM_REPO_CONFIG` is a separate, machine-owner-controlled escape hatch. When explicitly set, its bound file overlays the effective committed config after these trust rules, including code-executing fields. Do not set it globally without a correct `repo:` binding.
:::

## Machine-local overrides

Set `NM_REPO_CONFIG` to an absolute path outside the repository when you need repo-specific values that cannot be committed to the default branch:

```yaml
repo: https://github.com/example/project

agent: codex
commands:
  test: "go test ./internal/cli"
  lint: "make lint"
```

The file must declare `repo:` and its remote identity must match the registered upstream repository. Equivalent SSH and HTTPS GitHub forms match. The path and its resolved symlink target must remain outside the repository; relative paths are rejected because managed services run from a different working directory. `no-mistakes doctor` checks that the path is absolute, readable, parseable, and bound, while run startup also checks the binding against the selected repository.

The machine-local file overlays only fields present in it, after the committed pushed/default config trust resolution. This includes `commands`, `hooks`, the run-wide `agent`, and per-step routes; explicitly present empty values clear committed values. Unset `NM_REPO_CONFIG` leaves established config and recovery behavior unchanged.

launchd, systemd, and Windows Task Scheduler definitions forward the current value. Setting or unsetting it causes the next managed daemon start or restart to refresh the service definition; unlike proxy settings, an old machine-config path is not inherited after the variable is removed. The Windows task action points to an atomically written launcher under that `NM_HOME`; the launcher sets only `NM_REPO_CONFIG` and never persists proxy variables or credentials. Stale launchers are removed only after task replacement succeeds.

For enabled runs, no-mistakes stores full SHA-256 digests and private source paths or Git refs in the local database. The PR Pipeline section renders only source kinds and 12-character digest prefixes, never absolute paths. Recovery requires the same machine/global path and digest and reads committed configs from their launch-time Git refs, refusing drift instead of silently changing the run's config.

```yaml
# .no-mistakes.yaml

agent: codex

review:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
  adversary_agent: codex
  adversary_model: {name: gpt-5.6-sol, vendor: openai}

refresh:
  strategy: merge

commands:
  lint: "golangci-lint run ./..."
  # Targeted local validation only - not a full-repo CI-parity suite.
  test: "go test ./internal/cli -run '^TestDoctor' -count=1"
  format: "gofmt -w ."

hooks:
  post_worktree: "yarn install --immutable"

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

auto_fix:
  refresh: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

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
```

## Fields

### repo

Bind a machine-local config to one registered upstream repository.

| | |
| --- | --- |
| Type | `string` (Git remote URL) |
| Default | None |

This field is required in the file selected by `NM_REPO_CONFIG` and is not needed in committed `.no-mistakes.yaml`. The binding compares normalized remote identities, so common SSH and HTTPS forms for the same GitHub repository are equivalent. A missing, invalid, or mismatched binding fails the run before pipeline work starts.

### agent

Override the default agent for this repo and its setup-wizard suggestions.

| | |
| --- | --- |
| Type | `string` or `string[]` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves to the first supported native agent or ACP alias in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `cursor`.
`cursor` is an ACP alias for the `cursor` target with default command `cursor-agent acp`.
Its availability uses the global `acpx_path` and `acp_registry_overrides.cursor` settings when present.
`acp:<target>` uses the user-installed `acpx` binary configured in global config; `acp:cursor` uses the same default command as `cursor`.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If the selected explicit agent or `auto` is unavailable, the gate fails before its first pipeline step rather than reporting partial validation as passed.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
After resolving `auto`, entries that resolve to the same ACP target are deduplicated in list order, so `cursor` and `acp:cursor` provide one fallback and preserve whichever spelling appears first.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.
This per-repo `agent` value, including every fallback entry, is still read from the trusted default-branch `.no-mistakes.yaml` unless `allow_repo_commands` is enabled there.

### Per-step agent and model routes

Set `<step>.agent` to route `intent`, `refresh`, `review`, `test`, `document`, `lint`, `pr`, or `ci` to a different agent. The value accepts the same scalar or ordered fallback-list forms as the run-wide `agent`.

```yaml
agent: claude
review:
  agent: [codex, claude]
ci:
  agent: codex
```

Unconfigured steps inherit the run-wide route. A step route is resolved once at run startup and applies to every agent invocation in that step, including fixes. Review's durable reviewer/fixer sessions use only the Review route, and invocation telemetry records the concrete provider used after fallback.

Set `<step>.model` to a typed model identity with both an exact backend model name and an explicit vendor:

```yaml
review:
  agent: codex
  model:
    name: gpt-5.6-sol
    vendor: openai
```

Supported steps are `intent`, `refresh`, `review`, `test`, `document`, `lint`, `pr`, and `ci`. `push` is controller-deterministic and accepts neither an agent nor a model. The vendor is required and is never inferred from model naming. Vendor identifiers are lowercase letters, digits, and interior hyphens.

Each supported native backend receives the model through its verified interface, with the trusted per-step selection winning over a model default in `agent_args_override` for fresh invocations, fix rounds, and Claude/Codex resumed Review sessions. Claude and Codex accept their native model names. OpenCode requires `name` in `provider/model` form and receives the parsed provider and model IDs in each message request. Pi and Copilot accept their native model names. Rovo Dev model routing is refused because its managed server exposes no verified model-selection interface. `auto` skips incompatible or unsupported backends; if none is runnable, startup fails with the requested model and vendor. Explicit incompatible native routes also fail.

Every model route whose resolved agent is an ACP target or alias, including `cursor`, is rejected before work begins; the model compatibility rule remains fail-closed rather than silently claiming a model the ACP backend may normalize.

For a controller-run second opinion on high-risk changes, configure a distinct Review adversary:

```yaml
review:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
  adversary_agent: codex
  adversary_model: {name: gpt-5.6-sol, vendor: openai}
```

`review.adversary_agent` accepts the same scalar or ordered availability-fallback form as `agent`, but it is not part of `review.agent`'s invocation fallback list. Both model objects are required, and their declared vendors must differ. The controller runs the adversary only after the primary Review returns `risk_level: high`, in a cold session isolated from the primary reviewer/fixer sessions, then merges and namespaces its findings into the same Review gate. Low- and medium-risk reviews do not invoke it.

When [`commands.lint`](#commandslint) is empty, the agent-driven lint duty folds into the document step's combined housekeeping pass, which runs on the `document` route; the `lint` route then applies only if that step falls back to its own pass. Set `commands.lint` if you want the `lint` route to own the lint duty directly.

Every per-step selector is code-executing configuration. It comes from the pinned trusted default-branch copy unless trusted `allow_repo_commands: true` opts into the pushed copy; a pushed branch cannot self-enable or replace a route under the secure default.

ACP targets and aliases accept global `agent_args_override` entries when their target spawn command is composable. First-class step models remain native-only: ACP construction fails rather than silently claiming a model the backend may normalize.

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

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}`, `hooks.post_worktree`, `agent`, every per-step agent/model route, and the Review adversary route) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands`, `hooks`, `agent`, and per-step routes from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell or pick the launched agent on the daemon host. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### hooks.post_worktree

Deterministic preparation command run once in the newly-created run worktree before the `intent` step. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (no post-worktree preparation) |

Use this for worktree-local setup that later phases need, such as `yarn install`, symlinking an environment file, or warming a cache. It is controller work, not verification: it creates no pipeline step or receipt, and its effects remain in the run worktree for later phases. The command runs in its own process group; after it exits or is cancelled, no-mistakes terminates surviving descendants.

On failure, the run parks before `intent` at `gate.kind: environment`; no step record or auto-fix round is created. Correct the external environment, run `no-mistakes axi abort`, then start a fresh run. `--yes` never auto-resolves this park.

Because the hook executes arbitrary shell with the daemon's credentials, it follows the same trusted-default-branch boundary as `commands.*`. A pushed-branch hook is ignored unless the trusted default branch explicitly enables `allow_repo_commands`.

### disable_project_settings

Suppress project-level agent settings and instructions for every gate-agent start and resumed session.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This opt-in is intended for agent-orchestration repositories whose `AGENTS.md`, `CLAUDE.md`, or harness-specific project settings would give a validation agent an operator identity and authority that it must not adopt.
When enabled, no-mistakes suppresses the target checkout's project settings for every agent-driven gate step while preserving user-level agent configuration.
Codex, Claude, and Pi are the currently verified agents: Codex receives `project_doc_max_bytes=0` and `--ignore-rules`, Claude loads only its user setting source, and Pi runs with `--no-context-files` (preserving a pinned `--no-context-files` or `-nc` spelling).
The setting applies to both new and resumed sessions.

The gate fails before launching an agent if any resolved agent or fallback lacks a verified suppression mechanism.
It also fails if `agent_args_override` defeats suppression, such as a nonzero Codex `project_doc_max_bytes` or Claude setting sources that include `project` or `local`.
When this option is `false`, missing, or `null`, all agents retain their existing project-setting behavior.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A pushed branch cannot enable it or disable a trusted opt-in.
If the trusted commit or its present config file cannot be read and parsed, the run aborts rather than guessing that the option is disabled.

### commands.test

Explicit **targeted** local test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (agent selects the smallest relevant tests and evidence checks) |

`commands.test` is local **targeted validation** of the change and requested intent, not a CI-parity repository-wide regression command.
Broad regression belongs in remote CI and remains mandatory before a PR is ready; do not put a complete-suite walk here just to mirror CI.
no-mistakes does not guess whether an arbitrary shell string is "too broad" - the contract is documented and dogfooded, not enforced with language- or filename-specific heuristics.

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent detects and runs the smallest relevant tests itself (and is instructed never to run the complete repository suite).
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation, still under the same targeted-validation contract.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the agent-driven lint duty is folded into the document step's combined housekeeping pass: one agent invocation covers both documentation and lint, and the lint step consumes that result, reporting lint-category findings with the same gate semantics (blocking findings park for a decision).
Neither responsibility is skipped: when the document step has nothing to run against (or its structured output cannot be trusted), the lint step runs its own agent pass as before.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the combined housekeeping pass, or during the lint step when that pass cannot provide a result.

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

Pattern matching rules:

| Pattern | Rule |
| --- | --- |
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire subtree |
| `some/path/file.go` | Contains a slash - full path glob |

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
| --- | --- | --- |
| `auto_fix.refresh` | `int` | Inherits from global (default `3`) |
| `auto_fix.review` | `int` | Inherits from global (default `0`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.document` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable the follow-up auto-fix loop for a step (findings require manual approval).
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings pause for approval instead of starting another automatic fix loop.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Legacy aliases: `auto_fix.rebase` for `auto_fix.refresh`, and `auto_fix.babysit` for `auto_fix.ci`. Setting a canonical key together with its legacy alias is rejected as ambiguous.

### commit.fix_message

Override the auto-fix commit subject template for this repository.

| | |
| --- | --- |
| Type | `string` |
| Default | Inherits from global config, whose default is `no-mistakes({{.Step}}): {{.Summary}}` |

The value follows the [global `commit.fix_message` template syntax and validation rules](/no-mistakes/reference/global-config/#commitfix_message).
That includes the 1,024-byte template limit, 16-placeholder limit, 4,096-byte summary and rendered-subject limits, and rejection of bidi and invisible Unicode format characters.
The setting applies to the Review, Test, Document, and Lint fix path, not commits created by the Refresh, CI, or Push steps.

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

Override where evidence artifacts from the test step are stored.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
| --- | --- | --- |
| `test.evidence.store_in_repo` | `bool` | Inherits from global (default `false`) |
| `test.evidence.dir` | `string` | Inherits from global (default `.no-mistakes/evidence`) |

By default, test evidence stays in a temporary directory keyed by run ID and is referenced by local path.
Set `store_in_repo: true` to write evidence under `<dir>/<branch-slug>` inside the worktree so push can commit and publish it with the branch.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.
