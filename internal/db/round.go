package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	RoundSelectionSourceUser        = "user"
	RoundSelectionSourceAutoFix     = "auto_fix"
	RoundSelectionSourceUserWaived  = "user_declined"
	RoundSelectionSourceUserSkipped = "user_skipped"
	RoundSelectionSourceUserAborted = "user_aborted"
	// RoundSelectionSourceUserDeclined records that a human resolved the
	// approval gate without selecting a finding to fix. The explicit empty
	// selection distinguishes this decision from an unresolved round.
	RoundSelectionSourceUserDeclined = RoundSelectionSourceUserWaived

	RoundStatusActive    = "active"
	RoundStatusCompleted = "completed"
	RoundStatusFailed    = "failed"
)

const DeclinedSelectionJSON = "[]"

// StepRound represents one execution round within a pipeline step.
type StepRound struct {
	ID                string
	StepResultID      string
	Round             int
	Trigger           string // "initial" or "auto_fix" in the normalized view
	TriggerProvenance *string
	Status            string
	FindingsJSON      *string // nullable - findings produced by this round
	ReviewedHeadSHA   *string // non-authoritative commit candidate captured by a review round
	StartingHeadSHA   *string
	TrustedConfigSHA  *string
	ReplayConfigJSON  []byte
	GlobalConfigYAML  []byte
	RepoConfigYAML    []byte
	// UserFindingsJSON, when non-nil, is the merged finding list that was
	// dispatched to the fix agent after the user edited per-finding
	// instructions or added their own findings. It includes both the
	// selected agent-produced findings (with any attached user
	// instructions) and the user-authored findings.
	UserFindingsJSON *string
	// SelectedFindingIDs, when non-nil, is a JSON array of finding IDs that
	// were chosen (by the user or auto-fix filter) to be fixed AFTER this
	// round. It is populated on the round whose findings triggered the next
	// round, so that later rounds' prompts can tell which findings were
	// deliberately left unselected.
	SelectedFindingIDs *string
	SelectionSource    *string
	// FixSummary, when non-nil, is the agent's one-line commit summary for
	// the fix attempt performed during this round. It is only set when the
	// round itself was a fix round (trigger=="auto_fix").
	FixSummary *string
	// RepairFailureFingerprint and RepairResult are content-free audit facts
	// for bounded automatic repair. The fingerprint is a one-way hash of the
	// normalized failure identity; no finding or output content is duplicated.
	RepairFailureFingerprint *string
	RepairResult             *string
	ResultingHeadSHA         *string
	EvaluatedHeadSHA         *string
	Evaluation               *StepRoundEvaluation
	Decision                 *StepRoundDecision
	Repair                   *StepRoundRepair
	InvocationIDs            []string
	CommandAttemptIDs        []string
	ArtifactIDs              []string
	DurationMS               int64
	CreatedAt                int64
}

// StepRoundStats summarizes execution rounds for a step. It lets status
// surfaces show whether a running/fixing step is in an initial pass or a fix
// pass without reloading every round in callers.
type StepRoundStats struct {
	TotalRounds        int
	FixRounds          int
	LatestRound        int
	LatestTrigger      string
	LatestSelection    string
	LatestRoundAt      int64
	LatestFixRound     int
	LatestFixRoundAt   int64
	SelectedForFix     bool
	AutoSelectedForFix bool
	PendingFixSource   string
}

// IsFixRound reports whether this round was a fix attempt. Legacy "user_fix"
// rounds count: they were fix rounds dispatched by an explicit user selection.
func (r *StepRound) IsFixRound() bool {
	return r.Trigger == RoundTriggerAutoFix || r.Trigger == RoundTriggerUserFix
}

