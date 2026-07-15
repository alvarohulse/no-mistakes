package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParseSkipPushOptions(t *testing.T) {
	got, err := parseSkipPushOptions([]string{
		"ci.skip",
		"no-mistakes.skip=test,lint",
	})
	if err != nil {
		t.Fatalf("parseSkipPushOptions() error = %v", err)
	}
	want := []types.StepName{types.StepTest, types.StepLint}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestParseSkipPushOptionsRejectsUnknownStep(t *testing.T) {
	_, err := parseSkipPushOptions([]string{"no-mistakes.skip=test,deploy"})
	if err == nil {
		t.Fatal("expected unknown step to fail")
	}
}

func TestNormalizeNotifyGatePathResolvesLegacyDotGate(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo123.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(bare); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	t.Setenv("PWD", ".")

	got, err := normalizeNotifyGatePath(".")
	if err != nil {
		t.Fatalf("normalizeNotifyGatePath: %v", err)
	}
	if got == "." || !filepath.IsAbs(got) {
		t.Fatalf("normalizeNotifyGatePath(.) = %q, want absolute path", got)
	}
	want, err := filepath.EvalSymlinks(bare)
	if err != nil {
		want = bare
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		gotResolved = got
	}
	if gotResolved != want {
		t.Fatalf("normalizeNotifyGatePath(.) = %q (resolved %q), want %q", got, gotResolved, want)
	}
}

func TestFormatSkipPushOptions(t *testing.T) {
	got := formatSkipPushOptions([]types.StepName{types.StepTest, types.StepLint})
	want := []string{"no-mistakes.skip=test,lint"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestIntentPushOptionRoundTrip(t *testing.T) {
	// Multi-line, comma- and colon-bearing intent must survive the
	// line-oriented push-option transport intact.
	intent := "add retry to the uploader\n\nwhy: flaky network, commas, colons: ok"
	opt := formatIntentPushOption(intent)
	if opt == "" {
		t.Fatal("formatIntentPushOption returned empty for a non-empty intent")
	}
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != intent {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, intent)
	}
}

func TestFormatIntentPushOptionEmpty(t *testing.T) {
	if got := formatIntentPushOption("   "); got != "" {
		t.Fatalf("formatIntentPushOption(blank) = %q, want empty", got)
	}
}

func TestParseIntentPushOptionsNone(t *testing.T) {
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parseIntentPushOptions(no intent) = %q, want empty", got)
	}
}

func TestPRNotePushOptionRoundTrip(t *testing.T) {
	// Multi-line, markdown-bearing note must survive the line-oriented
	// push-option transport intact, and must not collide with the intent option.
	note := "## Notes\n\nrelease: v2, commas, colons: ok\n- bullet"
	opt := formatPRNotePushOption(note)
	if opt == "" {
		t.Fatal("formatPRNotePushOption returned empty for a non-empty note")
	}
	got, err := parsePRNotePushOptions([]string{
		"no-mistakes.skip=test",
		formatIntentPushOption("some intent"),
		opt,
	})
	if err != nil {
		t.Fatalf("parsePRNotePushOptions() error = %v", err)
	}
	if got != note {
		t.Fatalf("round-trip note = %q, want %q", got, note)
	}
	// The intent parser must not pick up the pr-note option and vice versa.
	gotIntent, err := parseIntentPushOptions([]string{opt, formatIntentPushOption("some intent")})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if gotIntent != "some intent" {
		t.Fatalf("intent parser leaked pr-note: got %q", gotIntent)
	}
}

func TestFormatPRNotePushOptionEmpty(t *testing.T) {
	if got := formatPRNotePushOption("   "); got != "" {
		t.Fatalf("formatPRNotePushOption(blank) = %q, want empty", got)
	}
}

func TestParsePRNotePushOptionsNone(t *testing.T) {
	got, err := parsePRNotePushOptions([]string{"no-mistakes.skip=test", "no-mistakes.intent=Zm9v"})
	if err != nil {
		t.Fatalf("parsePRNotePushOptions() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parsePRNotePushOptions(no note) = %q, want empty", got)
	}
}
