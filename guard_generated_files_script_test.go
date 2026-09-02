package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	guardBootstrapBase                   = "2f44ab72acf8c0a064330b668e676672335ebd98"
	guardBootstrapCommit                 = "cf50e0a35e0e635d114dbaeedd496374482d2c16"
	guardBootstrapAdoption               = "db354ad276cb8dce961802d7091a5d618b2417b2"
	guardBootstrapAdoptionParent         = "51463ebddddce564f122f6b3cfbc74af22cafd4d"
	guardBootstrapCanonical              = "aba31dbf93993ac4e8ab7b468982a7dad08e938e"
	guardBootstrapOlderCanonical         = "867d64d9c2df89f3f204ad1f5528e5bf7b460caa"
	guardProvenanceTagPrefix             = "no-mistakes/generated-file-provenance/"
	guardAdditionalTrustedHistoryCommits = 327
	guardMaxPRCommits                    = 512
	guardChangelogV1                     = "# Changelog\n\n## 1.1.0\n"
	guardChangelogVMid                   = "# Changelog\n\n## 1.1.5\n"
	guardChangelogV2                     = "# Changelog\n\n## 1.2.0\n"
	guardChangelogV3                     = "# Changelog\n\n## 1.3.0\n"
	guardChangelogV4                     = "# Changelog\n\n## 1.4.0\n"
	guardManifestV1                      = "{\".\":\"1.1.0\"}\n"
	guardManifestVMid                    = "{\".\":\"1.1.5\"}\n"
	guardManifestV2                      = "{\".\":\"1.2.0\"}\n"
	guardManifestV3                      = "{\".\":\"1.3.0\"}\n"
	guardManifestV4                      = "{\".\":\"1.4.0\"}\n"
)

func TestGeneratedFileGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated-file guard is a POSIX shell script")
	}

	t.Run("source-only change passes after validating upstream provenance", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "feat: change source", map[string]string{"source.txt": "feature\n"})
		fixture.assertPasses(fixture.upstream)
	})

	t.Run("source-only change fails when upstream provenance is unavailable", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "feat: change source", map[string]string{"source.txt": "feature\n"})
		fixture.assertFails(filepath.Join(t.TempDir(), "missing-upstream"))
	})

	t.Run("authenticated pending release passes before canonical merge", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		head := fixture.commit(fixture.pr, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))

		output, err := runGeneratedGuard(t, fixture.pr, fixture.script, fixture.base, head, fixture.upstream)
		if err == nil {
			t.Fatalf("guard should reject an unauthenticated pending release\n%s", output)
		}
		output, err = runGeneratedGuard(t, fixture.pr, fixture.script, fixture.base, head, fixture.upstream, head)
		if err != nil {
			t.Fatalf("guard should accept an authenticated pending release: %v\n%s", err, output)
		}
	})

	t.Run("authenticated pending release cannot bundle source changes", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		changes := guardReleaseFiles(guardChangelogV1, guardManifestV1)
		changes["source.txt"] = "bundled source change\n"
		head := fixture.commit(fixture.pr, "chore(main): release 1.1.0", changes)

		output, err := runGeneratedGuard(t, fixture.pr, fixture.script, fixture.base, head, fixture.upstream, head)
		if err == nil {
			t.Fatalf("guard should reject an authenticated pending release with source changes\n%s", output)
		}
	})

	t.Run("fork release attestation remains trusted by later pull requests", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		pending := fixture.commit(fixture.pr, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		output, err := runGeneratedGuard(t, fixture.pr, fixture.script, fixture.base, pending, fixture.upstream, pending)
		if err != nil {
			t.Fatalf("guard should accept the authenticated pending release: %v\n%s", err, output)
		}
		fixture.git(fixture.pr, "tag", guardProvenanceTagPrefix+pending, pending)
		fixture.base = pending
		fixture.commit(fixture.pr, "feat: source after release", map[string]string{"source.txt": "feature\n"})

		output, err = fixture.run(fixture.upstream)
		if err == nil {
			t.Fatalf("guard should reject a fork release without access to its attestation repository\n%s", output)
		}
		if !strings.Contains(output, "must match exactly one canonical generated-file tuple") {
			t.Fatalf("guard failure = %q, want missing durable provenance", output)
		}

		head := strings.TrimSpace(fixture.git(fixture.pr, "rev-parse", "HEAD"))
		output, err = runGeneratedGuardWithAttestations(
			t, fixture.pr, fixture.script, fixture.base, head, fixture.upstream, fixture.pr,
		)
		if err != nil {
			t.Fatalf("guard should consume the fork's durable release attestation: %v\n%s", err, output)
		}

		fixture.git(fixture.pr, "tag", "-d", guardProvenanceTagPrefix+pending)
		output, err = runGeneratedGuardWithAttestations(
			t, fixture.pr, fixture.script, fixture.base, head, fixture.upstream, fixture.pr,
		)
		if err == nil {
			t.Fatalf("guard should not retain a deleted release attestation\n%s", output)
		}
		if !strings.Contains(output, "must match exactly one canonical generated-file tuple") {
			t.Fatalf("guard failure = %q, want pruned durable provenance", output)
		}
	})

	t.Run("trusted bootstrap normalizes the historical base rollback", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		output, err := runGeneratedGuard(t, repo, script, guardBootstrapBase, guardBootstrapBase, upstream)
		if err != nil {
			t.Fatalf("guard should accept the trusted bootstrap normalization: %v\n%s", err, output)
		}
	})

	t.Run("trusted bootstrap rejects a relocated mainline adoption", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", guardBootstrapAdoption+"^{tree}"))
		base := strings.TrimSpace(guardGit(
			t,
			repo,
			"commit-tree",
			tree,
			"-p",
			guardBootstrapAdoptionParent,
			"-p",
			guardBootstrapAdoption,
			"-m",
			"Relocate provenance bootstrap adoption",
		))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err == nil {
			t.Fatalf("guard should reject a relocated bootstrap adoption\n%s", output)
		}
		if !strings.Contains(output, "exact pinned provenance adoption") {
			t.Fatalf("guard failure = %q, want pinned adoption rejection", output)
		}
	})

	t.Run("trusted bootstrap rejects a removed mainline adoption", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", guardBootstrapAdoption+"^{tree}"))
		base := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", guardBootstrapAdoptionParent, "-m", "Replace provenance adoption"))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err == nil {
			t.Fatalf("guard should reject a removed bootstrap adoption\n%s", output)
		}
		if !strings.Contains(output, "exact pinned provenance adoption") {
			t.Fatalf("guard failure = %q, want pinned adoption rejection", output)
		}
	})

	t.Run("trusted bootstrap audits later base history", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		guardGit(t, repo, "switch", "-q", "--detach", guardBootstrapBase)
		guardWriteFile(t, repo, "CHANGELOG.md", "transient manual edit\n")
		guardGit(t, repo, "add", "CHANGELOG.md")
		guardGit(t, repo, "commit", "-q", "-m", "docs: edit generated file")
		guardGit(t, repo, "checkout", guardBootstrapBase, "--", "CHANGELOG.md")
		guardGit(t, repo, "commit", "-q", "-m", "docs: restore generated file")
		base := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		guardWriteFile(t, repo, "source.txt", "feature\n")
		guardGit(t, repo, "add", "source.txt")
		guardGit(t, repo, "commit", "-q", "-m", "feat: change source")
		head := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))

		output, err := runGeneratedGuard(t, repo, script, base, head, upstream)
		if err == nil {
			t.Fatalf("guard should reject transient generated edits in base history\n%s", output)
		}
		if !strings.Contains(output, "generated files changed in a noncanonical commit") {
			t.Fatalf("guard failure = %q, want noncanonical generated change", output)
		}
	})

	t.Run("trusted bootstrap rejects an actual canonical release older than its floor", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		head := guardGeneratedMergeCommit(t, repo, guardBootstrapBase, guardBootstrapOlderCanonical)

		output, err := runGeneratedGuard(t, repo, script, guardBootstrapBase, head, upstream)
		if err == nil {
			t.Fatalf("guard should reject a canonical release older than the bootstrap floor\n%s", output)
		}
	})

	t.Run("trusted bootstrap accepts a commit-preserving canonical successor", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		head := guardGeneratedMergeCommit(t, repo, guardBootstrapBase, candidate)

		output, err := runGeneratedGuard(t, repo, script, guardBootstrapBase, head, upstream)
		if err != nil {
			t.Fatalf("guard should accept a canonical successor: %v\n%s", err, output)
		}
	})

	t.Run("trusted base accepts a host-rewritten canonical successor", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		base := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		guardWriteFile(t, repo, "source.txt", "feature\n")
		guardGit(t, repo, "add", "source.txt")
		guardGit(t, repo, "commit", "-q", "-m", "feat: change source")
		head := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))

		output, err := runGeneratedGuard(t, repo, script, base, head, upstream)
		if err != nil {
			t.Fatalf("guard should accept trusted host rewrite provenance: %v\n%s", err, output)
		}
	})

	t.Run("trusted base follows attested provenance across host rewrites", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		guardGit(t, repo, "switch", "-q", "--detach", guardBootstrapBase)
		guardWriteFile(t, repo, "CHANGELOG.md", guardChangelogV3)
		guardWriteFile(t, repo, ".release-please-manifest.json", guardManifestV3)
		guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
		guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 1.3.0")
		firstRelease := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		guardGit(t, repo, "tag", guardProvenanceTagPrefix+firstRelease, firstRelease)

		firstRewrite := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, firstRelease)
		guardWriteFile(t, repo, "CHANGELOG.md", guardChangelogV4)
		guardWriteFile(t, repo, ".release-please-manifest.json", guardManifestV4)
		guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
		guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 1.4.0")
		secondRelease := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		guardGit(t, repo, "tag", guardProvenanceTagPrefix+secondRelease, secondRelease)

		base := guardGeneratedRewriteCommit(t, repo, firstRewrite, secondRelease)
		output, err := runGeneratedGuardWithAttestations(t, repo, script, base, base, upstream, repo)
		if err != nil {
			t.Fatalf("guard should follow authenticated predecessor provenance across rewrites: %v\n%s", err, output)
		}
	})

	t.Run("trusted base preserves rewritten provenance through a later merge", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		rewrite := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		guardGit(t, repo, "switch", "-q", "-c", "stale-side", guardBootstrapBase)
		guardWriteFile(t, repo, "side.txt", "side\n")
		guardGit(t, repo, "add", "side.txt")
		guardGit(t, repo, "commit", "-q", "-m", "feat: stale side change")
		side := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", rewrite+"^{tree}"))
		base := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", rewrite, "-p", side, "-m", "Merge stale side"))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err != nil {
			t.Fatalf("guard should preserve rewritten provenance through a trusted merge: %v\n%s", err, output)
		}
	})

	t.Run("trusted base merge cannot discard a newer canonical side release", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", guardBootstrapBase+"^{tree}"))
		base := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", guardBootstrapBase, "-p", candidate, "-m", "Discard newer canonical side release"))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err == nil {
			t.Fatalf("guard should reject a trusted merge that discards newer canonical provenance\n%s", output)
		}
		if !strings.Contains(output, "roll back canonical provenance") {
			t.Fatalf("guard failure = %q, want canonical rollback rejection", output)
		}
	})

	t.Run("trusted base merge can adopt monotonic rewritten side provenance", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		rewrite := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", rewrite+"^{tree}"))
		base := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", guardBootstrapBase, "-p", rewrite, "-m", "Adopt rewritten canonical side release"))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err != nil {
			t.Fatalf("guard should accept a trusted merge that adopts monotonic rewritten provenance: %v\n%s", err, output)
		}
	})

	t.Run("trusted side merge preserves rewritten provenance", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		rewrite := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		guardGit(t, repo, "switch", "-q", "-c", "stale-rewrite-side", guardBootstrapBase)
		guardWriteFile(t, repo, "side.txt", "side\n")
		guardGit(t, repo, "add", "side.txt")
		guardGit(t, repo, "commit", "-q", "-m", "feat: stale side change")
		staleSide := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", rewrite+"^{tree}"))
		sideMerge := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", rewrite, "-p", staleSide, "-m", "Merge stale side into rewritten provenance"))
		base := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", rewrite, "-p", sideMerge, "-m", "Merge trusted rewritten side history"))

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err != nil {
			t.Fatalf("guard should preserve logical rewrite provenance in trusted side history: %v\n%s", err, output)
		}
	})

	t.Run("PR merge preserves rewritten base provenance", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		base := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		guardGit(t, repo, "switch", "-q", "-c", "stale-pr-side", guardBootstrapBase)
		guardWriteFile(t, repo, "side.txt", "side\n")
		guardGit(t, repo, "add", "side.txt")
		guardGit(t, repo, "commit", "-q", "-m", "feat: stale PR side change")
		side := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", base+"^{tree}"))
		head := strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", base, "-p", side, "-m", "Merge stale PR side"))

		output, err := runGeneratedGuard(t, repo, script, base, head, upstream)
		if err != nil {
			t.Fatalf("guard should preserve rewritten provenance through a PR merge: %v\n%s", err, output)
		}
	})

	t.Run("trusted base rejects a non-monotonic host rewrite", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		newerBase := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)
		base := guardGeneratedRewriteCommit(t, repo, newerBase, guardBootstrapCanonical)

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err == nil {
			t.Fatalf("guard should reject non-monotonic trusted rewrite provenance\n%s", output)
		}
		if !strings.Contains(output, "roll back canonical provenance") {
			t.Fatalf("guard failure = %q, want canonical rollback rejection", output)
		}
	})

	t.Run("untrusted head rejects a host-equivalent canonical rewrite", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		candidate := addGeneratedGuardCanonicalSuccessor(t, repo, upstream)
		head := guardGeneratedRewriteCommit(t, repo, guardBootstrapBase, candidate)

		output, err := runGeneratedGuard(t, repo, script, guardBootstrapBase, head, upstream)
		if err == nil {
			t.Fatalf("guard should reject rewritten provenance in an untrusted head\n%s", output)
		}
		if !strings.Contains(output, "generated files changed in a noncanonical commit") {
			t.Fatalf("guard failure = %q, want noncanonical generated change", output)
		}
	})

	t.Run("trusted base history has no fixed lifetime commit ceiling", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", guardBootstrapBase+"^{tree}"))
		base := guardBootstrapBase
		for i := 0; i < guardAdditionalTrustedHistoryCommits; i++ {
			base = strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", base, "-m", fmt.Sprintf("chore: trusted history %d", i)))
		}

		output, err := runGeneratedGuard(t, repo, script, base, base, upstream)
		if err != nil {
			t.Fatalf("guard should audit trusted history without a lifetime ceiling: %v\n%s", err, output)
		}
	})

	t.Run("PR history accepts the exact audit limit and rejects one more commit", func(t *testing.T) {
		repo, upstream, script := newGeneratedGuardBootstrapFixture(t)
		tree := strings.TrimSpace(guardGit(t, repo, "rev-parse", guardBootstrapBase+"^{tree}"))
		head := guardBootstrapBase
		for i := 0; i < guardMaxPRCommits; i++ {
			head = strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", head, "-m", fmt.Sprintf("chore: PR history %d", i)))
		}

		output, err := runGeneratedGuard(t, repo, script, guardBootstrapBase, head, upstream)
		if err != nil {
			t.Fatalf("guard should accept PR history at the audit limit: %v\n%s", err, output)
		}

		head = strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", head, "-m", "chore: exceed PR history limit"))
		output, err = runGeneratedGuard(t, repo, script, guardBootstrapBase, head, upstream)
		if err == nil {
			t.Fatalf("guard should reject PR history above the audit limit\n%s", output)
		}
		if !strings.Contains(output, "PR commits exceeds the audit limit") {
			t.Fatalf("guard failure = %q, want PR history audit limit rejection", output)
		}
	})

	t.Run("source-only head behind the current base release fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.prepareStaleFeatureAndCurrentBase()
		fixture.assertFails(fixture.upstream)
	})

	t.Run("manual generated edit fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "manual\n"})
		fixture.assertFails(fixture.upstream)
	})

	t.Run("altered then restored generated entry fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "manual\n"})
		fixture.commit(fixture.pr, "docs: restore changelog", map[string]string{"CHANGELOG.md": fixture.baseChangelog})
		fixture.assertFails(fixture.upstream)
	})

	t.Run("renaming a generated file fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.git(fixture.pr, "mv", "CHANGELOG.md", "CHANGELOG.old")
		fixture.git(fixture.pr, "commit", "-q", "-m", "docs: rename changelog")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("deleting a generated file fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.git(fixture.pr, "rm", "CHANGELOG.md")
		fixture.git(fixture.pr, "commit", "-q", "-m", "docs: delete changelog")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("upstream sync with many canonical commits and releases passes", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "feat: canonical source change", map[string]string{"source.txt": "canonical feature\n"})
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.mergeCanonicalMain()
		fixture.assertPasses(fixture.upstream)
	})

	t.Run("merge commit carrying canonical release entries passes", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.pr, "feat: fork source change", map[string]string{"fork.txt": "feature\n"})
		fixture.mergeCanonicalMainWithCommit()
		fixture.assertPasses(fixture.upstream)
	})

	t.Run("merge synthesizing entries from different parents fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		left := fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "left\n"})
		fixture.git(fixture.pr, "reset", "--hard", fixture.base)
		fixture.commit(fixture.pr, "chore: edit manifest", map[string]string{".release-please-manifest.json": "{\".\":\"right\"}\n"})
		fixture.git(fixture.pr, "merge", "--no-ff", "-m", "Merge synthetic generated entries", left)
		fixture.assertFailsContaining(fixture.upstream, "merge commit synthesizes generated entries")
	})

	t.Run("merge carrying the current base release entries passes", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.prepareStaleFeatureAndCurrentBase()
		fixture.git(fixture.pr, "merge", "--no-ff", "-m", "Merge current base", "FETCH_HEAD")
		fixture.assertPasses(fixture.upstream)
	})

	t.Run("merge retaining stale first-parent release entries fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.prepareStaleFeatureAndCurrentBase()
		fixture.git(fixture.pr, "merge", "--no-ff", "-s", "ours", "-m", "Merge current base", "FETCH_HEAD")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("later merge cannot restore an intermediate canonical release", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.base = fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.git(fixture.pr, "fetch", "-q", fixture.upstream, "main")
		fixture.git(fixture.pr, "reset", "--hard", "FETCH_HEAD")
		middle := fixture.commit(fixture.upstream, "chore(main): release 1.1.5", guardReleaseFiles(guardChangelogVMid, guardManifestVMid))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.git(fixture.pr, "fetch", "-q", fixture.upstream, "main")
		fixture.git(fixture.pr, "switch", "-q", "-c", "stale-release", middle)
		fixture.commit(fixture.pr, "feat: branch from intermediate release", map[string]string{"stale.txt": "feature\n"})
		fixture.git(fixture.pr, "switch", "-q", "main")
		fixture.git(fixture.pr, "fetch", "-q", fixture.upstream, "main")
		fixture.git(fixture.pr, "merge", "--no-ff", "-m", "Merge current canonical main", "FETCH_HEAD")
		fixture.git(fixture.pr, "merge", "--no-ff", "--no-commit", "-s", "ours", "stale-release")
		fixture.git(fixture.pr, "checkout", "stale-release", "--", "CHANGELOG.md", ".release-please-manifest.json")
		fixture.git(fixture.pr, "commit", "-q", "-m", "Merge stale release branch")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("base rollback cannot lower its canonical provenance floor", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		middle := fixture.commit(fixture.upstream, "chore(main): release 1.1.5", guardReleaseFiles(guardChangelogVMid, guardManifestVMid))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.git(fixture.pr, "fetch", "-q", fixture.upstream, "main")
		fixture.git(fixture.pr, "reset", "--hard", "FETCH_HEAD")
		fixture.base = fixture.commit(fixture.pr, "docs: restore old base release", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.git(fixture.pr, "switch", "-q", "-c", "stale-release", middle)
		fixture.commit(fixture.pr, "feat: branch from intermediate release", map[string]string{"stale.txt": "feature\n"})
		fixture.git(fixture.pr, "switch", "-q", "main")
		fixture.git(fixture.pr, "merge", "--no-ff", "--no-commit", "-s", "ours", "stale-release")
		fixture.git(fixture.pr, "checkout", "stale-release", "--", "CHANGELOG.md", ".release-please-manifest.json")
		fixture.git(fixture.pr, "commit", "-q", "-m", "Merge intermediate release branch")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("ambiguous matching releases fail", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.commit(fixture.upstream, "chore(main): restore 1.1.0 release files", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.mergeCanonicalMain()
		fixture.assertFails(fixture.upstream)
	})

	t.Run("duplicate tuples on disjoint canonical branches fail", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.git(fixture.upstream, "switch", "-q", "-c", "duplicate-left", fixture.base)
		left := fixture.commit(fixture.upstream, "chore(main): left release", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.git(fixture.upstream, "switch", "-q", "-c", "duplicate-right", fixture.base)
		right := fixture.commit(fixture.upstream, "chore(main): right release", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		tree := strings.TrimSpace(fixture.git(fixture.upstream, "rev-parse", left+"^{tree}"))
		base := strings.TrimSpace(fixture.git(fixture.upstream, "commit-tree", tree, "-p", left, "-p", right, "-m", "Merge duplicate releases"))
		fixture.git(fixture.upstream, "update-ref", "refs/heads/main", base)
		fixture.git(fixture.pr, "fetch", "-q", fixture.upstream, "main")
		fixture.git(fixture.pr, "reset", "--hard", "FETCH_HEAD")
		fixture.base = base

		fixture.assertFailsContaining(fixture.upstream, "reuses a generated-file tuple")
	})

	t.Run("copied canonical tuple fails even when the final release is unique", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.commit(fixture.upstream, "chore(main): copy release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "chore(main): release 1.3.0", guardReleaseFiles(guardChangelogV3, guardManifestV3))
		fixture.mergeCanonicalMain()
		fixture.assertFailsContaining(fixture.upstream, "generated-file tuple")
	})

	t.Run("copied canonical blobs without ancestry fail", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.pr, "docs: copy release files", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.assertFails(fixture.upstream)
	})

	t.Run("fork commit restoring an older canonical release fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.mergeCanonicalMain()
		fixture.commit(fixture.pr, "docs: restore older release files", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.assertFails(fixture.upstream)
	})

	t.Run("canonical commit with a source change is not a release candidate", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		changes := guardReleaseFiles(guardChangelogV1, guardManifestV1)
		changes["source.txt"] = "bundled source change\n"
		fixture.commit(fixture.upstream, "chore(main): bundled release", changes)
		fixture.mergeCanonicalMain()
		fixture.assertFails(fixture.upstream)
	})

	t.Run("canonical release with executable generated entries fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.git(fixture.upstream, "update-index", "--chmod=+x", "CHANGELOG.md", ".release-please-manifest.json")
		fixture.git(fixture.upstream, "commit", "-q", "--amend", "--no-edit")
		fixture.mergeCanonicalMain()
		fixture.assertFails(fixture.upstream)
	})

	t.Run("post-release mode drift fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.mergeCanonicalMain()
		fixture.git(fixture.pr, "update-index", "--chmod=+x", "CHANGELOG.md", ".release-please-manifest.json")
		fixture.git(fixture.pr, "commit", "-q", "-m", "docs: change generated file modes")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("post-release symlink drift fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.mergeCanonicalMain()
		blob := strings.TrimSpace(fixture.git(fixture.pr, "rev-parse", "HEAD:CHANGELOG.md"))
		fixture.git(fixture.pr, "update-index", "--cacheinfo", "120000,"+blob+",CHANGELOG.md")
		fixture.git(fixture.pr, "commit", "-q", "-m", "docs: replace changelog with symlink entry")
		fixture.assertFails(fixture.upstream)
	})

	t.Run("missing canonical main fails closed", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "manual\n"})
		missing := filepath.Join(t.TempDir(), "missing-upstream")
		fixture.assertFails(missing)
	})

	t.Run("multiple merge bases fail before the no-change fast path", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		tree := strings.TrimSpace(fixture.git(fixture.pr, "rev-parse", "HEAD^{tree}"))
		left := strings.TrimSpace(fixture.git(fixture.pr, "commit-tree", tree, "-p", fixture.base, "-m", "left"))
		right := strings.TrimSpace(fixture.git(fixture.pr, "commit-tree", tree, "-p", fixture.base, "-m", "right"))
		leftMerge := strings.TrimSpace(fixture.git(fixture.pr, "commit-tree", tree, "-p", left, "-p", right, "-m", "merge left"))
		rightMerge := strings.TrimSpace(fixture.git(fixture.pr, "commit-tree", tree, "-p", right, "-p", left, "-m", "merge right"))
		fixture.assertFailsBetween(leftMerge, rightMerge, filepath.Join(t.TempDir(), "missing-upstream"))
	})

	t.Run("malformed commit selectors fail closed", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		head := strings.TrimSpace(fixture.git(fixture.pr, "rev-parse", "HEAD"))
		missingUpstream := filepath.Join(t.TempDir(), "missing-upstream")
		fixture.assertFailsBetween("not-a-commit", head, missingUpstream)
		fixture.assertFailsBetween(fixture.base, "not-a-commit", missingUpstream)
	})

	t.Run("commits with excessive parents fail closed", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		tree := strings.TrimSpace(fixture.git(fixture.pr, "write-tree"))
		mergeArgs := []string{"commit-tree", tree}
		for i := 0; i < 33; i++ {
			parent := strings.TrimSpace(fixture.git(fixture.pr, "commit-tree", tree, "-p", fixture.base, "-m", strings.Repeat("parent", i+1)))
			mergeArgs = append(mergeArgs, "-p", parent)
		}
		mergeArgs = append(mergeArgs, "-m", "merge")
		head := strings.TrimSpace(fixture.git(fixture.pr, mergeArgs...))
		fixture.assertFailsBetweenContaining(fixture.base, head, fixture.upstream, "commit has too many parents")
	})
}

