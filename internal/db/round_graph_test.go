package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStructuredRoundPersistsOrderedEvaluationAndSubjectWithoutRoundJSON(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/structured-round", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	starting, resulting, evaluated, trusted := "base", "result", "evaluated", "trusted-config"
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind:    RoundEvaluationInitialReview,
		Summary: "two findings",
		Findings: []StepRoundFinding{
			{Ordinal: 0, ExternalID: "review-1", Severity: "error", File: "a.go", Line: 4, Description: "first", Action: types.ActionAutoFix},
			{Ordinal: 1, ExternalID: "review-2", Severity: "warning", File: "b.go", Line: 8, Description: "second", Action: types.ActionAskUser},
		},
		Artifacts: []StepRoundEvaluationArtifact{
			{Ordinal: 0, Kind: "html", Label: "local evidence", Path: "evidence/result.html"},
			{Ordinal: 1, Kind: "link", Label: "hosted evidence", URL: "https://example.com/result"},
			{Ordinal: 2, Kind: "log", Label: "inline transcript", Content: "result passed"},
		},
	}, StructuredRoundSubject{
		StartingHeadSHA: &starting, ResultingHeadSHA: &resulting, EvaluatedHeadSHA: &evaluated, TrustedConfigSHA: &trusted,
	}, nil, 12); err != nil {
		t.Fatal(err)
	}

	var rawFindings, rawSelected, rawFixSummary any
	if err := database.sql.QueryRow(`SELECT findings_json, selected_finding_ids, fix_summary FROM step_rounds WHERE id = ?`, round.ID).Scan(&rawFindings, &rawSelected, &rawFixSummary); err != nil {
		t.Fatal(err)
	}
	if rawFindings != nil || rawSelected != nil || rawFixSummary != nil {
		t.Fatalf("normalized round retained compatibility JSON: findings=%v selected=%v fix=%v", rawFindings, rawSelected, rawFixSummary)
	}

	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Evaluation == nil {
		t.Fatalf("round projection = %#v", rounds)
	}
	got := rounds[0]
	if got.Evaluation.Kind != RoundEvaluationInitialReview || got.StartingHeadSHA == nil || *got.StartingHeadSHA != starting || got.ResultingHeadSHA == nil || *got.ResultingHeadSHA != resulting || got.EvaluatedHeadSHA == nil || *got.EvaluatedHeadSHA != evaluated || got.TrustedConfigSHA == nil || *got.TrustedConfigSHA != trusted {
		t.Fatalf("round subject/evaluation = %#v", got)
	}
	if len(got.Evaluation.Findings) != 2 || got.Evaluation.Findings[0].ExternalID != "review-1" || got.Evaluation.Findings[1].ExternalID != "review-2" {
		t.Fatalf("finding order = %#v", got.Evaluation.Findings)
	}
	if len(got.Evaluation.Artifacts) != 3 || got.Evaluation.Artifacts[0].ID == "" || got.Evaluation.Artifacts[0].EvaluationID != got.Evaluation.ID || got.Evaluation.Artifacts[0].Path != "evidence/result.html" || got.Evaluation.Artifacts[1].URL != "https://example.com/result" || got.Evaluation.Artifacts[2].Content != "result passed" {
		t.Fatalf("evaluation artifacts = %#v", got.Evaluation.Artifacts)
	}
	if got.FindingsJSON == nil {
		t.Fatal("expected in-memory compatibility projection")
	}
	var projected types.Findings
	if err := json.Unmarshal([]byte(*got.FindingsJSON), &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Items) != 2 || projected.Items[0].ID != "review-1" || projected.Items[1].ID != "review-2" {
		t.Fatalf("projected findings = %#v", projected.Items)
	}
	if len(projected.Artifacts) != 3 || projected.Artifacts[0].Path != "evidence/result.html" || projected.Artifacts[1].URL != "https://example.com/result" || projected.Artifacts[2].Content != "result passed" {
		t.Fatalf("projected artifacts = %#v", projected.Artifacts)
	}
}

