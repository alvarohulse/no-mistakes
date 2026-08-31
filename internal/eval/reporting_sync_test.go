package eval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInspectSetsSelfScoresRecordedReviewsAgainstTheirGold(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "scored", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "hit", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "real bug", Severity: "error", Action: "auto-fix"},
			{ID: "missed", Kind: GoldFalseNegative, Source: goldSourceUserAdded, File: "main.go", Line: 9, Description: "missing audit", Severity: "warning", Action: "auto-fix"},
			{ID: "noise", Kind: GoldFalsePositive, Source: goldSourceShippedUnfixed, File: "main.go", Line: 5, Description: "style nit", Severity: "info", Action: "ask-user"},
		},
		roundFindings: findingsJSON(
			findingSpec{ID: "hit", Severity: "error", File: "main.go", Line: 1, Description: "real bug", Action: "auto-fix"},
			findingSpec{ID: "noise", Severity: "info", File: "main.go", Line: 5, Description: "style nit", Action: "ask-user"},
		),
	})

	score := mustSetSummary(t, store, "diversified").SelfScore
	if score.Labeled != 1 || score.TruePositive != 1 || score.FalseNegative != 1 || score.FalsePositive != 1 || score.Pending != 0 {
		t.Fatalf("self-score = %#v, want one labeled case with TP 1 / FN 1 / FP 1 / pending 0", score)
	}
}

func TestReplayReportsPlanAndPerResultProgress(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	var plannedCases int
	var plannedCohort string
	type progress struct{ completed, total int }
	var results []progress
	_, evaluations, err := Replay(ctx, store, ReplayOptions{
		Set:       "labeled",
		Candidate: Candidate{Agent: types.AgentClaude, Model: "test", Vendor: "anthropic"},
		Repeats:   2,
		OnPlan: func(session Session, cases []Case) {
			plannedCases = len(cases)
			plannedCohort = session.Cohort
		},
		OnResult: func(_ Evaluation, completed, total int) {
			results = append(results, progress{completed, total})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plannedCases != 1 || plannedCohort == "" || len(evaluations) != 2 || len(results) != 2 {
		t.Fatalf("plan cases/cohort/evaluations/progress = %d/%q/%d/%#v", plannedCases, plannedCohort, len(evaluations), results)
	}
	for i, got := range results {
		if got.completed != i+1 || got.total != 2 {
			t.Fatalf("progress %d = %+v, want completed %d of total 2", i, got, i+1)
		}
	}
}

func TestRepoDisplayNamesKeyResolvedNamesByCaptureFingerprint(t *testing.T) {
	repos := []*db.Repo{
		{ID: "a", WorkingPath: filepath.Join("/tmp", "clone-a"), UpstreamURL: "https://github.com/kunchenguid/no-mistakes.git"},
		{ID: "b", WorkingPath: filepath.Join("/tmp", "clone-b"), UpstreamURL: "git@example.test:org/other.git"},
		{ID: "c", WorkingPath: filepath.Join("/tmp", "clone-c"), UpstreamURL: "https://example.test/single-segment"},
		{ID: "d"},
		nil,
	}
	names := RepoDisplayNames(repos)
	for fingerprint, want := range map[string]string{
		fingerprint("https://github.com/kunchenguid/no-mistakes.git"): "kunchenguid/no-mistakes",
		fingerprint("git@example.test:org/other.git"):                 "org/other",
		fingerprint("https://example.test/single-segment"):            "clone-c",
	} {
		if got := names[fingerprint]; got != want {
			t.Fatalf("names[%s] = %q, want %q", fingerprint, got, want)
		}
	}
}

func TestInspectSetsKeepsRepositoriesWithTheSameDisplayNameSeparate(t *testing.T) {
	store := openEvalStore(t)
	first := fingerprint("https://github.com/org/repo.git")
	second := fingerprint("https://github.example.com/org/repo.git")
	store.SetRepoNames(map[string]string{first: "org/repo", second: "org/repo"})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "first", fingerprint: first, changedLines: 10})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "second", fingerprint: second, changedLines: 10})

	composition := mustSetSummary(t, store, "all").Composition
	if len(composition) != 2 {
		t.Fatalf("composition = %#v, want separate rows for two repository fingerprints", composition)
	}
	for _, row := range composition {
		if row.Repo != "org/repo" || row.Cases != 1 {
			t.Fatalf("composition row = %#v, want one case for the resolved repository name", row)
		}
	}
}

func mustSetSummary(t *testing.T, store *Store, name string) SetSummary {
	t.Helper()
	for _, summary := range mustInspectSets(t, store) {
		if summary.Name == name {
			return summary
		}
	}
	t.Fatalf("set %q not found", name)
	return SetSummary{}
}
