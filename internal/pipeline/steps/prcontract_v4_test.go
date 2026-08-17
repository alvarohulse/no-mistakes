package steps

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestContractV4ProjectsThreeIndependentCostClassesAndProvenance(t *testing.T) {
	policy := `{"version":6,"pricing":{"profiles":{"cursor":"cursor-token-rate"}}}`
	input, output, cacheRead, cacheWrite := 1_000_000, 1_000_000, 1_000_000, 1_000_000
	reported := 9.25
	provider := "anthropic"
	contract := BuildContract(ContractInput{
		Run:   &db.Run{ID: "run-1", ResolvedPolicy: &policy},
		Steps: []*db.StepResult{{ID: "review", StepName: types.StepReview, StepOrder: 3, Status: types.StepStatusCompleted}},
		Invocations: []db.AgentInvocation{{
			ID: "inv-1", StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "cursor",
			Model: "claude-opus-5", ModelProvider: &provider,
			StartedAt:        time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC).Unix(),
			DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cacheRead,
			DeltaCacheCreationTokens: &cacheWrite, ReportedCostUSD: &reported,
		}},
	})

	if contract.Version != 4 {
		t.Fatalf("version = %d, want 4", contract.Version)
	}
	run := contract.Sections.Pipeline.Steps[0].Agents[0]
	if run.Costs == nil {
		t.Fatal("cost classes are absent")
	}
	if run.Costs.HarnessReported.ValueUSD == nil || *run.Costs.HarnessReported.ValueUSD != 9.25 {
		t.Fatalf("harness-reported cost = %+v", run.Costs.HarnessReported)
	}
	if run.Costs.APIListEstimate.ValueUSD == nil || *run.Costs.APIListEstimate.ValueUSD != 36.75 {
		t.Fatalf("API-list cost = %+v", run.Costs.APIListEstimate)
	}
	if run.Costs.HarnessAdjustedEstimate.ValueUSD == nil || *run.Costs.HarnessAdjustedEstimate.ValueUSD != 37.75 {
		t.Fatalf("harness-adjusted cost = %+v", run.Costs.HarnessAdjustedEstimate)
	}
	if run.Costs.HarnessAdjustedEstimate.Provenance.ProfileID != "cursor-token-rate" {
		t.Fatalf("cost provenance = %+v", run.Costs.HarnessAdjustedEstimate.Provenance)
	}
}
