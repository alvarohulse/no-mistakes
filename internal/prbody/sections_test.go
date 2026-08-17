package prbody

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewOwnedDocumentIsDeterministicAndVerifiable(t *testing.T) {
	t.Parallel()

	patches := PatchSet{Version: PatchVersion, Sections: []SectionPatch{
		{ID: "summary", Content: "## Summary\n\nKeeps the body safe."},
		{ID: "static-tests", Content: "## Static Tests\n\n- `go test ./...` passed"},
	}}
	first, err := NewOwnedDocument(patches)
	if err != nil {
		t.Fatalf("NewOwnedDocument: %v", err)
	}
	second, err := NewOwnedDocument(patches)
	if err != nil {
		t.Fatalf("NewOwnedDocument repeat: %v", err)
	}
	if first != second {
		t.Fatalf("render is not deterministic\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	for _, marker := range []string{
		"<!-- no-mistakes:owned-sections:v1 -->",
		"<!-- no-mistakes:section:v1:summary:begin -->",
		"<!-- no-mistakes:section:v1:summary:end -->",
		"<!-- no-mistakes:section:v1:summary:sha256:",
	} {
		if !strings.Contains(first, marker) {
			t.Fatalf("document lacks %q:\n%s", marker, first)
		}
	}
	if err := ValidateOwnedDocument(first, ValidationLimits{}); err != nil {
		t.Fatalf("ValidateOwnedDocument: %v", err)
	}
}

func TestApplyOwnedPatchesPreservesEveryUnownedByte(t *testing.T) {
	t.Parallel()

	original, err := NewOwnedDocument(PatchSet{Version: PatchVersion, Sections: []SectionPatch{
		{ID: "summary", Content: "old summary"},
		{ID: "static-tests", Content: "old tests"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	original = "human preamble\r\n" + original
	original = strings.Replace(original, "\n\n<!-- no-mistakes:section:v1:static-tests:begin -->", "\n\n- [x] human checklist\r\n<!-- third-party:keep -->\n<!-- no-mistakes:section:v1:static-tests:begin -->", 1)
	original += "\n\r\nhuman suffix\n"

	updated, err := ApplyOwnedPatches(original, PatchSet{Version: PatchVersion, Sections: []SectionPatch{
		{ID: "summary", Content: "new summary"},
		{ID: "static-tests", Content: "new tests"},
	}})
	if err != nil {
		t.Fatalf("ApplyOwnedPatches: %v", err)
	}
	for _, exact := range []string{
		"human preamble\r\n",
		"- [x] human checklist\r\n<!-- third-party:keep -->\n",
		"\n\r\nhuman suffix\n",
	} {
		if !strings.Contains(updated, exact) {
			t.Fatalf("unowned bytes %q drifted:\n%s", exact, updated)
		}
	}
	if strings.Contains(updated, "old summary") || strings.Contains(updated, "old tests") {
		t.Fatalf("old owned content survived:\n%s", updated)
	}
	if !strings.Contains(updated, "new summary") || !strings.Contains(updated, "new tests") {
		t.Fatalf("new owned content missing:\n%s", updated)
	}
}

func TestOwnedDocumentFailsClosed(t *testing.T) {
	t.Parallel()

	valid, err := NewOwnedDocument(PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "safe"}}})
	if err != nil {
		t.Fatal(err)
	}
	begin := "<!-- no-mistakes:section:v1:summary:begin -->"
	end := "<!-- no-mistakes:section:v1:summary:end -->"

	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "missing watermark", body: strings.Replace(valid, "<!-- no-mistakes:owned-sections:v1 -->\n\n", "", 1), want: ErrMissingMarkers},
		{name: "missing end", body: strings.Replace(valid, end, "", 1), want: ErrCorruptMarkers},
		{name: "conflicting nested begin", body: strings.Replace(valid, "safe", "safe\n"+begin, 1), want: ErrConflictingMarkers},
		{name: "corrupt hash", body: strings.Replace(valid, "safe", "changed", 1), want: ErrCorruptMarkers},
		{name: "duplicate section", body: valid + "\n" + valid[strings.Index(valid, begin):], want: ErrConflictingMarkers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ApplyOwnedPatches(tt.body, PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "new"}}})
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOwnedDocumentRejectsMissingPatchTarget(t *testing.T) {
	t.Parallel()

	body, err := NewOwnedDocument(PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "safe"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyOwnedPatches(body, PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "review-evidence", Content: "clean"}}})
	if !errors.Is(err, ErrMissingMarkers) {
		t.Fatalf("error = %v, want ErrMissingMarkers", err)
	}
}

func TestOwnedDocumentValidationRejectsUnsafeCandidates(t *testing.T) {
	t.Parallel()

	safe, err := NewOwnedDocument(PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "safe"}}})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := NewOwnedDocument(PatchSet{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "ghp_abcdefghijklmnopqrstuvwx12"}}})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		body   string
		limits ValidationLimits
		want   error
	}{
		{name: "invalid utf8", body: string([]byte{0xff, 0xfe}), want: ErrInvalidUTF8},
		{name: "byte limit", body: safe, limits: ValidationLimits{MaxBytes: len(safe) - 1}, want: ErrOversize},
		{name: "host unit limit", body: safe, limits: ValidationLimits{MaxUnits: utf8.RuneCountInString(safe) - 1, MeasureUnits: utf8.RuneCountInString}, want: ErrOversize},
		{name: "secret", body: secret, want: ErrSecretDetected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOwnedDocument(tt.body, tt.limits)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPatchSetRejectsReplacementBodyAndInvalidSections(t *testing.T) {
	t.Parallel()

	tests := []PatchSet{
		{Version: PatchVersion},
		{Version: PatchVersion, Sections: []SectionPatch{{ID: "Summary", Content: "x"}}},
		{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "x"}, {ID: "summary", Content: "y"}}},
		{Version: PatchVersion, Sections: []SectionPatch{{ID: "summary", Content: "<!-- no-mistakes:section:v1:other:begin -->"}}},
	}
	for i, patches := range tests {
		if err := ValidatePatchSet(patches); err == nil {
			t.Fatalf("case %d: expected rejection for %+v", i, patches)
		}
	}
}
