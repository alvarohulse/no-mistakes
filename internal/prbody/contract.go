// Package prbody defines the data contract handed to an external PR body
// formatter (hooks.pr_body) and runs that formatter.
//
// The contract carries the PR body's raw material, never a pre-rendered
// layout. A formatter that receives markdown has already had its layout
// decision made for it, which defeats the point of the hook; so the pipeline
// section is per-step records, risk is its three stored fields, and testing is
// the test step's own summary, tested list, and artifacts.
package prbody

// Version is the contract version emitted by this build. A formatter that
// does not recognize the version should exit non-zero rather than guess; the
// pipeline treats a non-zero exit as "use the built-in body" and says so.
const Version = 3

// Contract is the complete payload written to the formatter's stdin.
//
// Two presence rules apply to Sections, because absence carries different
// meanings:
//
//   - Risk and Notes are always present, each with a boolean saying whether
//     anything was actually reported or supplied. Their absence is itself
//     information a reader needs ("no risk assessment ran", "the author left
//     no note"), so it is stated rather than implied.
//   - Every other section is an absent key when there is nothing to say, so a
//     formatter can tell "nothing to say" from "said nothing".
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
	// formatter should respect it; the pipeline clamps anything over it
	// afterwards and logs that it did.
	BodyLimit int `json:"body_limit"`

	// Title is the drafted conventional-commit PR title. The formatter owns
	// the body only - returning a title has no effect.
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
	// Intent is retained only so callers can decode a version 2 contract. Version
	// 3 producers leave it empty and report intent on the Intent pipeline step.
	Intent      *IntentSection   `json:"intent,omitempty"`
	Summary     *TextSection     `json:"summary,omitempty"`
	Notes       NotesSection     `json:"notes"`
	WhatChanged *TextSection     `json:"what_changed,omitempty"`
	Risk        RiskSection      `json:"risk"`
	Testing     *TestingSection  `json:"testing,omitempty"`
	Pipeline    *PipelineSection `json:"pipeline,omitempty"`
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
	Label      string        `json:"label"`
	Order      int           `json:"order"`
	Status     string        `json:"status"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	DurationMS *int64        `json:"duration_ms,omitempty"`
	Rounds     int           `json:"rounds"`
	Findings   StepFindings  `json:"findings"`
	Agents     []AgentRun    `json:"agents,omitempty"`
	Intent     *IntentResult `json:"intent,omitempty"`
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
	Round   int    `json:"round"`
	Purpose string `json:"purpose,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`
	// Vendor is the provider that served the model (anthropic, openai, ...).
	// Empty when the adapter does not report one.
	Vendor         string `json:"vendor,omitempty"`
	InvocationMode string `json:"invocation_mode,omitempty"`
	// Nested lists sub-agents the adapter's event stream exposed.
	// NestedReported separates "an adapter that reports nesting saw none" from
	// "this adapter exposes no such evidence"; without it, an empty list reads
	// as a claim the run made no sub-agent calls.
	Nested         []NestedAgent `json:"nested,omitempty"`
	NestedReported bool          `json:"nested_reported"`
}

// NestedAgent is a sub-agent invocation observed inside an AgentRun.
type NestedAgent struct {
	Identity       string `json:"identity,omitempty"`
	InvocationMode string `json:"invocation_mode,omitempty"`
}
