package prbody

import "github.com/kunchenguid/no-mistakes/internal/pricing"

// SampleForVersion returns the sample contract for one supported version, or
// nil when the version is not supported. Formatter authors are told to accept
// both v2 and v3 during a producer rollout, so both shapes have to be
// reachable from a single command rather than only the newest one.
func SampleForVersion(version int) *Contract {
	switch version {
	case 2:
		return SampleV2()
	case 3:
		return SampleV3()
	case Version:
		return Sample()
	default:
		return nil
	}
}

// SupportedVersions lists the contract versions this build can emit and read,
// newest first.
func SupportedVersions() []int { return []int{Version, 3, 2} }

// IsSupportedVersion reports whether a decoded contract's version is one this
// build understands.
func IsSupportedVersion(version int) bool {
	for _, supported := range SupportedVersions() {
		if version == supported {
			return true
		}
	}
	return false
}

// SampleV2 returns the version 2 shape of the same sample run: intent lives in
// its own top-level section, and none of the later contract additions are present.
// It is derived from Sample so the two cannot drift apart.
func SampleV2() *Contract {
	contract := SampleV3()
	contract.Version = 2
	contract.Metadata = ""

	if pipeline := contract.Sections.Pipeline; pipeline != nil {
		for i := range pipeline.Steps {
			step := &pipeline.Steps[i]
			if step.Intent != nil {
				contract.Sections.Intent = &IntentSection{
					Text:          step.Intent.Text,
					Source:        step.Intent.Source,
					Authoritative: step.Intent.Provided,
					Trusted:       false,
				}
			}
			step.Intent = nil
			step.Commands = nil
			step.Evidence = nil
			step.Explanation = ""
			for j := range step.Agents {
				run := &step.Agents[j]
				run.Provider = ""
				run.StartedAt = 0
				run.DurationMS = 0
				run.InputTokens = nil
				run.OutputTokens = nil
				run.UncachedInputTokens = nil
				run.CacheReadTokens = nil
				run.CacheWriteTokens = nil
				run.ReportedCostUSD = nil
				run.NestedCount = nil
			}
		}
	}
	contract.Sections.Summary = nil
	return contract
}

// SampleV3 returns the pre-v4 contract shape. Static test evidence is projected
// back into the legacy testing field, and v4-only review, user-testing, and cost
// receipt fields are absent.
func SampleV3() *Contract {
	contract := Sample()
	contract.Version = 3
	if static := contract.Sections.StaticTests; static != nil {
		contract.Sections.Testing = &TestingSection{
			Summary:   static.Summary,
			Tested:    append([]string(nil), static.Reported...),
			Artifacts: append([]Artifact(nil), static.Artifacts...),
		}
	}
	contract.Sections.StaticTests = nil
	contract.Sections.ReviewEvidence = nil
	contract.Sections.UserTesting = nil
	if pipeline := contract.Sections.Pipeline; pipeline != nil {
		for i := range pipeline.Steps {
			for j := range pipeline.Steps[i].Agents {
				pipeline.Steps[i].Agents[j].Costs = nil
			}
		}
	}
	return contract
}

