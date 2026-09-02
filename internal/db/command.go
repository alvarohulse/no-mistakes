package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runner"
)

const (
	CommandObserverController      = "controller"
	CommandDefinitionSourcePlanned = "planned"

	CommandOutcomePass         = "pass"
	CommandOutcomeFail         = "fail"
	CommandOutcomeProcessError = "process_error"
	CommandOutcomeCancelled    = "cancelled"
	CommandOutcomeTimeout      = "timeout"

	CommandRetryReasonUnchangedAfterRepair = "unchanged_after_repair"
)

// CommandDefinition is the exact resolved command and portable runner identity
// used by a run. ID excludes the resolved executable path and runner version so
// the same portable command retains one identity across machines.
type CommandDefinition struct {
	ID               string
	RunID            string
	Script           string
	Platform         string
	RunnerExecutable string
	RunnerArgs       []string
}

// CommandAttempt is one controller-observed execution of a command definition.
// A nil Outcome marks a process that started but whose completion was not
// durably observed, such as a daemon crash.
type CommandAttempt struct {
	ID                  string
	RunID               string
	CommandID           string
	StepID              string
	RoundID             string
	Sequence            int
	Purpose             string
	Observer            string
	Trigger             string
	BeforeSHA           string
	TestedSHA           *string
	CommandSource       string
	RunnerSchemaVersion int
	RunnerSource        string
	RunnerVersion       *string
	InputStateID        *string
	ResultStateID       *string
	StartedAt           int64
	CompletedAt         *int64
	DurationMS          *int64
	Outcome             *string
	ExitCode            *int
	Signal              *string
	RetryOfAttemptID    *string
	RetryReason         *string
}

type commandIdentity struct {
	Script           string   `json:"script"`
	Platform         string   `json:"platform"`
	RunnerExecutable string   `json:"runner_executable"`
	RunnerArgs       []string `json:"runner_args"`
}

func commandDefinitionID(resolved runner.Resolved) (string, error) {
	identity, err := json.Marshal(commandIdentity{
		Script:           resolved.Script,
		Platform:         resolved.Provenance.Platform,
		RunnerExecutable: resolved.Provenance.Executable,
		RunnerArgs:       resolved.Provenance.Args,
	})
	if err != nil {
		return "", fmt.Errorf("encode command definition identity: %w", err)
	}
	digest := sha256.Sum256(identity)
	return "cmd_" + hex.EncodeToString(digest[:]), nil
}

