package eval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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

func mustReadEvalFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
