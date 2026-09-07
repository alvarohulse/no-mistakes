package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	CommandObserverController      = "controller"
	CommandObserverProvider        = "provider"
	CommandDefinitionSourcePlanned = "planned"
	CommandProofReasonObservedPass = "observed_passing_attempt"

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
	OutputArtifactID    *string
	AcceptedAsProof     bool
	ProofReason         *string
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
// command. Completion fields are filled exactly once with its output artifact.
func (d *DB) StartCommandAttempt(attempt CommandAttempt) (*CommandAttempt, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("start command attempt: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := validateCommandAttemptStart(tx, attempt); err != nil {
		return nil, err
	}
	attempt.ID = newID()
	attempt.StartedAt = time.Now().UnixMilli()
	_, err = tx.Exec(
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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("start command attempt: commit: %w", err)
	}
	return &attempt, nil
}

type commandAttemptQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func validateCommandAttemptStart(q commandAttemptQuerier, attempt CommandAttempt) error {
	if attempt.RunID == "" || attempt.CommandID == "" || attempt.StepID == "" || attempt.RoundID == "" || attempt.Sequence < 1 || strings.TrimSpace(attempt.Purpose) == "" || attempt.Observer == "" || attempt.Trigger == "" || attempt.BeforeSHA == "" || attempt.CommandSource == "" || attempt.RunnerSchemaVersion < 1 || attempt.RunnerSource == "" {
		return fmt.Errorf("start command attempt: required identity is incomplete")
	}
	if attempt.TestedSHA != nil || attempt.ResultStateID != nil {
		return fmt.Errorf("start command attempt: completion identity must be empty")
	}
	var owned int
	if err := q.QueryRow(
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
	prior, err := getCommandAttempt(q, *attempt.RetryOfAttemptID)
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
	var priorRound, currentRound int
	if err := q.QueryRow(
		`SELECT prior.round, current.round
		 FROM step_rounds prior, step_rounds current
		 WHERE prior.id = ? AND current.id = ?`, prior.RoundID, attempt.RoundID,
	).Scan(&priorRound, &currentRound); err != nil {
		return fmt.Errorf("start command attempt: validate retry round order: %w", err)
	}
	if currentRound < priorRound || currentRound == priorRound && attempt.Sequence <= prior.Sequence {
		return fmt.Errorf("start command attempt: retry must follow the prior attempt")
	}
	var latestMatchingID string
	if err := q.QueryRow(
		`SELECT ca.id
		 FROM command_attempts ca
		 JOIN step_rounds sr ON sr.id = ca.round_id
		 WHERE ca.run_id = ? AND ca.command_id = ? AND ca.step_id = ? AND ca.purpose = ? AND ca.observer = ?
		   AND ca.command_source = ? AND ca.runner_schema_version = ? AND ca.runner_source = ?
		   AND ((ca.runner_version IS NULL AND ? IS NULL) OR ca.runner_version = ?)
		 ORDER BY sr.round DESC, ca.sequence DESC
		 LIMIT 1`,
		attempt.RunID, attempt.CommandID, attempt.StepID, attempt.Purpose, attempt.Observer,
		attempt.CommandSource, attempt.RunnerSchemaVersion, attempt.RunnerSource,
		attempt.RunnerVersion, attempt.RunnerVersion,
	).Scan(&latestMatchingID); err != nil {
		return fmt.Errorf("start command attempt: validate latest matching attempt: %w", err)
	}
	if latestMatchingID != prior.ID {
		return fmt.Errorf("start command attempt: retry must reference the immediate prior matching attempt")
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

// CompleteCommandAttemptWithOutputArtifact atomically persists a terminal
// attempt and its only output artifact as execution history. Generic completion
// never accepts an attempt as proof.
func (d *DB) CompleteCommandAttemptWithOutputArtifact(id, outcome string, exitCode *int, signal, resultStateID, testedSHA *string, artifact Artifact) (*Artifact, error) {
	return d.completeCommandAttemptWithOutputArtifact(id, outcome, exitCode, signal, resultStateID, testedSHA, artifact, false)
}

// CompleteControllerCommandAttemptWithOutputArtifact atomically records the
// controller's observed completion and accepts it as proof only when it meets
// the validation-proof contract. Provider observations can be accepted through
// this seam when the controller has persisted their exact observed result.
func (d *DB) CompleteControllerCommandAttemptWithOutputArtifact(id, outcome string, exitCode *int, signal, resultStateID, testedSHA *string, artifact Artifact) (*Artifact, error) {
	return d.completeCommandAttemptWithOutputArtifact(id, outcome, exitCode, signal, resultStateID, testedSHA, artifact, true)
}

func (d *DB) completeCommandAttemptWithOutputArtifact(id, outcome string, exitCode *int, signal, resultStateID, testedSHA *string, artifact Artifact, acceptAsProof bool) (*Artifact, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: begin transaction: %w", err)
	}
	defer tx.Rollback()

	attempt, err := getCommandAttempt(tx, id)
	if err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: %w", err)
	}
	if err := validateCommandAttemptCompletion(attempt, outcome, exitCode, signal, resultStateID, testedSHA); err != nil {
		return nil, err
	}
	acceptedAsProof := false
	if acceptAsProof {
		acceptedAsProof, err = commandAttemptCanEstablishProof(tx, attempt, outcome, exitCode, signal, resultStateID, testedSHA)
		if err != nil {
			return nil, err
		}
	}
	if attempt.CompletedAt != nil || attempt.OutputArtifactID != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: attempt is already complete")
	}
	if artifact.CommandAttemptID != nil && *artifact.CommandAttemptID != id {
		return nil, fmt.Errorf("complete command attempt with output artifact: artifact belongs to a different attempt")
	}
	if artifact.InvocationID != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: command output must not declare an invocation producer")
	}
	artifact.ID = newID()
	artifact.RunID = attempt.RunID
	artifact.StepID = &attempt.StepID
	artifact.RoundID = &attempt.RoundID
	artifact.CommandAttemptID = &attempt.ID
	artifact.CreatedAt = time.Now().UnixMilli()
	if err := validateArtifactForInsert(artifact); err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: %w", err)
	}
	if err := validateCommandOutputArtifact(attempt, artifact); err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO artifacts
		 (id, run_id, step_id, round_id, invocation_id, command_attempt_id, purpose, label, description,
		  storage_root, relative_path, kind, media_type, encoding, sha256, source_bytes, state, reason,
		  publication_state, publication_url, publication_commit_sha, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.RunID, artifact.StepID, artifact.RoundID, artifact.InvocationID, artifact.CommandAttemptID,
		artifact.Purpose, artifact.Label, artifact.Description, artifact.StorageRoot, artifact.RelativePath,
		artifact.Kind, artifact.MediaType, artifact.Encoding, artifact.SHA256, artifact.SourceBytes, artifact.State,
		artifact.Reason, artifact.PublicationState, artifact.PublicationURL, artifact.PublicationCommitSHA, artifact.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: insert artifact: %w", err)
	}

	completedAt := time.Now().UnixMilli()
	var proofReason *string
	if acceptedAsProof {
		reason := CommandProofReasonObservedPass
		proofReason = &reason
	}
	result, err := tx.Exec(
		`UPDATE command_attempts
		 SET completed_at = ?, duration_ms = MAX(0, ? - started_at), outcome = ?, exit_code = ?, signal = ?, result_state_id = ?, tested_sha = ?, output_artifact_id = ?, accepted_as_proof = ?, proof_reason = ?
		 WHERE id = ? AND completed_at IS NULL AND output_artifact_id IS NULL`,
		completedAt, completedAt, outcome, exitCode, signal, resultStateID, testedSHA, artifact.ID, acceptedAsProof, proofReason, id,
	)
	if err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: update attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: rows affected: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("complete command attempt with output artifact: attempt is missing or already complete")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("complete command attempt with output artifact: commit: %w", err)
	}
	return &artifact, nil
}