func TestStructuredRoundCompletionRollsBackCompatibilityProjection(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/structured-round-projection", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER reject_compatibility_projection
		BEFORE UPDATE OF findings_json ON step_results
		BEGIN
			SELECT RAISE(FAIL, 'injected compatibility projection failure');
		END`); err != nil {
		t.Fatal(err)
	}
	err = database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind:     RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{{ExternalID: "review-1", Description: "needs review", Action: types.ActionAskUser}},
	}, StructuredRoundSubject{}, nil, 1)
	if err == nil {
		t.Fatal("structured round completion succeeded after compatibility projection failure")
	}
	gotStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.FindingsJSON != nil {
		t.Fatalf("step findings survived rolled-back completion: %q", *gotStep.FindingsJSON)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != RoundStatusActive || rounds[0].Evaluation != nil {
		t.Fatalf("round after projection failure = %#v, want active without evaluation", rounds)
	}
}

func TestStructuredRoundCompletionRollsBackOnIgnoredRoundUpdate(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("/tmp/structured-round-ignored-update", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER ignore_structured_round_completion
		BEFORE UPDATE OF status ON step_rounds
		WHEN NEW.id = '` + round.ID + `'
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	err = database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind:     RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{{ExternalID: "review-1", Description: "needs review", Action: types.ActionAskUser}},
	}, StructuredRoundSubject{}, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "expected one round, updated 0") {
		t.Fatalf("complete structured round error = %v", err)
	}

	gotStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.FindingsJSON != nil {
		t.Fatalf("step findings survived rolled-back completion: %q", *gotStep.FindingsJSON)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != RoundStatusActive || rounds[0].Evaluation != nil {
		t.Fatalf("round after ignored update = %#v, want active without evaluation", rounds)
	}
}

func TestStructuredRoundDecisionPreservesSelectionNonSelectionAndUserAddition(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/structured-decision", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind:           RoundEvaluationInitialReview,
		Summary:        "evaluation summary",
		Tested:         []string{"go test ./internal/db"},
		TestingSummary: "database tests passed",
		RiskLevel:      "low",
		RiskRationale:  "isolated persistence change",
		RiskScope:      "internal/db",
		Artifacts: []StepRoundEvaluationArtifact{
			{Kind: "log", Label: "test output", Content: "PASS"},
		},
		Findings: []StepRoundFinding{
			{ExternalID: "review-1", Description: "first", Action: types.ActionAutoFix},
			{ExternalID: "review-2", Description: "second", Action: types.ActionAskUser},
		},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	merged := `{"findings":[{"id":"review-1","description":"first","action":"auto-fix","user_instructions":"touch parser only"},{"id":"user-2","severity":"info","description":"second audit","action":"auto-fix","source":"user"},{"id":"user-1","severity":"info","description":"new audit","action":"auto-fix","source":"user"}]}`
	selected := `["review-1","user-1","user-2"]`
	if err := database.SetStepRoundUserDecision(round.ID, &selected, RoundSelectionSourceUser, &merged); err != nil {
		t.Fatal(err)
	}
	decision, err := database.GetRoundDecision(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || decision.Source != RoundSelectionSourceUser || len(decision.Findings) != 4 {
		t.Fatalf("decision = %#v", decision)
	}
	selectedCount, unselectedCount, editedCount := 0, 0, 0
	for _, reference := range decision.Findings {
		switch reference.State {
		case RoundDecisionFindingSelected:
			selectedCount++
		case RoundDecisionFindingUnselected:
			unselectedCount++
		}
		if reference.Edited {
			editedCount++
		}
	}
	if selectedCount != 3 || unselectedCount != 1 || editedCount != 1 {
		t.Fatalf("decision references = %#v", decision.Findings)
	}
	evaluation, err := database.GetRoundEvaluation(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation == nil || len(evaluation.Findings) != 4 || evaluation.Findings[2].ExternalID != "user-2" || evaluation.Findings[3].ExternalID != "user-1" || evaluation.Findings[0].UserInstructions != "touch parser only" {
		t.Fatalf("evaluation after user decision = %#v", evaluation)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].UserFindingsJSON == nil {
		t.Fatalf("round user findings projection = %#v", rounds)
	}
	projected, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Items) != 3 || projected.Items[0].ID != "review-1" || projected.Items[1].ID != "user-1" || projected.Items[2].ID != "user-2" || projected.Items[0].UserInstructions != "touch parser only" {
		t.Fatalf("user findings projection = %#v", projected.Items)
	}
	if projected.Summary != "evaluation summary" || len(projected.Tested) != 1 || projected.Tested[0] != "go test ./internal/db" || projected.TestingSummary != "database tests passed" || projected.RiskLevel != "low" || projected.RiskRationale != "isolated persistence change" || projected.RiskScope != "internal/db" {
		t.Fatalf("user findings metadata = %#v", projected)
	}
	if len(projected.Artifacts) != 1 || projected.Artifacts[0].Kind != "log" || projected.Artifacts[0].Label != "test output" || projected.Artifacts[0].Content != "PASS" {
		t.Fatalf("user findings artifacts = %#v", projected.Artifacts)
	}

	declinedRound, _ := database.BeginStepRound(step.ID, 2, RoundTriggerAutoFix)
	if err := database.CompleteStepRoundStructured(declinedRound.ID, StepRoundEvaluation{Kind: RoundEvaluationRereview}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundDeclined(declinedRound.ID); err != nil {
		t.Fatal(err)
	}
	declined, err := database.GetRoundDecision(declinedRound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if declined == nil || !declined.ExplicitEmpty || declined.Source != RoundSelectionSourceUserDeclined {
		t.Fatalf("declined decision = %#v", declined)
	}
}

func TestStructuredRoundRekeysCollidingUserFinding(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/structured-decision-collision", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{
			{ExternalID: "review-1", Description: "selected agent finding", Action: types.ActionAutoFix},
			{ExternalID: "review-2", Description: "unselected agent finding", Action: types.ActionAskUser},
		},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}

	merged := `{"findings":[{"id":"review-1","description":"selected agent finding","action":"auto-fix"},{"id":"review-2","source":"user","description":"user-authored finding","action":"auto-fix"}]}`
	selected := `["review-1","review-2"]`
	if err := database.SetStepRoundUserDecision(round.ID, &selected, RoundSelectionSourceUser, &merged); err != nil {
		t.Fatal(err)
	}

	evaluation, err := database.GetRoundEvaluation(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := database.GetRoundDecision(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation == nil || decision == nil || len(evaluation.Findings) != 3 {
		t.Fatalf("evaluation/decision = %#v / %#v", evaluation, decision)
	}
	states := make(map[string]string, len(decision.Findings))
	for _, reference := range decision.Findings {
		for _, finding := range evaluation.Findings {
			if finding.ID == reference.FindingID {
				states[finding.ExternalID] = reference.State
			}
		}
	}
	if evaluation.Findings[1].Description != "unselected agent finding" || states["review-2"] != RoundDecisionFindingUnselected {
		t.Fatalf("unselected agent finding = %#v, states = %#v", evaluation.Findings[1], states)
	}
	if evaluation.Findings[2].ExternalID != "user-1" || evaluation.Findings[2].Description != "user-authored finding" || states["user-1"] != RoundDecisionFindingSelected {
		t.Fatalf("rekeyed user finding = %#v, states = %#v", evaluation.Findings[2], states)
	}
}

func TestStructuredRoundDeclinedDoesNotOverwriteRecordedSelection(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/structured-decision-decline", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind:     RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{{ExternalID: "review-1", Description: "selected finding", Action: types.ActionAutoFix}},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	if err := database.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundDeclined(round.ID); err != nil {
		t.Fatal(err)
	}

	decision, err := database.GetRoundDecision(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || decision.Source != RoundSelectionSourceAutoFix || decision.ExplicitEmpty || len(decision.Findings) != 1 || decision.Findings[0].State != RoundDecisionFindingSelected {
		t.Fatalf("recorded selection was overwritten: %#v", decision)
	}
}

func TestStructuredRoundRejectsDuplicateFindingIdentities(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/duplicate-finding", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{
			{ExternalID: "same", Description: "first"},
			{ExternalID: "same", Description: "second"},
		},
	}, StructuredRoundSubject{}, nil, 1)
	if err == nil {
		t.Fatal("expected duplicate finding identity to be rejected")
	}
	if evaluation, evalErr := database.GetRoundEvaluation(round.ID); evalErr != nil {
		t.Fatal(evalErr)
	} else if evaluation != nil {
		t.Fatalf("duplicate evaluation partially persisted = %#v", evaluation)
	}
}

func TestLegacyUserFixNormalizesOnReadWithoutFabricatingEvaluation(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/legacy-trigger", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	findings := `{"findings":[{"id":"review-1","description":"legacy"}]}`
	if _, err := database.InsertStepRound(step.ID, 1, RoundTriggerUserFix, &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	var storedTrigger string
	if err := database.sql.QueryRow(`SELECT trigger_type FROM step_rounds WHERE step_result_id = ?`, step.ID).Scan(&storedTrigger); err != nil {
		t.Fatal(err)
	}
	if storedTrigger != RoundTriggerAutoFix {
		t.Fatalf("new round stored trigger = %q, want %q", storedTrigger, RoundTriggerAutoFix)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Trigger != RoundTriggerAutoFix || rounds[0].TriggerProvenance == nil || *rounds[0].TriggerProvenance != RoundTriggerProvenanceLegacyUserFix {
		t.Fatalf("legacy trigger projection = %#v", rounds)
	}
	if rounds[0].Evaluation != nil || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != findings {
		t.Fatalf("legacy projection = %#v", rounds[0])
	}
}

func TestStructuredRoundHydratesLinkedInvocationAttemptAndArtifactIDs(t *testing.T) {
	database := openTestDB(t)
	run, step, round, attempt := newAcceptedCommandProofFixture(t, database, true)
	invocation, err := database.InsertAgentInvocation(AgentInvocation{
		RunID: run.ID, StepName: string(types.StepTest), Round: 1, RoundID: round.ID,
		Purpose: "test", Agent: "codex", SessionMode: InvocationModeCold,
		StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{Kind: RoundEvaluationValidation}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("rounds = %#v", rounds)
	}
	got := rounds[0]
	if len(got.InvocationIDs) != 1 || got.InvocationIDs[0] != invocation.ID || len(got.CommandAttemptIDs) != 1 || got.CommandAttemptIDs[0] != attempt.ID || len(got.ArtifactIDs) != 1 {
		t.Fatalf("round references = %#v", got)
	}
}

func TestStructuredRoundRepairsOnlyAutoFixRounds(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/structured-repair", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepDocument)
	summary := "document the change"
	resulting := "documented-head"

	initial, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepRoundStructured(initial.ID, StepRoundEvaluation{
		Kind: RoundEvaluationDocumentation,
	}, StructuredRoundSubject{ResultingHeadSHA: &resulting}, &summary, 1); err != nil {
		t.Fatal(err)
	}
	if repair, err := database.GetRoundRepair(initial.ID); err != nil {
		t.Fatal(err)
	} else if repair != nil {
		t.Fatalf("initial documentation round fabricated repair = %#v", repair)
	}

	fixRound, err := database.BeginStepRound(step.ID, 2, RoundTriggerAutoFix)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepRoundStructured(fixRound.ID, StepRoundEvaluation{
		Kind: RoundEvaluationDocumentation,
	}, StructuredRoundSubject{}, &summary, 1); err != nil {
		t.Fatal(err)
	}
	repair, err := database.GetRoundRepair(fixRound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair == nil || repair.FixSummary == nil || *repair.FixSummary != summary {
		t.Fatalf("fix round repair = %#v", repair)
	}
}

func TestStructuredRoundInitialRepairAuditPersists(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/initial-repair-audit", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, result := "sha256:initial-repair", RoundRepairResolved
	if err := database.CompleteStepRoundStructuredWithRepairAudit(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationValidation,
	}, StructuredRoundSubject{}, nil, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &result,
	}); err != nil {
		t.Fatal(err)
	}
	repair, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair == nil || repair.FailureFingerprint == nil || *repair.FailureFingerprint != fingerprint || repair.Result == nil || *repair.Result != result {
		t.Fatalf("initial repair audit = %#v", repair)
	}
}

func TestReserveCIFixAttemptAndRecordRoundRepairRollsBackOnReceiptFailure(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-reservation-rollback", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER reject_ci_repair_receipt
		BEFORE INSERT ON round_repairs
		WHEN NEW.round_id = '` + round.ID + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected CI repair receipt failure');
		END`); err != nil {
		t.Fatal(err)
	}

	fingerprint, result := "sha256:ci-repair", RoundRepairAttempted
	err = database.ReserveCIFixAttemptAndRecordRoundRepair(run.ID, round.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &result,
	})
	if err == nil || !strings.Contains(err.Error(), "injected CI repair receipt failure") {
		t.Fatalf("reserve CI repair attempt error = %v", err)
	}

	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.CIFixAttempts == nil || *gotRun.CIFixAttempts != 0 {
		t.Fatalf("CI fix attempts after failed reservation = %#v, want durable zero", gotRun.CIFixAttempts)
	}
	repair, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair != nil {
		t.Fatalf("repair after failed reservation = %#v, want none", repair)
	}
}

func TestReserveCIFixAttemptAndRecordRoundRepairRollsBackOnIgnoredBudgetUpdate(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-reservation-ignored-budget", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER ignore_ci_repair_reservation_budget
		BEFORE UPDATE OF ci_fix_attempts ON runs
		WHEN NEW.id = '` + run.ID + `'
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	fingerprint, result := "sha256:ci-repair", RoundRepairAttempted
	err = database.ReserveCIFixAttemptAndRecordRoundRepair(run.ID, round.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &result,
	})
	if err == nil || !strings.Contains(err.Error(), "expected one run, updated 0") {
		t.Fatalf("reserve CI repair attempt error = %v", err)
	}

	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.CIFixAttempts == nil || *gotRun.CIFixAttempts != 0 {
		t.Fatalf("CI fix attempts after ignored reservation = %#v, want durable zero", gotRun.CIFixAttempts)
	}
	repair, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair != nil {
		t.Fatalf("repair after ignored reservation = %#v, want none", repair)
	}
}

func TestBeginCIFixRepairRoundRollsBackOnIgnoredBudgetUpdate(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-begin-ignored-budget", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	if _, err := database.sql.Exec(`CREATE TRIGGER ignore_ci_repair_begin_budget
		BEFORE UPDATE OF ci_fix_attempts ON runs
		WHEN NEW.id = '` + run.ID + `'
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	fingerprint, result := "sha256:ci-repair", RoundRepairAttempted
	round, err := database.BeginCIFixRepairRound(step.ID, run.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &result,
	})
	if err == nil || !strings.Contains(err.Error(), "expected one run, updated 0") {
		t.Fatalf("begin CI repair round error = %v", err)
	}
	if round != nil {
		t.Fatalf("CI repair round = %#v, want none", round)
	}

	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.CIFixAttempts == nil || *gotRun.CIFixAttempts != 0 {
		t.Fatalf("CI fix attempts after ignored begin = %#v, want durable zero", gotRun.CIFixAttempts)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 0 {
		t.Fatalf("CI repair rounds after ignored begin = %#v, want none", rounds)
	}
}

