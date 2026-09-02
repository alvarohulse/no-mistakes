---
title: Environment Variables
description: All environment variables recognized by no-mistakes.
---

## `NM_HOME`

Override the data directory.

|         |                  |
| ------- | ---------------- |
| Type    | `string`         |
| Default | `~/.no-mistakes` |

When set, everything else moves under this root:

- Global config: `$NM_HOME/config.yaml`
- Gate repos: `$NM_HOME/repos/<id>.git`
- Worktrees: `$NM_HOME/worktrees/<repoID>/<runID>/`
- Run launch artifacts: `$NM_HOME/runs/<runID>/`
- Logs: `$NM_HOME/logs/`
- Database: `$NM_HOME/state.sqlite`
- Socket / PID / singleton lock: `$NM_HOME/socket`, `$NM_HOME/daemon.pid`, and `$NM_HOME/daemon.lock`
- Managed agent server PID records: `$NM_HOME/servers/`
- Local evaluation cases and registry: `$NM_HOME/eval/` (created by automatic collection or an explicit `no-mistakes eval` command)
- Managed service names get a short stable suffix derived from `$NM_HOME` so multiple installs don't collide.

## `NM_DAEMON_CONNECT_TIMEOUT`

Override how long a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging.

|         |                                                                                                   |
| ------- | ------------------------------------------------------------------------------------------------- |
| Type    | `string` (Go duration)                                                                            |
| Default | unset (falls back to the `daemon_connect_timeout` global config value, itself defaulting to `3s`) |

Takes precedence over `daemon_connect_timeout` in `config.yaml`. An empty, unparsable, or non-positive value is ignored and the config value (or its default) is used instead.

## `NO_MISTAKES_BITBUCKET_EMAIL`

Bitbucket Cloud account email used for PR creation and CI monitoring.

|         |                                               |
| ------- | --------------------------------------------- |
| Type    | `string`                                      |
| Default | (none; Bitbucket PR/CI steps skip when unset) |