func commandAttemptCanEstablishProof(q commandAttemptQuerier, attempt *CommandAttempt, outcome string, exitCode *int, signal, resultStateID, testedSHA *string) (bool, error) {
	if attempt == nil || attempt.Observer != CommandObserverController && attempt.Observer != CommandObserverProvider {
		return false, nil
	}
	if outcome != CommandOutcomePass || exitCode == nil || *exitCode != 0 || signal != nil {
		return false, nil
	}
	if testedSHA == nil || *testedSHA != attempt.BeforeSHA {
		return false, nil
	}
	cleanState := "git:" + attempt.BeforeSHA
	if attempt.InputStateID == nil || resultStateID == nil || *attempt.InputStateID != cleanState || *resultStateID != cleanState {
		return false, nil
	}
	var stepName types.StepName
	if err := q.QueryRow(`SELECT step_name FROM step_results WHERE id = ? AND run_id = ?`, attempt.StepID, attempt.RunID).Scan(&stepName); err != nil {
		return false, fmt.Errorf("accept command attempt as proof: load owning step: %w", err)
	}
	if !isValidationProofStep(stepName) || attempt.Purpose != string(stepName) {
		return false, nil
	}
	return true, nil
}

func isValidationProofStep(stepName types.StepName) bool {
	switch stepName {
	case types.StepBuild, types.StepTest, types.StepLint:
		return true
	default:
		return false
	}
}

