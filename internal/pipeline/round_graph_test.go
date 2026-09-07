package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStructuredRoundEvaluationClassifiesAndPreservesOrderedFindings(t *testing.T) {
	tests := []struct {
		name   string
		step   types.StepName
		fixing bool
		want   string
	}{
		{name: "initial review", step: types.StepReview, want: db.RoundEvaluationInitialReview},
		{name: "rereview", step: types.StepReview, fixing: true, want: db.RoundEvaluationRereview},
		{name: "validation", step: types.StepBuild, want: db.RoundEvaluationValidation},
		{name: "revalidation", step: types.StepTest, fixing: true, want: db.RoundEvaluationRevalidation},
		{name: "documentation", step: types.StepDocument, want: db.RoundEvaluationDocumentation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := structuredRoundEvaluation("round-1", "run-1", test.step, test.fixing, `{"findings":[{"id":"first","description":"one","action":"auto-fix"},{"id":"second","description":"two","action":"ask-user"}],"tested":["go test ./internal/pipeline"],"testing_summary":"targeted","artifacts":[{"kind":"html","label":"local evidence","path":"evidence/result.html"},{"kind":"link","label":"hosted evidence","url":"https://example.com/result"},{"kind":"log","label":"inline transcript","content":"result passed"}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if evaluation.Kind != test.want || len(evaluation.Findings) != 2 {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if evaluation.Findings[0].Ordinal != 0 || evaluation.Findings[0].ExternalID != "first" || evaluation.Findings[1].Ordinal != 1 || evaluation.Findings[1].ExternalID != "second" {
				t.Fatalf("finding order = %#v", evaluation.Findings)
			}
			if !evaluation.Findings[1].RequiresHumanReview || len(evaluation.Tested) != 1 || evaluation.TestingSummary != "targeted" {
				t.Fatalf("evaluation details = %#v", evaluation)
			}
			if len(evaluation.Artifacts) != 3 || evaluation.Artifacts[0].Path != "evidence/result.html" || evaluation.Artifacts[1].URL != "https://example.com/result" || evaluation.Artifacts[2].Content != "result passed" {
				t.Fatalf("evaluation artifacts = %#v", evaluation.Artifacts)
			}
		})
	}
}
