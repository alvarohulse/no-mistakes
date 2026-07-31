package agent

import (
	"reflect"
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

func TestComposeCursorACPContainmentReplacesWorkspaceOverrides(t *testing.T) {
	got, err := composeCursorACPContainment(
		"cursor-agent --workspace /unsafe --add-dir=/other --model claude-opus-5 acp",
		"/tmp/clean",
		"/repo/worktree",
	)
	if err != nil {
		t.Fatalf("composeCursorACPContainment() error = %v", err)
	}
	want := "cursor-agent --model claude-opus-5 --workspace /tmp/clean --add-dir /repo/worktree acp"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
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

func TestComposeACPTargetCommand_MatchesECMAScriptWhitespaceTokenization(t *testing.T) {
	tests := []struct {
		name      string
		separator rune
		quoted    bool
	}{
		{name: "vertical tab", separator: '\u000b', quoted: true},
		{name: "form feed", separator: '\u000c', quoted: true},
		{name: "no-break space", separator: '\u00a0', quoted: true},
		{name: "ogham space mark", separator: '\u1680', quoted: true},
		{name: "en quad", separator: '\u2000', quoted: true},
		{name: "hair space", separator: '\u200a', quoted: true},
		{name: "zero-width space is retained", separator: '\u200b', quoted: false},
		{name: "line separator", separator: '\u2028', quoted: true},
		{name: "paragraph separator", separator: '\u2029', quoted: true},
		{name: "narrow no-break space", separator: '\u202f', quoted: true},
		{name: "medium mathematical space", separator: '\u205f', quoted: true},
		{name: "ideographic space", separator: '\u3000', quoted: true},
		{name: "byte order mark", separator: '\ufeff', quoted: true},
		{name: "next line is retained", separator: '\u0085', quoted: false},
		{name: "mongolian vowel separator is retained", separator: '\u180e', quoted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := "left" + string(tt.separator) + "right"
			got, err := composeACPTargetCommand("cursor", "cursor-agent acp", []string{"--label", value}, "")
			if err != nil {
				t.Fatalf("composeACPTargetCommand() error = %v", err)
			}
			encoded := value
			if tt.quoted {
				encoded = `"` + value + `"`
			}
			if want := "cursor-agent --label " + encoded + " acp"; got != want {
				t.Fatalf("composeACPTargetCommand() = %q, want %q", got, want)
			}

			parsed, err := splitACPXCommandLine(got)
			if err != nil {
				t.Fatalf("splitACPXCommandLine() error = %v", err)
			}
			wantTokens := []string{"cursor-agent", "--label", value, "acp"}
			if !reflect.DeepEqual(parsed, wantTokens) {
				t.Fatalf("splitACPXCommandLine() = %q, want %q", parsed, wantTokens)
			}
		})
	}
}
