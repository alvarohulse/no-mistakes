//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorAgent_RunUsesStdinAndContainedWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(repo, "AGENTS.md"):                     "NM_UNTRUSTED_AGENTS",
		filepath.Join(repo, ".cursorrules"):                  "NM_UNTRUSTED_LEGACY",
		filepath.Join(repo, ".cursor", "rules", "audit.mdc"): "NM_UNTRUSTED_MDC",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	argsFile := filepath.Join(dir, "args.txt")
	cwdFile := filepath.Join(dir, "cwd.txt")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("NM_CURSOR_ARGS", argsFile)
	t.Setenv("NM_CURSOR_CWD", cwdFile)
	t.Setenv("NM_CURSOR_PROMPT", promptFile)
	script := filepath.Join(dir, "cursor-agent")
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$NM_CURSOR_ARGS"
previous=""
workspace=""
for arg in "$@"; do
  if [ "$previous" = "--workspace" ]; then workspace="$arg"; fi
  previous="$arg"
done
test -n "$workspace"
test ! -e "$workspace/AGENTS.md"
test ! -e "$workspace/.cursorrules"
test ! -e "$workspace/.cursor/rules"
pwd > "$NM_CURSOR_CWD"
cat > "$NM_CURSOR_PROMPT"
printf '%s\n' '{"type":"system","subtype":"init","cwd":"contained","session_id":"cursor-session","model":"GPT-5.6 Sol"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"cursor-session"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"cursor-session","usage":{"inputTokens":12,"outputTokens":3,"cacheReadTokens":4,"cacheWriteTokens":1}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &cursorAgent{bin: script, model: "gpt-5.6-sol"}
	result, err := a.Run(context.Background(), RunOpts{Prompt: "review this repository", CWD: repo})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "done" || result.SessionID != "cursor-session" || result.Model != "gpt-5.6-sol" {
		t.Fatalf("result = %+v", result)
	}

	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	workspace := argValue(args, "--workspace")
	if workspace == "" || workspace == repo {
		t.Fatalf("workspace = %q, want separate clean directory", workspace)
	}
	if got := argValue(args, "--add-dir"); got != repo {
		t.Fatalf("--add-dir = %q, want %q", got, repo)
	}
	for _, instruction := range []string{"AGENTS.md", ".cursorrules", ".cursor/rules"} {
		if _, err := os.Stat(filepath.Join(workspace, instruction)); !os.IsNotExist(err) {
			t.Fatalf("clean workspace unexpectedly contains %s", instruction)
		}
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("clean workspace was not removed: %v", err)
	}

	cwd, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(cwd)) != workspace {
		t.Fatalf("process cwd = %q, want clean workspace %q", strings.TrimSpace(string(cwd)), workspace)
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{repo, "git -C", "review this repository"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("stdin prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestCursorAgent_CancellationWithoutTerminalResultIsCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cursor-agent")
	contents := `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"cursor-session"}'
trap 'exit 143' TERM INT
while :; do sleep 1; done
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	a := &cursorAgent{bin: script}
	go func() {
		<-started
		cancel()
	}()

	_, err := a.Run(ctx, RunOpts{
		Prompt: "review",
		CWD:    dir,
		OnLifecycle: func(event LifecycleEvent) {
			if event.Phase == LifecyclePhaseStart {
				close(started)
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "parse") || strings.Contains(err.Error(), "no result") {
		t.Fatalf("cancellation was misclassified: %v", err)
	}
}

func TestCursorAgent_SignalExitWithoutTerminalResultIsCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cursor-agent")
	contents := `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"cursor-session"}'
kill -TERM $$
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := (&cursorAgent{bin: script}).Run(context.Background(), RunOpts{Prompt: "review", CWD: dir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "parse") || strings.Contains(err.Error(), "no result") {
		t.Fatalf("signal cancellation was misclassified: %v", err)
	}
}

func TestCursorAgent_PlainTextPreStreamFailureIsAnExitError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'invalid model selection\\n'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := (&cursorAgent{bin: script}).Run(context.Background(), RunOpts{Prompt: "review", CWD: dir})
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	for _, want := range []string{"cursor exited", "invalid model selection"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run() error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "parse events") {
		t.Fatalf("plain-text pre-stream failure was misclassified: %v", err)
	}
}

func TestCursorAgent_TerminalErrorPreservesTransientResult(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit_%d", exitCode), func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "cursor-agent")
			contents := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"429 rate_limited: retry later"}'
exit %d
`, exitCode)
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}

			_, err := (&cursorAgent{bin: script}).runOnce(context.Background(), RunOpts{Prompt: "review", CWD: dir})
			if err == nil {
				t.Fatal("runOnce() error = nil")
			}
			if !strings.Contains(err.Error(), "429 rate_limited") {
				t.Fatalf("runOnce() discarded terminal result: %v", err)
			}
			if _, ok := classifyTransient(err); !ok {
				t.Fatalf("runOnce() error is not classified as transient: %v", err)
			}
		})
	}
}

func TestCursorAgent_ResumeKeepsPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("NM_CURSOR_ARGS", argsFile)
	t.Setenv("NM_CURSOR_PROMPT", promptFile)
	script := filepath.Join(dir, "cursor-agent")
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$NM_CURSOR_ARGS"
cat > "$NM_CURSOR_PROMPT"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"session-123"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"resumed","session_id":"session-123"}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := "CURSOR_STDIN_MARKER_7f9c_"
	prompt := marker + strings.Repeat("x", 2*1024*1024)

	result, err := (&cursorAgent{bin: script}).Run(context.Background(), RunOpts{
		Prompt:  prompt,
		CWD:     dir,
		Session: &SessionRef{ID: "session-123"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Resumed || result.SessionID != "session-123" {
		t.Fatalf("result session = %q resumed=%v", result.SessionID, result.Resumed)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if got := argValue(args, "--resume"); got != "session-123" {
		t.Fatalf("--resume = %q, want session-123", got)
	}
	for _, arg := range args {
		if strings.Contains(arg, marker) {
			t.Fatalf("prompt leaked into argv: %q", arg)
		}
	}
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(promptData), prompt) {
		t.Fatal("large prompt was not preserved on stdin")
	}
}

func TestCursorAgent_StructuredOutputUsesSoftPromptContract(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("NM_CURSOR_ARGS", argsFile)
	t.Setenv("NM_CURSOR_PROMPT", promptFile)
	script := filepath.Join(dir, "cursor-agent")
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$NM_CURSOR_ARGS"
cat > "$NM_CURSOR_PROMPT"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"session-structured"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"ok\":true}","session_id":"session-structured"}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)

	result, err := (&cursorAgent{bin: script}).Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("structured output = %s", result.Output)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argsData), "json-schema") {
		t.Fatalf("Cursor has no native JSON-schema flag: %s", argsData)
	}
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no-mistakes final output contract", string(schema)} {
		if !strings.Contains(string(promptData), want) {
			t.Fatalf("soft structured-output prompt missing %q", want)
		}
	}
}
