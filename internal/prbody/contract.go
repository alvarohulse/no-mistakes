// Package prbody defines the data contract handed to an external PR body
// formatter (hooks.pr_body) and runs that formatter.
//
// The contract carries the PR body's raw material, never a pre-rendered
// layout. A formatter that receives markdown has already had its layout
// decision made for it, which defeats the point of the hook; so the pipeline
// section is per-step records, risk is its three stored fields, and static
// tests, review evidence, and user-testing instructions remain distinct.
package prbody

import "github.com/kunchenguid/no-mistakes/internal/legacycost"

// Version is the contract version emitted by this build. A formatter that
// does not recognize the version should exit non-zero rather than guess; the
// pipeline reports that failure and may fall back to built-in section content,
// subject to the same marker and publication validation as formatter output.
const Version = 5

// Contract is the complete payload written to the formatter's stdin.
//
// Presence rules apply to Sections, because absence carries different
// meanings:
//
//   - Risk and Notes are always present, each with a boolean saying whether
//     anything was actually reported or supplied. Their absence is itself
//     information a reader needs ("no risk assessment ran", "the author left
//     no note"), so it is stated rather than implied.
//   - Version 4 and 5 producers always emit UserTesting so its attestation state is
//     explicit; version 2 and 3 contracts omit that unsupported field.
//   - Every other optional section is absent when there is nothing to say.
type Contract struct {
	Version int    `json:"version"`
	RunID   string `json:"run_id"`

	Repo            Repo   `json:"repo"`
	Branch          string `json:"branch"`
	BaseBranch      string `json:"base_branch"`
	BaseSHA         string `json:"base_sha"`
	HeadSHA         string `json:"head_sha"`
	RefreshStrategy string `json:"refresh_strategy"`

	// Provider is the detected SCM host ("github", "azure", ...).
	Provider string `json:"provider"`
	// BodyLimit is the host's PR body character cap, 0 when unlimited. A
	// formatter should respect it; the pipeline rejects an oversized marked
	// candidate rather than truncating section content after hashing it.
	BodyLimit int `json:"body_limit"`

	// Title is the drafted conventional-commit PR title. The formatter owns
	// section content and the optional bootstrap layout only; returning a title
	// has no effect.
	Title string `json:"title"`
	// Metadata is opaque operator-supplied context. no-mistakes does not parse
	// or assign structure to it; formatters may interpret it for their own use.
	Metadata string `json:"metadata,omitempty"`

	Commits  []Commit `json:"commits,omitempty"`
	Sections Sections `json:"sections"`
}

// Repo identifies the repository the run belongs to.
type Repo struct {
	Root          string `json:"root,omitempty"`
	UpstreamURL   string `json:"upstream_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// Commit is one commit in the branch delta.
type Commit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// Sections holds the body's raw material.
type Sections struct {
	// Intent is retained only so callers can decode a version 2 contract.
	// Versions 3 through 5 report intent on the Intent pipeline step instead.
	Intent      *IntentSection `json:"intent,omitempty"`
	Summary     *TextSection   `json:"summary,omitempty"`
	Notes       NotesSection   `json:"notes"`
	WhatChanged *TextSection   `json:"what_changed,omitempty"`
	Risk        RiskSection    `json:"risk"`
	// Testing is retained only so callers can decode version 2 and 3
	// contracts. Version 4 and 5 producers use the three distinct fields below.
	Testing        *TestingSection        `json:"testing,omitempty"`
	StaticTests    *StaticTestsSection    `json:"static_tests,omitempty"`
	ReviewEvidence *ReviewEvidenceSection `json:"review_evidence,omitempty"`
	UserTesting    *UserTestingSection    `json:"user_testing,omitempty"`
	Pipeline       *PipelineSection       `json:"pipeline,omitempty"`
}

// IntentSection is the change author's goal for the branch.
type IntentSection struct {
	Text string `json:"text"`
	// Source is "agent" when supplied explicitly via `axi run --intent`, or
	// the name of the agent whose transcript it was inferred from.
	Source string `json:"source,omitempty"`
	// Authoritative is true only for an explicit --intent, which is the
	// author's own statement of the goal. An inferred intent is a hint.
	Authoritative bool `json:"authoritative"`
	// Trusted is always false: intent text is sanitized, adversarially
	// stripped, and secret-redacted before it reaches this contract, because
	// an inferred intent can carry anything a transcript carried.
	Trusted bool `json:"trusted"`
}

// NotesSection is the author's `--pr-note`.
type NotesSection struct {
	Text string `json:"text"`
	// Supplied distinguishes "the author wrote no note" from an empty one.
	Supplied bool `json:"supplied"`
	// Trusted is always true, and is the reason this section is called out
	// separately: the note is author-supplied and rendered verbatim. Unlike
	// intent it receives no adversarial stripping and no secret redaction, so
	// a formatter must not treat it as sanitized data - and nothing that
	// reaches it should ever contain a secret.
	Trusted bool `json:"trusted"`
}

// TextSection is a plain markdown fragment.
type TextSection struct {
	Text string `json:"text"`
}

// RiskSection is the review step's risk assessment, always present.
//
// The level is not cosmetic: `high` is what triggers the cross-vendor
// adversary review, so a reader needs the level and the reasoning behind it.
type RiskSection struct {
	Level     string `json:"level"`
	Rationale string `json:"rationale"`
	Scope     string `json:"scope"`
	// Reported is false when no review step recorded an assessment, which is
	// different from a review that assessed the change as low risk.
	Reported bool `json:"reported"`
}

// TestingSection is the test step's own evidence.
type TestingSection struct {
	Summary   string     `json:"summary,omitempty"`
	Tested    []string   `json:"tested,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// StaticTestsSection contains objective command outcomes and artifacts. It
// never contains instructions for a human to perform later.
type StaticTestsSection struct {
	Summary   string            `json:"summary,omitempty"`
	Commands  []PipelineCommand `json:"commands,omitempty"`
	Reported  []string          `json:"reported,omitempty"`
	Artifacts []Artifact        `json:"artifacts,omitempty"`
}

// ReviewEvidenceSection is the Review step's recorded evidence, kept apart
// from static command results and from future human testing instructions.
type ReviewEvidenceSection struct {
	Status   string       `json:"status"`
	Rounds   int          `json:"rounds"`
	Findings StepFindings `json:"findings"`
	Evidence []string     `json:"evidence,omitempty"`
}

// UserTestingSection contains instructions for a human. Attested is false
// unless an explicit human completion signal was supplied; instructions alone
// must never be rendered as test evidence.
type UserTestingSection struct {
	Instructions []string `json:"instructions,omitempty"`
	Attested     bool     `json:"attested"`
}

// Artifact is one piece of test evidence produced for human review.
type Artifact struct {
	Kind  string `json:"kind,omitempty"`
	Label string `json:"label"`
	Path  string `json:"path,omitempty"`
	URL   string `json:"url,omitempty"`
}

// PipelineSection is per-step execution data, not a rendered table.
type PipelineSection struct {
	Steps         []PipelineStep `json:"steps"`
	ConfigSources []ConfigSource `json:"config_sources,omitempty"`
	// Attribution is the link back to no-mistakes. Emitting it is the
	// formatter's choice; the URL is supplied so it need not be hardcoded.
	Attribution Attribution `json:"attribution"`
}

// Attribution is the "generated by" credit for the pipeline section.
type Attribution struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ConfigSource records one configuration layer that fed the run.
type ConfigSource struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest,omitempty"`
}

