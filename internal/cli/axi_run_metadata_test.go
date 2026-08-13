package cli

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMetadataPushOptionRoundTripsOpaqueTextAndExplicitClear(t *testing.T) {
	t.Parallel()

	metadata := "  resolves TEAM-123\nnot json: [still opaque]  "
	option := formatMetadataPushOption(&metadata)
	got, err := parseMetadataPushOptions([]string{option})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != metadata {
		t.Fatalf("metadata = %v, want exact opaque input %q", got, metadata)
	}

	empty := ""
	got, err = parseMetadataPushOptions([]string{formatMetadataPushOption(&empty)})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != "" {
		t.Fatalf("explicit clear = %v, want non-nil empty string", got)
	}
	if absent, err := parseMetadataPushOptions(nil); err != nil || absent != nil {
		t.Fatalf("absent metadata = %v, %v; want nil, nil", absent, err)
	}
}

func TestValidateMetadataBoundsUTF8Bytes(t *testing.T) {
	t.Parallel()

	if err := validateMetadata(strings.Repeat("m", maxMetadataBytes)); err != nil {
		t.Fatalf("metadata at limit: %v", err)
	}
	if err := validateMetadata(strings.Repeat("m", maxMetadataBytes+1)); err == nil {
		t.Fatal("oversized metadata was accepted")
	}
	invalid := string([]byte{0xff, 0xfe})
	if utf8.ValidString(invalid) {
		t.Fatal("test fixture is valid UTF-8")
	}
	if err := validateMetadata(invalid); err == nil {
		t.Fatal("invalid UTF-8 metadata was accepted")
	}
	if err := validateMetadata("before\x00after"); err == nil {
		t.Fatal("metadata containing NUL was accepted")
	}
}

func TestParseMetadataPushOptionEnforcesPublicInputBounds(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"oversized":     strings.Repeat("m", maxMetadataBytes+1),
		"invalid UTF-8": string([]byte{0xff}),
		"NUL":           "before\x00after",
	} {
		t.Run(name, func(t *testing.T) {
			option := metadataPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(value))
			if _, err := parseMetadataPushOptions([]string{option}); err == nil {
				t.Fatalf("decoded %s metadata was accepted", name)
			}
		})
	}
}
