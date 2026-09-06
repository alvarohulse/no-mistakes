package db

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

const (
	ArtifactStorageRootRun      = "run"
	ArtifactStorageRootEvidence = "evidence"

	ArtifactPurposeCommandOutput = "command_output"
	ArtifactKindCommandOutput    = "command-output"
	ArtifactStateAvailable       = "available"
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
	if artifact.SourceBytes < 0 {
		return fmt.Errorf("artifact: source bytes must not be negative")
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

const artifactSelectColumns = `id, run_id, step_id, round_id, invocation_id, command_attempt_id,
	purpose, label, description, storage_root, relative_path, kind, media_type, encoding,
	sha256, source_bytes, state, reason, publication_state, publication_url, publication_commit_sha, created_at`

const artifactSelectByID = `SELECT ` + artifactSelectColumns + ` FROM artifacts WHERE id = ?`

const artifactSelectByRun = `SELECT ` + artifactSelectColumns + ` FROM artifacts WHERE run_id = ? ORDER BY created_at, id`

func scanArtifact(row interface{ Scan(...any) error }, artifact *Artifact) error {
	return row.Scan(
		&artifact.ID, &artifact.RunID, &artifact.StepID, &artifact.RoundID, &artifact.InvocationID, &artifact.CommandAttemptID,
		&artifact.Purpose, &artifact.Label, &artifact.Description, &artifact.StorageRoot, &artifact.RelativePath,
		&artifact.Kind, &artifact.MediaType, &artifact.Encoding, &artifact.SHA256, &artifact.SourceBytes,
		&artifact.State, &artifact.Reason, &artifact.PublicationState, &artifact.PublicationURL, &artifact.PublicationCommitSHA,
		&artifact.CreatedAt,
	)
}
