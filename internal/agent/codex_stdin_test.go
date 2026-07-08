//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexAgent_Run_LargePromptViaStdin is the regression test for the E2BIG
// failure mode: a failing test step embeds its full captured output in the
// auto-fix prompt, which is routinely hundreds of KB to megabytes. When that
// prompt was passed as an `exec <prompt>` positional the exec overflowed the OS
// ARG_MAX and failed with "argument list too long" (fork/exec ...: argument
// list too long), surfacing as `agent fix tests: codex start: ...` and taking
// the pipeline step down. The prompt now travels on stdin (codex exec -), which
// has no such length ceiling.
//
// The test drives the real codexAgent.runOnce against a fake `codex` binary
// with a 4 MiB prompt - larger than ARG_MAX on Linux and macOS and far larger
// than Linux's per-argument MAX_ARG_STRLEN (128 KiB) - so a regression that
// reintroduces argv delivery fails here with an exec error.
func TestCodexAgent_Run_LargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	stdinCapture := filepath.Join(dir, "stdin.txt")

	// Fake codex: copy the whole stdin prompt to a file (proving stdin
	// transport and full delivery), then emit a minimal agent_message plus a
	// turn.completed event that codexAgent's parser accepts.
	script := "#!/bin/sh\n" +
		"cat > \"$NM_TEST_STDIN_CAPTURE\"\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("NM_TEST_STDIN_CAPTURE", stdinCapture)

	// 4 MiB prompt: comfortably over ARG_MAX everywhere, so a return to argv
	// delivery would fail the exec instead of reaching the fake.
	prompt := strings.Repeat("a", 4*1024*1024)

	ca := &codexAgent{bin: bin}
	res, err := ca.runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: dir})
	if err != nil {
		t.Fatalf("runOnce with large prompt failed (E2BIG regression?): %v", err)
	}
	if res == nil {
		t.Fatalf("expected a result, got nil")
	}
	if res.Text != "ok" {
		t.Fatalf("unexpected result text: %q", res.Text)
	}

	got, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if len(got) != len(prompt) {
		t.Fatalf("fake codex received %d prompt bytes on stdin, want %d", len(got), len(prompt))
	}
}
