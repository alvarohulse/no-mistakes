package prbody

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestSampleExercisesEverySection is the guard against the mistake that
// produced contract v1's rejected sample: it was built to be faithful to one
// real run, and that run had no author note and no risk assessment, so both
// came out as absent keys. A sample whose job is to let someone review the
// contract has to populate all of it.
func TestSampleExercisesEverySection(t *testing.T) {
	t.Parallel()
	sample := Sample()
	s := sample.Sections

	if sample.Metadata == "" {
		t.Error("sample has no metadata")
	}
	if s.Summary == nil || s.Summary.Text == "" {
		t.Error("sample has no summary")
	}
	if !s.Notes.Supplied || s.Notes.Text == "" {
		t.Error("sample has no author note")
	}
	if !s.Notes.Trusted {
		t.Error("sample note is not marked trusted")
	}
	if s.WhatChanged == nil || s.WhatChanged.Text == "" {
		t.Error("sample has no what_changed")
	}
	if !s.Risk.Reported || s.Risk.Level == "" || s.Risk.Rationale == "" || s.Risk.Scope == "" {
		t.Errorf("sample risk is incomplete: %+v", s.Risk)
	}
	if s.StaticTests == nil || s.StaticTests.Summary == "" || len(s.StaticTests.Commands) == 0 || len(s.StaticTests.Reported) == 0 || len(s.StaticTests.Artifacts) == 0 {
		t.Error("sample static testing is incomplete")
	}
	if s.ReviewEvidence == nil || s.ReviewEvidence.Status == "" || s.ReviewEvidence.Rounds == 0 || len(s.ReviewEvidence.Evidence) == 0 {
		t.Error("sample review evidence is incomplete")
	}
	if s.UserTesting == nil || len(s.UserTesting.Instructions) == 0 || s.UserTesting.Attested {
		t.Error("sample user testing must be an unattested instruction")
	}
	if s.Pipeline == nil || len(s.Pipeline.Steps) == 0 || len(s.Pipeline.ConfigSources) == 0 {
		t.Fatal("sample pipeline is incomplete")
	}
	if intent := s.Pipeline.Steps[0].Intent; intent == nil || intent.Text == "" || !intent.Provided {
		t.Errorf("sample intent result is incomplete: %+v", intent)
	}

	commandSteps := map[string]bool{"refresh": false, "build": false, "test": false, "lint": false, "push": false}
	var completeTelemetry bool
	for _, step := range s.Pipeline.Steps {
		if _, ok := commandSteps[step.Name]; ok {
			commandSteps[step.Name] = len(step.Commands) > 0
			for _, command := range step.Commands {
				if command.Round < 1 || command.Sequence < 1 || command.Command == "" || command.Outcome == "" {
					t.Errorf("sample command evidence is incomplete: %+v", command)
				}
			}
		}
		if (step.Status == "completed" || step.Status == "skipped") && step.Intent == nil && len(step.Commands) == 0 && len(step.Evidence) == 0 && step.Explanation == "" {
			t.Errorf("sample successful/skipped step %q has no evidence or explanation", step.Name)
		}
		for _, run := range step.Agents {
			if (run.Agent == "claude" || run.Agent == "codex") && run.InputTokens != nil && run.UncachedInputTokens != nil && run.CacheReadTokens != nil && run.CacheWriteTokens != nil {
				want := *run.UncachedInputTokens + *run.CacheReadTokens + *run.CacheWriteTokens
				if *run.InputTokens != want {
					t.Errorf("sample %s input total = %d, want canonical meter sum %d", run.Agent, *run.InputTokens, want)
				}
			}
			if run.Agent == "cursor" && run.InputTokens != nil && ((run.CacheReadTokens != nil && *run.CacheReadTokens > 0) || (run.CacheWriteTokens != nil && *run.CacheWriteTokens > 0)) {
				t.Errorf("sample Cursor input total is populated despite ambiguous cache-inclusive semantics: %+v", run)
			}
			if run.StartedAt > 0 && run.DurationMS > 0 && run.InputTokens != nil && run.OutputTokens != nil && run.UncachedInputTokens != nil && run.CacheReadTokens != nil && run.CacheWriteTokens != nil && run.ReportedCostUSD != nil {
				completeTelemetry = true
			}
		}
	}
	for step, populated := range commandSteps {
		if !populated {
			t.Errorf("sample %s step has no command evidence", step)
		}
	}
	if !completeTelemetry {
		t.Error("sample has no fully populated telemetry row")
	}
}

// TestSamplePipelineCoversTheWholePipeline keeps the sample honest as the
// pipeline grows: a formatter tuned against a nine-step sample silently drops
// the tenth step.
func TestSamplePipelineCoversTheWholePipeline(t *testing.T) {
	t.Parallel()
	steps := Sample().Sections.Pipeline.Steps

	want := []string{"intent", "refresh", "review", "build", "test", "document", "lint", "push", "pr", "ci"}
	if len(steps) != len(want) {
		t.Fatalf("sample has %d steps, want %d", len(steps), len(want))
	}
	for i, name := range want {
		if steps[i].Name != name {
			t.Errorf("step %d = %q, want %q", i, steps[i].Name, name)
		}
		if steps[i].Label == "" {
			t.Errorf("step %q has no display label", name)
		}
	}
}

