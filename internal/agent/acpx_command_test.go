package agent

import (
	"strings"
	"testing"
)

func TestComposeACPTargetCommand_FirstClassModelWinsAndStaysExact(t *testing.T) {
	got, err := composeACPTargetCommand(
		"cursor",
		`"/opt/Cursor Agent/cursor-agent" acp --profile work`,
		[]string{"--model=claude-sonnet-5", "--effort", "high"},
		"claude-opus-5[context=1m,effort=high,fast=false]",
	)
	if err != nil {
		t.Fatalf("composeACPTargetCommand() error = %v", err)
	}
	want := `"/opt/Cursor Agent/cursor-agent" --effort high --model claude-opus-5[context=1m,effort=high,fast=false] acp --profile work`
	if got != want {
		t.Fatalf("composeACPTargetCommand() = %q, want %q", got, want)
	}
}

func TestComposeACPTargetCommand_NonCursorArgsAppendToRawCommand(t *testing.T) {
	got, err := composeACPTargetCommand(
		"gemini",
		"gemini --acp",
		[]string{"--profile", "work"},
		"",
	)
	if err != nil {
		t.Fatalf("composeACPTargetCommand() error = %v", err)
	}
	if want := "gemini --acp --profile work"; got != want {
		t.Fatalf("composeACPTargetCommand() = %q, want %q", got, want)
	}
}

func TestComposeACPTargetCommand_RefusesAmbiguousCursorSubcommand(t *testing.T) {
	_, err := composeACPTargetCommand(
		"cursor",
		"cursor-agent --profile acp acp",
		[]string{"--model", "claude-opus-5"},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "exactly one acp subcommand") {
		t.Fatalf("composeACPTargetCommand() error = %v, want ambiguous-subcommand refusal", err)
	}
}
