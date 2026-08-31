package eval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCaptureTwiceLeavesIdenticalCorpusState(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first capture = %d cases, want 1", len(first))
	}
	labelsBefore := mustReadEvalFile(t, filepath.Join(first[0].Dir, "labels.json"))
	manifestBefore := mustReadEvalFile(t, filepath.Join(first[0].Dir, "manifest.json"))
	setsBefore := mustInspectSets(t, store)

	second, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second capture = %#v, want the same single case %q", second, first[0].ID)
	}
	if got := mustReadEvalFile(t, filepath.Join(first[0].Dir, "labels.json")); got != labelsBefore {
		t.Fatalf("second capture changed labels.json:\nbefore: %s\nafter: %s", labelsBefore, got)
	}
	if got := mustReadEvalFile(t, filepath.Join(first[0].Dir, "manifest.json")); got != manifestBefore {
		t.Fatalf("second capture changed manifest.json:\nbefore: %s\nafter: %s", manifestBefore, got)
	}
	if setsAfter := mustInspectSets(t, store); !reflect.DeepEqual(setsBefore, setsAfter) {
		t.Fatalf("second capture changed set summaries:\nbefore: %#v\nafter: %#v", setsBefore, setsAfter)
	}
	all, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("corpus has %d cases after double capture, want 1", len(all))
	}
}

func TestCaptureTwiceDoesNotDuplicateUserAddedGoldWithoutID(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 2; i++ {
		cases, err := Capture(ctx, store, p, sourceDB, run.ID)
		if err != nil {
			t.Fatalf("capture %d: %v", i+1, err)
		}
		if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
			t.Fatalf("capture %d gold = %#v, want exactly one ID-less user-added finding", i+1, cases[0].Labels.Findings)
		}
	}
}

func TestCaptureRepairsDuplicateUserAddedGoldWithoutID(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
		t.Fatalf("initial gold = %#v, want one ID-less user-added finding", cases)
	}
	corrupted := cases[0].Labels
	corrupted.Findings = append(corrupted.Findings, corrupted.Findings[0])
	if err := writeJSON(filepath.Join(cases[0].Dir, "labels.json"), corrupted); err != nil {
		t.Fatal(err)
	}

	repaired, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || len(repaired[0].Labels.Findings) != 1 {
		t.Fatalf("repaired gold = %#v, want one ID-less user-added finding", repaired)
	}
}

func TestRelabelRunTwiceLeavesIdenticalLabels(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	labelsAfterFirst := mustReadEvalFile(t, filepath.Join(cases[0].Dir, "labels.json"))
	second, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("relabel case counts differ: %d vs %d", len(first), len(second))
	}
	if got := mustReadEvalFile(t, filepath.Join(cases[0].Dir, "labels.json")); got != labelsAfterFirst {
		t.Fatalf("second relabel changed labels.json:\nfirst: %s\nsecond: %s", labelsAfterFirst, got)
	}
}

func TestInspectSetsTwiceIsStableAndKeepsDiversifiedPins(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(2)
	writeGoldStratum(t, store, "repo-a", "error", 2, 10)
	writeGoldStratum(t, store, "repo-b", "warning", 1, 20)

	first := mustInspectSets(t, store)
	pinsFirst := mustDiversifiedPinRows(t, store)
	second := mustInspectSets(t, store)
	pinsSecond := mustDiversifiedPinRows(t, store)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("set summaries changed on re-inspection:\nfirst: %#v\nsecond: %#v", first, second)
	}
	if !reflect.DeepEqual(pinsFirst, pinsSecond) {
		t.Fatalf("diversified pins changed on re-inspection:\nfirst: %#v\nsecond: %#v", pinsFirst, pinsSecond)
	}
}

func TestRefreshDiversifiedTwiceKeepsTheSamePins(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(2)
	writeGoldStratum(t, store, "repo-a", "error", 2, 10)
	writeGoldStratum(t, store, "repo-b", "warning", 1, 20)

	first, err := store.RefreshDiversified()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RefreshDiversified()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caseIDs(first), caseIDs(second)) {
		t.Fatalf("refresh pinned different cases: %v vs %v", caseIDs(first), caseIDs(second))
	}
}

