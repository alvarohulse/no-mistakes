package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	RoundTriggerInitial = "initial"
	RoundTriggerAutoFix = "auto_fix"
	RoundTriggerUserFix = "user_fix"

	RoundTriggerProvenanceLegacyUserFix = "legacy:user_fix"

	RoundEvaluationInitialReview = "initial_review"
	RoundEvaluationRereview      = "rereview"
	RoundEvaluationValidation    = "validation"
	RoundEvaluationRevalidation  = "revalidation"
	RoundEvaluationDocumentation = "documentation"

	RoundDecisionFindingSelected   = "selected"
	RoundDecisionFindingUnselected = "unselected"

	RoundRepairAttempted              = "attempted"
	RoundRepairResolved               = "resolved"
	RoundRepairStoppedNoProgress      = "stopped_no_progress"
	RoundRepairStoppedRepeatedFailure = "stopped_repeated_failure"
	RoundRepairStoppedAttemptLimit    = "stopped_attempt_limit"
)

// StepRoundEvaluation is the normalized evaluation produced by one round.
// Its findings are ordered and referenceable without reparsing a JSON blob.
type StepRoundEvaluation struct {
	ID             string
	RunID          string
	RoundID        string
	Kind           string
	Summary        string
	Tested         []string
	TestingSummary string
	RiskLevel      string
	RiskRationale  string
	RiskScope      string
	Findings       []StepRoundFinding
	Artifacts      []StepRoundEvaluationArtifact
	CreatedAt      int64
}

// StepRoundFinding is one stable, ordered finding within an evaluation.
// ExternalID retains the agent-facing identity used by approval prompts.
type StepRoundFinding struct {
	ID                  string
	EvaluationID        string
	Ordinal             int
	ExternalID          string
	Severity            string
	File                string
	Line                int
	Description         string
	Action              string
	Source              string
	UserInstructions    string
	ReviewScope         string
	RequiresHumanReview bool
}

type StepRoundEvaluationArtifact struct {
	ID           string
	EvaluationID string
	Ordinal      int
	Kind         string
	Label        string
	Path         string
	URL          string
	Content      string
}

// StepRoundDecision records a selection after an evaluation. Each finding is
// retained as selected or deliberately unselected; user additions live in the
// evaluation so a decision never embeds a second semantic copy of a finding.
type StepRoundDecision struct {
	ID            string
	RunID         string
	RoundID       string
	Source        string
	ExplicitEmpty bool
	Findings      []StepRoundDecisionFinding
	CreatedAt     int64
}

type StepRoundDecisionFinding struct {
	FindingID        string
	Ordinal          int
	SelectionOrdinal *int
	State            string
	UserInstructions string
	Edited           bool
}

// StepRoundRepair preserves the bounded repair outcome and content-free
// fingerprint for a fix round.
type StepRoundRepair struct {
	ID                 string
	RunID              string
	RoundID            string
	FixSummary         *string
	FailureFingerprint *string
	Result             *string
	ResultingHeadSHA   *string
	CreatedAt          int64
}

// StructuredRoundSubject is the commit/config identity observed by one round.
// Nil means unavailable; historical rows are never backfilled by inference.
type StructuredRoundSubject struct {
	StartingHeadSHA  *string
	ResultingHeadSHA *string
	EvaluatedHeadSHA *string
	TrustedConfigSHA *string
	ReplayConfigJSON []byte
}

func validRoundEvaluationKind(kind string) bool {
	switch kind {
	case RoundEvaluationInitialReview, RoundEvaluationRereview, RoundEvaluationValidation, RoundEvaluationRevalidation, RoundEvaluationDocumentation:
		return true
	default:
		return false
	}
}

func validRoundDecisionSource(source string) bool {
	return source == RoundSelectionSourceUser || source == RoundSelectionSourceAutoFix || source == RoundSelectionSourceUserDeclined
}