// TestSampleCoversNonHappyPathStates makes the sample useful for spotting a
// formatter that only styles success.
func TestSampleCoversNonHappyPathStates(t *testing.T) {
	t.Parallel()
	steps := Sample().Sections.Pipeline.Steps

	var sawFailed, sawRunning, sawMultiRound, sawFindings, sawTwoVendors bool
	vendors := map[string]bool{}
	for _, step := range steps {
		switch step.Status {
		case "failed":
			sawFailed = true
		case "running":
			sawRunning = true
		}
		if step.Rounds > 1 {
			sawMultiRound = true
		}
		if step.Findings.Total > 0 {
			sawFindings = true
		}
		for _, a := range step.Agents {
			if a.Vendor != "" {
				vendors[a.Vendor] = true
			}
		}
	}
	sawTwoVendors = len(vendors) > 1

	for name, ok := range map[string]bool{
		"a failed step":              sawFailed,
		"an in-flight step":          sawRunning,
		"a multi-round step":         sawMultiRound,
		"a step with findings":       sawFindings,
		"more than one model vendor": sawTwoVendors,
	} {
		if !ok {
			t.Errorf("sample does not include %s", name)
		}
	}
}

func TestSampleRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Sample())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Contract
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Version != Version {
		t.Fatalf("version = %d, want %d", back.Version, Version)
	}
	if back.Sections.Risk != Sample().Sections.Risk {
		t.Fatalf("risk did not round-trip: %+v", back.Sections.Risk)
	}
}

// A formatter under a multi-version rollout is told to accept older shapes, so the
// v2 sample has to stay reachable and has to be a real v2 contract: intent in
// its own section, and none of the v3-only additions present.
func TestSampleV2IsAVersion2Contract(t *testing.T) {
	t.Parallel()
	sample := SampleV2()

	if sample.Version != 2 {
		t.Fatalf("SampleV2 version = %d, want 2", sample.Version)
	}
	if sample.Metadata != "" {
		t.Error("v2 sample carries v3 metadata")
	}
	if sample.Sections.Summary != nil {
		t.Error("v2 sample carries a v3 summary section")
	}
	if sample.Sections.Intent == nil || sample.Sections.Intent.Text == "" || !sample.Sections.Intent.Authoritative {
		t.Fatalf("v2 sample has no authoritative intent section: %+v", sample.Sections.Intent)
	}
	if sample.Sections.WhatChanged == nil || sample.Sections.Testing == nil || !sample.Sections.Risk.Reported {
		t.Fatal("v2 sample lost a section every version 2 formatter reads")
	}
	if sample.Sections.Pipeline == nil || len(sample.Sections.Pipeline.Steps) == 0 {
		t.Fatal("v2 sample has no pipeline steps")
	}
	for _, step := range sample.Sections.Pipeline.Steps {
		if step.Intent != nil || len(step.Commands) > 0 || len(step.Evidence) > 0 || step.Explanation != "" {
			t.Errorf("v2 sample step %q carries v3-only evidence: %+v", step.Name, step)
		}
		for _, run := range step.Agents {
			if run.Provider != "" || run.StartedAt != 0 || run.DurationMS != 0 ||
				run.InputTokens != nil || run.OutputTokens != nil || run.UncachedInputTokens != nil ||
				run.CacheReadTokens != nil || run.CacheWriteTokens != nil || run.ReportedCostUSD != nil {
				t.Errorf("v2 sample agent row carries v3-only telemetry: %+v", run)
			}
		}
	}

	// Sample must stay unaffected by the downgrade.
	if Sample().Sections.Summary == nil {
		t.Error("SampleV2 mutated the shared current sample")
	}
}

func TestSupportedSampleVersionsExcludeProducerPricedV4(t *testing.T) {
	t.Parallel()
	for _, version := range []int{2, 3, Version} {
		if SampleForVersion(version) == nil || !IsSupportedVersion(version) {
			t.Errorf("supported version %d is unavailable", version)
		}
	}
	for _, version := range []int{1, 4} {
		if SampleForVersion(version) != nil || IsSupportedVersion(version) {
			t.Errorf("removed version %d remains supported", version)
		}
	}
}

func TestOlderSamplesOmitCurrentSectionsFromJSON(t *testing.T) {
	t.Parallel()

	for _, sample := range []*Contract{SampleV2(), SampleV3()} {
		raw, err := json.Marshal(sample)
		if err != nil {
			t.Fatalf("marshal v%d sample: %v", sample.Version, err)
		}
		for _, key := range []string{`"static_tests"`, `"review_evidence"`, `"user_testing"`} {
			if bytes.Contains(raw, []byte(key)) {
				t.Errorf("v%d sample carries current-only key %s: %s", sample.Version, key, raw)
			}
		}
	}
}