// PipelineStep is one step's execution record.
type PipelineStep struct {
	Name string `json:"name"`
	// Label is the display name for this run's settings: `refresh` renders as
	// "Rebase" or "Merge" depending on the strategy, so a formatter should
	// print Label and key off Name.
	Label       string            `json:"label"`
	Order       int               `json:"order"`
	Status      string            `json:"status"`
	SkipSource  *string           `json:"skip_source,omitempty"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	DurationMS  *int64            `json:"duration_ms,omitempty"`
	Rounds      int               `json:"rounds"`
	Findings    StepFindings      `json:"findings"`
	Agents      []AgentRun        `json:"agents,omitempty"`
	Intent      *IntentResult     `json:"intent,omitempty"`
	Commands    []PipelineCommand `json:"commands,omitempty"`
	Evidence    []string          `json:"evidence,omitempty"`
	Explanation string            `json:"explanation,omitempty"`
}

// PipelineCommand is one primary operation executed by a pipeline step.
type PipelineCommand struct {
	Round    int    `json:"round"`
	Sequence int    `json:"sequence"`
	Command  string `json:"command"`
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exit_code"`
}

// IntentResult is the Intent step's structured result. Provided distinguishes
// an explicit --intent from a transcript inference; Reason explains why no
// text was available without forcing a formatter to scrape step logs.
type IntentResult struct {
	Text     string        `json:"text,omitempty"`
	Source   string        `json:"source,omitempty"`
	Provided bool          `json:"provided"`
	Reason   *IntentReason `json:"reason,omitempty"`
}

// IntentReason is a stable machine-readable absence reason plus display text.
type IntentReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StepFindings counts a step's final findings.
type StepFindings struct {
	Total int `json:"total"`
	// BySeverity is keyed by the severity string the finding carried, so a new
	// severity shows up without a schema change.
	BySeverity map[string]int `json:"by_severity,omitempty"`
}

// AgentRun is one agent invocation within a step.
type AgentRun struct {
	Round               int      `json:"round"`
	Purpose             string   `json:"purpose,omitempty"`
	Agent               string   `json:"agent,omitempty"`
	Model               string   `json:"model,omitempty"`
	Provider            string   `json:"provider,omitempty"`
	StartedAt           int64    `json:"started_at,omitempty"`
	DurationMS          int64    `json:"duration_ms,omitempty"`
	InputTokens         *int     `json:"input_tokens,omitempty"`
	OutputTokens        *int     `json:"output_tokens,omitempty"`
	UncachedInputTokens *int     `json:"uncached_input_tokens,omitempty"`
	CacheReadTokens     *int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    *int     `json:"cache_write_tokens,omitempty"`
	ReportedCostUSD     *float64 `json:"reported_cost_usd,omitempty"`
	// Costs is retained only to decode legacy version 4 contracts. Version 5
	// producers emit raw meters and optional reported cost, never estimates.
	Costs *legacycost.CostClasses `json:"costs,omitempty"`
	// Vendor is the provider that served the model (anthropic, openai, ...).
	// Empty when the adapter does not report one.
	Vendor string `json:"vendor,omitempty"`
}
