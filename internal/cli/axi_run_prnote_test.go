package cli

import (
	"os"
	"path/filepath"
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