func validateCommandOutputArtifact(attempt *CommandAttempt, artifact Artifact) error {
	if artifact.Purpose != ArtifactPurposeCommandOutput {
		return fmt.Errorf("command output artifact: purpose must be %q, got %q", ArtifactPurposeCommandOutput, artifact.Purpose)
	}
	if artifact.Kind != ArtifactKindCommandOutput {
		return fmt.Errorf("command output artifact: kind must be %q, got %q", ArtifactKindCommandOutput, artifact.Kind)
	}
	if artifact.StorageRoot != ArtifactStorageRootRun {
		return fmt.Errorf("command output artifact: storage root must be %q, got %q", ArtifactStorageRootRun, artifact.StorageRoot)
	}
	expectedPath := path.Join(attempt.RunID, "command-output", attempt.ID+".log")
	if artifact.RelativePath != expectedPath {
		return fmt.Errorf("command output artifact: relative path must be %q, got %q", expectedPath, artifact.RelativePath)
	}
	if artifact.MediaType == "text/plain" && artifact.Encoding == "utf-8" ||
		artifact.MediaType == "application/octet-stream" && artifact.Encoding == "binary" {
		return nil
	}
	return fmt.Errorf("command output artifact: media type and encoding must be text/plain + utf-8 or application/octet-stream + binary, got %q + %q", artifact.MediaType, artifact.Encoding)
}