func TestReleasePleaseOutputVerifierRejectsReplacementHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-please output verifier is a POSIX shell script")
	}

	repo := t.TempDir()
	guardGit(t, repo, "init", "-q")
	guardGit(t, repo, "config", "user.email", "release-test@example.com")
	guardGit(t, repo, "config", "user.name", "Release Test")
	guardWriteFile(t, repo, "CHANGELOG.md", "# Changelog\n")
	guardWriteFile(t, repo, ".release-please-manifest.json", "{\".\":\"1.0.0\"}\n")
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "commit", "-q", "-m", "chore: base")
	base := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))

	expected := t.TempDir()
	guardWriteFile(t, expected, "CHANGELOG.md", "# Changelog\n\n## 1.1.0\n")
	guardWriteFile(t, expected, ".release-please-manifest.json", "{\".\":\"1.1.0\"}\n")
	guardWriteFile(t, repo, "CHANGELOG.md", "# Changelog\n\n## 1.1.0\n")
	guardWriteFile(t, repo, ".release-please-manifest.json", "{\".\":\"1.1.0\"}\n")
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 1.1.0")
	generatedHead := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))

	output, err := runReleasePleaseOutputVerifier(t, repo, generatedHead, expected)
	if err != nil {
		t.Fatalf("verifier should accept captured producer release blobs: %v\n%s", err, output)
	}

	guardGit(t, repo, "switch", "-q", "--detach", base)
	guardWriteFile(t, repo, "CHANGELOG.md", "forged release\n")
	guardWriteFile(t, repo, ".release-please-manifest.json", "{\".\":\"99.0.0\"}\n")
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 99.0.0")
	replacementHead := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))

	output, err = runReleasePleaseOutputVerifier(t, repo, replacementHead, expected)
	if err == nil {
		t.Fatalf("verifier should reject a force-pushed replacement head\n%s", output)
	}
	if !strings.Contains(output, "does not match captured release-please producer output") {
		t.Fatalf("verifier failure = %q, want captured-output mismatch", output)
	}
}

