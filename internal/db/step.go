package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepResult represents the result of a pipeline step execution.
type StepResult struct {
	ID             string
	RunID          string
	StepName       types.StepName
	StepOrder      int
	Status         types.StepStatus
	SkipSource     *string
	ExitCode       *int
	DurationMS     *int64
	LogPath        *string
	FindingsJSON   *string
	EvidenceJSON   *string
	PlannedCommand *string
	Error          *string
	StartedAt      *int64
	CompletedAt    *int64
	LastActivityAt *int64
	LastActivity   *string
	AgentPID       *int
	AutoFixLimit   *int
}

const stepResultColumns = `id, run_id, step_name, step_order, status, skip_source, exit_code, duration_ms, log_path, findings_json, evidence_json, planned_command, error, started_at, completed_at, last_activity_at, last_activity, agent_pid, auto_fix_limit`

const MaxStepEvidenceBytes = 64 * 1024

const MaxPlannedCommandBytes = 64 * 1024

const (
	CommandOutcomePassed = "passed"
	CommandOutcomeFailed = "failed"
	CommandOutcomeError  = "error"
)

// StepEvidence is bounded, structured evidence retained for PR formatters.
type StepEvidence struct {
	Commands    []CommandEvidence `json:"commands,omitempty"`
	Evidence    []string          `json:"evidence,omitempty"`
	Intent      *IntentEvidence   `json:"intent,omitempty"`
	Explanation string            `json:"explanation,omitempty"`
}

type CommandEvidence struct {
	Round         int                `json:"round"`
	Sequence      int                `json:"sequence"`
	Command       string             `json:"command"`
	Outcome       string             `json:"outcome"`
	ExitCode      *int               `json:"exit_code"`
	CommandSource string             `json:"command_source,omitempty"`
	Runner        *runner.Provenance `json:"runner,omitempty"`
}

type IntentEvidence struct {
	Text     string               `json:"text,omitempty"`
	Source   string               `json:"source,omitempty"`
	Provided bool                 `json:"provided"`
	Reason   *IntentAbsenceReason `json:"reason,omitempty"`
}

type IntentAbsenceReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func EncodeStepEvidence(evidence StepEvidence) (string, error) {
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("encode step evidence: %w", err)
	}
	if len(encoded) > MaxStepEvidenceBytes {
		return "", fmt.Errorf("step evidence exceeds %d bytes", MaxStepEvidenceBytes)
	}
	return string(encoded), nil
}

func (s *StepResult) Evidence() (StepEvidence, error) {
	if s == nil || s.EvidenceJSON == nil {
		return StepEvidence{}, nil
	}
	var evidence StepEvidence
	if err := json.Unmarshal([]byte(*s.EvidenceJSON), &evidence); err != nil {
		return StepEvidence{}, fmt.Errorf("decode step evidence: %w", err)
	}
	return evidence, nil
}

// InsertStepResult creates a new step result record.
func (d *DB) InsertStepResult(runID string, stepName types.StepName) (*StepResult, error) {
	s := &StepResult{
		ID:        newID(),
		RunID:     runID,
		StepName:  stepName,
		StepOrder: stepName.Order(),
		Status:    types.StepStatusPending,
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.RunID, s.StepName, s.StepOrder, s.Status,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step result: %w", err)
	}
	return s, nil
}

// GetStepResult returns a step result by ID.
func (d *DB) GetStepResult(id string) (*StepResult, error) {
	s := &StepResult{}
	err := d.sql.QueryRow(
		`SELECT `+stepResultColumns+` FROM step_results WHERE id = ?`, id,
	).Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.SkipSource, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.EvidenceJSON, &s.PlannedCommand, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get step result: %w", err)
	}
	return s, nil
}

