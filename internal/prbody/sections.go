package prbody

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/intent"
)

const (
	// PatchVersion is the formatter-output and owned-marker protocol version.
	// It is independent from the formatter input contract version.
	PatchVersion = 1
	// MaxOwnedDocumentBytes bounds a candidate before it can reach a hosted PR.
	MaxOwnedDocumentBytes = 1 << 20
)

var (
	ErrMissingMarkers     = errors.New("owned PR body markers are missing")
	ErrCorruptMarkers     = errors.New("owned PR body markers are corrupt")
	ErrConflictingMarkers = errors.New("owned PR body markers conflict")
	ErrUnownedDrift       = errors.New("unowned PR body bytes changed")
	ErrInvalidUTF8        = errors.New("PR body is not valid UTF-8")
	ErrOversize           = errors.New("PR body exceeds its size limit")
	ErrSecretDetected     = errors.New("PR body contains a possible secret")
)

const (
	watermark       = "<!-- no-mistakes:owned-sections:v1 -->"
	reservedPrefix  = "<!-- no-mistakes:"
	sectionIDRegexp = `[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?`
)

var (
	validSectionID = regexp.MustCompile(`^` + sectionIDRegexp + `$`)
	beginMarker    = regexp.MustCompile(`^<!-- no-mistakes:section:v1:(` + sectionIDRegexp + `):begin -->$`)
	endMarker      = regexp.MustCompile(`^<!-- no-mistakes:section:v1:(` + sectionIDRegexp + `):end -->$`)
	hashMarker     = regexp.MustCompile(`^<!-- no-mistakes:section:v1:(` + sectionIDRegexp + `):sha256:([0-9a-f]{64}) -->$`)
)

// PatchSet is the only successful stdout shape accepted from hooks.pr_body.
// It deliberately has no full-body field: formatters choose owned section
// content and may describe a one-time initial layout, while no-mistakes alone
// renders and merges the hosted body.
type PatchSet struct {
	Version   int              `json:"version"`
	Sections  []SectionPatch   `json:"sections"`
	Bootstrap *BootstrapLayout `json:"bootstrap,omitempty"`
}

// SectionPatch replaces one named no-mistakes-owned section.
type SectionPatch struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// BootstrapLayout is a one-time layout for a newly-created pull request. Each
// part is either exact unowned text or a reference to one owned section.
type BootstrapLayout struct {
	Parts []BootstrapPart `json:"parts"`
}

// BootstrapPart is a pointer-valued sum type so an omitted field remains
// distinct from an intentionally empty literal.
type BootstrapPart struct {
	Literal *string `json:"literal,omitempty"`
	Section *string `json:"section,omitempty"`
}

// ValidationLimits applies the provider-independent byte limit plus an
// optional provider-specific character/unit limit.
type ValidationLimits struct {
	MaxBytes     int
	MaxUnits     int
	MeasureUnits func(string) int
}

type ownedSection struct {
	id      string
	content string
	start   int
	end     int
}

type ownedDocument struct {
	body     string
	sections []ownedSection
	byID     map[string]ownedSection
}

type bodyLine struct {
	start int
	end   int
	next  int
	text  string
}

