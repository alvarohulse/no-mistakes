package stats

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

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
