package stats

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTextJSONAndCSVProjectTheSameSelectedFacts(t *testing.T) {
	database, run := newAuditRun(t)
	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	jsonOutput, err := report.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	textOutput := RenderText(report)
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": jsonOutput, "text": textOutput, "csv": csvOutput} {
		if !strings.Contains(output, run.ID) || !strings.Contains(output, string(types.RunCompleted)) {
			t.Fatalf("%s projection omitted selected run facts:\n%s", name, output)
		}
	}
	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{"schema_version", "repo_id", "run_id", "record_type", "entity_id", "section", "group", "metric", "value", "unit", "reported", "eligible", "complete", "basis", "reason"}
	if strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("CSV header = %v", rows[0])
	}
}

func TestTextJSONAndCSVProjectContentFreeCommandAndRepairFacts(t *testing.T) {
	database, run := newAuditRun(t)
	step, err := database.InsertStepResult(run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(step.ID, 1, "auto_fix", nil, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundRepairAudit(round.ID, "sha256:repair-fingerprint", "resolved"); err != nil {
		t.Fatal(err)
	}
	zero := 0
	version := "5.2.26"
	const privateCommand = "/private/worktree/COMMAND-MUST-NOT-BE-PROJECTED"
	if err := database.SetStepEvidence(step.ID, db.StepEvidence{Commands: []db.CommandEvidence{
		{
			Round: 1, Sequence: 1, Command: privateCommand, Outcome: db.CommandOutcomePassed, ExitCode: &zero,
			CommandSource: runner.SourceLinux,
			Runner: &runner.Provenance{
				SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceLinux,
				Executable: "zsh", Args: []string{"-lc"}, Version: &version,
			},
		},
		{Round: 1, Sequence: 2, Command: privateCommand, Outcome: db.CommandOutcomeError},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 25, ""); err != nil {
		t.Fatal(err)
	}

	report, err := BuildReport(database, Query{RunID: run.ID}, time.Unix(run.CreatedAt+1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Repairs) != 1 || report.Repairs[0].RepairFailureFingerprint == nil || *report.Repairs[0].RepairFailureFingerprint != "sha256:repair-fingerprint" || report.Repairs[0].RepairResult == nil || *report.Repairs[0].RepairResult != "resolved" {
		t.Fatalf("report repairs = %+v", report.Repairs)
	}
	jsonOutput, err := report.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	textOutput := RenderText(report)
	csvOutput, err := RenderCSV(report)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": jsonOutput, "text": textOutput, "csv": csvOutput} {
		if strings.Contains(output, privateCommand) {
			t.Fatalf("%s projection retained command content:\n%s", name, output)
		}
	}
	for _, want := range []string{"failure_fingerprint=sha256:repair-fingerprint", "result=resolved", "command " + run.ID + "/build/1/1", "runner=zsh", "command " + run.ID + "/build/1/2", "runner=—"} {
		if !strings.Contains(textOutput, want) {
			t.Fatalf("text projection omitted %q:\n%s", want, textOutput)
		}
	}
	if !strings.Contains(jsonOutput, `"repair_failure_fingerprint":"sha256:repair-fingerprint"`) || !strings.Contains(jsonOutput, `"runner":{"schema_version":1`) || !strings.Contains(jsonOutput, `"runner":null`) {
		t.Fatalf("JSON projection omitted audit facts:\n%s", jsonOutput)
	}

	rows, err := csv.NewReader(strings.NewReader(csvOutput)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	commandFacts := map[string]map[string][]string{}
	repairFacts := map[string]string{}
	for _, row := range rows[1:] {
		switch row[3] {
		case "command":
			if commandFacts[row[4]] == nil {
				commandFacts[row[4]] = map[string][]string{}
			}
			commandFacts[row[4]][row[7]] = row
		case "repair":
			repairFacts[row[7]] = row[8]
		}
	}
	firstID := step.ID + ":1:1"
	secondID := step.ID + ":1:2"
	if commandFacts[firstID]["outcome"][8] != db.CommandOutcomePassed || commandFacts[firstID]["command_source"][8] != runner.SourceLinux || commandFacts[firstID]["runner_executable"][8] != "zsh" || commandFacts[firstID]["runner_args"][8] != `["-lc"]` {
		t.Fatalf("first command CSV facts = %+v", commandFacts[firstID])
	}
	if commandFacts[secondID]["outcome"][8] != db.CommandOutcomeError || commandFacts[secondID]["runner_executable"][10] != "0" || commandFacts[secondID]["runner_executable"][12] != "false" || commandFacts[secondID]["runner_executable"][14] != "not_reported" {
		t.Fatalf("runner-less command CSV facts = %+v", commandFacts[secondID])
	}
	if repairFacts["failure_fingerprint"] != "sha256:repair-fingerprint" || repairFacts["result"] != "resolved" {
		t.Fatalf("repair CSV facts = %+v", repairFacts)
	}
}
