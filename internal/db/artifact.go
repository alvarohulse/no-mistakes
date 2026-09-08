package db

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	ArtifactStorageRootRun      = "run"
	ArtifactStorageRootEvidence = "evidence"

	ArtifactPurposeCommandOutput       = "command_output"
	ArtifactPurposeTestEvidence        = "test_evidence"
	ArtifactPurposeOperationDiagnostic = "operation_diagnostic"
	ArtifactKindCommandOutput          = "command-output"
	ArtifactKindOperationDiagnostic    = "operation-diagnostic"
	ArtifactStateAvailable             = "available"
)

// Artifact is an immutable formatter-readable file registered for one run.
// RelativePath is normalized from its selected StorageRoot and never stores an
// absolute machine path.
type Artifact struct {
	ID                   string
	RunID                string
	StepID               *string
	RoundID              *string
	InvocationID         *string
	CommandAttemptID     *string
	OperationID          *string
	Purpose              string
	Label                string
	Description          *string
	StorageRoot          string
	RelativePath         string
	Kind                 string
	MediaType            string
	Encoding             string
	SHA256               string
	SourceBytes          int64
	State                string
	Reason               *string
	PublicationState     *string
	PublicationURL       *string
	PublicationCommitSHA *string
	CreatedAt            int64
}

func validateArtifactForInsert(artifact Artifact) error {
	if artifact.RunID == "" || strings.TrimSpace(artifact.Purpose) == "" || strings.TrimSpace(artifact.Label) == "" ||
		strings.TrimSpace(artifact.Kind) == "" || strings.TrimSpace(artifact.MediaType) == "" || strings.TrimSpace(artifact.Encoding) == "" ||
		artifact.State != ArtifactStateAvailable {
		return fmt.Errorf("artifact: required metadata is incomplete")
	}
	if artifact.StorageRoot != ArtifactStorageRootRun && artifact.StorageRoot != ArtifactStorageRootEvidence {
		return fmt.Errorf("artifact: unsupported storage root %q", artifact.StorageRoot)
	}
	if normalized, err := normalizeArtifactRelativePath(artifact.RelativePath); err != nil {
		return fmt.Errorf("artifact: relative path: %w", err)
	} else if normalized != artifact.RelativePath {
		return fmt.Errorf("artifact: relative path must be normalized")
	}
	pathRunID, _, found := strings.Cut(artifact.RelativePath, "/")
	if !found || pathRunID != artifact.RunID {
		return fmt.Errorf("artifact: relative path must begin with its run ID")
	}
	if artifact.SourceBytes < 0 {
		return fmt.Errorf("artifact: source bytes must not be negative")
	}
	if artifact.OperationID != nil && strings.TrimSpace(*artifact.OperationID) == "" {
		return fmt.Errorf("artifact: operation producer is empty")
	}
	if !validArtifactSHA256(artifact.SHA256) {
		return fmt.Errorf("artifact: SHA-256 digest is invalid")
	}
	if artifact.Reason != nil || artifact.PublicationState != nil || artifact.PublicationURL != nil || artifact.PublicationCommitSHA != nil {
		return fmt.Errorf("artifact: available local artifact must not declare reason or publication")
	}
	return nil
}

func normalizeArtifactRelativePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("must be a non-empty root-relative path")
	}
	normalized := path.Clean(value)
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("must stay within the storage root")
	}
	return normalized, nil
}

func validArtifactSHA256(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == 32
}

