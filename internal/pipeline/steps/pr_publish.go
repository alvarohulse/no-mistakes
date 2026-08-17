package steps

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var (
	ErrPRBodyRevisionChanged = errors.New("hosted PR body revision changed before publication")
	ErrPRBodyVerification    = errors.New("hosted PR body failed post-publication verification")
)

type prBodyReader interface {
	Capabilities() scm.Capabilities
	ReadPRBody(context.Context, *scm.PR) (scm.PRBodySnapshot, error)
}

type prBodyUpdater interface {
	prBodyReader
	UpdatePR(context.Context, *scm.PR, scm.PRContent) (*scm.PR, error)
}

func requirePRBodyReadRevision(host prBodyReader) error {
	if host == nil || !host.Capabilities().PRBodyReadRevision {
		return fmt.Errorf("hosted PR body read/revision: %w", scm.ErrUnsupported)
	}
	return nil
}

// updateOwnedPRBody is the only existing-body publication path. The first
// read supplies the merge substrate, the second detects an edit made while the
// candidate was built, and the final read proves the exact bytes landed.
func updateOwnedPRBody(ctx context.Context, host prBodyUpdater, pr *scm.PR, patches prbody.PatchSet, limits prbody.ValidationLimits) error {
	if err := requirePRBodyReadRevision(host); err != nil {
		return err
	}
	original, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("read hosted PR body: %w", err)
	}
	// Keep the exact original snapshot until publication is verified. A future
	// full-body rewrite escape hatch must durably save this value before write;
	// this section-only path never needs or permits that escape hatch.
	candidate, err := prbody.ApplyOwnedPatches(original.Body, patches)
	if err != nil {
		return fmt.Errorf("apply owned PR sections: %w", err)
	}
	if err := prbody.ValidateOwnedDocument(candidate, limits); err != nil {
		return fmt.Errorf("validate owned PR candidate: %w", err)
	}
	if candidate == original.Body {
		return nil
	}

	beforeWrite, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("compare hosted PR body before write: %w", err)
	}
	if beforeWrite.Revision != original.Revision || beforeWrite.Body != original.Body {
		return ErrPRBodyRevisionChanged
	}
	if _, err := host.UpdatePR(ctx, pr, scm.PRContent{Body: candidate}); err != nil {
		return fmt.Errorf("publish owned PR body: %w", err)
	}

	published, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: reread failed: %w", ErrPRBodyVerification, err)
	}
	if err := prbody.ValidateOwnedDocument(published.Body, limits); err != nil {
		return fmt.Errorf("%w: %w", ErrPRBodyVerification, err)
	}
	want := scm.NewPRBodySnapshot(candidate)
	if published.Revision != want.Revision || published.Body != candidate {
		return ErrPRBodyVerification
	}
	return nil
}

func verifyCreatedPRBody(ctx context.Context, host prBodyReader, pr *scm.PR, expected string, limits prbody.ValidationLimits) error {
	if err := requirePRBodyReadRevision(host); err != nil {
		return err
	}
	if err := prbody.ValidateOwnedDocument(expected, limits); err != nil {
		return fmt.Errorf("validate created PR body: %w", err)
	}
	published, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: reread failed: %w", ErrPRBodyVerification, err)
	}
	if err := prbody.ValidateOwnedDocument(published.Body, limits); err != nil {
		return fmt.Errorf("%w: %w", ErrPRBodyVerification, err)
	}
	want := scm.NewPRBodySnapshot(expected)
	if published.Revision != want.Revision || published.Body != expected {
		return ErrPRBodyVerification
	}
	return nil
}