func TestGeneratedFileGuardWorkflowIsBaseControlled(t *testing.T) {
	workflow := strings.ReplaceAll(guardReadFile(t, ".github/workflows/guard-generated-file-provenance.yml"), "\r\n", "\n")

	for _, want := range []string{
		"pull_request_target:",
		"types: [opened, synchronize, reopened, edited]",
		"branches: [main]",
		"group: guard-generated-file-provenance-${{ github.event.pull_request.number }}",
		"cancel-in-progress: true",
		"contents: read",
		"actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803",
		"persist-credentials: false",
		"ref: ${{ github.event.pull_request.base.sha }}",
		"checked_out_base=$(git rev-parse --verify 'HEAD^{commit}')",
		`[ "$checked_out_base" != "$BASE_SHA" ]`,
		"Checked-out pull request base does not match the event",
		"refs/pull/${PR_NUMBER}/head",
		"Fetched pull request head does not match the event",
		`git show "${checked_out_base}:scripts/guard-generated-files.sh"`,
		"https://github.com/kunchenguid/no-mistakes.git",
		"https://github.com/alvarohulse/no-mistakes.git",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("trusted workflow must contain %q", want)
		}
	}
	for _, forbidden := range []string{
		"actions/checkout@v6",
		"github.event.pull_request.head.repo.clone_url",
		"github.event.pull_request.head.ref",
		"github.event.pull_request.user.login",
		"PENDING_RELEASE_SHA",
		`git show "${BASE_SHA}:scripts/guard-generated-files.sh"`,
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("trusted workflow must not use untrusted value %q", forbidden)
		}
	}

	baseValidation := `checked_out_base=$(git rev-parse --verify 'HEAD^{commit}')
          if [ "$checked_out_base" != "$BASE_SHA" ]; then
            echo "::error::Checked-out pull request base does not match the event" >&2
            exit 1
          fi`
	baseValidationStart := strings.Index(workflow, baseValidation)
	if baseValidationStart == -1 {
		t.Fatal("trusted workflow must validate the checked-out base before using it")
	}
	headValidation := `case "$PR_NUMBER" in
            ''|*[!0-9]*)
              echo "::error::Pull request number is not numeric" >&2
              exit 1
              ;;
          esac

          pr_head_ref=refs/no-mistakes/guard-generated-files/pr-head
          git fetch --no-tags --force origin \
            "+refs/pull/${PR_NUMBER}/head:${pr_head_ref}"
          fetched_head=$(git rev-parse --verify "${pr_head_ref}^{commit}")
          if [ "$fetched_head" != "$HEAD_SHA" ]; then
            echo "::error::Fetched pull request head does not match the event" >&2
            exit 1
          fi`
	headValidationStart := strings.Index(workflow, headValidation)
	if headValidationStart == -1 {
		t.Fatal("trusted workflow must validate the numeric pull request head before using it")
	}
	validationEnd := headValidationStart + len(headValidation)
	if baseValidationStart+len(baseValidation) > validationEnd {
		validationEnd = baseValidationStart + len(baseValidation)
	}
	for _, operation := range []string{
		`git show "${checked_out_base}:scripts/guard-generated-files.sh"`,
		`sh "$trusted_script"`,
	} {
		operationStart := strings.Index(workflow, operation)
		if operationStart == -1 {
			t.Errorf("trusted workflow must contain %q", operation)
			continue
		}
		if operationStart < validationEnd {
			t.Errorf("trusted workflow must validate the checked-out base before %q", operation)
		}
	}
}