// GetStepsByRun returns all step results for a run, in execution order.
func (d *DB) GetStepsByRun(runID string) ([]*StepResult, error) {
	rows, err := d.sql.Query(
		`SELECT `+stepResultColumns+` FROM step_results WHERE run_id = ? ORDER BY step_order`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get steps by run: %w", err)
	}
	defer rows.Close()
	var steps []*StepResult
	for rows.Next() {
		s := &StepResult{}
		if err := rows.Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.SkipSource, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.EvidenceJSON, &s.PlannedCommand, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit); err != nil {
			return nil, fmt.Errorf("scan step result: %w", err)
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

func (d *DB) SetStepEvidence(id string, evidence StepEvidence) error {
	encoded, err := EncodeStepEvidence(evidence)
	if err != nil {
		return err
	}
	if _, err := d.sql.Exec(`UPDATE step_results SET evidence_json = ? WHERE id = ?`, encoded, id); err != nil {
		return fmt.Errorf("set step evidence: %w", err)
	}
	return nil
}

// SetStepPlannedCommand retains the private, unredacted command that an
// unconfigured validation step must reuse after a daemon restart. Public PR
// evidence remains separately redacted and bounded by RecordCommand.
func (d *DB) SetStepPlannedCommand(id, command string) error {
	if len(command) > MaxPlannedCommandBytes {
		return fmt.Errorf("planned command exceeds %d bytes", MaxPlannedCommandBytes)
	}
	if _, err := d.sql.Exec(`UPDATE step_results SET planned_command = ? WHERE id = ?`, command, id); err != nil {
		return fmt.Errorf("set step planned command: %w", err)
	}
	return nil
}

func (d *DB) AppendStepCommandEvidence(id string, command CommandEvidence) error {
	step, err := d.GetStepResult(id)
	if err != nil {
		return err
	}
	if step == nil {
		return fmt.Errorf("append step command evidence: step not found")
	}
	evidence, err := step.Evidence()
	if err != nil {
		return err
	}
	evidence.Commands = append(evidence.Commands, command)
	return d.SetStepEvidence(id, evidence)
}

// AppendStepEvidenceNote records one non-shell observation for a step, for
// work the pipeline verified but did not itself execute as a command.
func (d *DB) AppendStepEvidenceNote(id, note string) error {
	step, err := d.GetStepResult(id)
	if err != nil {
		return err
	}
	if step == nil {
		return fmt.Errorf("append step evidence note: step not found")
	}
	evidence, err := step.Evidence()
	if err != nil {
		return err
	}
	evidence.Evidence = append(evidence.Evidence, note)
	return d.SetStepEvidence(id, evidence)
}

// SetStepEvidenceExplanation records why a step produced no command or
// evidence output, without discarding anything already recorded.
func (d *DB) SetStepEvidenceExplanation(id, explanation string) error {
	step, err := d.GetStepResult(id)
	if err != nil {
		return err
	}
	if step == nil {
		return fmt.Errorf("set step evidence explanation: step not found")
	}
	evidence, err := step.Evidence()
	if err != nil {
		return err
	}
	evidence.Explanation = explanation
	return d.SetStepEvidence(id, evidence)
}

// UpdateStepStatus updates a step's status.
func (d *DB) UpdateStepStatus(id string, status types.StepStatus) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status: %w", err)
	}
	return nil
}

// UpdateStepStatusWithDuration updates a step's status and execution duration together.
func (d *DB) UpdateStepStatusWithDuration(id string, status types.StepStatus, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, duration_ms = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, durationMS, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status with duration: %w", err)
	}
	return nil
}

func (d *DB) ParkStepForApproval(runID, stepID string, status types.StepStatus, durationMS int64, findingsJSON *string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin approval park: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	stepResult, err := tx.Exec(
		`UPDATE step_results SET status = ?, duration_ms = ?, findings_json = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`,
		status, durationMS, findingsJSON, ts, fmt.Sprintf("status: %s", status), stepID,
	)
	if err != nil {
		return fmt.Errorf("park step for approval: %w", err)
	}
	changed, err := stepResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("park step for approval rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("park step for approval: updated %d rows", changed)
	}
	runResult, err := tx.Exec(
		`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`,
		ts, ts, runID,
	)
	if err != nil {
		return fmt.Errorf("mark run awaiting approval: %w", err)
	}
	changed, err = runResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark run awaiting approval rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("mark run awaiting approval: updated %d rows", changed)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval park: %w", err)
	}
	return nil
}

// StartStep marks a step as running with a started_at timestamp.
func (d *DB) StartStep(id string) error {
	return d.StartStepWithAutoFixLimit(id, 0)
}

