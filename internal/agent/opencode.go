package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// opencodeAgent starts a persistent HTTP server via `opencode serve`
// and sends requests via REST with SSE streaming.
type opencodeAgent struct {
	bin       string
	extraArgs []string
	model     string
	effort    Effort
	mu        sync.Mutex
	server    *managedServer
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyTransient, a.recoverTransientRetry, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *opencodeAgent) recoverTransientRetry(label string) {
	if label != "connection refused" {
		return
	}
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
}

func (a *opencodeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD, opts.Env)
	if err != nil {
		return nil, err
	}

	// Create session with blanket permissions
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// Build prompt with schema instructions if provided
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	// Connect to SSE event stream
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	// Send message concurrently — blocks until agent completes
	type messageResult struct {
		resp *opencodeMessageResponse
		err  error
	}
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan messageResult, 1)
	go func() {
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, opts.JSONSchema)
		msgCh <- messageResult{resp: resp, err: err}
	}()

	// Process SSE events until session.idle
	state := &opencodeStreamState{
		sessionID:    sessionID,
		onChunk:      opts.OnChunk,
		textParts:    make(map[string]*opencodeTextPart),
		usageByMsg:   make(map[string]TokenUsage),
		observations: newAgentObservationCollector(true),
	}
	err = parseOpencodeSSE(eventBody, state)
	streamCancel()

	if err != nil {
		// Check if message request failed
		select {
		case mr := <-msgCh:
			if mr.err != nil {
				return opencodePartialResult(state), fmt.Errorf("opencode message: %w", mr.err)
			}
			foldOpencodeMessageResponse(state, mr.resp)
		default:
		}
		a.abortSession(baseURL, sessionID)
		return opencodePartialResult(state), fmt.Errorf("opencode events: %w", err)
	}

	// Wait for message response
	mr := <-msgCh
	if mr.err != nil {
		return opencodePartialResult(state), fmt.Errorf("opencode message: %w", mr.err)
	}

	foldOpencodeMessageResponse(state, mr.resp)

	// Prefer structured output from response
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Structured != nil {
		result := opencodePartialResult(state)
		result.Output = mr.resp.Info.Structured
		result.UsageCoverage = opencodeUsageCoverage(state)
		return result, nil
	}

	// Surface opencode's StructuredOutputError directly. When the model
	// fails to call the StructuredOutput tool after the configured retries,
	// opencode sets info.error.name = "StructuredOutputError" and the
	// streamed text is just reasoning prose - feeding it to
	// finalizeTextResult produces the misleading "invalid character 'N'
	// looking for beginning of value" error.
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Error.IsStructuredOutput() {
		retries := 0
		if mr.resp.Info.Error.Retries != nil {
			retries = *mr.resp.Info.Error.Retries
		}
		return opencodePartialResult(state), fmt.Errorf("opencode structured output failed after %d internal retries: %s",
			retries, mr.resp.Info.Error.Message)
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	result, err := finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
	if result != nil {
		result.UsageCoverage = opencodeUsageCoverage(state)
		result.AgentObservations = state.observations.observations
		result.AgentObservationsReported = true
		result.NestedAgentCount = state.observations.uniqueCount()
	}
	return result, err
}

func opencodeUsageCoverage(state *opencodeStreamState) UsageCoverage {
	if state == nil {
		return UsageCoverageUnknown
	}
	if !state.reachedIdle || len(state.assistantMsgIDs) == 0 {
		return UsageCoverageUnknown
	}
	for messageID := range state.assistantMsgIDs {
		usage, ok := state.usageByMsg[messageID]
		if !ok || !usage.Reported {
			return UsageCoverageUnknown
		}
	}
	return usageCoverageForCompleteStream(true, state.observations.uniqueCount() > 0)
}

func foldOpencodeMessageResponse(state *opencodeStreamState, response *opencodeMessageResponse) {
	responseText := ""
	responseFinalText := ""
	if response != nil && response.Info != nil {
		streamedText := state.lastText
		streamedFinalText := state.lastFinalText
		emitResponseChunk := func(chunk string) {
			if state.onChunk == nil || chunk == "" {
				return
			}
			state.emitSeparatorIfNeeded()
			state.onChunk(chunk)
			state.hasEmittedText = true
		}
		if response.Info.Role == "assistant" && response.Info.Tokens != nil {
			state.usageByMsg[response.Info.ID] = opencodeTokensToUsage(response.Info.Tokens)
			state.usage = accumulateUsage(state.usageByMsg)
		}
		for _, part := range response.Parts {
			state.observeOpencodeTool(part.ID, part.Type, part.Tool, part.State)
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			responseText += part.Text
			if part.Metadata != nil && part.Metadata.OpenAI != nil && part.Metadata.OpenAI.Phase == "final_answer" {
				responseFinalText += part.Text
			}
		}
		if responseText != "" {
			state.lastText = responseText
		}
		if responseFinalText != "" {
			state.lastFinalText = responseFinalText
		}
		if responseFinalText != "" {
			responseText = responseFinalText
		}
		if state.onChunk != nil && responseText != "" {
			streamedResponseText := streamedText
			if streamedFinalText != "" {
				streamedResponseText = streamedFinalText
			}
			switch {
			case !state.hasEmittedText:
				emitResponseChunk(responseText)
			case streamedResponseText == "":
				emitResponseChunk(responseText)
			case strings.HasPrefix(responseText, streamedResponseText):
				suffix := responseText[len(streamedResponseText):]
				emitResponseChunk(suffix)
			}
		}
	}
}

func opencodePartialResult(state *opencodeStreamState) *Result {
	if state == nil {
		return nil
	}
	return &Result{
		Text:                      state.lastText,
		Usage:                     state.usage,
		UsageReported:             state.usage.Reported,
		UsageCoverage:             UsageCoverageUnknown,
		CacheCreationReported:     state.usage.CacheCreationReported,
		AgentObservations:         state.observations.observations,
		AgentObservationsReported: true,
		NestedAgentCount:          state.observations.uniqueCount(),
	}
}

func (a *opencodeAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}
	return nil
}