// RegisterArtifact persists one non-command artifact with a database-owned
// stable ID. Re-registering identical metadata for the same physical path is
// idempotent; a mismatched duplicate is rejected rather than silently
// reinterpreting the immutable file.
func (d *DB) RegisterArtifact(artifact Artifact) (*Artifact, error) {
	if artifact.ID != "" {
		return nil, fmt.Errorf("register artifact: ID is assigned by the database")
	}
	if artifact.CommandAttemptID != nil {
		return nil, fmt.Errorf("register artifact: command output must be linked while completing its attempt")
	}
	if err := validateArtifactForInsert(artifact); err != nil {
		return nil, fmt.Errorf("register artifact: %w", err)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("register artifact: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := validateArtifactProducer(tx, artifact); err != nil {
		return nil, fmt.Errorf("register artifact: %w", err)
	}
	if existing, err := getArtifactByStoragePath(tx, artifact.StorageRoot, artifact.RelativePath); err != nil {
		return nil, err
	} else if existing != nil {
		return resolveArtifactRegistration(existing, artifact)
	}

	artifact.ID = newID()
	artifact.CreatedAt = time.Now().UnixMilli()
	if _, err := tx.Exec(
		`INSERT INTO artifacts
		 (id, run_id, step_id, round_id, invocation_id, command_attempt_id, operation_id, purpose, label, description,
		  storage_root, relative_path, kind, media_type, encoding, sha256, source_bytes, state, reason,
		  publication_state, publication_url, publication_commit_sha, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.RunID, artifact.StepID, artifact.RoundID, artifact.InvocationID, artifact.CommandAttemptID, artifact.OperationID,
		artifact.Purpose, artifact.Label, artifact.Description, artifact.StorageRoot, artifact.RelativePath,
		artifact.Kind, artifact.MediaType, artifact.Encoding, artifact.SHA256, artifact.SourceBytes, artifact.State,
		artifact.Reason, artifact.PublicationState, artifact.PublicationURL, artifact.PublicationCommitSHA, artifact.CreatedAt,
	); err != nil {
		existing, lookupErr := getArtifactByStoragePath(tx, artifact.StorageRoot, artifact.RelativePath)
		if lookupErr != nil {
			return nil, fmt.Errorf("register artifact: insert: %w", err)
		}
		if existing != nil {
			return resolveArtifactRegistration(existing, artifact)
		}
		return nil, fmt.Errorf("register artifact: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("register artifact: commit: %w", err)
	}
	return &artifact, nil
}

type artifactQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (d *DB) getArtifactByStoragePath(storageRoot, relativePath string) (*Artifact, error) {
	return getArtifactByStoragePath(d.sql, storageRoot, relativePath)
}

func getArtifactByStoragePath(q artifactQuerier, storageRoot, relativePath string) (*Artifact, error) {
	artifact := &Artifact{}
	if err := scanArtifact(q.QueryRow(
		`SELECT `+artifactSelectColumns+` FROM artifacts WHERE storage_root = ? AND relative_path = ?`,
		storageRoot, relativePath,
	), artifact); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get artifact by storage path: %w", err)
	}
	return artifact, nil
}

func validateArtifactProducer(q artifactQuerier, artifact Artifact) error {
	if artifact.StepID == nil && artifact.RoundID == nil && artifact.InvocationID == nil && artifact.OperationID == nil {
		return fmt.Errorf("artifact: producer metadata is required")
	}

	var stepName string
	var roundNumber int
	if artifact.OperationID != nil {
		var runID, operationStepID, operationRoundID string
		err := q.QueryRow(
			`SELECT run_id, step_id, round_id FROM operations WHERE id = ?`,
			*artifact.OperationID,
		).Scan(&runID, &operationStepID, &operationRoundID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("artifact: operation producer %q does not exist", *artifact.OperationID)
		}
		if err != nil {
			return fmt.Errorf("artifact: load operation producer: %w", err)
		}
		if runID != artifact.RunID {
			return fmt.Errorf("artifact: operation producer does not belong to run")
		}
		if artifact.StepID != nil && operationStepID != *artifact.StepID {
			return fmt.Errorf("artifact: operation and step producers are not connected")
		}
		if artifact.RoundID != nil && operationRoundID != *artifact.RoundID {
			return fmt.Errorf("artifact: operation and round producers are not connected")
		}
	}
	if artifact.StepID != nil {
		var runID string
		err := q.QueryRow(`SELECT run_id, step_name FROM step_results WHERE id = ?`, *artifact.StepID).Scan(&runID, &stepName)
		if err == sql.ErrNoRows {
			return fmt.Errorf("artifact: step producer %q does not exist", *artifact.StepID)
		}
		if err != nil {
			return fmt.Errorf("artifact: load step producer: %w", err)
		}
		if runID != artifact.RunID {
			return fmt.Errorf("artifact: step producer does not belong to run")
		}
	}

	if artifact.RoundID != nil {
		var runID, roundStepID, roundStepName string
		err := q.QueryRow(
			`SELECT s.run_id, s.id, s.step_name, r.round
			 FROM step_rounds r
			 JOIN step_results s ON s.id = r.step_result_id
			 WHERE r.id = ?`,
			*artifact.RoundID,
		).Scan(&runID, &roundStepID, &roundStepName, &roundNumber)
		if err == sql.ErrNoRows {
			return fmt.Errorf("artifact: round producer %q does not exist", *artifact.RoundID)
		}
		if err != nil {
			return fmt.Errorf("artifact: load round producer: %w", err)
		}
		if runID != artifact.RunID {
			return fmt.Errorf("artifact: round producer does not belong to run")
		}
		if artifact.StepID != nil && roundStepID != *artifact.StepID {
			return fmt.Errorf("artifact: step and round producers are not connected")
		}
		if artifact.StepID == nil {
			stepName = roundStepName
		}
	}

	if artifact.InvocationID != nil {
		var runID, invocationStepName string
		var invocationRound int
		err := q.QueryRow(
			`SELECT run_id, step_name, round FROM agent_invocations WHERE id = ?`,
			*artifact.InvocationID,
		).Scan(&runID, &invocationStepName, &invocationRound)
		if err == sql.ErrNoRows {
			return fmt.Errorf("artifact: invocation producer %q does not exist", *artifact.InvocationID)
		}
		if err != nil {
			return fmt.Errorf("artifact: load invocation producer: %w", err)
		}
		if runID != artifact.RunID {
			return fmt.Errorf("artifact: invocation producer does not belong to run")
		}
		if artifact.StepID != nil || artifact.RoundID != nil {
			if invocationStepName != stepName {
				return fmt.Errorf("artifact: invocation and step producers are not connected")
			}
		}
		if artifact.RoundID != nil && invocationRound != roundNumber {
			return fmt.Errorf("artifact: invocation and round producers are not connected")
		}
	}
	return nil
}

func resolveArtifactRegistration(existing *Artifact, incoming Artifact) (*Artifact, error) {
	if sameArtifactRegistration(*existing, incoming) {
		return existing, nil
	}
	return nil, fmt.Errorf("register artifact: metadata conflicts with existing artifact at %s", incoming.RelativePath)
}

func sameArtifactRegistration(left, right Artifact) bool {
	return left.RunID == right.RunID &&
		sameOptionalString(left.StepID, right.StepID) &&
		sameOptionalString(left.RoundID, right.RoundID) &&
		sameOptionalString(left.InvocationID, right.InvocationID) &&
		sameOptionalString(left.CommandAttemptID, right.CommandAttemptID) &&
		sameOptionalString(left.OperationID, right.OperationID) &&
		left.Purpose == right.Purpose &&
		left.Label == right.Label &&
		sameOptionalString(left.Description, right.Description) &&
		left.StorageRoot == right.StorageRoot &&
		left.RelativePath == right.RelativePath &&
		left.Kind == right.Kind &&
		left.MediaType == right.MediaType &&
		left.Encoding == right.Encoding &&
		left.SHA256 == right.SHA256 &&
		left.SourceBytes == right.SourceBytes &&
		left.State == right.State &&
		sameOptionalString(left.Reason, right.Reason) &&
		sameOptionalString(left.PublicationState, right.PublicationState) &&
		sameOptionalString(left.PublicationURL, right.PublicationURL) &&
		sameOptionalString(left.PublicationCommitSHA, right.PublicationCommitSHA)
}

// GetArtifact returns one registered artifact by stable identity.
func (d *DB) GetArtifact(id string) (*Artifact, error) {
	artifact := &Artifact{}
	if err := scanArtifact(d.sql.QueryRow(artifactSelectByID, id), artifact); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	return artifact, nil
}

// GetArtifactsByRun returns artifacts in their durable creation order.
func (d *DB) GetArtifactsByRun(runID string) ([]*Artifact, error) {
	rows, err := d.sql.Query(artifactSelectByRun, runID)
	if err != nil {
		return nil, fmt.Errorf("get artifacts by run: %w", err)
	}
	defer rows.Close()
	var artifacts []*Artifact
	for rows.Next() {
		artifact := &Artifact{}
		if err := scanArtifact(rows, artifact); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

const artifactSelectColumns = `id, run_id, step_id, round_id, invocation_id, command_attempt_id, operation_id,
	purpose, label, description, storage_root, relative_path, kind, media_type, encoding,
	sha256, source_bytes, state, reason, publication_state, publication_url, publication_commit_sha, created_at`

const artifactSelectByID = `SELECT ` + artifactSelectColumns + ` FROM artifacts WHERE id = ?`

const artifactSelectByRun = `SELECT ` + artifactSelectColumns + ` FROM artifacts WHERE run_id = ? ORDER BY created_at, id`

func scanArtifact(row interface{ Scan(...any) error }, artifact *Artifact) error {
	return row.Scan(
		&artifact.ID, &artifact.RunID, &artifact.StepID, &artifact.RoundID, &artifact.InvocationID, &artifact.CommandAttemptID, &artifact.OperationID,
		&artifact.Purpose, &artifact.Label, &artifact.Description, &artifact.StorageRoot, &artifact.RelativePath,
		&artifact.Kind, &artifact.MediaType, &artifact.Encoding, &artifact.SHA256, &artifact.SourceBytes,
		&artifact.State, &artifact.Reason, &artifact.PublicationState, &artifact.PublicationURL, &artifact.PublicationCommitSHA,
		&artifact.CreatedAt,
	)
}