func validateCommandAttemptCompletion(attempt *CommandAttempt, outcome string, exitCode *int, signal, resultStateID, testedSHA *string) error {
	if !validCommandOutcome(outcome) {
		return fmt.Errorf("complete command attempt: invalid outcome %q", outcome)
	}
	if signal != nil && exitCode != nil {
		return fmt.Errorf("complete command attempt: exit code and signal are mutually exclusive")
	}
	if outcome == CommandOutcomePass && (exitCode == nil || *exitCode != 0 || signal != nil) {
		return fmt.Errorf("complete command attempt: passing outcome requires exit code zero")
	}
	if outcome == CommandOutcomeFail && (exitCode == nil || *exitCode == 0) && signal == nil {
		return fmt.Errorf("complete command attempt: failing outcome requires non-zero exit code or signal")
	}
	if testedSHA != nil && outcome != CommandOutcomePass {
		return fmt.Errorf("complete command attempt: tested commit requires passing outcome")
	}
	if testedSHA != nil {
		if attempt.InputStateID == nil || resultStateID == nil || *attempt.InputStateID != *resultStateID || *testedSHA != attempt.BeforeSHA {
			return fmt.Errorf("complete command attempt: tested commit requires unchanged input state")
		}
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
	return getCommandAttempt(d.sql, id)
}

func getCommandAttempt(q commandAttemptQuerier, id string) (*CommandAttempt, error) {
	attempt := &CommandAttempt{}
	if err := scanCommandAttempt(q.QueryRow(
		`SELECT id, run_id, command_id, step_id, round_id, sequence, purpose, observer, trigger_type, before_sha, tested_sha,
		        command_source, runner_schema_version, runner_source, runner_version, input_state_id, result_state_id,
		        started_at, completed_at, duration_ms, outcome, exit_code, signal, retry_of_attempt_id, retry_reason, output_artifact_id,
		        accepted_as_proof, proof_reason
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
		        ca.started_at, ca.completed_at, ca.duration_ms, ca.outcome, ca.exit_code, ca.signal, ca.retry_of_attempt_id, ca.retry_reason, ca.output_artifact_id,
		        ca.accepted_as_proof, ca.proof_reason
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

// GetAcceptedCommandAttemptsByTestedSHA returns proof accepted for exactly one
// tested commit. It deliberately does not follow ancestry or substitute the
// run's current head: a successful check proves only the SHA it observed.
func (d *DB) GetAcceptedCommandAttemptsByTestedSHA(runID, testedSHA string) ([]*CommandAttempt, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(testedSHA) == "" {
		return nil, fmt.Errorf("get accepted command attempts by tested SHA: run ID and tested SHA are required")
	}
	rows, err := d.sql.Query(
		`SELECT ca.id, ca.run_id, ca.command_id, ca.step_id, ca.round_id, ca.sequence, ca.purpose, ca.observer, ca.trigger_type, ca.before_sha, ca.tested_sha,
		        ca.command_source, ca.runner_schema_version, ca.runner_source, ca.runner_version, ca.input_state_id, ca.result_state_id,
		        ca.started_at, ca.completed_at, ca.duration_ms, ca.outcome, ca.exit_code, ca.signal, ca.retry_of_attempt_id, ca.retry_reason, ca.output_artifact_id,
		        ca.accepted_as_proof, ca.proof_reason
		 FROM command_attempts ca
		 JOIN runs run ON run.id = ca.run_id
		 JOIN step_results sr ON sr.id = ca.step_id
		 JOIN step_rounds r ON r.id = ca.round_id
		 WHERE ca.run_id = ? AND ca.tested_sha = ? AND ca.accepted_as_proof = 1
		   AND sr.status = ? AND r.status = ?
		   AND sr.step_name IN (?, ?, ?) AND ca.purpose = sr.step_name
		   AND run.status NOT IN (?, ?)
		 ORDER BY sr.step_order, r.round, ca.sequence`,
		runID, testedSHA, types.StepStatusCompleted, RoundStatusCompleted,
		types.StepBuild, types.StepTest, types.StepLint, types.RunFailed, types.RunCancelled,
	)
	if err != nil {
		return nil, fmt.Errorf("get accepted command attempts by tested SHA: %w", err)
	}
	defer rows.Close()
	var attempts []*CommandAttempt
	for rows.Next() {
		attempt := &CommandAttempt{}
		if err := scanCommandAttempt(rows, attempt); err != nil {
			return nil, fmt.Errorf("scan accepted command attempt by tested SHA: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accepted command attempts by tested SHA: %w", err)
	}
	return attempts, nil
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
		&attempt.OutputArtifactID, &attempt.AcceptedAsProof, &attempt.ProofReason,
	)
}
