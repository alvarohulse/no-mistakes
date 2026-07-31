package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type cursorInvocation struct {
	Prompt    string
	Workspace string
	Repo      string
	Model     string
	ResumeID  string
}

func validateCursorContainment(invocation cursorInvocation, cwd, pwd string) error {
	workspace, err := filepath.Abs(invocation.Workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	repo, err := filepath.Abs(invocation.Repo)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	actualCWD, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	if actualCWD != workspace {
		return fmt.Errorf("process cwd %q does not match clean workspace %q", actualCWD, workspace)
	}
	if strings.TrimSpace(pwd) == "" {
		return fmt.Errorf("process PWD is empty")
	}
	actualPWD, err := filepath.Abs(pwd)
	if err != nil {
		return fmt.Errorf("resolve PWD: %w", err)
	}
	if actualPWD != workspace {
		return fmt.Errorf("process PWD %q does not match clean workspace %q", actualPWD, workspace)
	}
	if repo == workspace {
		return fmt.Errorf("repo and clean workspace must be distinct")
	}
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return fmt.Errorf("added repo %q is not a directory", repo)
	}
	for _, path := range []string{"AGENTS.md", ".cursorrules", filepath.Join(".cursor", "rules")} {
		if _, err := os.Stat(filepath.Join(workspace, path)); err == nil {
			return fmt.Errorf("clean workspace contains project instruction path %q", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect clean workspace path %q: %w", path, err)
		}
	}
	return nil
}

func runCursor(args []string, promptReader io.Reader, scenario *Scenario) int {
	invocation, err := extractCursorInvocation(args, promptReader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: cursor prompt: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: cursor cwd: %v\n", err)
		return 1
	}
	if err := validateCursorContainment(invocation, cwd, os.Getenv("PWD")); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: cursor containment: %v\n", err)
		return 1
	}
	if os.Getenv("FAKE_CURSOR_EXPECT_INSTRUCTION_MARKERS") == "1" {
		if err := validateCursorInstructionMarkers(invocation.Repo); err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: cursor marker fixture: %v\n", err)
			return 1
		}
	}
	if out, err := exec.Command("git", "-C", invocation.Repo, "rev-parse", "--show-toplevel").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: cursor git target: %v: %s\n", err, out)
		return 1
	}
	logInvocation("cursor", invocation.Prompt, args)
	action := scenario.Match(invocation.Prompt)
	if err := applyActionInDir(invocation.Repo, action); err != nil {
		return 1
	}
	body, err := cursorResponseBody(action, invocation.Prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: cursor response: %v\n", err)
		return 1
	}
	sessionID := invocation.ResumeID
	if sessionID == "" {
		sessionID = "fake-cursor-session"
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"cwd":        invocation.Workspace,
		"session_id": sessionID,
		"model":      invocation.Model,
	})
	_ = enc.Encode(map[string]any{
		"type":      "tool_call",
		"subtype":   "started",
		"call_id":   "git-target",
		"tool_call": map[string]any{"shellToolCall": map[string]any{"args": map[string]string{"command": "git -C <added-root> rev-parse --show-toplevel"}}},
	})
	_ = enc.Encode(map[string]any{
		"type":      "tool_call",
		"subtype":   "completed",
		"call_id":   "git-target",
		"tool_call": map[string]any{"shellToolCall": map[string]any{"result": map[string]any{"success": true}}},
	})
	_ = enc.Encode(map[string]any{
		"type":       "assistant",
		"message":    map[string]any{"content": []any{map[string]string{"type": "text", "text": body}}},
		"session_id": sessionID,
	})
	_ = enc.Encode(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"result":     body,
		"session_id": sessionID,
		"usage": map[string]int{
			"inputTokens":      100,
			"outputTokens":     50,
			"cacheReadTokens":  0,
			"cacheWriteTokens": 0,
		},
	})
	return 0
}

func validateCursorInstructionMarkers(repo string) error {
	for _, entry := range []struct {
		path   string
		marker string
	}{
		{path: "AGENTS.md", marker: "NM_E2E_CURSOR_AGENTS"},
		{path: ".cursorrules", marker: "NM_E2E_CURSOR_LEGACY"},
		{path: filepath.Join(".cursor", "rules", "containment.mdc"), marker: "NM_E2E_CURSOR_MDC"},
	} {
		data, err := os.ReadFile(filepath.Join(repo, entry.path))
		if err != nil {
			return fmt.Errorf("read added-repo marker %s: %w", entry.path, err)
		}
		if !strings.Contains(string(data), entry.marker) {
			return fmt.Errorf("added-repo marker %s is absent", entry.marker)
		}
	}
	return nil
}

func cursorResponseBody(action Action, prompt string) (string, error) {
	const marker = "## no-mistakes final output contract"
	markerAt := strings.LastIndex(prompt, marker)
	if markerAt < 0 {
		return action.textOrDefault(), nil
	}
	start := strings.Index(prompt[markerAt:], "{")
	if start < 0 {
		return "", fmt.Errorf("structured-output contract has no schema")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(prompt[markerAt+start:]), &schema); err != nil {
		return "", fmt.Errorf("parse structured-output schema: %w", err)
	}
	if action.StructuredRaw != "" {
		return action.StructuredRaw, nil
	}
	filtered := make(map[string]any, len(schema.Properties))
	for key := range schema.Properties {
		if value, ok := action.Structured[key]; ok {
			filtered[key] = value
		}
	}
	body, err := json.Marshal(filtered)
	if err != nil {
		return "", fmt.Errorf("marshal structured output: %w", err)
	}
	return string(body), nil
}

func extractCursorInvocation(args []string, promptReader io.Reader) (cursorInvocation, error) {
	var invocation cursorInvocation
	printMode := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p", "--print":
			printMode = true
		case "--workspace":
			if i+1 < len(args) {
				i++
				invocation.Workspace = args[i]
			}
		case "--add-dir":
			if i+1 < len(args) {
				i++
				invocation.Repo = args[i]
			}
		case "--model":
			if i+1 < len(args) {
				i++
				invocation.Model = args[i]
			}
		case "--resume":
			if i+1 < len(args) {
				i++
				invocation.ResumeID = args[i]
			}
		}
	}
	if !printMode {
		return cursorInvocation{}, fmt.Errorf("missing -p")
	}
	if invocation.Workspace == "" || invocation.Repo == "" {
		return cursorInvocation{}, fmt.Errorf("missing contained --workspace/--add-dir roots")
	}
	prompt, err := io.ReadAll(promptReader)
	if err != nil {
		return cursorInvocation{}, fmt.Errorf("read stdin: %w", err)
	}
	invocation.Prompt = string(prompt)
	return invocation, nil
}
