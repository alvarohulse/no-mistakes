package prbody

import (
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
	s := Sample().Sections

	if s.Intent == nil || s.Intent.Text == "" {
		t.Error("sample has no intent")
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
	if s.Testing == nil || s.Testing.Summary == "" || len(s.Testing.Tested) == 0 || len(s.Testing.Artifacts) == 0 {
		t.Error("sample testing is incomplete")
	}
	if s.Pipeline == nil || len(s.Pipeline.Steps) == 0 || len(s.Pipeline.ConfigSources) == 0 {
		t.Fatal("sample pipeline is incomplete")
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

	var sawFailed, sawRunning, sawMultiRound, sawFindings, sawNested, sawTwoVendors bool
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
			if len(a.Nested) > 0 {
				sawNested = true
			}
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
		"a nested agent":             sawNested,
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
