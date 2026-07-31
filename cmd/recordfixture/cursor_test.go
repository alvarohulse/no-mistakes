package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureCursorUsesManagedStreamContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture recorder shell stub is Unix-only")
	}
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "out.jsonl")
	argsPath := filepath.Join(tmp, "args.txt")
	promptPath := filepath.Join(tmp, "prompt.txt")
	binPath := filepath.Join(tmp, "cursor-agent")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
cat > "$PROMPT_FILE"
printf '%s\n' '{"type":"system","subtype":"init","cwd":"/tmp/recordcursor-test"}'
printf '%s\n' '{"type":"tool_call","subtype":"started","call_id":"read-1","tool_call":{"readToolCall":{"args":{"path":"/tmp/probe.txt"}}}}'
printf '%s\n' '{"type":"tool_call","subtype":"completed","call_id":"read-1","tool_call":{"readToolCall":{"result":{"success":{"content":"FIXTURE_REPO_ACCESS"}}}}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"OK"}'
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cursor-agent: %v", err)
	}
	t.Setenv("ARGS_FILE", argsPath)
	t.Setenv("PROMPT_FILE", promptPath)

	if err := captureCursor(t.Context(), binPath, []string{"--model", "gpt-5.2"}, "prompt text", outPath); err != nil {
		t.Fatalf("captureCursor: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	joined := string(args)
	for _, want := range []string{"--model\ngpt-5.2\n", "-p\n", "--output-format\nstream-json\n", "--workspace\n", "--add-dir\n", "--force\n", "--trust\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q:\n%s", want, joined)
		}
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	for _, want := range []string{"prompt text", "file-read tool", "probe.txt"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("stdin prompt missing %q: %q", want, prompt)
		}
	}
	fixture, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	for _, want := range []string{`"type":"system"`, `"type":"tool_call"`, `"type":"result"`} {
		if !strings.Contains(string(fixture), want) {
			t.Fatalf("fixture missing %q:\n%s", want, fixture)
		}
	}
}

func TestValidateCursorFixtureRejectsStartedOnlyToolCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.jsonl")
	fixture := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"tool_call","subtype":"started","tool_call":{"readToolCall":{}}}`,
		`{"type":"result","subtype":"success"}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := validateCursorFixture(path); err == nil {
		t.Fatal("validateCursorFixture() accepted a started-only tool call")
	}
}