func TestReplayTwiceKeepsCorpusUntouchedAndCohortStable(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	installFakeReviewAgent(t, p, findings)
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayConfigJSON, err := config.MarshalEvalReplayConfig(&config.Config{
		IgnorePatterns: []string{"generated/**"},
		Prompts:        config.PromptConfig{Review: "review error paths"},
	})
	if err != nil {
		t.Fatal(err)
	}
	versionedRound, err := sourceDB.InsertReviewStepRoundWithReplayConfig(
		steps[0].ID, 2, "initial", &findings, nil, run.HeadSHA,
		stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), replayConfigJSON, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := sourceDB.SetStepRoundSelection(versionedRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	captured, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var replayCase Case
	for _, c := range captured {
		if c.SourceRoundID == versionedRound.ID {
			replayCase = c
			break
		}
	}
	if replayCase.ID == "" {
		t.Fatalf("versioned replay case missing from %#v", captured)
	}
	labelsBefore := mustReadEvalFile(t, filepath.Join(replayCase.Dir, "labels.json"))
	manifestBefore := mustReadEvalFile(t, filepath.Join(replayCase.Dir, "manifest.json"))
	replayPath := filepath.Join(replayCase.Dir, "config", "replay.json")
	replayConfigBefore := mustReadEvalFile(t, replayPath)
	if replayConfigBefore != string(replayConfigJSON) {
		t.Fatalf("replay config = %s, want %s", replayConfigBefore, replayConfigJSON)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(replayPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("replay config mode = %o, want 600", info.Mode().Perm())
		}
	}

	opts := ReplayOptions{Set: "labeled", Candidate: Candidate{Agent: types.AgentClaude, Model: "test", Vendor: "test"}, Repeats: 1}
	firstSession, firstEvals, err := Replay(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, secondEvals, err := Replay(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}

	if firstSession.ID == secondSession.ID {
		t.Fatalf("both replays reused session %q, want distinct measurement sessions", firstSession.ID)
	}
	if firstSession.Cohort != secondSession.Cohort {
		t.Fatalf("identical inputs produced different cohorts %q vs %q", firstSession.Cohort, secondSession.Cohort)
	}
	if len(firstEvals) != 1 || len(secondEvals) != 1 {
		t.Fatalf("evaluations = %d and %d, want 1 each", len(firstEvals), len(secondEvals))
	}
	a, b := firstEvals[0], secondEvals[0]
	if a.TruePositive != b.TruePositive || a.FalseNegative != b.FalseNegative || a.FalsePositive != b.FalsePositive || a.Pending != b.Pending {
		t.Fatalf("replay scores were not deterministic: %#v vs %#v", a, b)
	}
	if got := mustReadEvalFile(t, filepath.Join(replayCase.Dir, "labels.json")); got != labelsBefore {
		t.Fatalf("replay changed labels.json:\nbefore: %s\nafter: %s", labelsBefore, got)
	}
	if got := mustReadEvalFile(t, filepath.Join(replayCase.Dir, "manifest.json")); got != manifestBefore {
		t.Fatalf("replay changed manifest.json:\nbefore: %s\nafter: %s", manifestBefore, got)
	}
	if got := mustReadEvalFile(t, replayPath); got != replayConfigBefore {
		t.Fatalf("replay changed replay.json:\nbefore: %s\nafter: %s", replayConfigBefore, got)
	}
	var reservations int
	if err := store.db.QueryRow(`SELECT count(*) FROM replay_case_reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("replays left %d stale case reservations", reservations)
	}
}

func TestReportTwiceRendersIdentically(t *testing.T) {
	store := openEvalStore(t)
	c := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "reported", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "a", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "a", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "a", Severity: "error", File: "main.go", Line: 1, Description: "a", Action: "auto-fix"}),
	})
	if err := store.persistEvaluation(c, Evaluation{
		ID: "evaluation", SessionID: "session", CaseID: c.ID, Candidate: "claude+test",
		Repeat: 1, Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := RenderReport(second), RenderReport(first); got != want {
		t.Fatalf("report changed between identical reads:\nfirst: %s\nsecond: %s", want, got)
	}
}

type diversifiedPinRow struct {
	CaseID   string
	Stratum  string
	Rank     int
	PinnedAt int64
}

func mustDiversifiedPinRows(t *testing.T, store *Store) []diversifiedPinRow {
	t.Helper()
	rows, err := store.db.Query(`SELECT case_id, stratum, rank, pinned_at FROM diversified_pins ORDER BY case_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []diversifiedPinRow
	for rows.Next() {
		var row diversifiedPinRow
		if err := rows.Scan(&row.CaseID, &row.Stratum, &row.Rank, &row.PinnedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustReadEvalFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
