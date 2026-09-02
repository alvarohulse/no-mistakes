package db

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunNarrativeRoundTripAndWriteOnce(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/run-narrative", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: run.ID, StepName: "pr", Round: 1, Purpose: "pr", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 10, CompletedAt: 11, DurationMS: 1000, ExitStatus: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}

	narrative := RunNarrative{
		RunID: run.ID, Source: NarrativeSourceAgent, DraftingInvocationID: &invocation.ID,
		DraftedAt: 12, BaseSHA: "base-1", HeadSHA: "head-1",
		TitleMode: NarrativeTitleModeAgent, TitleText: "feat(pipeline): persist PR narratives",
		Summary: "Persist one narrative for the run.", WhatChanged: "- Reuse the saved draft.",
	}
	if err := d.InsertRunNarrative(narrative); err != nil {
		t.Fatalf("InsertRunNarrative() error = %v", err)
	}
	got, err := d.GetRunNarrative(run.ID)
	if err != nil {
		t.Fatalf("GetRunNarrative() error = %v", err)
	}
	if got == nil || !reflect.DeepEqual(*got, narrative) {
		t.Fatalf("GetRunNarrative() = %#v, want %#v", got, narrative)
	}

	replacement := narrative
	replacement.Summary = "A later retry tried to replace the draft."
	if err := d.InsertRunNarrative(replacement); err == nil {
		t.Fatal("second narrative insert succeeded; narrative must be write-once per run")
	}
	got, err = d.GetRunNarrative(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Summary != narrative.Summary {
		t.Fatalf("duplicate insert replaced narrative: %#v", got)
	}
}

func TestRunNarrativeFallbackProvenanceAndMissingRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/fallback-narrative", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head-1", "base-1")

	narrative := RunNarrative{
		RunID: run.ID, Source: NarrativeSourceFallback, DraftedAt: 12,
		BaseSHA: "base-1", HeadSHA: "head-1", TitleMode: NarrativeTitleModeAgent,
		TitleText: "chore: update pull request", Summary: "Fallback summary.", WhatChanged: "- fallback change",
	}
	if err := d.InsertRunNarrative(narrative); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRunNarrative(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Source != NarrativeSourceFallback || got.DraftingInvocationID != nil {
		t.Fatalf("fallback provenance = %#v", got)
	}
	missing, err := d.GetRunNarrative("missing-run")
	if err != nil || missing != nil {
		t.Fatalf("missing narrative = %#v, %v; want nil, nil", missing, err)
	}
}

func TestRunNarrativeRejectsAgentSourceWithoutInvocation(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/missing-narrative-invocation", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head-1", "base-1")

	err := d.InsertRunNarrative(RunNarrative{
		RunID: run.ID, Source: NarrativeSourceAgent, DraftedAt: 12,
		BaseSHA: "base-1", HeadSHA: "head-1", TitleMode: NarrativeTitleModeAgent,
		TitleText: "feat: unprovenanced draft", Summary: "Summary.", WhatChanged: "- Change.",
	})
	if err == nil || !strings.Contains(err.Error(), "drafting invocation is required") {
		t.Fatalf("InsertRunNarrative() error = %v, want missing invocation rejection", err)
	}
}

func TestRunNarrativeRejectsAgentInvocationFromAnotherRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/cross-run-narrative", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head-1", "base-1")
	otherRun, _ := d.InsertRun(repo.ID, "other-feature", "head-2", "base-1")
	invocation, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: otherRun.ID, StepName: "pr", Round: 1, Purpose: "pr", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 10, CompletedAt: 11, DurationMS: 1000, ExitStatus: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = d.InsertRunNarrative(RunNarrative{
		RunID: run.ID, Source: NarrativeSourceAgent, DraftingInvocationID: &invocation.ID, DraftedAt: 12,
		BaseSHA: "base-1", HeadSHA: "head-1", TitleMode: NarrativeTitleModeAgent,
		TitleText: "feat: cross-run draft", Summary: "Summary.", WhatChanged: "- Change.",
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong to run") {
		t.Fatalf("InsertRunNarrative() error = %v, want cross-run invocation rejection", err)
	}
}

func TestOpenMigratesRunNarratives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
	`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer d.Close()
	var table string
	if err := d.sql.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'run_narratives'`).Scan(&table); err != nil {
		t.Fatalf("run_narratives table missing after migration: %v", err)
	}
}
