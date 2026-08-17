package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
)

func pruneRichRuns(database *db.DB, p *paths.Paths, evidenceRoot string, policy evidenceReapPolicy, now time.Time) {
	pruned, err := runstats.PruneRichRunData(database, now, policy.Retention, policy.MaxRuns, func(runID string) error {
		return removeRichRunArtifacts(p, evidenceRoot, runID)
	})
	if err != nil {
		slog.Warn("rich run retention incomplete", "error", err, "pruned", pruned)
		return
	}
	if pruned > 0 {
		slog.Info("pruned rich run data", "runs", pruned)
	}
}

func removeRichRunArtifacts(p *paths.Paths, evidenceRoot, runID string) error {
	if filepath.Base(runID) != runID || runID == "." || runID == ".." {
		return fmt.Errorf("invalid run ID %q for artifact cleanup", runID)
	}
	legacyRoot := filepath.Join(os.TempDir(), legacyEvidenceDirName)
	pathsToRemove := []string{
		p.RunLogDir(runID),
		filepath.Join(evidenceRoot, runID),
	}
	if filepath.Clean(legacyRoot) != filepath.Clean(evidenceRoot) {
		pathsToRemove = append(pathsToRemove, filepath.Join(legacyRoot, runID))
	}
	var failures []error
	for _, path := range pathsToRemove {
		if err := os.RemoveAll(path); err != nil {
			failures = append(failures, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(failures...)
}
