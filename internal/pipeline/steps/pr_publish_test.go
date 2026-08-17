package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type scriptedPRBodyHost struct {
	capable     bool
	reads       []scm.PRBodySnapshot
	readCalls   int
	updates     []scm.PRContent
	updateErr   error
	created     *scm.PR
	createCalls int
}

func (h *scriptedPRBodyHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{PRBodyReadRevision: h.capable}
}

func (h *scriptedPRBodyHost) ReadPRBody(context.Context, *scm.PR) (scm.PRBodySnapshot, error) {
	if h.readCalls >= len(h.reads) {
		return scm.PRBodySnapshot{}, errors.New("unexpected body read")
	}
	snapshot := h.reads[h.readCalls]
	h.readCalls++
	return snapshot, nil
}

func (h *scriptedPRBodyHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.updates = append(h.updates, content)
	if h.updateErr != nil {
		return nil, h.updateErr
	}
	return pr, nil
}

func (h *scriptedPRBodyHost) CreatePR(_ context.Context, _, _ string, content scm.PRContent) (*scm.PR, error) {
	h.createCalls++
	h.updates = append(h.updates, content)
	return h.created, nil
}

func ownedBodyForPublisherTest(t *testing.T, content string) string {
	t.Helper()
	body, err := prbody.NewOwnedDocument(prbody.PatchSet{Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: content}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUpdateOwnedPRBodyComparesPublishesAndRereadVerifies(t *testing.T) {
	t.Parallel()

	original := "human preamble\r\n" + ownedBodyForPublisherTest(t, "old") + "\n- [x] human checklist\n"
	candidate, err := prbody.ApplyOwnedPatches(original, prbody.PatchSet{Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: "new"}}})
	if err != nil {
		t.Fatal(err)
	}
	host := &scriptedPRBodyHost{capable: true, reads: []scm.PRBodySnapshot{
		scm.NewPRBodySnapshot(original),
		scm.NewPRBodySnapshot(original),
		scm.NewPRBodySnapshot(candidate),
	}}

	err = updateOwnedPRBody(context.Background(), host, &scm.PR{Number: "42"}, prbody.PatchSet{
		Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: "new"}},
	}, prbody.ValidationLimits{})
	if err != nil {
		t.Fatalf("updateOwnedPRBody: %v", err)
	}
	if len(host.updates) != 1 || host.updates[0].Body != candidate || host.updates[0].Title != "" || host.updates[0].Base != "" {
		t.Fatalf("updates = %+v", host.updates)
	}
	if !strings.Contains(host.updates[0].Body, "human preamble\r\n") || !strings.Contains(host.updates[0].Body, "- [x] human checklist\n") {
		t.Fatalf("unowned content drifted:\n%s", host.updates[0].Body)
	}
}

func TestUpdateOwnedPRBodyRefusesConcurrentRevisionChangeBeforeWrite(t *testing.T) {
	t.Parallel()

	original := ownedBodyForPublisherTest(t, "old")
	concurrent := "human edit\n" + original
	host := &scriptedPRBodyHost{capable: true, reads: []scm.PRBodySnapshot{
		scm.NewPRBodySnapshot(original),
		scm.NewPRBodySnapshot(concurrent),
	}}

	err := updateOwnedPRBody(context.Background(), host, &scm.PR{Number: "42"}, prbody.PatchSet{
		Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: "new"}},
	}, prbody.ValidationLimits{})
	if !errors.Is(err, ErrPRBodyRevisionChanged) {
		t.Fatalf("error = %v, want ErrPRBodyRevisionChanged", err)
	}
	if len(host.updates) != 0 {
		t.Fatalf("concurrent edit was overwritten: %+v", host.updates)
	}
}

func TestUpdateOwnedPRBodyFailsClosedBeforeWrite(t *testing.T) {
	t.Parallel()

	valid := ownedBodyForPublisherTest(t, "old")
	secretPatch := prbody.PatchSet{Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: "ghp_abcdefghijklmnopqrstuvwx12"}}}
	tests := []struct {
		name    string
		capable bool
		body    string
		patches prbody.PatchSet
		limits  prbody.ValidationLimits
		want    error
	}{
		{name: "unsupported provider", capable: false, body: valid, patches: secretPatch, want: scm.ErrUnsupported},
		{name: "missing markers", capable: true, body: "human body", patches: secretPatch, want: prbody.ErrMissingMarkers},
		{name: "corrupt marker hash", capable: true, body: strings.Replace(valid, "old", "edited", 1), patches: secretPatch, want: prbody.ErrCorruptMarkers},
		{name: "conflicting markers", capable: true, body: valid + "\n" + valid, patches: secretPatch, want: prbody.ErrConflictingMarkers},
		{name: "invalid utf8", capable: true, body: string([]byte{0xff, 0xfe}), patches: secretPatch, want: prbody.ErrInvalidUTF8},
		{name: "secret candidate", capable: true, body: valid, patches: secretPatch, want: prbody.ErrSecretDetected},
		{name: "oversized candidate", capable: true, body: valid, patches: prbody.PatchSet{Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: strings.Repeat("x", 100)}}}, limits: prbody.ValidationLimits{MaxBytes: len(valid)}, want: prbody.ErrOversize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := &scriptedPRBodyHost{capable: tt.capable}
			if tt.capable {
				host.reads = []scm.PRBodySnapshot{scm.NewPRBodySnapshot(tt.body)}
			}
			err := updateOwnedPRBody(context.Background(), host, &scm.PR{Number: "42"}, tt.patches, tt.limits)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if len(host.updates) != 0 {
				t.Fatalf("unsafe candidate was published: %+v", host.updates)
			}
		})
	}
}

func TestUpdateOwnedPRBodyFailsWhenRereadDiffers(t *testing.T) {
	t.Parallel()

	original := ownedBodyForPublisherTest(t, "old")
	host := &scriptedPRBodyHost{capable: true, reads: []scm.PRBodySnapshot{
		scm.NewPRBodySnapshot(original),
		scm.NewPRBodySnapshot(original),
		scm.NewPRBodySnapshot(original),
	}}

	err := updateOwnedPRBody(context.Background(), host, &scm.PR{Number: "42"}, prbody.PatchSet{
		Version: prbody.PatchVersion, Sections: []prbody.SectionPatch{{ID: "summary", Content: "new"}},
	}, prbody.ValidationLimits{})
	if !errors.Is(err, ErrPRBodyVerification) {
		t.Fatalf("error = %v, want ErrPRBodyVerification", err)
	}
	if len(host.updates) != 1 {
		t.Fatalf("updates = %d, want one uncertain publication", len(host.updates))
	}
}

func TestVerifyCreatedPRBodyRequiresCapabilityAndExactReread(t *testing.T) {
	t.Parallel()

	body := ownedBodyForPublisherTest(t, "new")
	pr := &scm.PR{Number: "99"}
	host := &scriptedPRBodyHost{capable: true, reads: []scm.PRBodySnapshot{scm.NewPRBodySnapshot(body)}}
	if err := verifyCreatedPRBody(context.Background(), host, pr, body, prbody.ValidationLimits{}); err != nil {
		t.Fatalf("verifyCreatedPRBody: %v", err)
	}

	unsupported := &scriptedPRBodyHost{}
	if err := verifyCreatedPRBody(context.Background(), unsupported, pr, body, prbody.ValidationLimits{}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("unsupported error = %v", err)
	}
}
