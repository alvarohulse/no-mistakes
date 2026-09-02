package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	NarrativeSourceAgent    = "agent"
	NarrativeSourceFallback = "fallback"

	NarrativeTitleModeAgent     = "agent"
	NarrativeTitleModeFallback  = "fallback"
	NarrativeTitleModePreserved = "preserved"
)

// RunNarrative is the single PR draft owned by one run. Retries and replay
// reuse this record; a later run receives its own independently drafted row.
type RunNarrative struct {
	RunID                string
	Source               string
	DraftingInvocationID *string
	DraftedAt            int64
	BaseSHA              string
	HeadSHA              string
	TitleMode            string
	TitleText            string
	Summary              string
	WhatChanged          string
}

// InsertRunNarrative persists the run's draft exactly once.
func (d *DB) InsertRunNarrative(n RunNarrative) error {
	if err := validateRunNarrative(n); err != nil {
		return err
	}
	if n.Source == NarrativeSourceAgent {
		var invocationRunID string
		err := d.sql.QueryRow(`SELECT run_id FROM agent_invocations WHERE id = ?`, *n.DraftingInvocationID).Scan(&invocationRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("insert run narrative: drafting invocation %q does not exist", *n.DraftingInvocationID)
		}
		if err != nil {
			return fmt.Errorf("insert run narrative: verify drafting invocation: %w", err)
		}
		if invocationRunID != n.RunID {
			return fmt.Errorf("insert run narrative: drafting invocation %q does not belong to run %q", *n.DraftingInvocationID, n.RunID)
		}
	}
	_, err := d.sql.Exec(`INSERT INTO run_narratives (
		run_id, source, drafting_invocation_id, drafted_at, base_sha, head_sha,
		title_mode, title_text, summary, what_changed
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.RunID, n.Source, n.DraftingInvocationID, n.DraftedAt, n.BaseSHA, n.HeadSHA,
		n.TitleMode, n.TitleText, n.Summary, n.WhatChanged,
	)
	if err != nil {
		return fmt.Errorf("insert run narrative: %w", err)
	}
	return nil
}

// GetRunNarrative returns the run's persisted draft, or nil for a legacy run
// or a run that has not reached PR drafting yet.
func (d *DB) GetRunNarrative(runID string) (*RunNarrative, error) {
	var n RunNarrative
	err := d.sql.QueryRow(`SELECT
		run_id, source, drafting_invocation_id, drafted_at, base_sha, head_sha,
		title_mode, title_text, summary, what_changed
		FROM run_narratives WHERE run_id = ?`, runID,
	).Scan(
		&n.RunID, &n.Source, &n.DraftingInvocationID, &n.DraftedAt, &n.BaseSHA, &n.HeadSHA,
		&n.TitleMode, &n.TitleText, &n.Summary, &n.WhatChanged,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run narrative: %w", err)
	}
	return &n, nil
}

func validateRunNarrative(n RunNarrative) error {
	if strings.TrimSpace(n.RunID) == "" {
		return fmt.Errorf("insert run narrative: run ID is required")
	}
	if n.Source != NarrativeSourceAgent && n.Source != NarrativeSourceFallback {
		return fmt.Errorf("insert run narrative: invalid source %q", n.Source)
	}
	if n.Source == NarrativeSourceAgent && (n.DraftingInvocationID == nil || strings.TrimSpace(*n.DraftingInvocationID) == "") {
		return fmt.Errorf("insert run narrative: drafting invocation is required for agent source")
	}
	if n.Source == NarrativeSourceFallback && n.DraftingInvocationID != nil {
		return fmt.Errorf("insert run narrative: fallback source cannot reference a drafting invocation")
	}
	if n.DraftedAt <= 0 || strings.TrimSpace(n.BaseSHA) == "" || strings.TrimSpace(n.HeadSHA) == "" {
		return fmt.Errorf("insert run narrative: draft time, base SHA, and head SHA are required")
	}
	if n.TitleMode != NarrativeTitleModeAgent && n.TitleMode != NarrativeTitleModeFallback && n.TitleMode != NarrativeTitleModePreserved {
		return fmt.Errorf("insert run narrative: invalid title mode %q", n.TitleMode)
	}
	if strings.TrimSpace(n.TitleText) == "" || strings.TrimSpace(n.WhatChanged) == "" {
		return fmt.Errorf("insert run narrative: title and what changed are required")
	}
	return nil
}
