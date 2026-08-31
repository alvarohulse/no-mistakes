package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
)

func pruneRichRuns(database *db.DB, p *paths.Paths, evidenceRoot string, policy evidenceReapPolicy, now time.Time) {
	cleanup := &runstats.RunArtifactCleanup{
		Targets: func(runID string) []string { return richRunArtifactTargets(p, evidenceRoot, runID) },
		Remove:  runstats.RemoveRunArtifactTargets,
	}
	pruned, err := runstats.PruneRichRunData(database, now, policy.Retention, policy.MaxRuns, cleanup)
	if err != nil {
		slog.Warn("rich run retention incomplete", "error", err, "pruned", pruned)
		return
	}
	if pruned > 0 {
		slog.Info("pruned rich run data", "runs", pruned)
	}
}

func richRunArtifactTargets(p *paths.Paths, evidenceRoot, runID string) []string {
	legacyRoot := filepath.Join(os.TempDir(), legacyEvidenceDirName)
	targets := []string{
		p.RunDir(runID),
		p.RunLogDir(runID),
		filepath.Join(evidenceRoot, runID),
	}
	if filepath.Clean(legacyRoot) != filepath.Clean(evidenceRoot) {
		targets = append(targets, filepath.Join(legacyRoot, runID))
	}
	return targets
}
