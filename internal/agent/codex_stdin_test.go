//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexAgent_Run_LargePromptViaStdin guards against passing large repair
// prompts through argv, which exceeds ARG_MAX before the child can start.
func TestCodexAgent_Run_LargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	stdinCapture := filepath.Join(dir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"cat > \"$NM_TEST_STDIN_CAPTURE\"\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("NM_TEST_STDIN_CAPTURE", stdinCapture)

	prompt := strings.Repeat("a", 4*1024*1024)
	result, err := (&codexAgent{bin: bin}).runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: dir})
	if err != nil {
		t.Fatalf("runOnce with large prompt failed: %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("result = %#v, want text ok", result)
	}

	got, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if len(got) != len(prompt) {
		t.Fatalf("fake codex received %d prompt bytes, want %d", len(got), len(prompt))
	}
}
