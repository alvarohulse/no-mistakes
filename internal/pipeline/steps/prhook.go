package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	prBodyAttributionName = "no-mistakes"
	prBodyAttributionURL  = "https://github.com/kunchenguid/no-mistakes"
)

// prBodyScope is the PR's placement: which branch onto which base, on which
// host, under what body cap.
type prBodyScope struct {
	branch     string
	baseBranch string
	baseSHA    string
	provider   string
	bodyLimit  int
}

// applyPRBodyHook replaces the built-in body with an external formatter's,
// when hooks.pr_body is configured.
//
// Every failure returns the built-in body and says so in the pipeline log. A
// formatter is a convenience; a PR that cannot be described is not a reason to
// stop shipping, and a body that silently lost its template is worse than one
// that never had it.
func applyPRBodyHook(sctx *pipeline.StepContext, records RunRecords, content prContent, whatChanged string, scope prBodyScope) prContent {
	if sctx == nil || sctx.Config == nil {
		return content
	}
	hook := strings.TrimSpace(sctx.Config.Hooks.PRBody)
	if hook == "" {
		return content
	}

	contract := buildPRBodyContract(sctx, records, content.Summary, whatChanged, content.Title, scope)
	result, err := prbody.RunHook(sctx.Ctx, prbody.HookOptions{
		Command:  hook,
		Dir:      sctx.WorkDir,
		Contract: contract,
		Grace:    sctx.Config.ProcessTerminationGrace,
		Env:      sctx.Env,
	})
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: %v; using the built-in PR body", err))
		return content
	}
	if result.Diagnostics != "" {
		sctx.Log("pr_body hook: " + result.Diagnostics)
	}

	body, err := prbody.NewOwnedDocument(result.Patches)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: pr_body hook returned invalid patches: %v; using the built-in PR body", err))
		return content
	}
	if err := prbody.ValidateOwnedDocument(body, prbody.ValidationLimits{
		MaxBytes:     maxPullRequestBodyBytes,
		MaxUnits:     scope.bodyLimit,
		MeasureUnits: scm.PRBodyLen,
	}); err != nil {
		sctx.Log(fmt.Sprintf("warning: pr_body hook returned an unsafe candidate: %v; using the built-in PR body", err))
		return content
	}
	content.Body = body
	content.OwnedPatches = result.Patches
	sctx.Log(fmt.Sprintf("pr_body hook formatted the body in %s", result.Duration.Round(1e6)))
	return content
}