// ValidatePatchSet checks the formatter-controlled data before rendering it.
func ValidatePatchSet(patches PatchSet) error {
	if patches.Version != PatchVersion {
		return fmt.Errorf("owned PR patch version %d, expected %d", patches.Version, PatchVersion)
	}
	if len(patches.Sections) == 0 {
		return errors.New("owned PR patch set has no sections")
	}
	seen := make(map[string]struct{}, len(patches.Sections))
	for _, section := range patches.Sections {
		if !validSectionID.MatchString(section.ID) {
			return fmt.Errorf("invalid owned PR section id %q", section.ID)
		}
		if _, exists := seen[section.ID]; exists {
			return fmt.Errorf("duplicate owned PR section %q", section.ID)
		}
		seen[section.ID] = struct{}{}
		if !utf8.ValidString(section.Content) {
			return fmt.Errorf("owned PR section %q: %w", section.ID, ErrInvalidUTF8)
		}
		if strings.Contains(section.Content, reservedPrefix) {
			return fmt.Errorf("owned PR section %q contains a reserved marker", section.ID)
		}
	}
	if patches.Bootstrap != nil {
		if err := validateBootstrapLayout(*patches.Bootstrap, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateBootstrapLayout(layout BootstrapLayout, sections map[string]struct{}) error {
	referenced := make(map[string]struct{}, len(sections))
	for i, part := range layout.Parts {
		switch {
		case part.Literal != nil && part.Section == nil:
			if !utf8.ValidString(*part.Literal) {
				return fmt.Errorf("owned PR bootstrap part %d literal: %w", i, ErrInvalidUTF8)
			}
			if strings.Contains(*part.Literal, reservedPrefix) {
				return fmt.Errorf("owned PR bootstrap part %d literal contains a reserved marker", i)
			}
		case part.Literal == nil && part.Section != nil:
			id := *part.Section
			if _, exists := sections[id]; !exists {
				return fmt.Errorf("owned PR bootstrap part %d references unknown section %q", i, id)
			}
			if _, exists := referenced[id]; exists {
				return fmt.Errorf("owned PR bootstrap references section %q more than once", id)
			}
			referenced[id] = struct{}{}
		default:
			return fmt.Errorf("owned PR bootstrap part %d must contain exactly one of literal or section", i)
		}
	}
	for _, section := range sortedSectionIDs(sections) {
		if _, exists := referenced[section]; !exists {
			return fmt.Errorf("owned PR bootstrap does not reference section %q", section)
		}
	}
	return nil
}

func sortedSectionIDs(sections map[string]struct{}) []string {
	ids := make([]string, 0, len(sections))
	for id := range sections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NewOwnedDocument renders a fresh body. A bootstrap layout interleaves exact
// unowned literals with the versioned owned sections; without one, the legacy
// sections-only layout is retained. ApplyOwnedPatches preserves every unowned
// byte after this one-time render.
func NewOwnedDocument(patches PatchSet) (string, error) {
	if err := ValidatePatchSet(patches); err != nil {
		return "", err
	}
	body := ""
	if patches.Bootstrap == nil {
		blocks := make([]string, 0, len(patches.Sections))
		for _, section := range patches.Sections {
			blocks = append(blocks, renderOwnedSection(section.ID, section.Content))
		}
		body = watermark + "\n\n" + strings.Join(blocks, "\n\n")
	} else {
		sections := make(map[string]SectionPatch, len(patches.Sections))
		for _, section := range patches.Sections {
			sections[section.ID] = section
		}
		var rendered strings.Builder
		rendered.WriteString(watermark)
		rendered.WriteString("\n\n")
		for _, part := range patches.Bootstrap.Parts {
			if part.Literal != nil {
				rendered.WriteString(*part.Literal)
				continue
			}
			section := sections[*part.Section]
			rendered.WriteString(renderOwnedSection(section.ID, section.Content))
		}
		body = rendered.String()
	}
	if _, err := parseOwnedDocument(body); err != nil {
		return "", fmt.Errorf("render owned PR body: %w", err)
	}
	return body, nil
}

// ApplyOwnedPatches updates only already-owned sections and never applies the
// optional bootstrap layout. It never bootstraps a missing section into an
// existing body: missing/corrupt/conflicting markers require an operator
// decision instead of guessing an insertion point.
func ApplyOwnedPatches(body string, patches PatchSet) (string, error) {
	if err := ValidatePatchSet(patches); err != nil {
		return "", err
	}
	doc, err := parseOwnedDocument(body)
	if err != nil {
		return "", err
	}

	replacements := make(map[string]string, len(patches.Sections))
	for _, patch := range patches.Sections {
		if _, exists := doc.byID[patch.ID]; !exists {
			return "", fmt.Errorf("section %q: %w", patch.ID, ErrMissingMarkers)
		}
		replacements[patch.ID] = renderOwnedSection(patch.ID, patch.Content)
	}

	sections := append([]ownedSection(nil), doc.sections...)
	sort.Slice(sections, func(i, j int) bool { return sections[i].start > sections[j].start })
	candidate := body
	for _, section := range sections {
		replacement, ok := replacements[section.id]
		if !ok {
			continue
		}
		candidate = candidate[:section.start] + replacement + candidate[section.end:]
	}

	parsedCandidate, err := parseOwnedDocument(candidate)
	if err != nil {
		return "", fmt.Errorf("validate patched PR body: %w", err)
	}
	if string(unownedBytes(doc)) != string(unownedBytes(parsedCandidate)) {
		return "", ErrUnownedDrift
	}
	if len(doc.sections) != len(parsedCandidate.sections) {
		return "", ErrUnownedDrift
	}
	for i := range doc.sections {
		if doc.sections[i].id != parsedCandidate.sections[i].id {
			return "", ErrUnownedDrift
		}
	}
	return candidate, nil
}

// ValidateOwnedDocument verifies marker integrity and rejects unsafe output.
func ValidateOwnedDocument(body string, limits ValidationLimits) error {
	if !utf8.ValidString(body) {
		return ErrInvalidUTF8
	}
	maxBytes := limits.MaxBytes
	if maxBytes == 0 {
		maxBytes = MaxOwnedDocumentBytes
	}
	if maxBytes < 0 || len(body) > maxBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrOversize, len(body), maxBytes)
	}
	if limits.MaxUnits > 0 {
		if limits.MeasureUnits == nil {
			return errors.New("PR body unit limit has no measurement function")
		}
		if units := limits.MeasureUnits(body); units > limits.MaxUnits {
			return fmt.Errorf("%w: %d host units exceeds %d", ErrOversize, units, limits.MaxUnits)
		}
	}
	if intent.RedactSecrets(body) != body {
		return ErrSecretDetected
	}
	_, err := parseOwnedDocument(body)
	return err
}

func renderOwnedSection(id, content string) string {
	digest := sha256.Sum256([]byte(content))
	return fmt.Sprintf("<!-- no-mistakes:section:v1:%s:begin -->\n%s\n<!-- no-mistakes:section:v1:%s:end -->\n<!-- no-mistakes:section:v1:%s:sha256:%s -->",
		id, content, id, id, hex.EncodeToString(digest[:]))
}

func parseOwnedDocument(body string) (*ownedDocument, error) {
	if !utf8.ValidString(body) {
		return nil, ErrInvalidUTF8
	}
	lines := splitBodyLines(body)
	doc := &ownedDocument{body: body, byID: make(map[string]ownedSection)}
	watermarks := 0

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line.text == watermark {
			watermarks++
			if watermarks > 1 {
				return nil, fmt.Errorf("duplicate watermark: %w", ErrConflictingMarkers)
			}
			continue
		}

		begin := beginMarker.FindStringSubmatch(line.text)
		if begin == nil {
			if strings.HasPrefix(line.text, reservedPrefix) {
				return nil, fmt.Errorf("unexpected marker %q: %w", line.text, ErrCorruptMarkers)
			}
			continue
		}
		id := begin[1]
		if _, exists := doc.byID[id]; exists {
			return nil, fmt.Errorf("duplicate section %q: %w", id, ErrConflictingMarkers)
		}
		if line.next == line.end {
			return nil, fmt.Errorf("section %q begin marker has no content boundary: %w", id, ErrCorruptMarkers)
		}

		contentStart := line.next
		foundEnd := false
		for j := i + 1; j < len(lines); j++ {
			candidate := lines[j]
			if candidate.text == watermark || beginMarker.MatchString(candidate.text) {
				return nil, fmt.Errorf("nested marker in section %q: %w", id, ErrConflictingMarkers)
			}
			end := endMarker.FindStringSubmatch(candidate.text)
			if end == nil {
				if strings.HasPrefix(candidate.text, reservedPrefix) {
					return nil, fmt.Errorf("unexpected marker in section %q: %w", id, ErrCorruptMarkers)
				}
				continue
			}
			if end[1] != id {
				return nil, fmt.Errorf("section %q closed by %q: %w", id, end[1], ErrConflictingMarkers)
			}
			if j+1 >= len(lines) {
				return nil, fmt.Errorf("section %q has no hash marker: %w", id, ErrCorruptMarkers)
			}
			hash := hashMarker.FindStringSubmatch(lines[j+1].text)
			if hash == nil || hash[1] != id {
				return nil, fmt.Errorf("section %q has invalid hash marker: %w", id, ErrCorruptMarkers)
			}

			rawContent := body[contentStart:candidate.start]
			if !strings.HasSuffix(rawContent, "\n") {
				return nil, fmt.Errorf("section %q content has no end boundary: %w", id, ErrCorruptMarkers)
			}
			content := strings.TrimSuffix(rawContent, "\n")
			digest := sha256.Sum256([]byte(content))
			if hex.EncodeToString(digest[:]) != hash[2] {
				return nil, fmt.Errorf("section %q hash mismatch: %w", id, ErrCorruptMarkers)
			}
			section := ownedSection{id: id, content: content, start: line.start, end: lines[j+1].end}
			doc.sections = append(doc.sections, section)
			doc.byID[id] = section
			i = j + 1
			foundEnd = true
			break
		}
		if !foundEnd {
			return nil, fmt.Errorf("section %q has no end marker: %w", id, ErrCorruptMarkers)
		}
	}

	if watermarks == 0 {
		return nil, ErrMissingMarkers
	}
	if len(doc.sections) == 0 {
		return nil, ErrMissingMarkers
	}
	return doc, nil
}

func splitBodyLines(body string) []bodyLine {
	lines := make([]bodyLine, 0, strings.Count(body, "\n")+1)
	for start := 0; start <= len(body); {
		rel := strings.IndexByte(body[start:], '\n')
		if rel < 0 {
			lines = append(lines, bodyLine{start: start, end: len(body), next: len(body), text: body[start:]})
			break
		}
		end := start + rel
		lines = append(lines, bodyLine{start: start, end: end, next: end + 1, text: body[start:end]})
		start = end + 1
		if start == len(body) {
			lines = append(lines, bodyLine{start: start, end: start, next: start, text: ""})
			break
		}
	}
	return lines
}

func unownedBytes(doc *ownedDocument) []byte {
	if doc == nil {
		return nil
	}
	var b strings.Builder
	last := 0
	for _, section := range doc.sections {
		b.WriteString(doc.body[last:section.start])
		last = section.end
	}
	b.WriteString(doc.body[last:])
	return []byte(b.String())
}
