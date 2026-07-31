//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStubAcpx writes a stub acpx binary that records its argv (one arg per
// line) to the file named by NM_TEST_ACPX_ARGS_FILE and emits a minimal valid
// acpx JSON event stream.
func writeStubAcpx(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$NM_TEST_ACPX_ARGS_FILE"
prev=""
for arg in "$@"; do
	if [ "$prev" = "-f" ] && [ -n "$NM_TEST_ACPX_PROMPT_FILE" ]; then
		cp "$arg" "$NM_TEST_ACPX_PROMPT_FILE"
	fi
	prev="$arg"
done
printf '{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"cursor stub reply"}}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAcpxAgent_Run_CursorSpawnsContainedDefaultCommand proves the explicit
// Cursor ACP path keeps its registered default command while forcing project
// instruction discovery into a clean primary workspace.
func TestAcpxAgent_Run_CursorSpawnsDefaultCommandWithoutOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent types.AgentName
	}{
		{name: "explicit acp:cursor target", agent: "acp:cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "argv.txt")
			promptFile := filepath.Join(dir, "prompt.txt")
			t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
			t.Setenv("NM_TEST_ACPX_PROMPT_FILE", promptFile)
			stub := writeStubAcpx(t, dir)

			a, err := New(tc.agent, stub, nil)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.agent, err)
			}
			res, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Text != "cursor stub reply" {
				t.Errorf("result text = %q, want stub acpx output", res.Text)
			}

			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("stub acpx never recorded argv: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(argv) < 2 || argv[0] != "--agent" {
				t.Fatalf("spawned argv = %q, want leading --agent <command>", argv)
			}
			command, err := splitACPXCommandLine(argv[1])
			if err != nil {
				t.Fatalf("parse Cursor ACP command: %v", err)
			}
			workspace := argValue(command, "--workspace")
			if workspace == "" || workspace == dir {
				t.Fatalf("Cursor ACP workspace = %q, want separate clean directory", workspace)
			}
			if got := argValue(command, "--add-dir"); got != dir {
				t.Fatalf("Cursor ACP --add-dir = %q, want %q", got, dir)
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("Cursor ACP clean workspace was not removed: %v", err)
			}
			if len(argv) < 3 || argv[len(argv)-3] != "exec" || argv[len(argv)-2] != "-f" || argv[len(argv)-1] == "" {
				t.Errorf("spawned argv = %q, want trailing exec -f <prompt-file>", argv)
			}
			promptData, err := os.ReadFile(promptFile)
			if err != nil {
				t.Fatalf("stub acpx never copied prompt file: %v", err)
			}
			if string(promptData) != "review this change" {
				t.Errorf("prompt file = %q, want original prompt", promptData)
			}
			for _, arg := range argv {
				if arg == "cursor" {
					t.Errorf("spawned argv = %q, must not pass the bare target when the default command is supplied", argv)
				}
			}
			t.Logf("spawned: acpx %s", strings.Join(argv, " "))
		})
	}
}

func TestAcpxAgent_Run_CursorPassesConfiguredArgsToTargetSpawn(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.txt")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
	t.Setenv("NM_TEST_ACPX_PROMPT_FILE", promptFile)
	stub := writeStubAcpx(t, dir)

	a, err := New("acp:cursor", stub, []string{"--model", "claude-opus-5", "--profile", "work profile"})
	if err != nil {
		t.Fatalf("New(%q): %v", types.AgentCursor, err)
	}
	res, err := a.Run(context.Background(), RunOpts{Prompt: "review this change", CWD: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "cursor stub reply" {
		t.Errorf("result text = %q, want stub acpx output", res.Text)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("stub acpx never recorded argv: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(argv) < 2 || argv[0] != "--agent" {
		t.Fatalf("spawned argv = %q, want leading --agent <command>", argv)
	}
	command, err := splitACPXCommandLine(argv[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(command, "--add-dir"); got != dir {
		t.Fatalf("target --add-dir = %q, want %q", got, dir)
	}
	command = withoutFlagValues(command, "--workspace", "--add-dir")
	if got, want := strings.Join(command, "\x00"), strings.Join([]string{"cursor-agent", "--model", "claude-opus-5", "--profile", "work profile", "acp"}, "\x00"); got != want {
		t.Errorf("target command = %q, want %q", command, want)
	}
	if promptData, err := os.ReadFile(promptFile); err != nil {
		t.Fatalf("stub acpx never copied prompt file: %v", err)
	} else if string(promptData) != "review this change" {
		t.Errorf("prompt file = %q, want original prompt", promptData)
	}
	for _, arg := range argv {
		if arg == "review this change" {
			t.Fatalf("spawned argv = %q, prompt must not be passed in argv", argv)
		}
	}
}

func TestAcpxAgent_Run_CursorFirstClassModelWinsAndReportsIdentity(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.txt")
	promptFile := filepath.Join(dir, "prompt.txt")
	t.Setenv("NM_TEST_ACPX_ARGS_FILE", argsFile)
	t.Setenv("NM_TEST_ACPX_PROMPT_FILE", promptFile)
	stub := writeStubAcpx(t, dir)

	a, err := NewWithOptions(
		"acp:cursor",
		stub,
		[]string{"-m", "claude-sonnet-5", "--model=claude-haiku-5", "--effort", "high"},
		Options{Model: "claude-opus-5", Vendor: "anthropic"},
	)
	if err != nil {
		t.Fatalf("NewWithOptions(%q): %v", types.AgentCursor, err)
	}
	defer a.Close()
	if got := ConfiguredModel(a); got != (ModelIdentity{Name: "claude-opus-5", Vendor: "anthropic"}) {
		t.Fatalf("configured model = %#v", got)
	}

	var attempt Attempt
	res, err := a.Run(context.Background(), RunOpts{
		Prompt: "review this change",
		CWD:    dir,
		OnAttempt: func(got Attempt) {
			attempt = got
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Model != "claude-opus-5" || res.ModelProvider != "anthropic" {
		t.Fatalf("result model identity = %q/%q", res.Model, res.ModelProvider)
	}
	if attempt.Result == nil || attempt.Result.Model != "claude-opus-5" || attempt.Result.ModelProvider != "anthropic" {
		t.Fatalf("attempt result = %+v, want configured model identity", attempt.Result)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("stub acpx never recorded argv: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(argv) < 2 || argv[0] != "--agent" {
		t.Fatalf("spawned argv = %q, want leading --agent <command>", argv)
	}
	command, err := splitACPXCommandLine(argv[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(command, "--add-dir"); got != dir {
		t.Fatalf("target --add-dir = %q, want %q", got, dir)
	}
	command = withoutFlagValues(command, "--workspace", "--add-dir")
	if got, want := strings.Join(command, "\x00"), strings.Join([]string{"cursor-agent", "--effort", "high", "--model", "claude-opus-5", "acp"}, "\x00"); got != want {
		t.Fatalf("target command = %q, want %q", command, want)
	}
	if promptData, err := os.ReadFile(promptFile); err != nil {
		t.Fatalf("stub acpx never copied prompt file: %v", err)
	} else if string(promptData) != "review this change" {
		t.Fatalf("prompt file = %q, want original prompt", promptData)
	}
	for _, arg := range argv {
		if arg == "review this change" {
			t.Fatalf("spawned argv = %q, prompt must not be passed in argv", argv)
		}
	}
}
