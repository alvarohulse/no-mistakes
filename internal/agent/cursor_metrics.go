package agent

import (
	"encoding/json"
	"strings"
	"time"
)

type cursorMetricsAccumulator struct {
	modelRoundtrips  int
	toolCalls        int
	categories       ToolCategoryCounts
	subprocessWaitMS int64
	starts           map[string]time.Time
}

func newCursorMetricsAccumulator() *cursorMetricsAccumulator {
	return &cursorMetricsAccumulator{starts: map[string]time.Time{}}
}

func (m *cursorMetricsAccumulator) onEvent(event *cursorEvent, at time.Time) {
	if m == nil || event == nil {
		return
	}
	switch event.Type {
	case "assistant":
		m.modelRoundtrips++
	case "tool_call":
		if event.Subtype == "started" {
			m.starts[event.CallID] = at
			return
		}
		if event.Subtype != "completed" {
			return
		}
		m.modelRoundtrips++
		m.toolCalls++
		for _, category := range cursorToolCategories(event.ToolCall) {
			m.categories.Add(category)
		}
		if start, ok := m.starts[event.CallID]; ok {
			if duration := at.Sub(start).Milliseconds(); duration > 0 {
				m.subprocessWaitMS += duration
			}
			delete(m.starts, event.CallID)
		}
	}
}

func (m *cursorMetricsAccumulator) metrics() InvocationMetrics {
	return InvocationMetrics{
		ModelRoundtrips:  m.modelRoundtrips,
		ToolCalls:        m.toolCalls,
		ToolCategories:   m.categories,
		SubprocessWaitMS: m.subprocessWaitMS,
	}
}

func cursorToolCategories(toolCall map[string]json.RawMessage) []ToolCategory {
	for name, raw := range toolCall {
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, "toolcall") {
			continue
		}
		switch {
		case strings.Contains(lower, "read"), strings.Contains(lower, "grep"), strings.Contains(lower, "search"), strings.Contains(lower, "list"):
			return []ToolCategory{ToolRead}
		case strings.Contains(lower, "write"), strings.Contains(lower, "edit"), strings.Contains(lower, "delete"), strings.Contains(lower, "patch"):
			return []ToolCategory{ToolEdit}
		case strings.Contains(lower, "shell"), strings.Contains(lower, "terminal"), strings.Contains(lower, "command"):
			var call struct {
				Args struct {
					Command string `json:"command"`
				} `json:"args"`
			}
			if json.Unmarshal(raw, &call) == nil {
				if categories := ClassifyToolCommand(call.Args.Command); len(categories) > 0 {
					return categories
				}
			}
			return []ToolCategory{ToolOther}
		default:
			return []ToolCategory{ToolOther}
		}
	}
	return []ToolCategory{ToolOther}
}