// EnsureCommandDefinition stores a command once per run and returns its stable
// semantic identity. Exact repetitions intentionally share the definition;
// their executions remain separate CommandAttempt rows.
func (d *DB) EnsureCommandDefinition(runID string, resolved runner.Resolved) (*CommandDefinition, error) {
	id, err := commandDefinitionID(resolved)
	if err != nil {
		return nil, err
	}
	argsJSON, err := json.Marshal(resolved.Provenance.Args)
	if err != nil {
		return nil, fmt.Errorf("encode command runner arguments: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT OR IGNORE INTO command_definitions
		 (run_id, id, script, platform, runner_executable, runner_args_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		runID, id, resolved.Script, resolved.Provenance.Platform,
		resolved.Provenance.Executable, string(argsJSON),
	)
	if err != nil {
		return nil, fmt.Errorf("ensure command definition: %w", err)
	}
	return d.getCommandDefinition(runID, id)
}

func (d *DB) getCommandDefinition(runID, id string) (*CommandDefinition, error) {
	definition := &CommandDefinition{}
	var argsJSON string
	if err := d.sql.QueryRow(
		`SELECT id, run_id, script, platform, runner_executable, runner_args_json
		 FROM command_definitions WHERE run_id = ? AND id = ?`, runID, id,
	).Scan(
		&definition.ID, &definition.RunID, &definition.Script, &definition.Platform,
		&definition.RunnerExecutable, &argsJSON,
	); err != nil {
		return nil, fmt.Errorf("get command definition: %w", err)
	}
	if err := json.Unmarshal([]byte(argsJSON), &definition.RunnerArgs); err != nil {
		return nil, fmt.Errorf("decode command runner arguments: %w", err)
	}
	return definition, nil
}

// GetCommandDefinitionsByRun returns definitions in stable identity order.
func (d *DB) GetCommandDefinitionsByRun(runID string) ([]*CommandDefinition, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, script, platform, runner_executable, runner_args_json
		 FROM command_definitions WHERE run_id = ? ORDER BY id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get command definitions by run: %w", err)
	}
	defer rows.Close()
	var definitions []*CommandDefinition
	for rows.Next() {
		definition := &CommandDefinition{}
		var argsJSON string
		if err := rows.Scan(
			&definition.ID, &definition.RunID, &definition.Script, &definition.Platform,
			&definition.RunnerExecutable, &argsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan command definition: %w", err)
		}
		if err := json.Unmarshal([]byte(argsJSON), &definition.RunnerArgs); err != nil {
			return nil, fmt.Errorf("decode command runner arguments: %w", err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, rows.Err()
}

// StartCommandAttempt persists the execution identity before launching the
// command. Completion fields are filled exactly once by CompleteCommandAttempt.
func (d *DB) StartCommandAttempt(attempt CommandAttempt) (*CommandAttempt, error) {
	if err := validateCommandAttemptStart(d, attempt); err != nil {
		return nil, err
	}
	attempt.ID = newID()
	attempt.StartedAt = time.Now().UnixMilli()
	_, err := d.sql.Exec(
		`INSERT INTO command_attempts
		 (id, run_id, command_id, step_id, round_id, sequence, purpose, observer, trigger_type, before_sha, tested_sha,
		  command_source, runner_schema_version, runner_source, runner_version, input_state_id, result_state_id,
		  started_at, retry_of_attempt_id, retry_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.ID, attempt.RunID, attempt.CommandID, attempt.StepID, attempt.RoundID,
		attempt.Sequence, attempt.Purpose, attempt.Observer, attempt.Trigger,
		attempt.BeforeSHA, attempt.TestedSHA, attempt.CommandSource, attempt.RunnerSchemaVersion,
		attempt.RunnerSource, attempt.RunnerVersion, attempt.InputStateID, attempt.ResultStateID, attempt.StartedAt,
		attempt.RetryOfAttemptID, attempt.RetryReason,
	)
	if err != nil {
		return nil, fmt.Errorf("start command attempt: %w", err)
	}
	return &attempt, nil
}

func validateCommandAttemptStart(d *DB, attempt CommandAttempt) error {
	if attempt.RunID == "" || attempt.CommandID == "" || attempt.StepID == "" || attempt.RoundID == "" || attempt.Sequence < 1 || strings.TrimSpace(attempt.Purpose) == "" || attempt.Observer == "" || attempt.Trigger == "" || attempt.BeforeSHA == "" || attempt.CommandSource == "" || attempt.RunnerSchemaVersion < 1 || attempt.RunnerSource == "" {
		return fmt.Errorf("start command attempt: required identity is incomplete")
	}
	if attempt.TestedSHA != nil || attempt.ResultStateID != nil {
		return fmt.Errorf("start command attempt: completion identity must be empty")
	}
	var owned int
	if err := d.sql.QueryRow(
		`SELECT count(*) FROM step_rounds sr
		 JOIN step_results s ON s.id = sr.step_result_id
		 WHERE sr.id = ? AND s.id = ? AND s.run_id = ?`,
		attempt.RoundID, attempt.StepID, attempt.RunID,
	).Scan(&owned); err != nil {
		return fmt.Errorf("validate command attempt ownership: %w", err)
	}
	if owned != 1 {
		return fmt.Errorf("start command attempt: step and round do not belong to run")
	}
	if attempt.RetryOfAttemptID == nil {
		if attempt.RetryReason != nil {
			return fmt.Errorf("start command attempt: retry reason requires retry attempt")
		}
		return nil
	}
	if attempt.RetryReason == nil || strings.TrimSpace(*attempt.RetryReason) == "" {
		return fmt.Errorf("start command attempt: retry attempt requires reason")
	}
	prior, err := d.getCommandAttempt(*attempt.RetryOfAttemptID)
	if err != nil {
		return fmt.Errorf("start command attempt: load retry attempt: %w", err)
	}
	if prior.CompletedAt == nil || prior.Outcome == nil {
		return fmt.Errorf("start command attempt: retry attempt is incomplete")
	}
	if !RetryableCommandOutcome(*prior.Outcome) {
		return fmt.Errorf("start command attempt: prior attempt outcome is not retryable: %q", *prior.Outcome)
	}
	if !validCommandRetryReason(*attempt.RetryReason) {
		return fmt.Errorf("start command attempt: unsupported retry reason %q", *attempt.RetryReason)
	}
	if prior.RunID != attempt.RunID || prior.CommandID != attempt.CommandID || prior.StepID != attempt.StepID || prior.Purpose != attempt.Purpose || prior.Observer != attempt.Observer || prior.CommandSource != attempt.CommandSource || prior.RunnerSchemaVersion != attempt.RunnerSchemaVersion || prior.RunnerSource != attempt.RunnerSource || !sameOptionalString(prior.RunnerVersion, attempt.RunnerVersion) {
		return fmt.Errorf("start command attempt: retry must keep the same operation and input")
	}
	if prior.BeforeSHA != attempt.BeforeSHA {
		return fmt.Errorf("start command attempt: retry requires unchanged subject")
	}
	if prior.InputStateID == nil || prior.ResultStateID == nil || attempt.InputStateID == nil ||
		*prior.InputStateID != *prior.ResultStateID || *prior.ResultStateID != *attempt.InputStateID {
		return fmt.Errorf("start command attempt: retry requires unchanged input state")
	}
	return nil
}

func RetryableCommandOutcome(outcome string) bool {
	switch outcome {
	case CommandOutcomeFail, CommandOutcomeProcessError, CommandOutcomeCancelled, CommandOutcomeTimeout:
		return true
	default:
		return false
	}
}

func validCommandRetryReason(reason string) bool {
	return reason == CommandRetryReasonUnchangedAfterRepair
}

func sameOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func OptionalStringsEqual(left, right *string) bool {
	return sameOptionalString(left, right)
}

// CompleteCommandAttempt stores the controller-observed terminal result once.
// testedSHA is accepted only when the exact clean repository state observed
// before and after execution is unchanged.
func (d *DB) CompleteCommandAttempt(id, outcome string, exitCode *int, signal, resultStateID, testedSHA *string) error {
	if !validCommandOutcome(outcome) {
		return fmt.Errorf("complete command attempt: invalid outcome %q", outcome)
	}
	if signal != nil && exitCode != nil {
		return fmt.Errorf("complete command attempt: exit code and signal are mutually exclusive")
	}
	if outcome == CommandOutcomePass && (exitCode == nil || *exitCode != 0 || signal != nil) {
		return fmt.Errorf("complete command attempt: passing outcome requires exit code zero")
	}
	if outcome == CommandOutcomeFail && exitCode == nil && signal == nil {
		return fmt.Errorf("complete command attempt: failing outcome requires exit code or signal")
	}
	attempt, err := d.getCommandAttempt(id)
	if err != nil {
		return fmt.Errorf("complete command attempt: %w", err)
	}
	if testedSHA != nil {
		if attempt.InputStateID == nil || resultStateID == nil || *attempt.InputStateID != *resultStateID || *testedSHA != attempt.BeforeSHA {
			return fmt.Errorf("complete command attempt: tested commit requires unchanged input state")
		}
	}
	completedAt := time.Now().UnixMilli()
	result, err := d.sql.Exec(
		`UPDATE command_attempts
		 SET completed_at = ?, duration_ms = MAX(0, ? - started_at), outcome = ?, exit_code = ?, signal = ?, result_state_id = ?, tested_sha = ?
		 WHERE id = ? AND completed_at IS NULL`,
		completedAt, completedAt, outcome, exitCode, signal, resultStateID, testedSHA, id,
	)
	if err != nil {
		return fmt.Errorf("complete command attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete command attempt: rows affected: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("complete command attempt: attempt is missing or already complete")
	}
	return nil
}

func validCommandOutcome(outcome string) bool {
	switch outcome {
	case CommandOutcomePass, CommandOutcomeFail, CommandOutcomeProcessError, CommandOutcomeCancelled, CommandOutcomeTimeout:
		return true
	default:
		return false
	}
}

func (d *DB) getCommandAttempt(id string) (*CommandAttempt, error) {
	attempt := &CommandAttempt{}
	if err := scanCommandAttempt(d.sql.QueryRow(
		`SELECT id, run_id, command_id, step_id, round_id, sequence, purpose, observer, trigger_type, before_sha, tested_sha,
		        command_source, runner_schema_version, runner_source, runner_version, input_state_id, result_state_id,
		        started_at, completed_at, duration_ms, outcome, exit_code, signal, retry_of_attempt_id, retry_reason
		 FROM command_attempts WHERE id = ?`, id,
	), attempt); err != nil {
		return nil, fmt.Errorf("get command attempt: %w", err)
	}
	return attempt, nil
}

// GetCommandAttemptsByRun returns every execution in durable pipeline order.
// Identical commands are never collapsed.
func (d *DB) GetCommandAttemptsByRun(runID string) ([]*CommandAttempt, error) {
	rows, err := d.sql.Query(
		`SELECT ca.id, ca.run_id, ca.command_id, ca.step_id, ca.round_id, ca.sequence, ca.purpose, ca.observer, ca.trigger_type, ca.before_sha, ca.tested_sha,
		        ca.command_source, ca.runner_schema_version, ca.runner_source, ca.runner_version, ca.input_state_id, ca.result_state_id,
		        ca.started_at, ca.completed_at, ca.duration_ms, ca.outcome, ca.exit_code, ca.signal, ca.retry_of_attempt_id, ca.retry_reason
		 FROM command_attempts ca
		 JOIN step_results sr ON sr.id = ca.step_id
		 JOIN step_rounds r ON r.id = ca.round_id
		 WHERE ca.run_id = ? ORDER BY sr.step_order, r.round, ca.sequence`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get command attempts by run: %w", err)
	}
	defer rows.Close()
	var attempts []*CommandAttempt
	for rows.Next() {
		attempt := &CommandAttempt{}
		if err := scanCommandAttempt(rows, attempt); err != nil {
			return nil, fmt.Errorf("scan command attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func scanCommandAttempt(row interface{ Scan(...any) error }, attempt *CommandAttempt) error {
	return row.Scan(
		&attempt.ID, &attempt.RunID, &attempt.CommandID, &attempt.StepID,
		&attempt.RoundID, &attempt.Sequence, &attempt.Purpose, &attempt.Observer,
		&attempt.Trigger, &attempt.BeforeSHA, &attempt.TestedSHA,
		&attempt.CommandSource, &attempt.RunnerSchemaVersion, &attempt.RunnerSource, &attempt.RunnerVersion,
		&attempt.InputStateID, &attempt.ResultStateID, &attempt.StartedAt,
		&attempt.CompletedAt, &attempt.DurationMS, &attempt.Outcome, &attempt.ExitCode,
		&attempt.Signal, &attempt.RetryOfAttemptID, &attempt.RetryReason,
	)
}
