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

type ownedPRBodyUpdate struct {
	original  scm.PRBodySnapshot
	candidate string
	limits    prbody.ValidationLimits
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
	update, err := prepareOwnedPRBodyUpdate(ctx, host, pr, patches, limits)
	if err != nil {
		return err
	}
	return publishOwnedPRBodyUpdate(ctx, host, pr, update, "")
}

// prepareOwnedPRBodyUpdate resolves and validates the complete hosted body
// without mutating the pull request. The caller may then combine a base
// retarget with the body write in one provider update.
func prepareOwnedPRBodyUpdate(ctx context.Context, host prBodyUpdater, pr *scm.PR, patches prbody.PatchSet, limits prbody.ValidationLimits) (ownedPRBodyUpdate, error) {
	update := ownedPRBodyUpdate{limits: limits}
	if err := requirePRBodyReadRevision(host); err != nil {
		return update, err
	}
	original, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return update, fmt.Errorf("read hosted PR body: %w", err)
	}
	update.original = original
	// Keep the exact original snapshot until publication is verified. A future
	// full-body rewrite escape hatch must durably save this value before write;
	// this section-only path never needs or permits that escape hatch.
	candidate, err := prbody.ApplyOwnedPatches(original.Body, patches)
	if err != nil {
		return update, fmt.Errorf("apply owned PR sections: %w", err)
	}
	update.candidate = candidate
	if err := prbody.ValidateOwnedDocument(candidate, limits); err != nil {
		return update, fmt.Errorf("validate owned PR candidate: %w", err)
	}
	return update, nil
}

func publishOwnedPRBodyUpdate(ctx context.Context, host prBodyUpdater, pr *scm.PR, update ownedPRBodyUpdate, base string) error {
	if update.candidate == update.original.Body && base == "" {
		return nil
	}

	beforeWrite, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("compare hosted PR body before write: %w", err)
	}
	if beforeWrite.Revision != update.original.Revision || beforeWrite.Body != update.original.Body {
		return ErrPRBodyRevisionChanged
	}
	content := scm.PRContent{Base: base}
	if update.candidate != update.original.Body {
		content.Body = update.candidate
	}
	if _, err := host.UpdatePR(ctx, pr, content); err != nil {
		return fmt.Errorf("publish owned PR body: %w", err)
	}

	published, err := host.ReadPRBody(ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: reread failed: %w", ErrPRBodyVerification, err)
	}
	if err := prbody.ValidateOwnedDocument(published.Body, update.limits); err != nil {
		return fmt.Errorf("%w: %w", ErrPRBodyVerification, err)
	}
	want := scm.NewPRBodySnapshot(update.candidate)
	if published.Revision != want.Revision || published.Body != update.candidate {
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