func validRoundRepairResult(result string) bool {
	switch result {
	case RoundRepairAttempted, RoundRepairResolved, RoundRepairStoppedNoProgress, RoundRepairStoppedRepeatedFailure, RoundRepairStoppedAttemptLimit:
		return true
	default:
		return false
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// CompleteStepRoundStructured atomically completes an active round with its
// evaluation, subject, and initial repair receipt. The compatibility JSON
// columns remain empty; readers project them in memory for legacy consumers.
func (d *DB) CompleteStepRoundStructured(roundID string, evaluation StepRoundEvaluation, subject StructuredRoundSubject, fixSummary *string, durationMS int64) error {
	if !validRoundEvaluationKind(evaluation.Kind) {
		return fmt.Errorf("complete structured step round: invalid evaluation kind %q", evaluation.Kind)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("complete structured step round: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var runID, stepName, trigger, status string
	if err := tx.QueryRow(`SELECT s.run_id, s.step_name, r.trigger_type, r.status
		FROM step_rounds r JOIN step_results s ON s.id = r.step_result_id WHERE r.id = ?`, roundID).Scan(&runID, &stepName, &trigger, &status); err != nil {
		return fmt.Errorf("complete structured step round: load round: %w", err)
	}
	if status != RoundStatusActive {
		return fmt.Errorf("complete structured step round: round %q is not active", roundID)
	}
	if evaluation.RunID == "" {
		evaluation.RunID = runID
	}
	if evaluation.RoundID == "" {
		evaluation.RoundID = roundID
	}
	if evaluation.RunID != runID || evaluation.RoundID != roundID {
		return fmt.Errorf("complete structured step round: evaluation does not belong to round")
	}
	if evaluation.ID == "" {
		evaluation.ID = newID()
	}
	if evaluation.CreatedAt == 0 {
		evaluation.CreatedAt = now()
	}
	tested := evaluation.Tested
	if tested == nil {
		tested = []string{}
	}
	testedJSON, err := json.Marshal(tested)
	if err != nil {
		return fmt.Errorf("complete structured step round: encode tested claims: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO round_evaluations
		(id, run_id, round_id, kind, summary, tested_json, testing_summary, risk_level, risk_rationale, risk_scope, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evaluation.ID, evaluation.RunID, evaluation.RoundID, evaluation.Kind, evaluation.Summary, string(testedJSON), evaluation.TestingSummary,
		evaluation.RiskLevel, evaluation.RiskRationale, evaluation.RiskScope, evaluation.CreatedAt); err != nil {
		return fmt.Errorf("complete structured step round: insert evaluation: %w", err)
	}

	seenIDs := make(map[string]struct{}, len(evaluation.Findings))
	seenExternalIDs := make(map[string]struct{}, len(evaluation.Findings))
	for ordinal := range evaluation.Findings {
		finding := &evaluation.Findings[ordinal]
		if finding.ID == "" {
			finding.ID = newID()
		}
		if _, exists := seenIDs[finding.ID]; exists {
			return fmt.Errorf("complete structured step round: duplicate finding identity %q", finding.ID)
		}
		if finding.EvaluationID == "" {
			finding.EvaluationID = evaluation.ID
		}
		if finding.EvaluationID != evaluation.ID {
			return fmt.Errorf("complete structured step round: finding %q does not belong to evaluation", finding.ID)
		}
		finding.Ordinal = ordinal
		if finding.ExternalID == "" {
			finding.ExternalID = finding.ID
		}
		finding.ExternalID = strings.TrimSpace(finding.ExternalID)
		if finding.ExternalID == "" {
			return fmt.Errorf("complete structured step round: finding %q has empty external identity", finding.ID)
		}
		if _, exists := seenExternalIDs[finding.ExternalID]; exists {
			return fmt.Errorf("complete structured step round: duplicate external finding identity %q", finding.ExternalID)
		}
		if finding.Source == "" {
			finding.Source = types.FindingSourceAgent
		}
		if finding.Action == "" {
			finding.Action = types.ActionAskUser
		}
		finding.RequiresHumanReview = finding.RequiresHumanReview || finding.Action == types.ActionAskUser
		seenIDs[finding.ID] = struct{}{}
		seenExternalIDs[finding.ExternalID] = struct{}{}
		if _, err := tx.Exec(`INSERT INTO round_findings
			(id, run_id, evaluation_id, ordinal, external_id, severity, file, line, description, action, source, user_instructions, review_scope, requires_human_review)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.ID, runID, evaluation.ID, finding.Ordinal, finding.ExternalID, finding.Severity, finding.File, finding.Line,
			finding.Description, finding.Action, finding.Source, finding.UserInstructions, finding.ReviewScope, finding.RequiresHumanReview); err != nil {
			return fmt.Errorf("complete structured step round: insert finding %q: %w", finding.ID, err)
		}
	}

	seenArtifactIDs := make(map[string]struct{}, len(evaluation.Artifacts))
	for ordinal := range evaluation.Artifacts {
		artifact := &evaluation.Artifacts[ordinal]
		if artifact.ID == "" {
			artifact.ID = newID()
		}
		if _, exists := seenArtifactIDs[artifact.ID]; exists {
			return fmt.Errorf("complete structured step round: duplicate evaluation artifact identity %q", artifact.ID)
		}
		if artifact.EvaluationID == "" {
			artifact.EvaluationID = evaluation.ID
		}
		if artifact.EvaluationID != evaluation.ID {
			return fmt.Errorf("complete structured step round: artifact %q does not belong to evaluation", artifact.ID)
		}
		artifact.Ordinal = ordinal
		seenArtifactIDs[artifact.ID] = struct{}{}
		if _, err := tx.Exec(`INSERT INTO round_evaluation_artifacts
			(id, run_id, evaluation_id, ordinal, kind, label, path, url, content)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			artifact.ID, runID, evaluation.ID, artifact.Ordinal, artifact.Kind, artifact.Label, artifact.Path, artifact.URL, artifact.Content); err != nil {
			return fmt.Errorf("complete structured step round: insert artifact %q: %w", artifact.ID, err)
		}
	}

	var reviewedHead *string
	if stepName == string(types.StepReview) {
		reviewedHead = subject.EvaluatedHeadSHA
	}
	if _, err := tx.Exec(`UPDATE step_rounds SET
		findings_json = NULL, user_findings_json = NULL, selected_finding_ids = NULL, selection_source = NULL,
		fix_summary = NULL, repair_failure_fingerprint = NULL, repair_result = NULL,
		reviewed_head_sha = ?, starting_head_sha = ?, trusted_config_sha = ?, replay_config_json = ?,
		resulting_head_sha = ?, evaluated_head_sha = ?, duration_ms = ?, status = ?
		WHERE id = ? AND status = ?`,
		reviewedHead, subject.StartingHeadSHA, subject.TrustedConfigSHA, subject.ReplayConfigJSON,
		subject.ResultingHeadSHA, subject.EvaluatedHeadSHA, durationMS, RoundStatusCompleted, roundID, RoundStatusActive); err != nil {
		return fmt.Errorf("complete structured step round: update round: %w", err)
	}
	if trigger == RoundTriggerAutoFix || (fixSummary != nil && strings.TrimSpace(*fixSummary) != "") {
		if err := insertRoundRepair(tx, StepRoundRepair{ID: newID(), RunID: runID, RoundID: roundID, FixSummary: fixSummary, ResultingHeadSHA: subject.ResultingHeadSHA, CreatedAt: now()}); err != nil {
			return fmt.Errorf("complete structured step round: insert repair: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete structured step round: commit: %w", err)
	}
	return nil
}

// SetStepRoundStructuredDecision records explicit stable finding references.
// Any omitted source finding is stored as deliberately unselected.
func (d *DB) SetStepRoundStructuredDecision(decision StepRoundDecision) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("set structured round decision: begin transaction: %w", err)
	}
	defer tx.Rollback()
	evaluation, err := getRoundEvaluation(tx, decision.RoundID)
	if err != nil {
		return err
	}
	if evaluation == nil {
		return fmt.Errorf("set structured round decision: round %q has no evaluation", decision.RoundID)
	}
	if decision.RunID == "" {
		decision.RunID = evaluation.RunID
	}
	if decision.RunID != evaluation.RunID || !validRoundDecisionSource(decision.Source) {
		return fmt.Errorf("set structured round decision: invalid decision identity or source")
	}
	if decision.Source == RoundSelectionSourceUserDeclined {
		decision.ExplicitEmpty = true
	}
	if decision.ID == "" {
		decision.ID = newID()
	}
	if decision.CreatedAt == 0 {
		decision.CreatedAt = now()
	}
	references := make(map[string]StepRoundDecisionFinding, len(decision.Findings))
	selectionOrdinals := make(map[int]string)
	nextSelectionOrdinal := 0
	for _, reference := range decision.Findings {
		if reference.FindingID == "" || (reference.State != RoundDecisionFindingSelected && reference.State != RoundDecisionFindingUnselected) {
			return fmt.Errorf("set structured round decision: invalid finding reference")
		}
		if _, exists := references[reference.FindingID]; exists {
			return fmt.Errorf("set structured round decision: duplicate finding reference %q", reference.FindingID)
		}
		if reference.State == RoundDecisionFindingSelected {
			if reference.SelectionOrdinal == nil {
				ordinal := nextSelectionOrdinal
				reference.SelectionOrdinal = &ordinal
			}
			if *reference.SelectionOrdinal < 0 {
				return fmt.Errorf("set structured round decision: invalid selection ordinal %d", *reference.SelectionOrdinal)
			}
			if findingID, exists := selectionOrdinals[*reference.SelectionOrdinal]; exists {
				return fmt.Errorf("set structured round decision: duplicate selection ordinal %d for findings %q and %q", *reference.SelectionOrdinal, findingID, reference.FindingID)
			}
			selectionOrdinals[*reference.SelectionOrdinal] = reference.FindingID
			if *reference.SelectionOrdinal >= nextSelectionOrdinal {
				nextSelectionOrdinal = *reference.SelectionOrdinal + 1
			}
		} else if reference.SelectionOrdinal != nil {
			return fmt.Errorf("set structured round decision: unselected finding %q has a selection ordinal", reference.FindingID)
		}
		references[reference.FindingID] = reference
	}
	if decision.ExplicitEmpty {
		for _, reference := range references {
			if reference.State == RoundDecisionFindingSelected {
				return fmt.Errorf("set structured round decision: explicit empty decision selects a finding")
			}
		}
	}
	for _, finding := range evaluation.Findings {
		if _, found := references[finding.ID]; !found {
			references[finding.ID] = StepRoundDecisionFinding{FindingID: finding.ID, State: RoundDecisionFindingUnselected}
		}
	}
	if len(references) != len(evaluation.Findings) {
		return fmt.Errorf("set structured round decision: finding does not belong to evaluation")
	}
	if err := replaceRoundDecision(tx, decision, evaluation.Findings, references); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set structured round decision: commit: %w", err)
	}
	return nil
}

func (d *DB) SetStepRoundStructuredRepair(repair StepRoundRepair) error {
	if repair.Result != nil && !validRoundRepairResult(*repair.Result) {
		return fmt.Errorf("set structured round repair: invalid result %q", *repair.Result)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("set structured round repair: begin transaction: %w", err)
	}
	defer tx.Rollback()
	var runID string
	if err := tx.QueryRow(`SELECT s.run_id FROM step_rounds r JOIN step_results s ON s.id = r.step_result_id WHERE r.id = ?`, repair.RoundID).Scan(&runID); err != nil {
		return fmt.Errorf("set structured round repair: load round: %w", err)
	}
	if repair.RunID == "" {
		repair.RunID = runID
	}
	if repair.RunID != runID {
		return fmt.Errorf("set structured round repair: repair does not belong to round")
	}
	if repair.ID == "" {
		repair.ID = newID()
	}
	if repair.CreatedAt == 0 {
		repair.CreatedAt = now()
	}
	if _, err := tx.Exec(`INSERT INTO round_repairs (id, run_id, round_id, fix_summary, failure_fingerprint, result, resulting_head_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(round_id) DO UPDATE SET
		fix_summary = COALESCE(excluded.fix_summary, round_repairs.fix_summary),
		failure_fingerprint = COALESCE(excluded.failure_fingerprint, round_repairs.failure_fingerprint),
		result = COALESCE(excluded.result, round_repairs.result),
		resulting_head_sha = COALESCE(excluded.resulting_head_sha, round_repairs.resulting_head_sha)`,
		repair.ID, repair.RunID, repair.RoundID, repair.FixSummary, repair.FailureFingerprint, repair.Result, repair.ResultingHeadSHA, repair.CreatedAt); err != nil {
		return fmt.Errorf("set structured round repair: persist repair: %w", err)
	}
	if _, err := tx.Exec(`UPDATE step_rounds SET resulting_head_sha = COALESCE(?, resulting_head_sha) WHERE id = ?`, repair.ResultingHeadSHA, repair.RoundID); err != nil {
		return fmt.Errorf("set structured round repair: update subject: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set structured round repair: commit: %w", err)
	}
	return nil
}

func (d *DB) roundHasEvaluation(roundID string) (bool, error) {
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM round_evaluations WHERE round_id = ?`, roundID).Scan(&count); err != nil {
		return false, fmt.Errorf("check normalized round evaluation: %w", err)
	}
	return count == 1, nil
}

func (d *DB) clearStructuredRoundDecision(roundID string) error {
	if _, err := d.sql.Exec(`DELETE FROM round_decisions WHERE round_id = ?`, roundID); err != nil {
		return fmt.Errorf("clear structured round decision: %w", err)
	}
	return nil
}

func (d *DB) setStructuredDecisionByExternalIDs(roundID string, selectedIDs []string, source string, userFindingsJSON *string, explicitEmpty bool) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("set structured round decision: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := setStructuredDecisionByExternalIDsTx(tx, roundID, selectedIDs, source, userFindingsJSON, explicitEmpty, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set structured round decision: commit: %w", err)
	}
	return nil
}

func (d *DB) setStructuredDecisionIfAbsentByExternalIDs(roundID string, selectedIDs []string, source string, userFindingsJSON *string, explicitEmpty bool) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("set structured round decision: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := setStructuredDecisionByExternalIDsTx(tx, roundID, selectedIDs, source, userFindingsJSON, explicitEmpty, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set structured round decision: commit: %w", err)
	}
	return nil
}

func setStructuredDecisionByExternalIDsTx(tx *sql.Tx, roundID string, selectedIDs []string, source string, userFindingsJSON *string, explicitEmpty, onlyIfAbsent bool) error {
	if !validRoundDecisionSource(source) {
		return fmt.Errorf("set structured round decision: invalid source %q", source)
	}
	evaluation, err := getRoundEvaluation(tx, roundID)
	if err != nil {
		return err
	}
	if evaluation == nil {
		return fmt.Errorf("set structured round decision: round %q has no evaluation", roundID)
	}
	byExternal := make(map[string]*StepRoundFinding, len(evaluation.Findings))
	for i := range evaluation.Findings {
		byExternal[evaluation.Findings[i].ExternalID] = &evaluation.Findings[i]
		byExternal[evaluation.Findings[i].ID] = &evaluation.Findings[i]
	}
	edited := make(map[string]bool)
	selectedAliases := make(map[string]string)
	if userFindingsJSON != nil && strings.TrimSpace(*userFindingsJSON) != "" {
		merged, parseErr := types.ParseFindingsJSON(*userFindingsJSON)
		if parseErr != nil {
			return fmt.Errorf("set structured round decision: parse user findings: %w", parseErr)
		}
		seen := make(map[string]bool, len(merged.Items))
		for _, item := range merged.Items {
			item.ID = strings.TrimSpace(item.ID)
			if item.ID == "" || seen[item.ID] {
				return fmt.Errorf("set structured round decision: user finding identity is missing or duplicated")
			}
			seen[item.ID] = true
			if existing := byExternal[item.ID]; existing != nil {
				if item.Source == types.FindingSourceUser && existing.Source != types.FindingSourceUser {
					externalID := item.ID
					for suffix := 1; ; suffix++ {
						item.ID = fmt.Sprintf("user-%d", suffix)
						if byExternal[item.ID] == nil {
							break
						}
					}
					selectedAliases[externalID] = item.ID
				} else {
					existing.UserInstructions = item.UserInstructions
					if _, err := tx.Exec(`UPDATE round_findings SET user_instructions = ? WHERE id = ?`, existing.UserInstructions, existing.ID); err != nil {
						return fmt.Errorf("set structured round decision: update finding edit: %w", err)
					}
					edited[existing.ID] = item.UserInstructions != ""
					continue
				}
			}
			if item.Source == "" {
				item.Source = types.FindingSourceUser
			}
			if item.Action == "" {
				item.Action = types.ActionAutoFix
			}
			finding := StepRoundFinding{
				ID: newID(), EvaluationID: evaluation.ID, Ordinal: len(evaluation.Findings), ExternalID: item.ID,
				Severity: item.Severity, File: item.File, Line: item.Line, Description: item.Description, Action: item.Action,
				Source: item.Source, UserInstructions: item.UserInstructions, ReviewScope: item.ReviewScope,
				RequiresHumanReview: item.ActionOrDefault() == types.ActionAskUser,
			}
			if _, err := tx.Exec(`INSERT INTO round_findings
				(id, run_id, evaluation_id, ordinal, external_id, severity, file, line, description, action, source, user_instructions, review_scope, requires_human_review)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				finding.ID, evaluation.RunID, evaluation.ID, finding.Ordinal, finding.ExternalID, finding.Severity, finding.File,
				finding.Line, finding.Description, finding.Action, finding.Source, finding.UserInstructions, finding.ReviewScope, finding.RequiresHumanReview); err != nil {
				return fmt.Errorf("set structured round decision: insert user finding: %w", err)
			}
			evaluation.Findings = append(evaluation.Findings, finding)
			byExternal[finding.ExternalID] = &evaluation.Findings[len(evaluation.Findings)-1]
			byExternal[finding.ID] = &evaluation.Findings[len(evaluation.Findings)-1]
			edited[finding.ID] = finding.UserInstructions != ""
		}
	}
	selected := make(map[string]int, len(selectedIDs))
	for _, id := range selectedIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if alias := selectedAliases[id]; alias != "" {
			id = alias
		}
		if byExternal[id] == nil {
			return fmt.Errorf("set structured round decision: selected finding %q does not belong to evaluation", id)
		}
		findingID := byExternal[id].ID
		if _, exists := selected[findingID]; exists {
			return fmt.Errorf("set structured round decision: duplicate selected finding %q", id)
		}
		selected[findingID] = len(selected)
	}
	if explicitEmpty || source == RoundSelectionSourceUserDeclined {
		explicitEmpty = true
		if len(selected) != 0 {
			return fmt.Errorf("set structured round decision: explicit empty decision selects findings")
		}
	}
	references := make(map[string]StepRoundDecisionFinding, len(evaluation.Findings))
	for _, finding := range evaluation.Findings {
		state := RoundDecisionFindingUnselected
		var selectionOrdinal *int
		if ordinal, found := selected[finding.ID]; found {
			state = RoundDecisionFindingSelected
			selectionOrdinal = &ordinal
		}
		references[finding.ID] = StepRoundDecisionFinding{FindingID: finding.ID, SelectionOrdinal: selectionOrdinal, State: state, UserInstructions: finding.UserInstructions, Edited: edited[finding.ID]}
	}
	decision := StepRoundDecision{ID: newID(), RunID: evaluation.RunID, RoundID: roundID, Source: source, ExplicitEmpty: explicitEmpty, CreatedAt: now()}
	if onlyIfAbsent {
		return insertRoundDecisionIfAbsent(tx, decision, evaluation.Findings, references)
	}
	if err := replaceRoundDecision(tx, decision, evaluation.Findings, references); err != nil {
		return err
	}
	return nil
}

