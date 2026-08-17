package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// legacyEvidenceDirName is the fixed directory no-mistakes used to create
// directly inside the system temp directory. It was never reaped by anything in
// this program: on Linux the daemon's TMPDIR is unset, so it resolved to the
// shared /tmp - a RAM-backed tmpfs on current Ubuntu - and it accumulated one
// subdirectory per run until an OS timer or a reboot happened to clear it.
//
// Evidence now lives under the app root (paths.EvidenceDir). This name survives
// only so an upgraded daemon drains what earlier versions left behind, under
// the same retention policy and the same active-run guard. Delete this constant
// and reapLegacyEvidence once installs from before the relocation are gone.
const legacyEvidenceDirName = "no-mistakes-evidence"

// evidenceReapPolicy bounds how much rich run data survives. A non-positive
// Retention keeps rich data indefinitely. Positive values are widened to the
// mandatory 14-day minimum, and MaxRuns can widen the mandatory newest-50
// floor.
type evidenceReapPolicy struct {
	Retention time.Duration
	MaxRuns   int
}

// evidenceReapPolicyFor resolves the policy from a loaded global config,
// falling back to the built-in defaults when configuration is unavailable.
// Retention and the configured newest-run floor are global-only settings (see
// config.EvidenceRaw), so no repository is consulted here.
func evidenceReapPolicyFor(global *config.GlobalConfig) evidenceReapPolicy {
	policy := evidenceReapPolicy{
		Retention: config.DefaultEvidenceRetention,
		MaxRuns:   config.DefaultEvidenceMaxRuns,
	}
	if global == nil {
		return policy
	}
	resolved := config.Merge(global, &config.RepoConfig{})
	policy.Retention = resolved.Test.Evidence.Retention
	policy.MaxRuns = resolved.Test.Evidence.MaxRuns
	return policy
}

// evidenceRootFor resolves this machine's evidence root from a loaded global
// config, falling back to the app-root default.
func evidenceRootFor(p *paths.Paths, global *config.GlobalConfig) string {
	configured := ""
	if global != nil {
		configured = config.Merge(global, &config.RepoConfig{}).Test.Evidence.LocalRoot
	}
	return p.EvidenceRoot(configured)
}

// reapEvidence removes empty terminal-run directories and leftovers whose rich
// run row was already archived. Non-empty retention is owned by pruneRichRuns,
// which holds the database write lock across artifact cleanup and archival so a
// concurrent pin cannot succeed during deletion.
func reapEvidence(d *db.DB, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // directory may not exist yet, which is the normal case
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		run, err := d.GetRun(runID)
		if err != nil {
			slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", err)
			continue
		}
		if run == nil {
			receipt, receiptErr := d.GetRunMetricReceipt(runID)
			if receiptErr != nil {
				slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", receiptErr)
				continue
			}
			if receipt != nil {
				if removeEvidenceDir(filepath.Join(root, runID), runID) {
					removed++
				}
			}
			continue
		}
		if run.PinnedAt != nil {
			continue
		}
		if skip, reason := skipWorktreeCleanup(d, runID); skip {
			slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", reason)
			continue
		}
		path := filepath.Join(root, runID)
		inner, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		if len(inner) == 0 {
			// Remove only the directory itself. If a writer raced the empty
			// check and created an artifact, this fails rather than deleting it.
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}

	if removed > 0 {
		slog.Info("reaped run evidence", "root", root, "removed", removed)
	}
}

func removeEvidenceDir(path, runID string) bool {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("failed to remove run evidence", "path", path, "run_id", runID, "error", err)
		return false
	}
	return true
}

// reapLegacyEvidence drains the pre-relocation directory in the system temp
// directory under the same policy. Contents are deliberately NOT migrated: the
// absolute paths already written into older PR bodies name the old location, so
// moving files would not repair them, and a wholesale removal could delete
// artifacts a run started before the upgrade is still writing. Reaping by the
// same rules, with the same active-run guard, is bounded and surprises nobody.
func reapLegacyEvidence(d *db.DB, current string) {
	legacy := filepath.Join(os.TempDir(), legacyEvidenceDirName)
	if legacy == current {
		return // an operator who pointed local_root back at it owns it now
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	reapEvidence(d, legacy)
	// Remove the root itself once it is empty; os.Remove fails harmlessly while
	// anything remains.
	_ = os.Remove(legacy)
}