type generatedGuardFixture struct {
	t             *testing.T
	script        string
	upstream      string
	pr            string
	base          string
	baseChangelog string
}

func newGeneratedGuardFixture(t *testing.T) *generatedGuardFixture {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("scripts", "guard-generated-files.sh"))
	if err != nil {
		t.Fatalf("resolve guard script: %v", err)
	}
	root := t.TempDir()
	fixture := &generatedGuardFixture{
		t:        t,
		script:   script,
		upstream: filepath.Join(root, "upstream"),
		pr:       filepath.Join(root, "pr"),
	}
	fixture.git("", "clone", "-q", ".", fixture.upstream)
	fixture.configureRepo(fixture.upstream)
	fixture.git(fixture.upstream, "switch", "-q", "-C", "main", guardBootstrapBase)
	fixture.base = guardBootstrapBase
	fixture.baseChangelog = guardReadFile(t, filepath.Join(fixture.upstream, "CHANGELOG.md"))
	fixture.git("", "clone", "-q", fixture.upstream, fixture.pr)
	fixture.configureRepo(fixture.pr)
	return fixture
}

func newGeneratedGuardBootstrapFixture(t *testing.T) (string, string, string) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("scripts", "guard-generated-files.sh"))
	if err != nil {
		t.Fatalf("resolve guard script: %v", err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	upstream := filepath.Join(root, "upstream.git")
	guardGit(t, "", "clone", "-q", ".", repo)
	guardGit(t, repo, "config", "user.email", "guard-test@example.com")
	guardGit(t, repo, "config", "user.name", "Guard Test")
	guardGit(t, repo, "config", "commit.gpgsign", "false")
	guardGit(t, "", "clone", "-q", "--bare", ".", upstream)
	guardGit(t, "", "--git-dir="+upstream, "update-ref", "refs/heads/main", guardBootstrapCanonical)
	return repo, upstream, script
}

func addGeneratedGuardCanonicalSuccessor(t *testing.T, repo, upstream string) string {
	t.Helper()
	guardGit(t, repo, "switch", "-q", "-c", "canonical-successor", guardBootstrapCanonical)
	guardWriteFile(t, repo, "CHANGELOG.md", guardChangelogV3)
	guardWriteFile(t, repo, ".release-please-manifest.json", guardManifestV3)
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 1.43.0")
	candidate := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
	guardGit(t, "", "--git-dir="+upstream, "fetch", "-q", repo, "refs/heads/canonical-successor:refs/heads/main")
	return candidate
}

func guardGeneratedMergeCommit(t *testing.T, repo, base, candidate string) string {
	t.Helper()
	guardGit(t, repo, "switch", "-q", "--detach", base)
	guardGit(t, repo, "checkout", candidate, "--", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	tree := strings.TrimSpace(guardGit(t, repo, "write-tree"))
	return strings.TrimSpace(guardGit(t, repo, "commit-tree", tree, "-p", base, "-p", candidate, "-m", "Merge canonical release"))
}

func guardGeneratedRewriteCommit(t *testing.T, repo, base, candidate string) string {
	t.Helper()
	guardGit(t, repo, "switch", "-q", "--detach", base)
	guardGit(t, repo, "checkout", candidate, "--", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "add", "CHANGELOG.md", ".release-please-manifest.json")
	guardGit(t, repo, "commit", "-q", "-m", "chore: host rewrite canonical release")
	return strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
}

func (f *generatedGuardFixture) configureRepo(repo string) {
	f.t.Helper()
	f.git(repo, "config", "user.email", "guard-test@example.com")
	f.git(repo, "config", "user.name", "Guard Test")
	f.git(repo, "config", "commit.gpgsign", "false")
}

func (f *generatedGuardFixture) commit(repo, message string, files map[string]string) string {
	f.t.Helper()
	for path, content := range files {
		guardWriteFile(f.t, repo, path, content)
	}
	f.git(repo, "add", "--all")
	f.git(repo, "commit", "-q", "-m", message)
	return strings.TrimSpace(f.git(repo, "rev-parse", "HEAD"))
}

func (f *generatedGuardFixture) mergeCanonicalMain() {
	f.t.Helper()
	f.git(f.pr, "fetch", "-q", f.upstream, "main")
	f.git(f.pr, "merge", "--ff-only", "FETCH_HEAD")
}

func (f *generatedGuardFixture) mergeCanonicalMainWithCommit() {
	f.t.Helper()
	f.git(f.pr, "fetch", "-q", f.upstream, "main")
	f.git(f.pr, "merge", "--no-ff", "-m", "Merge canonical main", "FETCH_HEAD")
}

func (f *generatedGuardFixture) prepareStaleFeatureAndCurrentBase() {
	f.t.Helper()
	f.commit(f.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
	f.git(f.pr, "fetch", "-q", f.upstream, "main")
	f.git(f.pr, "reset", "--hard", "FETCH_HEAD")
	f.commit(f.pr, "feat: fork source change", map[string]string{"fork.txt": "feature\n"})
	f.base = f.commit(f.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
	f.git(f.pr, "fetch", "-q", f.upstream, "main")
}

func (f *generatedGuardFixture) assertPasses(upstream string) {
	f.t.Helper()
	output, err := f.run(upstream)
	if err != nil {
		f.t.Fatalf("guard should pass: %v\n%s", err, output)
	}
}

func (f *generatedGuardFixture) assertFails(upstream string) {
	f.t.Helper()
	output, err := f.run(upstream)
	if err == nil {
		f.t.Fatalf("guard should fail\n%s", output)
	}
}

func (f *generatedGuardFixture) assertFailsContaining(upstream, want string) {
	f.t.Helper()
	output, err := f.run(upstream)
	if err == nil {
		f.t.Fatalf("guard should fail\n%s", output)
	}
	if !strings.Contains(output, want) {
		f.t.Fatalf("guard failure = %q, want it to contain %q", output, want)
	}
}

func (f *generatedGuardFixture) assertFailsBetween(base, head, upstream string) {
	f.t.Helper()
	output, err := f.runBetween(base, head, upstream)
	if err == nil {
		f.t.Fatalf("guard should fail\n%s", output)
	}
}

func (f *generatedGuardFixture) assertFailsBetweenContaining(base, head, upstream, want string) {
	f.t.Helper()
	output, err := f.runBetween(base, head, upstream)
	if err == nil {
		f.t.Fatalf("guard should fail\n%s", output)
	}
	if !strings.Contains(output, want) {
		f.t.Fatalf("guard failure = %q, want it to contain %q", output, want)
	}
}

func (f *generatedGuardFixture) run(upstream string) (string, error) {
	f.t.Helper()
	head := strings.TrimSpace(f.git(f.pr, "rev-parse", "HEAD"))
	return f.runBetween(f.base, head, upstream)
}

func (f *generatedGuardFixture) runBetween(base, head, upstream string) (string, error) {
	f.t.Helper()
	return runGeneratedGuard(f.t, f.pr, f.script, base, head, upstream)
}

func runGeneratedGuard(t *testing.T, repo, script, base, head, upstream string, pending ...string) (string, error) {
	t.Helper()
	return runGeneratedGuardWithAttestations(t, repo, script, base, head, upstream, upstream, pending...)
}

func runGeneratedGuardWithAttestations(t *testing.T, repo, script, base, head, upstream, attestations string, pending ...string) (string, error) {
	t.Helper()
	if len(pending) > 1 {
		t.Fatalf("run generated-file guard with %d pending release SHAs", len(pending))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{script, base, head, upstream, attestations}
	if len(pending) == 1 && pending[0] != "" {
		args = append(args, pending[0])
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Dir = repo
	cmd.Env = guardCommandEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("generated-file guard exceeded its deadline: %v\n%s", ctx.Err(), output)
	}
	return string(output), err
}

func runReleasePleaseOutputVerifier(t *testing.T, repo, head, expected string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("scripts", "verify-release-please-output.sh"))
	if err != nil {
		t.Fatalf("resolve release-please output verifier: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script, head, expected)
	cmd.Dir = repo
	cmd.Env = guardCommandEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("release-please output verifier exceeded its deadline: %v\n%s", ctx.Err(), output)
	}
	return string(output), err
}

func (f *generatedGuardFixture) git(dir string, args ...string) string {
	f.t.Helper()
	return guardGit(f.t, dir, args...)
}

func guardGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmdArgs := args
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = guardCommandEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(cmdArgs, " "), err, output)
	}
	return string(output)
}

func guardReleaseFiles(changelog, manifest string) map[string]string {
	return map[string]string{
		"CHANGELOG.md":                  changelog,
		".release-please-manifest.json": manifest,
	}
}

func guardWriteFile(t *testing.T, repo, path, content string) {
	t.Helper()
	fullPath := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func guardReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func guardCommandEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_CONFIG_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "LC_ALL=C")
}
