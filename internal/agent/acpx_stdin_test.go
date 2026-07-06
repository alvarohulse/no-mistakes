//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcpxAgent_Run_LargePromptViaStdin guards the acpx prompt transport: the
// prompt travels on stdin, never as an argv element or a temp file passed via
// -f/--file. Delivering the prompt inline overflows the OS command-line length
// limit for the routinely-huge auto-fix prompts (ARG_MAX; 8191 chars on
// Windows), and delivering it via -f made the flag a hard runtime dependency
// for every acpx build. acpx `exec` reads the prompt from stdin when no
// positional prompt is given, so stdin has no length ceiling and needs no
// specific flag.
//
// The test drives the real acpxAgent.runOnce against a fake `acpx` binary with
// a 4 MiB prompt - larger than ARG_MAX on Linux and macOS - so a regression
// that reintroduces argv delivery fails here with an exec error.
func TestAcpxAgent_Run_LargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	stdinCapture := filepath.Join(dir, "stdin.txt")

	// Fake acpx: copy the whole stdin prompt to a file (proving stdin transport
	// and full delivery), then emit a minimal session/update event the acpx
	// parser accepts as agent output.
	script := "#!/bin/sh\n" +
		"cat > \"$NM_TEST_STDIN_CAPTURE\"\n" +
		"printf '%s\\n' '{\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"text\":\"done\"}}}'\n"
	bin := filepath.Join(dir, "acpx")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake acpx: %v", err)
	}

	t.Setenv("NM_TEST_STDIN_CAPTURE", stdinCapture)

	// 4 MiB prompt: comfortably over ARG_MAX everywhere, so a return to argv
	// delivery would fail the exec instead of reaching the fake.
	prompt := strings.Repeat("a", 4*1024*1024)

	a := &acpxAgent{bin: bin, target: "gemini"}
	res, err := a.runOnce(context.Background(), RunOpts{Prompt: prompt, CWD: dir})
	if err != nil {
		t.Fatalf("runOnce with large prompt failed (ARG_MAX regression?): %v", err)
	}
	if res == nil {
		t.Fatalf("expected a result, got nil")
	}
	if res.Text != "done" {
		t.Fatalf("result text = %q, want %q", res.Text, "done")
	}

	got, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if len(got) != len(prompt) {
		t.Fatalf("fake acpx received %d prompt bytes on stdin, want %d", len(got), len(prompt))
	}
}
