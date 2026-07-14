package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePRNote(t *testing.T) {
	dir := t.TempDir()
	noteFile := filepath.Join(dir, "note.md")
	if err := os.WriteFile(noteFile, []byte("\n\n## Notes\n\nlonger content\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		prNote     string
		prNoteFile string
		want       string
		wantErr    bool
	}{
		{name: "neither set", want: ""},
		{name: "inline note passes through", prNote: "ship it", want: "ship it"},
		{name: "file is read and trimmed", prNoteFile: noteFile, want: "## Notes\n\nlonger content"},
		{name: "both set is an error", prNote: "a", prNoteFile: noteFile, wantErr: true},
		{name: "missing file is an error", prNoteFile: filepath.Join(dir, "absent.md"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePRNote(tt.prNote, tt.prNoteFile)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePRNote(%q, %q) expected error, got nil", tt.prNote, tt.prNoteFile)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePRNote(%q, %q) error = %v", tt.prNote, tt.prNoteFile, err)
			}
			if got != tt.want {
				t.Fatalf("resolvePRNote(%q, %q) = %q, want %q", tt.prNote, tt.prNoteFile, got, tt.want)
			}
		})
	}
}

func TestResolvePRNoteRejectsOversizedPushOption(t *testing.T) {
	// One byte over the conservative transport ceiling must be rejected, whether
	// supplied inline or via a file, with an actionable error.
	oversizedNote := strings.Repeat("x", maxPRNotePushOptionBytes+1)
	noteFile := filepath.Join(t.TempDir(), "oversized-note.md")
	if err := os.WriteFile(noteFile, []byte(oversizedNote), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		prNote     string
		prNoteFile string
	}{
		{name: "inline", prNote: oversizedNote},
		{name: "file", prNoteFile: noteFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolvePRNote(tt.prNote, tt.prNoteFile)
			if err == nil {
				t.Fatal("expected an oversized PR note error")
			}
			if !strings.Contains(err.Error(), "too large for the push-option transport") || !strings.Contains(err.Error(), "shorten") {
				t.Fatalf("expected an actionable transport-size error, got %q", err)
			}
		})
	}
}

func TestResolvePRNoteAcceptsNoteAtLimit(t *testing.T) {
	// A note exactly at the ceiling is accepted (the limit is a conservative
	// Windows-safe source size, well under the base64-inflated transport caps).
	note := strings.Repeat("x", maxPRNotePushOptionBytes)
	got, err := resolvePRNote(note, "")
	if err != nil {
		t.Fatalf("resolvePRNote at limit error = %v", err)
	}
	if got != note {
		t.Fatalf("resolvePRNote at limit did not pass the note through unchanged")
	}
}
