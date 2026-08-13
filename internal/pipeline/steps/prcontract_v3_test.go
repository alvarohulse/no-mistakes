package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestContractV3CarriesDistinctSummaryAndWhatChanged(t *testing.T) {
	t.Parallel()

	contract := BuildContract(ContractInput{
		Summary:     "Fixes stale alerts by calling `AlertMessage.close` after recheck.",
		WhatChanged: "- Close the alert after a successful recheck",
	})

	if contract.Version != 3 {
		t.Fatalf("version = %d, want 3", contract.Version)
	}
	if contract.Sections.Summary == nil || contract.Sections.Summary.Text != "Fixes stale alerts by calling `AlertMessage.close` after recheck." {
		t.Fatalf("summary = %+v", contract.Sections.Summary)
	}
	if contract.Sections.WhatChanged == nil || contract.Sections.WhatChanged.Text != "- Close the alert after a successful recheck" {
		t.Fatalf("what_changed = %+v", contract.Sections.WhatChanged)
	}
}

func TestContractV3PlacesIntentOnTheIntentStep(t *testing.T) {
	t.Parallel()

	contract := BuildContract(ContractInput{
		Run: &db.Run{ID: "run-1"},
		Steps: []*db.StepResult{{
			ID: "intent-step", StepName: types.StepIntent, StepOrder: 1, Status: types.StepStatusCompleted,
		}},
		Intent:              "Close stale alerts after recheck.",
		IntentSource:        db.RunIntentSourceAgent,
		IntentAuthoritative: true,
	})

	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) != 1 {
		t.Fatalf("pipeline = %+v", contract.Sections.Pipeline)
	}
	intent := contract.Sections.Pipeline.Steps[0].Intent
	if intent == nil || intent.Text != "Close stale alerts after recheck." || intent.Source != db.RunIntentSourceAgent || !intent.Provided {
		t.Fatalf("intent result = %+v", intent)
	}
}
