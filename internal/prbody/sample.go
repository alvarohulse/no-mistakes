package prbody

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
		Commits: []Commit{
			{SHA: "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Subject: "fix(scheduler): bound the retry window"},
			{SHA: "9f2c1ad7e5b04c8e1a6f3d90b27c4e8815d6a3f1", Subject: "test(scheduler): cover the exhausted-budget path"},
		},
		Sections: Sections{
			Intent: &IntentSection{
				Text:          "Retries on a saturated queue never stop, so one poisoned job pins a worker until the pod is cycled. Bound the window and surface exhaustion instead of retrying forever.",
				Source:        "agent",
				Authoritative: true,
				Trusted:       false,
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
				Rationale: "Changes a retry path every queued job traverses. The new failure mode is reachable in production, but it is a bounded fail-fast that replaces an unbounded hang, and the exhaustion path is covered by tests.",
				Scope:     "source-or-external",
				Reported:  true,
			},
			Testing: &TestingSection{
				Summary:   "Added coverage for the exhausted-budget path and the invalid-window rejection; ran the scheduler package plus the queue integration suite.",
				Tested:    []string{"go test ./internal/scheduler/...", "go test -tags=integration ./internal/queue/..."},
				Artifacts: []Artifact{{Kind: "log", Label: "scheduler suite output", Path: ".no-mistakes/artifacts/scheduler-test.log"}},
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
						Agents: []AgentRun{{
							Round: 1, Purpose: "intent", Agent: "claude", Model: "claude-opus-5",
							Vendor: "anthropic", InvocationMode: "harness_cli", NestedReported: true,
						}},
					},
					{
						// Label varies with the run's refresh strategy: this
						// same step renders as "Merge" under merge.
						Name: "refresh", Label: "Rebase", Order: 2, Status: "completed",
						ExitCode: &exit, DurationMS: ms(1180), Rounds: 1,
					},
					{
						Name: "review", Label: "Review", Order: 3, Status: "completed",
						ExitCode: &exit, DurationMS: ms(311420), Rounds: 2,
						Findings: StepFindings{Total: 3, BySeverity: map[string]int{"P1": 1, "P2": 2}},
						Agents: []AgentRun{
							{
								Round: 1, Purpose: "review", Agent: "claude", Model: "claude-opus-5",
								Vendor: "anthropic", InvocationMode: "harness_cli", NestedReported: true,
								Nested: []NestedAgent{{Identity: "Explore", InvocationMode: "subagent_tool"}},
							},
							{
								Round: 2, Purpose: "review-fix", Agent: "claude", Model: "claude-opus-5",
								Vendor: "anthropic", InvocationMode: "harness_cli", NestedReported: true,
							},
						},
					},
					{
						Name: "build", Label: "Build", Order: 4, Status: "completed",
						ExitCode: &exit, DurationMS: ms(48310), Rounds: 1,
					},
					{
						// A step that failed, auto-fixed, and passed on the
						// retry: the exit code is the final one, the round
						// count is how it got there.
						Name: "test", Label: "Test", Order: 5, Status: "completed",
						ExitCode: &exit, DurationMS: ms(602750), Rounds: 2,
						Findings: StepFindings{Total: 1, BySeverity: map[string]int{"P0": 1}},
						Agents: []AgentRun{{
							Round: 2, Purpose: "test-evidence", Agent: "codex", Model: "gpt-5.6-sol",
							Vendor: "openai", InvocationMode: "harness_cli", NestedReported: false,
						}},
					},
					{
						Name: "document", Label: "Document", Order: 6, Status: "completed",
						ExitCode: &exit, DurationMS: ms(94120), Rounds: 1,
					},
					{
						Name: "lint", Label: "Lint", Order: 7, Status: "completed",
						ExitCode: &exit, DurationMS: ms(21870), Rounds: 1,
					},
					{
						Name: "push", Label: "Push", Order: 8, Status: "completed",
						ExitCode: &exit, DurationMS: ms(3410), Rounds: 1,
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