func TestBeginCIFixRepairRoundRollsBackOnIgnoredRepairReceipt(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-begin-ignored-receipt", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	if _, err := database.sql.Exec(`CREATE TRIGGER ignore_ci_repair_begin_receipt
		BEFORE INSERT ON round_repairs
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	result := RoundRepairAttempted
	round, err := database.BeginCIFixRepairRound(step.ID, run.ID, 0, StepRoundRepair{Result: &result})
	if err == nil || !strings.Contains(err.Error(), "expected one receipt, updated 0") {
		t.Fatalf("begin CI repair round error = %v", err)
	}
	if round != nil {
		t.Fatalf("CI repair round = %#v, want none", round)
	}

	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 0 {
		t.Fatalf("CI repair rounds after ignored receipt = %#v, want none", rounds)
	}
	var repairCount int
	if err := database.sql.QueryRow(`SELECT COUNT(*) FROM round_repairs WHERE run_id = ?`, run.ID).Scan(&repairCount); err != nil {
		t.Fatal(err)
	}
	if repairCount != 0 {
		t.Fatalf("repair receipts after ignored insert = %d, want none", repairCount)
	}
}

func TestCompleteStructuredRoundUpdatesReservedCIFixRepair(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-completion", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}

	fingerprint, attempted := "sha256:ci-repair", RoundRepairAttempted
	if err := database.ReserveCIFixAttemptAndRecordRoundRepair(run.ID, round.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &attempted,
	}); err != nil {
		t.Fatal(err)
	}
	reserved, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved == nil {
		t.Fatal("expected attempted CI repair receipt")
	}

	resolved, resulting, summary := RoundRepairResolved, "fixed-head", "repair failing CI"
	if err := database.CompleteStepRoundStructuredWithRepairAudit(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationValidation,
	}, StructuredRoundSubject{ResultingHeadSHA: &resulting}, &summary, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &resolved,
	}); err != nil {
		t.Fatal(err)
	}

	completed, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed == nil || completed.ID != reserved.ID || completed.Result == nil || *completed.Result != RoundRepairResolved || completed.ResultingHeadSHA == nil || *completed.ResultingHeadSHA != resulting || completed.FixSummary == nil || *completed.FixSummary != summary {
		t.Fatalf("completed CI repair = %#v, want reserved receipt updated with final result and head", completed)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != RoundStatusCompleted || rounds[0].Evaluation == nil || rounds[0].ResultingHeadSHA == nil || *rounds[0].ResultingHeadSHA != resulting {
		t.Fatalf("completed CI round = %#v, want completed evaluation and subject", rounds)
	}
}