func insertRoundDecisionIfAbsent(tx *sql.Tx, decision StepRoundDecision, findings []StepRoundFinding, references map[string]StepRoundDecisionFinding) error {
	result, err := tx.Exec(`INSERT INTO round_decisions (id, run_id, round_id, source, explicit_empty, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(round_id) DO NOTHING`,
		decision.ID, decision.RunID, decision.RoundID, decision.Source, decision.ExplicitEmpty, decision.CreatedAt)
	if err != nil {
		return fmt.Errorf("set structured round decision: insert decision: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set structured round decision: insert decision rows affected: %w", err)
	}
	if inserted == 0 {
		return nil
	}
	for ordinal, finding := range findings {
		reference := references[finding.ID]
		if _, err := tx.Exec(`INSERT INTO round_decision_findings (decision_id, finding_id, ordinal, selection_ordinal, state, user_instructions, edited) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			decision.ID, finding.ID, ordinal, reference.SelectionOrdinal, reference.State, reference.UserInstructions, reference.Edited); err != nil {
			return fmt.Errorf("set structured round decision: insert finding reference: %w", err)
		}
	}
	return nil
}

func replaceRoundDecision(tx *sql.Tx, decision StepRoundDecision, findings []StepRoundFinding, references map[string]StepRoundDecisionFinding) error {
	if _, err := tx.Exec(`DELETE FROM round_decisions WHERE round_id = ?`, decision.RoundID); err != nil {
		return fmt.Errorf("set structured round decision: clear prior decision: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO round_decisions (id, run_id, round_id, source, explicit_empty, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		decision.ID, decision.RunID, decision.RoundID, decision.Source, decision.ExplicitEmpty, decision.CreatedAt); err != nil {
		return fmt.Errorf("set structured round decision: insert decision: %w", err)
	}
	for ordinal, finding := range findings {
		reference := references[finding.ID]
		if _, err := tx.Exec(`INSERT INTO round_decision_findings (decision_id, finding_id, ordinal, selection_ordinal, state, user_instructions, edited) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			decision.ID, finding.ID, ordinal, reference.SelectionOrdinal, reference.State, reference.UserInstructions, reference.Edited); err != nil {
			return fmt.Errorf("set structured round decision: insert finding reference: %w", err)
		}
	}
	return nil
}

