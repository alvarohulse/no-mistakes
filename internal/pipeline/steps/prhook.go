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

// applyPRBodyHook selects external owned-section patches instead of the
// built-in generated section when hooks.pr_body is configured.
//
// Every formatter failure selects built-in section content and says so in the
// pipeline log. Publication still fails closed when an existing body lacks the
// matching verified marker set; fallback never authorizes a full-body rewrite.
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