func TestCompleteStructuredRoundRetainsReservedCIFixRepairOnFinalWriteFailure(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-completion-rollback", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}

	fingerprint, attempted := "sha256:ci-repair", RoundRepairAttempted
	if err := database.ReserveCIFixAttemptAndRecordRoundRepair(run.ID, round.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &attempted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER reject_ci_repair_completion
		BEFORE UPDATE OF failure_fingerprint, result ON round_repairs
		WHEN NEW.round_id = '` + round.ID + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected CI repair completion failure');
		END`); err != nil {
		t.Fatal(err)
	}

	resolved, resulting := RoundRepairResolved, "fixed-head"
	err = database.CompleteStepRoundStructuredWithRepairAudit(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationValidation,
	}, StructuredRoundSubject{ResultingHeadSHA: &resulting}, nil, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &resolved,
	})
	if err == nil || !strings.Contains(err.Error(), "injected CI repair completion failure") {
		t.Fatalf("complete CI round error = %v", err)
	}

	repair, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair == nil || repair.Result == nil || *repair.Result != RoundRepairAttempted || repair.ResultingHeadSHA != nil {
		t.Fatalf("repair after failed final completion = %#v, want original attempted receipt", repair)
	}
	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.CIFixAttempts == nil || *gotRun.CIFixAttempts != 1 {
		t.Fatalf("CI fix attempts after failed final completion = %#v, want spent attempt retained", gotRun.CIFixAttempts)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != RoundStatusActive || rounds[0].Evaluation != nil || rounds[0].ResultingHeadSHA != nil {
		t.Fatalf("round after failed final completion = %#v, want incomplete active round", rounds)
	}
}

func TestCompleteStructuredRoundRollsBackOnIgnoredRepairReceiptUpdate(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/ci-repair-completion-ignored-receipt", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepCI)
	round, err := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err != nil {
		t.Fatal(err)
	}

	fingerprint, attempted := "sha256:ci-repair", RoundRepairAttempted
	if err := database.ReserveCIFixAttemptAndRecordRoundRepair(run.ID, round.ID, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &attempted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER ignore_ci_repair_completion_receipt
		BEFORE UPDATE ON round_repairs
		WHEN NEW.round_id = '` + round.ID + `'
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	resolved, resulting := RoundRepairResolved, "fixed-head"
	err = database.CompleteStepRoundStructuredWithRepairAudit(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationValidation,
	}, StructuredRoundSubject{ResultingHeadSHA: &resulting}, nil, 1, StepRoundRepair{
		FailureFingerprint: &fingerprint,
		Result:             &resolved,
	})
	if err == nil || !strings.Contains(err.Error(), "expected one receipt, updated 0") {
		t.Fatalf("complete CI round error = %v", err)
	}

	repair, err := database.GetRoundRepair(round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repair == nil || repair.Result == nil || *repair.Result != RoundRepairAttempted || repair.ResultingHeadSHA != nil {
		t.Fatalf("repair after ignored final completion = %#v, want original attempted receipt", repair)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Status != RoundStatusActive || rounds[0].Evaluation != nil || rounds[0].ResultingHeadSHA != nil {
		t.Fatalf("round after ignored final completion = %#v, want incomplete active round", rounds)
	}
}

func TestStructuredRoundDecisionPreservesSubmittedSelectionOrder(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/structured-selection-order", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.BeginStepRound(step.ID, 1, RoundTriggerInitial)
	if err := database.CompleteStepRoundStructured(round.ID, StepRoundEvaluation{
		Kind: RoundEvaluationInitialReview,
		Findings: []StepRoundFinding{
			{ExternalID: "review-1", Description: "first", Action: types.ActionAutoFix},
			{ExternalID: "review-2", Description: "second", Action: types.ActionAutoFix},
			{ExternalID: "review-3", Description: "third", Action: types.ActionAskUser},
		},
	}, StructuredRoundSubject{}, nil, 1); err != nil {
		t.Fatal(err)
	}

	selected := `["review-2","review-1"]`
	if err := database.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Evaluation == nil || rounds[0].Decision == nil || rounds[0].SelectedFindingIDs == nil {
		t.Fatalf("round projection = %#v", rounds)
	}
	var projected []string
	if err := json.Unmarshal([]byte(*rounds[0].SelectedFindingIDs), &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected) != 2 || projected[0] != "review-2" || projected[1] != "review-1" {
		t.Fatalf("selected finding order = %#v", projected)
	}
	if rounds[0].Evaluation.Findings[0].ExternalID != "review-1" || rounds[0].Evaluation.Findings[1].ExternalID != "review-2" || rounds[0].Evaluation.Findings[2].ExternalID != "review-3" {
		t.Fatalf("evaluation order = %#v", rounds[0].Evaluation.Findings)
	}
	states := make(map[string]string, len(rounds[0].Decision.Findings))
	for _, reference := range rounds[0].Decision.Findings {
		for _, finding := range rounds[0].Evaluation.Findings {
			if finding.ID == reference.FindingID {
				states[finding.ExternalID] = reference.State
			}
		}
	}
	if states["review-1"] != RoundDecisionFindingSelected || states["review-2"] != RoundDecisionFindingSelected || states["review-3"] != RoundDecisionFindingUnselected {
		t.Fatalf("decision states = %#v", states)
	}
}
