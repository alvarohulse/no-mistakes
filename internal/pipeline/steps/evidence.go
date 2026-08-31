package steps

import "github.com/kunchenguid/no-mistakes/internal/pipeline"

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