Used alongside `NO_MISTAKES_BITBUCKET_API_TOKEN`. See [Provider Integration](/no-mistakes/guides/provider-integration/#bitbucket-cloud).

## `NO_MISTAKES_BITBUCKET_API_TOKEN`

Bitbucket Cloud API token.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

Get one from [Bitbucket account settings](https://bitbucket.org/account/settings/app-passwords/).

## `NO_MISTAKES_BITBUCKET_API_BASE_URL`

Override the Bitbucket Cloud API base URL.

|         |                                 |
| ------- | ------------------------------- |
| Type    | `string`                        |
| Default | `https://api.bitbucket.org/2.0` |

Useful for mocking in tests or pointing at a proxy.

## `AZURE_DEVOPS_EXT_PAT`

Azure DevOps Personal Access Token inherited by the daemon for non-interactive `az` CLI auth.
Alternatively, authenticate the Azure DevOps extension with `az devops login`.

|         |                                                    |
| ------- | -------------------------------------------------- |
| Type    | `string`                                           |
| Default | (none)                                             |

See [Provider Integration](/no-mistakes/guides/provider-integration/#azure-devops).

## `GITHUB_TOKEN`

GitHub token used to authenticate updater release requests.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

When set, the updater sends the token as a Bearer authorization header for release metadata requests, including background update checks, and release asset downloads. `GITHUB_TOKEN` takes precedence over `GH_TOKEN`; when neither variable is set, these requests remain anonymous. The token is not printed, logged, or persisted.

## `GH_TOKEN`

Fallback GitHub token used by `no-mistakes update` when `GITHUB_TOKEN` is unset or empty.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

See [`GITHUB_TOKEN`](#github_token) for the updater's authentication behavior and precedence.

## `NO_MISTAKES_NO_UPDATE_CHECK`

Disable background update checks.

|         |                                                |
| ------- | ---------------------------------------------- |
| Type    | `1` to disable, anything else to leave enabled |
| Default | unset (checks enabled)                         |

Update checks run on every CLI invocation except `update` itself and version queries (`--version` / `-v`, which stay side-effect-free), hit GitHub releases, cache the result in `$NM_HOME/update-check.json`, and print a one-line notification to stderr when a newer version is available. Dev builds (non-semver versions) suppress the check automatically.

## `XDG_DATA_HOME`

Data directory used to discover OpenCode transcripts for intent extraction.

|         |                  |
| ------- | ---------------- |
| Type    | `string`         |
| Default | `~/.local/share` |

When set, no-mistakes looks for OpenCode's intent transcript database at `$XDG_DATA_HOME/opencode/opencode.db`.
When unset, it falls back to `~/.local/share/opencode/opencode.db`.

## `GLAB_CONFIG_DIR`

Directory holding glab's `config.yml`, consulted when detecting self-hosted GitLab.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

When the upstream hostname carries no `gitlab` marker, no-mistakes reads glab's configured hosts from `$GLAB_CONFIG_DIR/config.yml` to decide whether the host is a GitLab instance. It takes precedence over `XDG_CONFIG_HOME`. See [Provider Integration](/no-mistakes/guides/provider-integration/#self-hosted-githubgitlab).

## `GH_CONFIG_DIR`

Directory holding gh's `hosts.yml`, consulted when detecting self-hosted GitHub Enterprise.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | (none)   |

When the upstream hostname is not `github.com`, no-mistakes reads gh's configured hosts from `$GH_CONFIG_DIR/hosts.yml` to decide whether the host is a GitHub Enterprise instance. It takes precedence over `XDG_CONFIG_HOME`. See [Provider Integration](/no-mistakes/guides/provider-integration/#self-hosted-githubgitlab).

## `XDG_CONFIG_HOME`

Config directory used to locate glab's `config.yml` for self-hosted GitLab detection and gh's `hosts.yml` for self-hosted GitHub Enterprise detection.

|         |             |
| ------- | ----------- |
| Type    | `string`    |
| Default | `~/.config` |

When `GLAB_CONFIG_DIR` is unset, no-mistakes looks for glab's configured hosts at `$XDG_CONFIG_HOME/glab-cli/config.yml`, falling back to `~/.config/glab-cli/config.yml` when `XDG_CONFIG_HOME` is unset.
When `GH_CONFIG_DIR` is unset, no-mistakes looks for gh's configured hosts at `$XDG_CONFIG_HOME/gh/hosts.yml`, falling back to `~/.config/gh/hosts.yml` when `XDG_CONFIG_HOME` is unset.

## `NO_MISTAKES_UMAMI_HOST`

Override the telemetry collection host.

|         |                             |
| ------- | --------------------------- |
| Type    | `URL`                       |
| Default | `https://a.kunchenguid.com` |

When set, telemetry sends events to this host's `/api/send` endpoint. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_HOST`. If no runtime value is found, it falls back to any host embedded at build time and then the default self-hosted Umami instance.

## `NO_MISTAKES_UMAMI_WEBSITE_ID`

Override or enable the telemetry website ID.

|         |                                                                         |
| ------- | ----------------------------------------------------------------------- |
| Type    | `string`                                                                |
| Default | embedded in Makefile and release builds; unset in unembedded dev builds |

When set, telemetry uses this website ID at runtime. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_WEBSITE_ID`. If no runtime value is found, it falls back to any website ID embedded at build time.

## `NM_METADATA`

Opaque metadata supplied for the current run.

|         |                                      |
| ------- | ------------------------------------ |
| Type    | `string`                             |
| Default | empty when the run has no metadata   |

[`axi run --metadata`](/no-mistakes/reference/cli/#no-mistakes-axi-run) stores one bounded UTF-8 string exactly and does not parse it as JSON, keys, or associations. Pipeline commands and agent subprocesses receive that exact value as `NM_METADATA`; agent prompts receive a sanitized copy clearly marked as untrusted data; and the PR body formatter contract exposes the original string. An explicitly empty value is preserved as `NM_METADATA=` and clears inherited metadata on rerun. A run with no metadata also sets `NM_METADATA=`, so a value exported into the daemon's own environment can never reach a run that did not ask for it.

Metadata is non-secret input. Secret-like text is redacted only in prompt and display projections, not from the exact persisted or environment value.

When telemetry is enabled, `no-mistakes` sends command, run, approval, fix, and wizard events, completed step events with `awaiting_approval`, `fix_review`, or `failed` status, and pageviews for the human surfaces `/wizard` and `/tui` and the state-changing agent surfaces `/axi/run`, `/axi/respond`, and `/axi/abort` to Umami.
Mutation pageviews are sent alongside command events, so command status and duration remain available.
They include only flag-derived context: `/axi/run` records whether `--yes`, `--intent`, `--skip`, or a PR-note flag was present, and `/axi/respond` records the sanitized action and whether `--yes` was present.

Read-only surfaces (`axi` home, `axi status`, `axi logs`, `status`, `runs`) emit no pageview and rate-limit their command event: it is sent when the observed run state changed since the last emit, and otherwise at most once per 10 minutes, with the dedupe state persisted at `<NM_HOME>/telemetry-gate.json` so agent polling loops stay bounded across processes.
The `axi logs` command event records the sanitized step, whether `--full` was present, and whether `--run` was present; `axi status` records whether `--run` was present.
Each explicit human CLI, AXI, or TUI branch-sync check/apply attempt emits one command event and no additional pageview.
Its fields are bounded enums and booleans only: surface, mode, state, relation, target kind, pipeline phase, PR state, result, refusal reason, dirty state, and duration.
It never sends a SHA, run ID, path, branch name, URL, remote name, or command argument.

### What stays local and what leaves the machine

Everything sent to the telemetry service is low-cardinality: command names, statuses, durations, counts, flag booleans, agent and step names, and - on the single terminal `run finished` event - the bounded performance rollup `agent_invocations`, `resumed_invocations`, and `fallback_invocations` (small counts only).
Run IDs, repository paths, branch names, session identities, prompts, model outputs, diffs, and per-invocation performance records are never sent to the telemetry service.

By default, the generated PR body deliberately exposes one bounded subset of this local run evidence to the repository host: the built-in body publishes a compact Pipeline table of each top-level invocation's step, round, and agent, plus each rendered step's redacted primary commands and evidence notes. That default subset carries no token counts, cost, session identity, prompts, model outputs, or machine paths; the explicitly enabled effective-config disclosure described below is the exception. The [PR step reference](/no-mistakes/reference/pipeline-steps/#pr) owns its layout.
PR body contract v5 offers a configured formatter a wider subset: one row per top-level invocation and round with step, agent, model, provider, start time, duration, nullable total/uncached/cache-read/cache-write/output token meters, and nullable CLI-reported USD cost. Static command results, Review evidence, and human User Testing instructions are distinct fields. The formatter owns public pricing data, dated rates, harness profiles, and estimation from these raw facts; no-mistakes never synthesizes a reported cost. Supported formatter contracts are v5, v3, and v2; v4 is rejected because its producer-calculated cost receipts were removed. Invocation rows contain no session identity, prompts, outputs, diffs, credentials, or paths; the wider formatter contract separately carries repository/run placement and bounded test-artifact path fields. When a machine-local config override from the global config's [`overrides`](/no-mistakes/reference/global-config/#overrides) map is active, the contract also supplies generic source kinds and digest prefixes; full source digests, the matched key, and the global config path stay local.

Detailed performance evidence stays on the machine in the local state database (`<NM_HOME>/state.sqlite`): one `agent_invocations` row per top-level harness invocation, plus each run's accumulated parked-at-gate time.
Each row records run and step identity, purpose (such as review, review-fix, or lint-plan), the adapter-observed model and provider when available or the configured route identity otherwise, the cold/started/resumed/fallback session mode, a truncated session-identity hash, timestamps, duration, exit status, failure category, and `usage_coverage` alongside the session-fidelity metrics below. Coverage is `complete` only when the adapter's live stream proves its top-level totals account for all work; otherwise it is `unknown`. It is independent of nullable meter presence, and historical rows migrate to `unknown` without inference or backfill. A full Review row also stores the final content-free candidate pool (`agent`, model name/vendor, and optional flag) so the selected route remains auditable; fixer and non-Review rows leave that field null.
It never stores prompts, model outputs, diffs, raw command arguments, secret values, or credentials - only bounded counts, low-cardinality categories, and durations.
The same database keeps rich command definitions and controller-observed command attempts for active retained runs. Definitions contain the exact resolved script and portable runner identity; attempts include state, timing, outcome, exit or signal, and retry linkage. They are neither telemetry nor PR-body data, and stats projects only the content-free command receipts recorded in step evidence.

Each started run also retains its full resolved configuration locally in
`<NM_HOME>/runs/<runID>/effective-config.yaml`, including commands, hooks,
prompts, and other policy values, with source annotations. Its value-free
integrity sidecar is stored alongside it. Neither file is sent through
telemetry. The owner can explicitly enable [`effective_config.publish`](/no-mistakes/reference/repo-config/#effective_configpublish), which sends the complete comment-free YAML plus its run ID and sidecar digests to the repository host in the built-in GitHub PR body. This is exact disclosure, not secret-safe rendering: values may include commands, hooks, prompts, paths, or credentials, and carry no confidentiality guarantee. Successful custom formatter v5 output and non-GitHub PRs do not publish it. Both local files are pruned with the run's rich local data.

The additive session-fidelity fields are nullable and read back as unknown rather than a fabricated zero when the adapter did not report them, so rows written before a field existed, and adapters that do not surface a datum, stay honest.
The raw counters remain available locally; use the nullable per-round and canonical fields to determine whether the adapter reported comparable usage. The canonical run audit emits legacy raw input/output/cache-read counters only when the matching per-round meter proves that the adapter reported usage:

- Token detail: `input_tokens`/`output_tokens`/`cache_read_tokens`/`cache_creation_tokens` are raw CLI counters and may be cumulative across a resumed session. `delta_input_tokens`, `delta_output_tokens`, `delta_cache_read_tokens`, and `delta_cache_creation_tokens` are the per-round amounts. `fresh_input_tokens` is the canonical uncached-input meter only when the adapter's cache relationship is verified; ambiguous cache splits stay unknown. `reported_cost_usd` stores a CLI-reported charge when available; no-mistakes does not derive list-price or harness-adjusted estimates.
- Activity: `model_roundtrips` (a proxy for productive model turns), `tool_calls`, and a bounded tool-category histogram (`tool_wait_calls`, `tool_test_lint_calls`, `tool_edit_calls`, `tool_read_calls`, `tool_git_calls`, `tool_other_calls`); a compound command counts once per sub-command, so the histogram can sum higher than `tool_calls`.
- Timing split: `subprocess_wait_ms` is the wall-clock spent inside tool subprocesses; model/reasoning time is the invocation duration minus it, clamped at zero.
- Context: `workload_files`/`workload_lines` (bounded change size), `finding_count` (findings in the structured output), and `fallback_reason` (why a failed resume forced a fresh session, one of transient/parse/exit/spawn/unsupported/other).
- Attribution scope: each record describes only the top-level harness invocation selected for the step. Child-agent identities and counts are not collected or inferred; coverage remains `unknown` when an adapter cannot prove that the top-level meters include all work.

The count and timing definitions and the closed `usage_coverage` vocabulary live in one authoritative place (`internal/agent/invocationmetrics.go`).
Inspect the evidence with `no-mistakes stats --agents` (per-purpose aggregate tables followed by detailed invocation facts) or `no-mistakes stats --run <id>` (one run's steps, content-free command/runner and repair-progress receipts, skip and Review-route receipts, per-step top-level invocations, explicit usage coverage, the per-round-vs-cumulative token split, and parked time). Add `--format json` for the normative local `Report` contract, where each nullable aggregate meter carries coverage, or use the direct repository/time/step/agent/model/purpose/status filters with text, JSON, or long-form CSV projections. The filters scope the dashboard and per-purpose aggregates as well as detailed records. CSV carries one row per normative JSON leaf and names it with `json_path`.

Rich run rows and local artifacts follow the [`test.evidence` retention policy](/no-mistakes/reference/global-config/#local-storage-and-cleanup). Before an older rich run is removed, no-mistakes stores an immutable content-free metric receipt outside the run's foreign-key cascade. Archived runs remain in every stats surface with `rich_data_retained: false`; receipts keep binary build provenance, categorical routes/statuses, per-round status, per-invocation `usage_coverage`, timings, nullable meters and activity, finding counts, nullable CLI-reported cost, bounded-repair fingerprints/results, and stripped per-command outcome/source plus runner provenance. Durable command definitions, attempt state, retry links, and exact command text are removed rather than retained. Producer pricing receipts and estimated cost classes are removed rather than retained. Receipts from before usage coverage existed read it as `unknown`, without inference or backfill. They never keep run branch/head/base identity, paths, config refs, session keys, findings prose, prompts, outputs, diffs, command text, resolved executable paths/command argv, or tool arguments; allowed runner facts are the bare shell identity, fixed shell args, source/platform category, and optional numeric version.

## `NO_MISTAKES_TELEMETRY`

Disable telemetry collection.

|         |                                                                   |
| ------- | ----------------------------------------------------------------- |
| Type    | `0`, `false`, or `off` to disable; anything else to leave enabled |
| Default | unset                                                             |

When set to a disabling value, telemetry stays off even if a runtime or embedded website ID is available.

## Environment the daemon sees

When the daemon runs through a managed service (launchd, systemd user service, Task Scheduler), the macOS and Linux service definitions include a default `PATH` with common user and system binary directories. macOS and Linux also bake in any proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`) set when the service is installed or refreshed; the Windows task deliberately does not persist proxy variables or credentials. Proxy values are preserved across later macOS/Linux service refreshes and restarts when absent from the current shell. Both upper- and lower-case proxy spellings are forwarded exactly as set. Because a proxy URL can embed credentials, generated macOS/Linux service files are restricted to owner-only `0600` permissions whenever proxy values are present; otherwise they keep the conventional `0644` mode. At daemon startup, the daemon resolves environment from the login shell on macOS and Linux, preserves its `PATH` order, and appends missing well-known directories such as `~/.local/bin`, `~/go/bin`, `~/.cargo/bin`, `~/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, and `/bin`. If login-shell resolution fails or returns no entries, the daemon logs a warning and uses an augmented process-environment fallback that may omit version-manager directories such as nvm, fnm, or volta. On Windows it reuses the current process environment.

If your env vars aren't set in your login shell's rc files (`.zprofile`, `.zshrc`, `.profile`, `.bash_profile`, `.bashrc`, PowerShell profile), the daemon won't see them. Put them somewhere a login shell will load, then restart the daemon to pick them up.