// Sample returns a contract that exercises every section.
//
// This is deliberately not a transcript of a real run. A sample built to be
// faithful to one run is faithful to that run's gaps too - the run that
// motivated this contract carried no author note and no risk assessment, so
// both came out as absent keys: correct under the presence rules, and useless
// for reviewing the shape. Fidelity to a run and coverage of a contract are
// different jobs. This one covers the contract.
func Sample() *Contract {
	exit := 0
	failExit := 1
	ms := func(v int64) *int64 { return &v }
	integer := func(v int) *int { return &v }
	usd := func(v float64) *float64 { return &v }
	costs := func(reported, list, adjusted float64) *pricing.CostClasses {
		return &pricing.CostClasses{
			HarnessReported: pricing.CostEstimate{
				ValueUSD: usd(reported), Coverage: pricing.Coverage{Reported: 1, Eligible: 1}, Complete: true,
				Basis: "agent_invocations.reported_cost_usd",
			},
			APIListEstimate: pricing.CostEstimate{
				ValueUSD: usd(list), Coverage: pricing.Coverage{Reported: 4, Eligible: 4}, Complete: true,
				Basis:      "canonical_delta_token_meters_x_public_list_rate",
				Provenance: pricing.Provenance{CatalogVersion: 1, CatalogSHA256: "sha256:sample", PriceSourceURL: "https://example.com/public-pricing"},
			},
			HarnessAdjustedEstimate: pricing.CostEstimate{
				ValueUSD: usd(adjusted), Coverage: pricing.Coverage{Reported: 4, Eligible: 4}, Complete: true,
				Basis:      "public_list_estimate_plus_harness_profile",
				Provenance: pricing.Provenance{CatalogVersion: 1, CatalogSHA256: "sha256:sample", ProfileID: "sample-profile", ProfileVersion: 1},
			},
		}
	}
	command := func(round, sequence int, text, outcome string, exitCode *int) PipelineCommand {
		return PipelineCommand{Round: round, Sequence: sequence, Command: text, Outcome: outcome, ExitCode: exitCode}
	}

	return &Contract{
		Version: Version,
		RunID:   "01SAMPLE0000000000000000000",
		Repo: Repo{
			Root:          "/home/you/repos/example",
			UpstreamURL:   "git@github.com:example/example.git",
			DefaultBranch: "main",
		},
		Branch:          "you/eng-4471-bound-the-retry-window",
		BaseBranch:      "main",
		BaseSHA:         "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		HeadSHA:         "9f2c1ad7e5b04c8e1a6f3d90b27c4e8815d6a3f1",
		RefreshStrategy: "rebase",
		Provider:        "github",
		BodyLimit:       0,
		Title:           "fix(scheduler): bound the retry window",
		Metadata:        "resolves ENG-4471\ncontributes to ENG-4520",
		Commits: []Commit{
			{SHA: "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Subject: "fix(scheduler): bound the retry window"},
			{SHA: "9f2c1ad7e5b04c8e1a6f3d90b27c4e8815d6a3f1", Subject: "test(scheduler): cover the exhausted-budget path"},
		},
		Sections: Sections{
			Summary: &TextSection{
				Text: "Bounds scheduler retries so one poisoned job cannot pin a worker indefinitely. Exhausted jobs now fail explicitly through `scheduler.retry.exhausted`.",
			},
			Notes: NotesSection{
				Text:     "Deliberately not touching the dead-letter path in this PR - it needs the queue-depth metric first, tracked separately.",
				Supplied: true,
				Trusted:  true,
			},
			WhatChanged: &TextSection{
				Text: "- Cap scheduler retries at `maxRetryWindow` and fail the job once the budget is spent\n- Emit `scheduler.retry.exhausted` with the job ID and elapsed window\n- Treat a non-positive configured window as invalid at load time rather than as unlimited",
			},
			Risk: RiskSection{
				Level:     "medium",
				Rationale: "Changes the `retryBudget` path every queued job traverses. The new failure mode is reachable in production, but it is a **bounded fail-fast** that replaces an unbounded hang, and the exhaustion path is covered by tests.",
				Scope:     "source-or-external",
				Reported:  true,
			},
			StaticTests: &StaticTestsSection{
				Summary:  "Added coverage for the exhausted-budget path and the invalid-window rejection; ran the scheduler package plus the queue integration suite.",
				Reported: []string{"go test ./internal/scheduler/...", "go test -tags=integration ./internal/queue/..."},
				Commands: []PipelineCommand{
					command(1, 1, "go test ./internal/scheduler/...", "passed", &exit),
					command(1, 2, "go test -tags=integration ./internal/queue/...", "passed", &exit),
				},
				Artifacts: []Artifact{{Kind: "log", Label: "scheduler suite output", Path: ".no-mistakes/artifacts/scheduler-test.log"}},
			},
			ReviewEvidence: &ReviewEvidenceSection{
				Status: "completed", Rounds: 2,
				Findings: StepFindings{Total: 3, BySeverity: map[string]int{"P1": 1, "P2": 2}},
				Evidence: []string{"Reviewed the complete branch diff against the explicit intent."},
			},
			UserTesting: &UserTestingSection{
				Instructions: []string{"Trigger a retry-exhausted job and confirm the operator-facing failure state."},
				Attested:     false,
			},
			Pipeline: &PipelineSection{
				Attribution: Attribution{Name: "no-mistakes", URL: "https://github.com/kunchenguid/no-mistakes"},
				ConfigSources: []ConfigSource{
					{Kind: "global", Digest: "sha256:5f2d8c1b9a04"},
					{Kind: "machine-repo", Digest: "sha256:c71e40ab6d92"},
				},
				Steps: []PipelineStep{
					{
						Name: "intent", Label: "Intent", Order: 1, Status: "completed",
						ExitCode: &exit, DurationMS: ms(2140), Rounds: 1,
						Intent: &IntentResult{
							Text:     "Retries on a saturated queue never stop, so one poisoned job pins a worker until the pod is cycled. Bound the window and surface exhaustion instead of retrying forever.",
							Source:   "agent",
							Provided: true,
						},
						Agents: []AgentRun{{
							Round: 1, Purpose: "intent", Agent: "claude", Model: "claude-opus-5",
							Provider: "anthropic", Vendor: "anthropic", InvocationMode: "harness_cli",
							StartedAt: 1786500000, DurationMS: 2140,
							InputTokens: integer(1400), OutputTokens: integer(180), UncachedInputTokens: integer(700),
							CacheReadTokens: integer(500), CacheWriteTokens: integer(200), ReportedCostUSD: usd(0.08),
							Costs:          costs(0.08, 0.09, 0.09),
							NestedReported: true, NestedCount: integer(0),
						}},
					},
					{
						// Label varies with the run's refresh strategy: this
						// same step renders as "Merge" under merge.
						Name: "refresh", Label: "Rebase", Order: 2, Status: "completed",
						ExitCode: &exit, DurationMS: ms(1180), Rounds: 1,
						Commands: []PipelineCommand{command(1, 1, "git rebase origin/main", "passed", &exit)},
					},
					{
						Name: "review", Label: "Review", Order: 3, Status: "completed",
						ExitCode: &exit, DurationMS: ms(311420), Rounds: 2,
						Findings: StepFindings{Total: 3, BySeverity: map[string]int{"P1": 1, "P2": 2}},
						Evidence: []string{"Reviewed the complete branch diff against the explicit intent."},
						Agents: []AgentRun{
							{
								Round: 1, Purpose: "review", Agent: "claude", Model: "claude-opus-5",
								Provider: "anthropic", Vendor: "anthropic", InvocationMode: "harness_cli",
								StartedAt: 1786500300, DurationMS: 241000,
								InputTokens: integer(900000), OutputTokens: integer(18000), UncachedInputTokens: integer(120000),
								CacheReadTokens: integer(730000), CacheWriteTokens: integer(50000), ReportedCostUSD: usd(4.65),
								NestedReported: true, NestedCount: integer(2),
								Nested: []NestedAgent{
									{Identity: "Explore:session-1", InvocationMode: "subagent_tool"},
									{Identity: "Explore:session-2", InvocationMode: "subagent_tool"},
								},
							},
							{
								Round: 2, Purpose: "review-fix", Agent: "cursor", Model: "claude-4.5-sonnet",
								InvocationMode: "harness_cli", StartedAt: 1786500541, DurationMS: 70420,
								OutputTokens: integer(6400), UncachedInputTokens: integer(9000),
								CacheReadTokens: integer(32000), CacheWriteTokens: integer(11000), NestedReported: false,
							},
						},
					},
					{
						Name: "build", Label: "Build", Order: 4, Status: "completed",
						ExitCode: &exit, DurationMS: ms(48310), Rounds: 1,
						Commands: []PipelineCommand{command(1, 1, "go build ./cmd/...", "passed", &exit)},
					},
					{
						// A step that failed, auto-fixed, and passed on the
						// retry: the exit code is the final one, the round
						// count is how it got there.
						Name: "test", Label: "Test", Order: 5, Status: "completed",
						ExitCode: &exit, DurationMS: ms(602750), Rounds: 2,
						Findings: StepFindings{Total: 1, BySeverity: map[string]int{"P0": 1}},
						Commands: []PipelineCommand{
							command(1, 1, "go test ./internal/scheduler/...", "failed", &failExit),
							command(2, 1, "go test ./internal/scheduler/...", "passed", &exit),
						},
						Evidence: []string{"Scheduler regression suite passed after repairing the exhausted-budget assertion."},
						Agents: []AgentRun{{
							Round: 2, Purpose: "test-evidence", Agent: "codex", Model: "gpt-5.6-sol",
							Provider: "openai", Vendor: "openai", InvocationMode: "harness_cli",
							StartedAt: 1786500620, DurationMS: 178300,
							InputTokens: integer(93000), OutputTokens: integer(12400), UncachedInputTokens: integer(13000),
							CacheReadTokens: integer(72000), CacheWriteTokens: integer(8000), NestedReported: false,
						}},
					},
					{
						Name: "document", Label: "Document", Order: 6, Status: "skipped",
						DurationMS: ms(12), Rounds: 1,
						Explanation: "No documentation-owned behavior changed.",
					},
					{
						Name: "lint", Label: "Lint", Order: 7, Status: "completed",
						ExitCode: &exit, DurationMS: ms(21870), Rounds: 1,
						Commands: []PipelineCommand{command(1, 1, "golangci-lint run ./...", "passed", &exit)},
					},
					{
						Name: "push", Label: "Push", Order: 8, Status: "completed",
						ExitCode: &exit, DurationMS: ms(3410), Rounds: 1,
						Commands: []PipelineCommand{command(1, 1, "git push origin you/eng-4471-bound-the-retry-window", "passed", &exit)},
					},
					{
						Name: "pr", Label: "PR", Order: 9, Status: "running",
						DurationMS: ms(1020), Rounds: 1,
					},
					{
						// A terminal failure, so a formatter that only styles
						// the happy path is visibly wrong against this sample.
						Name: "ci", Label: "CI", Order: 10, Status: "failed",
						ExitCode: &failExit, DurationMS: ms(1284300), Rounds: 1,
						Findings: StepFindings{Total: 1, BySeverity: map[string]int{"P1": 1}},
					},
				},
			},
		},
	}
}