// StartStepWithAutoFixLimit marks a step as running and records the effective
// auto-fix limit that status surfaces use while the step is active.
func (d *DB) StartStepWithAutoFixLimit(id string, autoFixLimit int) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, started_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL, auto_fix_limit = ? WHERE id = ?`, types.StepStatusRunning, ts, ts, "step started", autoFixLimitDBValue(autoFixLimit), id)
	if err != nil {
		return fmt.Errorf("start step: %w", err)
	}
	return nil
}

func (d *DB) SetStepAutoFixLimit(id string, autoFixLimit int) error {
	if _, err := d.sql.Exec(`UPDATE step_results SET auto_fix_limit = ? WHERE id = ?`, autoFixLimitDBValue(autoFixLimit), id); err != nil {
		return fmt.Errorf("set step auto-fix limit: %w", err)
	}
	return nil
}

func autoFixLimitDBValue(autoFixLimit int) any {
	if autoFixLimit <= 0 {
		return nil
	}
	return autoFixLimit
}

// CompleteStep marks a step as completed with timing and result info.
func (d *DB) CompleteStep(id string, exitCode int, durationMS int64, logPath string) error {
	return d.CompleteStepWithStatus(id, types.StepStatusCompleted, exitCode, durationMS, logPath)
}

// CompleteStepWithStatus marks a step as finished with timing and result info.
func (d *DB) CompleteStepWithStatus(id string, status types.StepStatus, exitCode int, durationMS int64, logPath string) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		status, exitCode, durationMS, logPath, now(), now(), fmt.Sprintf("status: %s", status), id,
	)
	if err != nil {
		return fmt.Errorf("complete step: %w", err)
	}
	return nil
}

// CompleteStepAsSkipped persists a source-aware pre-run skip receipt.
func (d *DB) CompleteStepAsSkipped(id string, source types.SkipSource) error {
	if !source.Valid() {
		return fmt.Errorf("complete skipped step: unsupported skip source %q", source)
	}
	ts := now()
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, skip_source = ?, exit_code = 0, duration_ms = 0, log_path = '', completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		types.StepStatusSkipped, source, ts, ts, fmt.Sprintf("status: %s", types.StepStatusSkipped), id,
	)
	if err != nil {
		return fmt.Errorf("complete skipped step: %w", err)
	}
	return nil
}

// CompleteReviewStep atomically completes a successful review and replaces
// the run's exact review-approved head. Neither write survives if the other
// fails, so a failed completion cannot create approval authority and a
// completed review cannot lack it.
func (d *DB) CompleteReviewStep(id, runID, approvedHeadSHA string, exitCode int, durationMS int64, logPath string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin complete review step: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	result, err := tx.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		types.StepStatusCompleted, exitCode, durationMS, logPath, ts, ts, fmt.Sprintf("status: %s", types.StepStatusCompleted), id,
	)
	if err != nil {
		return fmt.Errorf("complete review step: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("complete review step: step row not found")
	}
	result, err = tx.Exec(`UPDATE runs SET review_approved_head_sha = ?, updated_at = ? WHERE id = ?`, approvedHeadSHA, ts, runID)
	if err != nil {
		return fmt.Errorf("record review-approved head: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("record review-approved head: run row not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completed review: %w", err)
	}
	return nil
}

// FailStep marks a step as failed with an error message and duration.
func (d *DB) FailStep(id string, errMsg string, durationMS int64) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, error = ?, duration_ms = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		types.StepStatusFailed, errMsg, durationMS, now(), now(), "step failed: "+errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("fail step: %w", err)
	}
	return nil
}

// TouchStepActivity records the latest meaningful activity for an active step
// without changing its status or current agent pid.
func (d *DB) TouchStepActivity(id string, text string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ? WHERE id = ?`, now(), text, id)
	if err != nil {
		return fmt.Errorf("touch step activity: %w", err)
	}
	return nil
}

// SetStepAgentActivity records an agent lifecycle activity and replaces the
// active agent pid. Passing nil clears the pid after the process exits.
func (d *DB) SetStepAgentActivity(id string, text string, agentPID *int) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ?, agent_pid = ? WHERE id = ?`, now(), text, agentPID, id)
	if err != nil {
		return fmt.Errorf("set step agent activity: %w", err)
	}
	return nil
}

// SetStepDuration sets the execution-only duration on a step result.
func (d *DB) SetStepDuration(id string, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET duration_ms = ? WHERE id = ?`, durationMS, id)
	if err != nil {
		return fmt.Errorf("set step duration: %w", err)
	}
	return nil
}

// SetStepFindings sets the findings JSON on a step result.
func (d *DB) SetStepFindings(id string, findingsJSON string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = ? WHERE id = ?`, findingsJSON, id)
	if err != nil {
		return fmt.Errorf("set step findings: %w", err)
	}
	return nil
}

// ClearStepFindings removes any stored findings JSON from a step result.
func (d *DB) ClearStepFindings(id string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear step findings: %w", err)
	}
	return nil
}
