package steps

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestContractV5ProjectsRawMetersAndCLIReportedCost(t *testing.T) {
	input, output, cacheRead, cacheWrite := 1_000_000, 1_000_000, 1_000_000, 1_000_000
	reported := 9.25
	provider := "anthropic"
	contract := BuildContract(ContractInput{
		Run:   &db.Run{ID: "run-1"},
		Steps: []*db.StepResult{{ID: "review", StepName: types.StepReview, StepOrder: 3, Status: types.StepStatusCompleted}},
		Invocations: []db.AgentInvocation{{
			ID: "inv-1", StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "cursor",
			Model: "claude-opus-5", ModelProvider: &provider,
			StartedAt:        time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC).Unix(),
			DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cacheRead,
			DeltaCacheCreationTokens: &cacheWrite, ReportedCostUSD: &reported,
		}},
	})

	if contract.Version != 5 {
		t.Fatalf("version = %d, want 5", contract.Version)
	}
	run := contract.Sections.Pipeline.Steps[0].Agents[0]
	if run.ReportedCostUSD == nil || *run.ReportedCostUSD != reported {
		t.Fatalf("reported cost = %v, want %v", run.ReportedCostUSD, reported)
	}
	if run.Provider != provider || run.UncachedInputTokens == nil || *run.UncachedInputTokens != input {
		t.Fatalf("raw invocation facts = %+v", run)
	}
}

func TestContractV5LeavesUnreportedCostAbsent(t *testing.T) {
	input, output := 1_000_000, 1_000_000
	provider := "anthropic"
	contract := BuildContract(ContractInput{
		Steps: []*db.StepResult{{ID: "review", StepName: types.StepReview, StepOrder: 3, Status: types.StepStatusCompleted}},
		Invocations: []db.AgentInvocation{{
			ID: "legacy", StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "cursor",
			Model: "claude-opus-5", ModelProvider: &provider, StartedAt: time.Now().Unix(), DeltaInputTokens: &input, DeltaOutputTokens: &output,
		}},
	})
	if got := contract.Sections.Pipeline.Steps[0].Agents[0].ReportedCostUSD; got != nil {
		t.Fatalf("unreported cost became %v", got)
	}
}

func TestContractV5SeparatesStaticTestsReviewEvidenceAndUserTesting(t *testing.T) {
	t.Parallel()

	testFindings := `{"findings":[],"summary":"","testing_summary":"go tests passed","tested":["go test ./..."]}`
	reviewFindings := `{"findings":[{"severity":"P2","description":"fixed","action":"auto-fix"}],"summary":"reviewed","risk_level":"low","risk_rationale":"bounded"}`
	exitCode := 0
	testEvidence, err := db.EncodeStepEvidence(db.StepEvidence{Commands: []db.CommandEvidence{{
		Round: 1, Sequence: 1, Command: "go test ./...", Outcome: db.CommandOutcomePassed, ExitCode: &exitCode,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	reviewEvidence, err := db.EncodeStepEvidence(db.StepEvidence{Evidence: []string{"Reviewed the complete branch diff."}})
	if err != nil {
		t.Fatal(err)
	}
	contract := BuildContract(ContractInput{
		Steps: []*db.StepResult{
			{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &reviewFindings, EvidenceJSON: &reviewEvidence},
			{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &testFindings, EvidenceJSON: &testEvidence},
		},
		Rounds: map[string][]*db.StepRound{
			"review": {{ID: "review-round"}},
			"test":   {{ID: "test-round"}},
		},
		UserTestingInstructions: []string{"Open Settings and confirm the saved value."},
	})

	if contract.Version != 5 {
		t.Fatalf("version = %d, want 5", contract.Version)
	}
	if contract.Sections.Testing != nil {
		t.Fatalf("legacy testing field is populated: %+v", contract.Sections.Testing)
	}
	if contract.Sections.StaticTests == nil || len(contract.Sections.StaticTests.Commands) != 1 {
		t.Fatalf("static_tests = %+v", contract.Sections.StaticTests)
	}
	if contract.Sections.ReviewEvidence == nil || contract.Sections.ReviewEvidence.Findings.Total != 1 || len(contract.Sections.ReviewEvidence.Evidence) != 1 {
		t.Fatalf("review_evidence = %+v", contract.Sections.ReviewEvidence)
	}
	if contract.Sections.UserTesting == nil || len(contract.Sections.UserTesting.Instructions) != 1 || contract.Sections.UserTesting.Attested {
		t.Fatalf("user_testing = %+v", contract.Sections.UserTesting)
	}
}

func TestContractV5KeepsApprovedFailedTestsOutOfStaticEvidence(t *testing.T) {
	t.Parallel()

	testFindings := `{"findings":[],"summary":"","testing_summary":"tests completed","tested":["go test ./...","make e2e"]}`
	firstExitCode, secondExitCode := 1, 2
	testEvidence, err := db.EncodeStepEvidence(db.StepEvidence{Commands: []db.CommandEvidence{
		{Round: 1, Sequence: 1, Command: "go test ./...", Outcome: db.CommandOutcomeFailed, ExitCode: &firstExitCode},
		{Round: 2, Sequence: 1, Command: "make e2e", Outcome: db.CommandOutcomeFailed, ExitCode: &secondExitCode},
	}})
	if err != nil {
		t.Fatal(err)
	}
	contract := BuildContract(ContractInput{Steps: []*db.StepResult{{
		ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted,
		FindingsJSON: &testFindings, EvidenceJSON: &testEvidence,
	}}})

	staticTests := contract.Sections.StaticTests
	if staticTests == nil {
		t.Fatal("static_tests omitted; want an explicit empty section")
	}
	if staticTests.Summary != "" || len(staticTests.Commands) != 0 || len(staticTests.Reported) != 0 || len(staticTests.Artifacts) != 0 {
		t.Fatalf("static_tests = %+v, want no passing evidence", staticTests)
	}
	pipelineCommands := contract.Sections.Pipeline.Steps[0].Commands
	if len(pipelineCommands) != 2 {
		t.Fatalf("pipeline commands = %+v, want both failed attempts", pipelineCommands)
	}
	first, second := pipelineCommands[0], pipelineCommands[1]
	if first.Round != 1 || first.Sequence != 1 || first.Command != "go test ./..." || first.Outcome != db.CommandOutcomeFailed || first.ExitCode == nil || *first.ExitCode != firstExitCode {
		t.Fatalf("first pipeline command = %+v, want the exact first failed attempt", first)
	}
	if second.Round != 2 || second.Sequence != 1 || second.Command != "make e2e" || second.Outcome != db.CommandOutcomeFailed || second.ExitCode == nil || *second.ExitCode != secondExitCode {
		t.Fatalf("second pipeline command = %+v, want the exact second failed attempt", second)
	}
}

func TestContractV5UserTestingCompletionRequiresExplicitAttestation(t *testing.T) {
	t.Parallel()

	contract := BuildContract(ContractInput{
		UserTestingInstructions: []string{"Exercise the checkout flow."},
		UserTestingAttested:     true,
	})
	if contract.Sections.UserTesting == nil || !contract.Sections.UserTesting.Attested {
		t.Fatal("explicit user-testing attestation was not retained")
	}

	withoutAttestation := BuildContract(ContractInput{UserTestingInstructions: []string{"Exercise the checkout flow."}})
	if withoutAttestation.Sections.UserTesting == nil || withoutAttestation.Sections.UserTesting.Attested {
		t.Fatal("instructions alone were presented as completed user testing")
	}
}
