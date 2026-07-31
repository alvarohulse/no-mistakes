package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
)

const cursorScannerMaxTokenSize = 256 * 1024 * 1024

type cursorEvent struct {
	Type      string                     `json:"type"`
	Subtype   string                     `json:"subtype"`
	SessionID string                     `json:"session_id"`
	Model     string                     `json:"model"`
	IsError   bool                       `json:"is_error"`
	Result    string                     `json:"result"`
	CallID    string                     `json:"call_id"`
	Message   cursorMessage              `json:"message"`
	ToolCall  map[string]json.RawMessage `json:"tool_call"`
	Usage     *cursorUsage               `json:"usage"`
}

type cursorMessage struct {
	Content []cursorContent `json:"content"`
}

type cursorContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type cursorUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

type cursorParsedResult struct {
	Text      string
	SessionID string
	Model     string
	Subtype   string
	IsError   bool
	Usage     TokenUsage
	PlainText string
	Terminal  bool
}

func parseCursorEvents(
	ctx context.Context,
	r io.Reader,
	onChunk func(string),
	onEvent func(*cursorEvent, time.Time),
) (*cursorParsedResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), cursorScannerMaxTokenSize)
	var parsed cursorParsedResult
	var plainText strings.Builder

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event cursorEvent
		if err := json.Unmarshal(line, &event); err != nil {
			if plainText.Len() > 0 {
				plainText.WriteByte('\n')
			}
			plainText.Write(line)
			continue
		}
		if onEvent != nil {
			onEvent(&event, time.Now())
		}
		if event.SessionID != "" {
			parsed.SessionID = event.SessionID
		}
		if event.Model != "" {
			parsed.Model = event.Model
		}
		switch event.Type {
		case "assistant":
			for _, content := range event.Message.Content {
				if content.Type == "text" && content.Text != "" && onChunk != nil {
					onChunk(content.Text)
				}
			}
		case "result":
			parsed.Terminal = true
			parsed.Text = event.Result
			parsed.Subtype = event.Subtype
			parsed.IsError = event.IsError
			if event.Usage != nil {
				parsed.Usage = TokenUsage{
					InputTokens:           event.Usage.InputTokens,
					OutputTokens:          event.Usage.OutputTokens,
					CacheReadTokens:       event.Usage.CacheReadTokens,
					CacheCreationTokens:   event.Usage.CacheWriteTokens,
					Reported:              true,
					CacheCreationReported: true,
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	parsed.PlainText = plainText.String()
	return &parsed, nil
}
