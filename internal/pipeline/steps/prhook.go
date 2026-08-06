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

	contract := buildPRBodyContract(sctx, records, whatChanged, content.Title, scope)
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

	content.Body = clampHookPRBody(sctx, result.Body, scope.bodyLimit)
	sctx.Log(fmt.Sprintf("pr_body hook formatted the body in %s", result.Duration.Round(1e6)))
	return content
}

// clampHookPRBody enforces the host caps on a formatter's output. The contract
// tells the formatter what the caps are so it can degrade its own layout
// deliberately; this is the backstop for one that did not.
func clampHookPRBody(sctx *pipeline.StepContext, body string, bodyLimit int) string {
	if bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit {
		sctx.Log(fmt.Sprintf("pr_body hook returned %d characters against the host's %d limit; clamping",
			scm.PRBodyLen(body), bodyLimit))
		body = scm.ClampPRBody(body, bodyLimit)
	}
	if len(body) > maxPullRequestBodyBytes {
		sctx.Log(fmt.Sprintf("pr_body hook returned %d bytes against the %d byte cap; truncating",
			len(body), maxPullRequestBodyBytes))
		body = truncateTextAtLineBoundary(body, maxPullRequestBodyBytes, essentialPRBodyTruncationMarker())
	}
	return body
}
