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
	if !strings.HasPrefix(first, "<!-- no-mistakes:owned-sections:v1 -->\n\n<!-- no-mistakes:section:v1:summary:begin -->") ||
		!strings.Contains(first, " -->\n\n<!-- no-mistakes:section:v1:static-tests:begin -->") {
		t.Fatalf("patch-only output no longer uses the legacy sections-only layout:\n%s", first)
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

func TestNewOwnedDocumentBootstrapsOwnedSectionsIntoLiteralLayout(t *testing.T) {
	t.Parallel()

	preamble := "Repository template preamble.\n\n# Summary\n\n"
	checklist := "\n\n# Test Plan\n\n- [ ] Keep this checklist byte-for-byte.\r\n"
	footer := "\nRepository-owned footer.\n"
	patches := PatchSet{
		Version: PatchVersion,
		Sections: []SectionPatch{
			{ID: "summary", Content: "Generated summary."},
			{ID: "static-tests", Content: "- `go test ./...` passed"},
		},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer(preamble)},
			{Section: stringPointer("summary")},
			{Literal: stringPointer(checklist)},
			{Section: stringPointer("static-tests")},
			{Literal: stringPointer(footer)},
		}},
	}

	body, err := NewOwnedDocument(patches)
	if err != nil {
		t.Fatalf("NewOwnedDocument: %v", err)
	}
	if err := ValidateOwnedDocument(body, ValidationLimits{}); err != nil {
		t.Fatalf("ValidateOwnedDocument: %v", err)
	}
	for _, exact := range []string{preamble, checklist, footer} {
		if !strings.Contains(body, exact) {
			t.Fatalf("literal bytes %q missing from bootstrapped document:\n%s", exact, body)
		}
	}
	if summary, tests := strings.Index(body, "Generated summary."), strings.Index(body, "- `go test ./...` passed"); summary < 0 || tests < summary {
		t.Fatalf("owned sections were not rendered at their references:\n%s", body)
	}
}

func TestApplyOwnedPatchesIgnoresBootstrapAndPreservesInitialLayout(t *testing.T) {
	t.Parallel()

	initial, err := NewOwnedDocument(PatchSet{
		Version:  PatchVersion,
		Sections: []SectionPatch{{ID: "summary", Content: "old summary"}},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer("# Summary\n\n")},
			{Section: stringPointer("summary")},
			{Literal: stringPointer("\n\n- [ ] Human checklist\n")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := ApplyOwnedPatches(initial, PatchSet{
		Version:  PatchVersion,
		Sections: []SectionPatch{{ID: "summary", Content: "new summary"}},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer("THIS REPLACEMENT LAYOUT MUST BE IGNORED\n")},
			{Section: stringPointer("summary")},
		}},
	})
	if err != nil {
		t.Fatalf("ApplyOwnedPatches: %v", err)
	}
	if !strings.Contains(updated, "# Summary\n\n") || !strings.Contains(updated, "\n\n- [ ] Human checklist\n") {
		t.Fatalf("initial unowned layout drifted:\n%s", updated)
	}
	if strings.Contains(updated, "THIS REPLACEMENT LAYOUT") || strings.Contains(updated, "old summary") {
		t.Fatalf("bootstrap layout or old owned content survived:\n%s", updated)
	}
	if !strings.Contains(updated, "new summary") {
		t.Fatalf("updated owned content missing:\n%s", updated)
	}
}

func stringPointer(value string) *string { return &value }

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
	bootstrapSecret, err := NewOwnedDocument(PatchSet{
		Version:  PatchVersion,
		Sections: []SectionPatch{{ID: "summary", Content: "safe"}},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer("ghp_abcdefghijklmnopqrstuvwx12\n\n")},
			{Section: stringPointer("summary")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapLarge, err := NewOwnedDocument(PatchSet{
		Version:  PatchVersion,
		Sections: []SectionPatch{{ID: "summary", Content: "safe"}},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer(strings.Repeat("template ", 100) + "\n\n")},
			{Section: stringPointer("summary")},
		}},
	})
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
		{name: "bootstrap literal byte limit", body: bootstrapLarge, limits: ValidationLimits{MaxBytes: len(bootstrapLarge) - 1}, want: ErrOversize},
		{name: "bootstrap literal secret", body: bootstrapSecret, want: ErrSecretDetected},
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

func TestPatchSetRejectsInvalidBootstrapLayouts(t *testing.T) {
	t.Parallel()

	literal := "template\n"
	summary := "summary"
	testsID := "static-tests"
	unknown := "unknown"
	invalidUTF8 := string([]byte{0xff})
	reserved := "<!-- no-mistakes:owned-sections:v1 -->\n"
	base := []SectionPatch{{ID: summary, Content: "summary"}, {ID: testsID, Content: "tests"}}

	tests := []struct {
		name  string
		parts []BootstrapPart
	}{
		{name: "part has neither field", parts: []BootstrapPart{{}, {Section: &summary}, {Section: &testsID}}},
		{name: "part has both fields", parts: []BootstrapPart{{Literal: &literal, Section: &summary}, {Section: &testsID}}},
		{name: "duplicate section reference", parts: []BootstrapPart{{Section: &summary}, {Section: &summary}, {Section: &testsID}}},
		{name: "missing section reference", parts: []BootstrapPart{{Section: &summary}}},
		{name: "unknown section reference", parts: []BootstrapPart{{Section: &summary}, {Section: &testsID}, {Section: &unknown}}},
		{name: "invalid utf8 literal", parts: []BootstrapPart{{Literal: &invalidUTF8}, {Section: &summary}, {Section: &testsID}}},
		{name: "reserved marker literal", parts: []BootstrapPart{{Literal: &reserved}, {Section: &summary}, {Section: &testsID}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			patches := PatchSet{Version: PatchVersion, Sections: base, Bootstrap: &BootstrapLayout{Parts: tt.parts}}
			if err := ValidatePatchSet(patches); err == nil {
				t.Fatalf("ValidatePatchSet accepted invalid bootstrap: %+v", tt.parts)
			}
		})
	}
}

func TestNewOwnedDocumentRejectsBootstrapWithoutSectionMarkerBoundaries(t *testing.T) {
	t.Parallel()

	_, err := NewOwnedDocument(PatchSet{
		Version:  PatchVersion,
		Sections: []SectionPatch{{ID: "summary", Content: "generated"}},
		Bootstrap: &BootstrapLayout{Parts: []BootstrapPart{
			{Literal: stringPointer("# Summary")},
			{Section: stringPointer("summary")},
		}},
	})
	if !errors.Is(err, ErrMissingMarkers) && !errors.Is(err, ErrCorruptMarkers) {
		t.Fatalf("error = %v, want an invalid marker-layout error", err)
	}
}