func insertRoundRepair(tx *sql.Tx, repair StepRoundRepair) error {
	_, err := tx.Exec(`INSERT INTO round_repairs (id, run_id, round_id, fix_summary, failure_fingerprint, result, resulting_head_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, repair.ID, repair.RunID, repair.RoundID, repair.FixSummary, repair.FailureFingerprint, repair.Result, repair.ResultingHeadSHA, repair.CreatedAt)
	return err
}

type roundGraphQuerier interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func (d *DB) GetRoundEvaluation(roundID string) (*StepRoundEvaluation, error) {
	return getRoundEvaluation(d.sql, roundID)
}

func getRoundEvaluation(q roundGraphQuerier, roundID string) (*StepRoundEvaluation, error) {
	evaluation := &StepRoundEvaluation{}
	var testedJSON string
	if err := q.QueryRow(`SELECT id, run_id, round_id, kind, summary, tested_json, testing_summary, risk_level, risk_rationale, risk_scope, created_at
		FROM round_evaluations WHERE round_id = ?`, roundID).Scan(
		&evaluation.ID, &evaluation.RunID, &evaluation.RoundID, &evaluation.Kind, &evaluation.Summary, &testedJSON,
		&evaluation.TestingSummary, &evaluation.RiskLevel, &evaluation.RiskRationale, &evaluation.RiskScope, &evaluation.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get round evaluation: %w", err)
	}
	if err := json.Unmarshal([]byte(testedJSON), &evaluation.Tested); err != nil {
		return nil, fmt.Errorf("decode round evaluation tested claims: %w", err)
	}
	rows, err := q.Query(`SELECT id, evaluation_id, ordinal, external_id, severity, file, line, description, action, source, user_instructions, review_scope, requires_human_review
		FROM round_findings WHERE evaluation_id = ? ORDER BY ordinal`, evaluation.ID)
	if err != nil {
		return nil, fmt.Errorf("get round evaluation findings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		finding := StepRoundFinding{}
		var human int
		if err := rows.Scan(&finding.ID, &finding.EvaluationID, &finding.Ordinal, &finding.ExternalID, &finding.Severity, &finding.File, &finding.Line, &finding.Description, &finding.Action, &finding.Source, &finding.UserInstructions, &finding.ReviewScope, &human); err != nil {
			return nil, fmt.Errorf("scan round evaluation finding: %w", err)
		}
		finding.RequiresHumanReview = human != 0
		evaluation.Findings = append(evaluation.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate round evaluation findings: %w", err)
	}
	artifactRows, err := q.Query(`SELECT id, evaluation_id, ordinal, kind, label, path, url, content
		FROM round_evaluation_artifacts WHERE evaluation_id = ? ORDER BY ordinal`, evaluation.ID)
	if err != nil {
		return nil, fmt.Errorf("get round evaluation artifacts: %w", err)
	}
	defer artifactRows.Close()
	for artifactRows.Next() {
		artifact := StepRoundEvaluationArtifact{}
		if err := artifactRows.Scan(&artifact.ID, &artifact.EvaluationID, &artifact.Ordinal, &artifact.Kind, &artifact.Label, &artifact.Path, &artifact.URL, &artifact.Content); err != nil {
			return nil, fmt.Errorf("scan round evaluation artifact: %w", err)
		}
		evaluation.Artifacts = append(evaluation.Artifacts, artifact)
	}
	if err := artifactRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate round evaluation artifacts: %w", err)
	}
	return evaluation, nil
}

func (d *DB) GetRoundDecision(roundID string) (*StepRoundDecision, error) {
	decision := &StepRoundDecision{}
	var explicit int
	if err := d.sql.QueryRow(`SELECT id, run_id, round_id, source, explicit_empty, created_at FROM round_decisions WHERE round_id = ?`, roundID).Scan(
		&decision.ID, &decision.RunID, &decision.RoundID, &decision.Source, &explicit, &decision.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get round decision: %w", err)
	}
	decision.ExplicitEmpty = explicit != 0
	rows, err := d.sql.Query(`SELECT finding_id, ordinal, selection_ordinal, state, user_instructions, edited FROM round_decision_findings WHERE decision_id = ? ORDER BY ordinal`, decision.ID)
	if err != nil {
		return nil, fmt.Errorf("get round decision findings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		finding := StepRoundDecisionFinding{}
		var selectionOrdinal sql.NullInt64
		var edited int
		if err := rows.Scan(&finding.FindingID, &finding.Ordinal, &selectionOrdinal, &finding.State, &finding.UserInstructions, &edited); err != nil {
			return nil, fmt.Errorf("scan round decision finding: %w", err)
		}
		if selectionOrdinal.Valid {
			value := int(selectionOrdinal.Int64)
			finding.SelectionOrdinal = &value
		}
		finding.Edited = edited != 0
		decision.Findings = append(decision.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate round decision findings: %w", err)
	}
	return decision, nil
}

func orderedSelectedDecisionFindings(decision *StepRoundDecision) []StepRoundDecisionFinding {
	if decision == nil {
		return nil
	}
	selected := make([]StepRoundDecisionFinding, 0, len(decision.Findings))
	for _, finding := range decision.Findings {
		if finding.State == RoundDecisionFindingSelected {
			selected = append(selected, finding)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		left, right := selected[i], selected[j]
		switch {
		case left.SelectionOrdinal != nil && right.SelectionOrdinal != nil:
			return *left.SelectionOrdinal < *right.SelectionOrdinal
		case left.SelectionOrdinal != nil:
			return true
		case right.SelectionOrdinal != nil:
			return false
		default:
			return left.Ordinal < right.Ordinal
		}
	})
	return selected
}

func (d *DB) GetRoundRepair(roundID string) (*StepRoundRepair, error) {
	repair := &StepRoundRepair{}
	var result sql.NullString
	if err := d.sql.QueryRow(`SELECT id, run_id, round_id, fix_summary, failure_fingerprint, result, resulting_head_sha, created_at FROM round_repairs WHERE round_id = ?`, roundID).Scan(
		&repair.ID, &repair.RunID, &repair.RoundID, &repair.FixSummary, &repair.FailureFingerprint, &result, &repair.ResultingHeadSHA, &repair.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get round repair: %w", err)
	}
	if result.Valid {
		repair.Result = &result.String
	}
	return repair, nil
}

// CompatibilityFindingsJSON projects structured findings only for callers
// that still consume the old wire value. It never persists a second copy.
func CompatibilityFindingsJSON(evaluation *StepRoundEvaluation) (*string, error) {
	if evaluation == nil {
		return nil, nil
	}
	findings := types.Findings{
		Summary: evaluation.Summary, Tested: append([]string(nil), evaluation.Tested...), TestingSummary: evaluation.TestingSummary,
		RiskLevel: evaluation.RiskLevel, RiskRationale: evaluation.RiskRationale, RiskScope: evaluation.RiskScope,
	}
	for _, artifact := range evaluation.Artifacts {
		findings.Artifacts = append(findings.Artifacts, types.TestArtifact{
			Kind: artifact.Kind, Label: artifact.Label, Path: artifact.Path, URL: artifact.URL, Content: artifact.Content,
		})
	}
	for _, item := range evaluation.Findings {
		findings.Items = append(findings.Items, types.Finding{ID: item.ExternalID, Severity: item.Severity, File: item.File, Line: item.Line, Description: item.Description, Action: item.Action, Source: item.Source, UserInstructions: item.UserInstructions, ReviewScope: item.ReviewScope})
	}
	raw, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return nil, err
	}
	return &raw, nil
}

func (d *DB) hydrateRoundGraph(round *StepRound) error {
	evaluation, err := d.GetRoundEvaluation(round.ID)
	if err != nil {
		return fmt.Errorf("hydrate round evaluation: %w", err)
	}
	if evaluation != nil {
		round.Evaluation = evaluation
		projected, err := CompatibilityFindingsJSON(evaluation)
		if err != nil {
			return fmt.Errorf("hydrate round findings projection: %w", err)
		}
		round.FindingsJSON = projected
	}
	decision, err := d.GetRoundDecision(round.ID)
	if err != nil {
		return fmt.Errorf("hydrate round decision: %w", err)
	}
	if decision != nil {
		round.Decision = decision
		source := decision.Source
		round.SelectionSource = &source
		selected := make([]string, 0, len(decision.Findings))
		byID := make(map[string]StepRoundFinding, len(evaluation.Findings))
		for _, finding := range evaluation.Findings {
			byID[finding.ID] = finding
		}
		userFindings := types.Findings{}
		hasUserOverride := false
		for _, reference := range orderedSelectedDecisionFindings(decision) {
			finding, found := byID[reference.FindingID]
			if !found {
				return fmt.Errorf("hydrate round decision: missing finding %q", reference.FindingID)
			}
			selected = append(selected, finding.ExternalID)
			if finding.Source == types.FindingSourceUser || reference.Edited || reference.UserInstructions != "" {
				hasUserOverride = true
			}
			userFindings.Items = append(userFindings.Items, types.Finding{ID: finding.ExternalID, Severity: finding.Severity, File: finding.File, Line: finding.Line, Description: finding.Description, Action: finding.Action, Source: finding.Source, UserInstructions: reference.UserInstructions, ReviewScope: finding.ReviewScope})
		}
		if decision.ExplicitEmpty || len(selected) > 0 {
			raw, err := json.Marshal(selected)
			if err != nil {
				return fmt.Errorf("hydrate round selection projection: %w", err)
			}
			value := string(raw)
			round.SelectedFindingIDs = &value
		}
		if hasUserOverride {
			raw, err := types.MarshalFindingsJSON(userFindings)
			if err != nil {
				return fmt.Errorf("hydrate round user findings projection: %w", err)
			}
			round.UserFindingsJSON = &raw
		}
	}
	repair, err := d.GetRoundRepair(round.ID)
	if err != nil {
		return fmt.Errorf("hydrate round repair: %w", err)
	}
	if repair != nil {
		round.Repair = repair
		round.FixSummary = repair.FixSummary
		round.RepairFailureFingerprint = repair.FailureFingerprint
		round.RepairResult = repair.Result
		if round.ResultingHeadSHA == nil {
			round.ResultingHeadSHA = repair.ResultingHeadSHA
		}
	}
	return d.hydrateRoundReferences(round)
}

func (d *DB) hydrateRoundReferences(round *StepRound) error {
	queries := []struct {
		destination *[]string
		query       string
		label       string
	}{
		{&round.InvocationIDs, `SELECT id FROM agent_invocations WHERE round_id = ? ORDER BY started_at, id`, "invocation"},
		{&round.CommandAttemptIDs, `SELECT id FROM command_attempts WHERE round_id = ? ORDER BY sequence, id`, "command attempt"},
		{&round.ArtifactIDs, `SELECT id FROM artifacts WHERE round_id = ? ORDER BY created_at, id`, "artifact"},
	}
	for _, entry := range queries {
		rows, err := d.sql.Query(entry.query, round.ID)
		if err != nil {
			return fmt.Errorf("get round %s references: %w", entry.label, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan round %s reference: %w", entry.label, err)
			}
			*entry.destination = append(*entry.destination, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate round %s references: %w", entry.label, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close round %s references: %w", entry.label, err)
		}
	}
	return nil
}