// StepFixSummaries returns one entry per fix round for a step, in round order:
// the agent's one-line fix summary, or "" when the round recorded none.
func (d *DB) StepFixSummaries(stepResultID string) ([]string, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	var summaries []string
	for _, r := range rounds {
		if (r.Status != "" && r.Status != RoundStatusCompleted) || !r.IsFixRound() {
			continue
		}
		summary := ""
		if r.FixSummary != nil {
			summary = *r.FixSummary
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// StepRoundStats returns aggregate round information for a step result.
func (d *DB) StepRoundStats(stepResultID string) (StepRoundStats, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return StepRoundStats{}, err
	}
	var stats StepRoundStats
	latestSelectedRound := 0
	latestSelectedSource := ""
	for _, r := range rounds {
		if r.Status != "" && r.Status != RoundStatusCompleted {
			continue
		}
		stats.TotalRounds++
		stats.LatestRound = r.Round
		stats.LatestTrigger = r.Trigger
		stats.LatestRoundAt = r.CreatedAt
		if r.SelectionSource != nil {
			stats.LatestSelection = *r.SelectionSource
		}
		if hasSelectedFinding(r.SelectedFindingIDs) {
			stats.SelectedForFix = true
			stats.AutoSelectedForFix = r.SelectionSource != nil && *r.SelectionSource == RoundSelectionSourceAutoFix
			latestSelectedRound = r.Round
			latestSelectedSource = stats.LatestSelection
		}
		if r.IsFixRound() {
			stats.FixRounds++
			stats.LatestFixRound = stats.FixRounds
			stats.LatestFixRoundAt = r.CreatedAt
		}
	}
	if latestSelectedRound == stats.LatestRound {
		stats.PendingFixSource = latestSelectedSource
	}
	return stats, nil
}

func hasSelectedFinding(raw *string) bool {
	if raw == nil {
		return false
	}
	var ids []string
	if json.Unmarshal([]byte(*raw), &ids) != nil {
		return false
	}
	for _, id := range ids {
		if id != "" {
			return true
		}
	}
	return false
}

// InsertStepRound creates a new round record for a step result. fixSummary may
// be nil for non-fix rounds or when the agent produced no summary.
func (d *DB) InsertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, durationMS int64) (*StepRound, error) {
	return d.insertStepRound(stepResultID, round, trigger, RoundStatusCompleted, findingsJSON, fixSummary, nil, nil, nil, nil, nil, nil, durationMS)
}

// BeginStepRound creates the durable round identity before its work starts so
// command attempts can reference the exact owning round as they execute.
func (d *DB) BeginStepRound(stepResultID string, round int, trigger string) (*StepRound, error) {
	if trigger != RoundTriggerInitial && trigger != RoundTriggerAutoFix {
		return nil, fmt.Errorf("begin step round: unsupported trigger %q", trigger)
	}
	return d.insertStepRound(stepResultID, round, trigger, RoundStatusActive, nil, nil, nil, nil, nil, nil, nil, nil, 0)
}

// CompleteStepRound fills the result fields of a round created by
// BeginStepRound. Selection and repair audit fields remain independently
// writable because they are decided after the execution result is observed.
func (d *DB) CompleteStepRound(id string, findingsJSON *string, fixSummary *string, durationMS int64) error {
	return d.completeStepRound(id, findingsJSON, fixSummary, nil, nil, nil, nil, nil, nil, durationMS)
}

// CompleteReviewStepRound fills a pre-created Review round and preserves the
// exact commit/config provenance already owned by the existing Review APIs.
func (d *DB) CompleteReviewStepRound(id string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, replayConfigJSON, globalConfigYAML, repoConfigYAML []byte, durationMS int64) error {
	var reviewed, starting, trusted *string
	if reviewedHeadSHA != "" {
		reviewed = &reviewedHeadSHA
	}
	if startingHeadSHA != "" {
		starting = &startingHeadSHA
	}
	if trustedConfigSHA != "" {
		trusted = &trustedConfigSHA
	}
	return d.completeStepRound(id, findingsJSON, fixSummary, reviewed, starting, trusted, replayConfigJSON, globalConfigYAML, repoConfigYAML, durationMS)
}

func (d *DB) completeStepRound(id string, findingsJSON *string, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA *string, replayConfigJSON, globalConfigYAML, repoConfigYAML []byte, durationMS int64) error {
	result, err := d.sql.Exec(
		`UPDATE step_rounds SET findings_json = ?, fix_summary = ?, reviewed_head_sha = ?, starting_head_sha = ?, trusted_config_sha = ?, replay_config_json = ?, global_config_yaml = ?, repo_config_yaml = ?, duration_ms = ?, status = ? WHERE id = ? AND status = ?`,
		findingsJSON, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA,
		replayConfigJSON, globalConfigYAML, repoConfigYAML, durationMS, RoundStatusCompleted, id, RoundStatusActive,
	)
	if err != nil {
		return fmt.Errorf("complete step round: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete step round: rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("complete step round: round %q not found", id)
	}
	return nil
}

func (d *DB) FailStepRound(id string, durationMS int64) error {
	result, err := d.sql.Exec(`UPDATE step_rounds SET duration_ms = ?, status = ? WHERE id = ? AND status = ?`, durationMS, RoundStatusFailed, id, RoundStatusActive)
	if err != nil {
		return fmt.Errorf("fail step round: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail step round rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("fail step round: round %q is missing or complete", id)
	}
	return nil
}

// InsertReviewStepRound persists a review round's examined commit as a
// non-authoritative candidate. A recovered parked gate can promote this exact
// candidate only after approval; merely storing it grants no push authority.
func (d *DB) InsertReviewStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA string, durationMS int64) (*StepRound, error) {
	return d.InsertReviewStepRoundWithProvenance(stepResultID, round, trigger, findingsJSON, fixSummary, reviewedHeadSHA, "", "", nil, nil, durationMS)
}

func (d *DB) InsertReviewStepRoundWithProvenance(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	return d.insertReviewStepRoundWithConfig(stepResultID, round, trigger, findingsJSON, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA, nil, globalConfigYAML, repoConfigYAML, durationMS)
}

// InsertReviewStepRoundWithReplayConfig stores the versioned, candidate-independent
// Review inputs used by new eval captures. Legacy YAML columns remain readable
// for cases recorded before this contract existed, but new rounds never fill them.
func (d *DB) InsertReviewStepRoundWithReplayConfig(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, replayConfigJSON []byte, durationMS int64) (*StepRound, error) {
	return d.insertReviewStepRoundWithConfig(stepResultID, round, trigger, findingsJSON, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA, replayConfigJSON, nil, nil, durationMS)
}

func (d *DB) insertReviewStepRoundWithConfig(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, replayConfigJSON, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	var reviewed, starting, trusted *string
	if reviewedHeadSHA != "" {
		reviewed = &reviewedHeadSHA
	}
	if startingHeadSHA != "" {
		starting = &startingHeadSHA
	}
	if trustedConfigSHA != "" {
		trusted = &trustedConfigSHA
	}
	return d.insertStepRound(stepResultID, round, trigger, RoundStatusCompleted, findingsJSON, fixSummary, reviewed, starting, trusted, replayConfigJSON, globalConfigYAML, repoConfigYAML, durationMS)
}

func (d *DB) insertStepRound(stepResultID string, round int, trigger, status string, findingsJSON *string, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA *string, replayConfigJSON, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	var triggerProvenance *string
	if trigger == RoundTriggerUserFix {
		trigger = RoundTriggerAutoFix
		legacy := RoundTriggerProvenanceLegacyUserFix
		triggerProvenance = &legacy
	}
	r := &StepRound{
		ID:                newID(),
		StepResultID:      stepResultID,
		Round:             round,
		Trigger:           trigger,
		TriggerProvenance: triggerProvenance,
		Status:            status,
		FindingsJSON:      findingsJSON,
		ReviewedHeadSHA:   reviewedHeadSHA,
		StartingHeadSHA:   startingHeadSHA,
		TrustedConfigSHA:  trustedConfigSHA,
		ReplayConfigJSON:  append([]byte(nil), replayConfigJSON...),
		GlobalConfigYAML:  append([]byte(nil), globalConfigYAML...),
		RepoConfigYAML:    append([]byte(nil), repoConfigYAML...),
		FixSummary:        fixSummary,
		DurationMS:        durationMS,
		CreatedAt:         now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, status, trigger_provenance, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, replay_config_json, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, resulting_head_sha, evaluated_head_sha, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.StepResultID, r.Round, r.Trigger, r.Status, r.TriggerProvenance, r.FindingsJSON, r.ReviewedHeadSHA, r.StartingHeadSHA, r.TrustedConfigSHA, r.ReplayConfigJSON, r.GlobalConfigYAML, r.RepoConfigYAML, r.UserFindingsJSON, r.SelectedFindingIDs, r.SelectionSource, r.FixSummary, r.ResultingHeadSHA, r.EvaluatedHeadSHA, r.DurationMS, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return r, nil
}

// SetStepRoundSelection records which findings were selected for fix AFTER the
// given round produced its findings, along with whether that selection came
// from the user or auto-fix filtering. Passing nil or an empty string clears
// both columns. Passing DeclinedSelectionJSON with a source records a decision
// that selected nothing.
func (d *DB) SetStepRoundSelection(id string, selectedFindingIDs *string, source string) error {
	if normalized, err := d.roundHasEvaluation(id); err != nil {
		return err
	} else if normalized {
		var selected []string
		if selectedFindingIDs != nil && strings.TrimSpace(*selectedFindingIDs) != "" {
			if err := json.Unmarshal([]byte(*selectedFindingIDs), &selected); err != nil {
				return fmt.Errorf("set step round selection: decode selected finding IDs: %w", err)
			}
		}
		if len(selected) == 0 && (selectedFindingIDs == nil || strings.TrimSpace(*selectedFindingIDs) == "") {
			return d.clearStructuredRoundDecision(id)
		}
		return d.setStructuredDecisionByExternalIDs(id, selected, source, nil, false)
	}
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, id,
	); err != nil {
		return fmt.Errorf("set step round selection: %w", err)
	}
	return nil
}

// SetStepRoundDeclined records that a human resolved this round's approval
// gate without selecting any finding to fix. It never overwrites an existing
// selection.
func (d *DB) SetStepRoundDeclined(id string) error {
	return d.SetStepRoundWaived(id)
}

func (d *DB) SetStepRoundWaived(id string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("set step round declined: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := setStepRoundWaivedTx(tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set step round declined: commit: %w", err)
	}
	return nil
}

func setStepRoundWaivedTx(tx *sql.Tx, id string) error {
	return setStepRoundExplicitEmptyDecisionTx(tx, id, RoundSelectionSourceUserWaived)
}

func setStepRoundExplicitEmptyDecisionTx(tx *sql.Tx, id, source string) error {
	if !validRoundDecisionSource(source) {
		return fmt.Errorf("set step round decision: invalid source %q", source)
	}
	evaluation, err := getRoundEvaluation(tx, id)
	if err != nil {
		return err
	}
	if evaluation != nil {
		var existingDecisionID string
		err := tx.QueryRow(`SELECT id FROM round_decisions WHERE round_id = ?`, id).Scan(&existingDecisionID)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("set step round decision: load existing decision: %w", err)
		}
		return setStructuredDecisionByExternalIDsTx(tx, id, nil, source, nil, true)
	}
	declined := DeclinedSelectionJSON
	if _, err := tx.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?
		  WHERE id = ? AND selection_source IS NULL`,
		declined, source, id,
	); err != nil {
		return fmt.Errorf("set step round decision: %w", err)
	}
	return nil
}

func (d *DB) SetStepRoundUserDecision(id string, selectedFindingIDs *string, source string, userFindingsJSON *string) error {
	if normalized, err := d.roundHasEvaluation(id); err != nil {
		return err
	} else if normalized {
		var selected []string
		if selectedFindingIDs != nil && strings.TrimSpace(*selectedFindingIDs) != "" {
			if err := json.Unmarshal([]byte(*selectedFindingIDs), &selected); err != nil {
				return fmt.Errorf("set step round user decision: decode selected finding IDs: %w", err)
			}
		}
		return d.setStructuredDecisionByExternalIDs(id, selected, source, userFindingsJSON, false)
	}
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user decision: %w", err)
	}
	return nil
}

// PersistStepRoundFixDecisionAndMarkStepFixing records the decision that
// authorizes a user-requested repair and exposes the step as fixing in one
// transaction. A repair must never begin from a decision that is absent or
// only partly stored.
func (d *DB) PersistStepRoundFixDecisionAndMarkStepFixing(stepResultID, roundID string, selectedFindingIDs *string, source string, userFindingsJSON *string) error {
	return d.persistStepRoundFixDecisionAndMarkStepFixing(stepResultID, roundID, selectedFindingIDs, source, userFindingsJSON, nil)
}

// PersistStepRoundAutoFixDecisionAndMarkStepFixing atomically records the
// automatic repair decision, its attempted audit receipt, and the fixing
// transition that authorizes the next repair invocation. Normalized rounds
// receive graph records; legacy rounds keep both values in step_rounds.
func (d *DB) PersistStepRoundAutoFixDecisionAndMarkStepFixing(stepResultID, roundID string, selectedFindingIDs *string, attemptedRepair StepRoundRepair) error {
	if selectedFindingIDs == nil || strings.TrimSpace(*selectedFindingIDs) == "" || strings.TrimSpace(*selectedFindingIDs) == DeclinedSelectionJSON {
		return fmt.Errorf("persist step round fix decision: automatic repair requires selected findings")
	}
	return d.persistStepRoundFixDecisionAndMarkStepFixing(stepResultID, roundID, selectedFindingIDs, RoundSelectionSourceAutoFix, nil, &attemptedRepair)
}

func (d *DB) persistStepRoundFixDecisionAndMarkStepFixing(stepResultID, roundID string, selectedFindingIDs *string, source string, userFindingsJSON *string, attemptedRepair *StepRoundRepair) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("persist step round fix decision: begin transaction: %w", err)
	}
	defer tx.Rollback()

	if !validRoundDecisionSource(source) {
		return fmt.Errorf("persist step round fix decision: invalid source %q", source)
	}
	var runID string
	if err := tx.QueryRow(`SELECT s.run_id FROM step_rounds r JOIN step_results s ON s.id = r.step_result_id WHERE r.id = ? AND r.step_result_id = ?`, roundID, stepResultID).Scan(&runID); err != nil {
		return fmt.Errorf("persist step round fix decision: load round: %w", err)
	}

	var selected []string
	if selectedFindingIDs != nil && strings.TrimSpace(*selectedFindingIDs) != "" {
		if err := json.Unmarshal([]byte(*selectedFindingIDs), &selected); err != nil {
			return fmt.Errorf("persist step round fix decision: decode selected finding IDs: %w", err)
		}
	}
	evaluation, err := getRoundEvaluation(tx, roundID)
	if err != nil {
		return err
	}
	if evaluation != nil {
		if err := setStructuredDecisionByExternalIDsTx(tx, roundID, selected, source, userFindingsJSON, false); err != nil {
			return err
		}
	} else {
		var selectionSource *string
		if selectedFindingIDs != nil && strings.TrimSpace(*selectedFindingIDs) != "" {
			selectionSource = &source
		}
		if _, err := tx.Exec(
			`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ? WHERE id = ?`,
			selectedFindingIDs, selectionSource, userFindingsJSON, roundID,
		); err != nil {
			return fmt.Errorf("persist step round fix decision: update legacy decision: %w", err)
		}
	}
	if attemptedRepair != nil {
		if source != RoundSelectionSourceAutoFix || attemptedRepair.FailureFingerprint == nil || attemptedRepair.Result == nil || *attemptedRepair.Result != RoundRepairAttempted {
			return fmt.Errorf("persist step round fix decision: automatic repair audit must record an attempted fingerprint")
		}
		if attemptedRepair.RoundID == "" {
			attemptedRepair.RoundID = roundID
		}
		if attemptedRepair.RunID == "" {
			attemptedRepair.RunID = runID
		}
		if attemptedRepair.RoundID != roundID || attemptedRepair.RunID != runID {
			return fmt.Errorf("persist step round fix decision: automatic repair audit does not belong to round")
		}
		if attemptedRepair.ID == "" {
			attemptedRepair.ID = newID()
		}
		if attemptedRepair.CreatedAt == 0 {
			attemptedRepair.CreatedAt = now()
		}
		if evaluation != nil {
			if err := upsertRoundRepair(tx, *attemptedRepair); err != nil {
				return fmt.Errorf("persist step round fix decision: insert automatic repair audit: %w", err)
			}
		} else {
			// Refresh and Push rounds intentionally stay on the legacy receipt
			// path. Keep their repair audit in step_rounds as well; inserting a
			// normalized round_repairs row would create an orphan graph record.
			if _, err := tx.Exec(
				`UPDATE step_rounds SET repair_failure_fingerprint = ?, repair_result = ? WHERE id = ?`,
				attemptedRepair.FailureFingerprint, attemptedRepair.Result, roundID,
			); err != nil {
				return fmt.Errorf("persist step round fix decision: update legacy repair audit: %w", err)
			}
		}
	}
	result, err := tx.Exec(
		`UPDATE step_results SET status = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`,
		types.StepStatusFixing, now(), fmt.Sprintf("status: %s", types.StepStatusFixing), stepResultID,
	)
	if err != nil {
		return fmt.Errorf("persist step round fix decision: mark step fixing: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("persist step round fix decision: mark step fixing rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("persist step round fix decision: expected one step result, updated %d", changed)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persist step round fix decision: commit: %w", err)
	}
	return nil
}

// SetStepRoundSelectedFindingIDs preserves the old API for callers that do not
// need to distinguish how the selection was made.
func (d *DB) SetStepRoundSelectedFindingIDs(id string, selectedFindingIDs *string) error {
	return d.SetStepRoundSelection(id, selectedFindingIDs, RoundSelectionSourceUser)
}

// SetStepRoundUserFindings records the merged finding list (with user
// instructions attached and user-added findings appended) that was
// dispatched to the fix agent for the round. Passing nil clears the column.
func (d *DB) SetStepRoundUserFindings(id string, userFindingsJSON *string) error {
	if normalized, err := d.roundHasEvaluation(id); err != nil {
		return err
	} else if normalized {
		decision, err := d.GetRoundDecision(id)
		if err != nil {
			return err
		}
		if decision == nil {
			return fmt.Errorf("set step round user findings: normalized round has no decision")
		}
		selected := make([]string, 0, len(decision.Findings))
		for _, finding := range orderedSelectedDecisionFindings(decision) {
			selected = append(selected, finding.FindingID)
		}
		return d.setStructuredDecisionByExternalIDs(id, selected, decision.Source, userFindingsJSON, decision.ExplicitEmpty)
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET user_findings_json = ? WHERE id = ?`,
		userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user findings: %w", err)
	}
	return nil
}

