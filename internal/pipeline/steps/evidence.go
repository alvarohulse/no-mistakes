package steps

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// testEvidenceDir is where the test step writes a run's evidence artifacts.
//
// Evidence is always collected OUTSIDE the worktree, keyed by run ID, so a
// pipeline run can never commit artifacts into the branch it is validating.
// They remain in owner-local storage and are never staged, committed, or pushed
// by the pipeline.
//
// The path itself is resolved once by the executor (see
// pipeline.StepContext.EvidenceDir) and read from the step context here. Steps
// must never rebuild it from os.TempDir(): on Linux the daemon's TMPDIR is
// unset, so that resolved to the shared /tmp - a fixed name nobody reaped, on
// a filesystem current Ubuntu backs with RAM.
func testEvidenceDir(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	return sctx.EvidenceDir
}

// registerTestEvidenceArtifacts indexes agent-reported files in place. URL and
// inline-content artifacts remain part of the findings payload only because
// they have no owner-local file to verify or retain.
func registerTestEvidenceArtifacts(sctx *pipeline.StepContext, reported []types.TestArtifact) error {
	var paths []types.TestArtifact
	for _, candidate := range reported {
		if strings.TrimSpace(candidate.Path) != "" && filepath.IsAbs(candidate.Path) {
			paths = append(paths, candidate)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if sctx == nil || sctx.Run == nil || sctx.Paths == nil || sctx.DB == nil || sctx.StepResultID == "" || sctx.RoundID == "" {
		return fmt.Errorf("durable run, step, round, paths, and database are required")
	}
	configuredEvidenceRoot := ""
	if sctx.Config != nil {
		configuredEvidenceRoot = sctx.Config.Test.Evidence.LocalRoot
	}
	evidenceRoot := sctx.Paths.EvidenceRoot(configuredEvidenceRoot)
	store, err := artifact.NewStore(sctx.Paths, configuredEvidenceRoot)
	if err != nil {
		return err
	}
	stepID := sctx.StepResultID
	roundID := sctx.RoundID
	for _, candidate := range paths {
		if !pathWithinRoot(candidate.Path, evidenceRoot) {
			if pathWithinRoot(candidate.Path, sctx.WorkDir) {
				continue
			}
			return fmt.Errorf("index %q: evidence file must stay within the evidence root", candidate.Path)
		}
		indexed, err := store.IndexEvidenceFile(sctx.Run.ID, candidate.Path)
		if err != nil {
			return fmt.Errorf("index %q: %w", candidate.Path, err)
		}
		kind := strings.TrimSpace(candidate.Kind)
		if kind == "" {
			kind = "file"
		}
		indexed.RunID = sctx.Run.ID
		indexed.StepID = &stepID
		indexed.RoundID = &roundID
		indexed.Purpose = db.ArtifactPurposeTestEvidence
		indexed.Label = candidate.Label
		indexed.Kind = kind
		if _, err := sctx.DB.RegisterArtifact(indexed); err != nil {
			return fmt.Errorf("register %q: %w", candidate.Path, err)
		}
	}
	return nil
}

func pathWithinRoot(target, root string) bool {
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(targetAbs))
	if err != nil || relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
