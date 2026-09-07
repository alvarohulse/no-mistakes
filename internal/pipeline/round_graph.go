package pipeline

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func roundEvaluationKind(stepName types.StepName, fixing bool) string {
	switch stepName {
	case types.StepReview:
		if fixing {
			return db.RoundEvaluationRereview
		}
		return db.RoundEvaluationInitialReview
	case types.StepDocument:
		return db.RoundEvaluationDocumentation
	default:
		if fixing {
			return db.RoundEvaluationRevalidation
		}
		return db.RoundEvaluationValidation
	}
}

func structuredRoundEvaluation(roundID, runID string, stepName types.StepName, fixing bool, raw string) (db.StepRoundEvaluation, error) {
	evaluation := db.StepRoundEvaluation{RunID: runID, RoundID: roundID, Kind: roundEvaluationKind(stepName, fixing)}
	if strings.TrimSpace(raw) == "" {
		return evaluation, nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return db.StepRoundEvaluation{}, fmt.Errorf("parse structured round findings: %w", err)
	}
	evaluation.Summary = findings.Summary
	evaluation.Tested = append([]string(nil), findings.Tested...)
	evaluation.TestingSummary = findings.TestingSummary
	evaluation.RiskLevel = findings.RiskLevel
	evaluation.RiskRationale = findings.RiskRationale
	evaluation.RiskScope = findings.RiskScope
	for ordinal, artifact := range findings.Artifacts {
		evaluation.Artifacts = append(evaluation.Artifacts, db.StepRoundEvaluationArtifact{
			Ordinal: ordinal, Kind: artifact.Kind, Label: artifact.Label, Path: artifact.Path, URL: artifact.URL, Content: artifact.Content,
		})
	}
	for ordinal, item := range findings.Items {
		action := item.ActionOrDefault()
		evaluation.Findings = append(evaluation.Findings, db.StepRoundFinding{
			Ordinal: ordinal, ExternalID: item.ID, Severity: item.Severity, File: item.File, Line: item.Line,
			Description: item.Description, Action: action, Source: item.Source, UserInstructions: item.UserInstructions,
			ReviewScope: item.ReviewScope, RequiresHumanReview: action == types.ActionAskUser,
		})
	}
	return evaluation, nil
}

func roundStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