// SetStepRoundRepairAudit records privacy-safe progress facts for one round.
// Empty values clear their columns.
func (d *DB) SetStepRoundRepairAudit(id, failureFingerprint, result string) error {
	if normalized, err := d.roundHasEvaluation(id); err != nil {
		return err
	} else if normalized {
		return d.SetStepRoundStructuredRepair(StepRoundRepair{RoundID: id, FailureFingerprint: optionalString(failureFingerprint), Result: optionalString(result)})
	}
	var fingerprintValue, resultValue *string
	if failureFingerprint != "" {
		fingerprintValue = &failureFingerprint
	}
	if result != "" {
		resultValue = &result
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET repair_failure_fingerprint = ?, repair_result = ? WHERE id = ?`,
		fingerprintValue, resultValue, id,
	); err != nil {
		return fmt.Errorf("set step round repair audit: %w", err)
	}
	return nil
}

// GetRoundsByStep returns all rounds for a step result, ordered by round number.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT id, step_result_id, round, trigger_type, status, trigger_provenance, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, replay_config_json, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, repair_failure_fingerprint, repair_result, resulting_head_sha, evaluated_head_sha, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get rounds by step: %w", err)
	}
	var rounds []*StepRound
	for rows.Next() {
		r := &StepRound{}
		if err := rows.Scan(&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.Status, &r.TriggerProvenance, &r.FindingsJSON, &r.ReviewedHeadSHA, &r.StartingHeadSHA, &r.TrustedConfigSHA, &r.ReplayConfigJSON, &r.GlobalConfigYAML, &r.RepoConfigYAML, &r.UserFindingsJSON, &r.SelectedFindingIDs, &r.SelectionSource, &r.FixSummary, &r.RepairFailureFingerprint, &r.RepairResult, &r.ResultingHeadSHA, &r.EvaluatedHeadSHA, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		if r.Trigger == RoundTriggerUserFix {
			legacy := RoundTriggerProvenanceLegacyUserFix
			r.Trigger = RoundTriggerAutoFix
			r.TriggerProvenance = &legacy
		}
		rounds = append(rounds, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, round := range rounds {
		if err := d.hydrateRoundGraph(round); err != nil {
			return nil, err
		}
	}
	return rounds, nil
}
