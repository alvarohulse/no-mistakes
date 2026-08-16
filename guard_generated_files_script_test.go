package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	guardChangelogV1 = "# Changelog\n\n## 1.1.0\n"
	guardChangelogV2 = "# Changelog\n\n## 1.2.0\n"
	guardManifestV1  = "{\".\":\"1.1.0\"}\n"
	guardManifestV2  = "{\".\":\"1.2.0\"}\n"
)

func TestGeneratedFileGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated-file guard is a POSIX shell script")
	}

	t.Run("source-only change passes without fetching upstream", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "feat: change source", map[string]string{"source.txt": "feature\n"})
		fixture.assertPasses(filepath.Join(t.TempDir(), "missing-upstream"))
	})

	t.Run("manual generated edit fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "manual\n"})
		fixture.assertFails(fixture.upstream)
	})

	t.Run("altered then restored generated entry fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.pr, "docs: edit changelog", map[string]string{"CHANGELOG.md": "manual\n"})
		fixture.commit(fixture.pr, "docs: restore changelog", map[string]string{"CHANGELOG.md": "# Changelog\n"})
		fixture.assertFails(fixture.upstream)
	})

	t.Run("renaming a generated file fails", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.git(fixture.pr, "mv", "CHANGELOG.md", "CHANGELOG.old")
		fixture.git(fixture.pr, "commit", "-q", "-m", "docs: rename changelog")
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

	t.Run("ambiguous matching releases fail", func(t *testing.T) {
		fixture := newGeneratedGuardFixture(t)
		fixture.commit(fixture.upstream, "chore(main): release 1.1.0", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.commit(fixture.upstream, "chore(main): release 1.2.0", guardReleaseFiles(guardChangelogV2, guardManifestV2))
		fixture.commit(fixture.upstream, "chore(main): restore 1.1.0 release files", guardReleaseFiles(guardChangelogV1, guardManifestV1))
		fixture.mergeCanonicalMain()
		fixture.assertFails(fixture.upstream)
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
}

func TestGeneratedFileGuardWorkflowIsBaseControlled(t *testing.T) {
	workflow := guardReadFile(t, ".github/workflows/guard-generated-file-provenance.yml")

	for _, want := range []string{
		"pull_request_target:",
		"types: [opened, synchronize, reopened, edited]",
		"contents: read",
		"persist-credentials: false",
		"ref: ${{ github.event.pull_request.base.sha }}",
		"checked_out_base=$(git rev-parse --verify 'HEAD^{commit}')",
		`[ "$checked_out_base" != "$BASE_SHA" ]`,
		"Checked-out pull request base does not match the event",
		"refs/pull/${PR_NUMBER}/head",
		"Fetched pull request head does not match the event",
		`git show "${checked_out_base}:scripts/guard-generated-files.sh"`,
		"https://github.com/kunchenguid/no-mistakes.git",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("trusted workflow must contain %q", want)
		}
	}
	for _, forbidden := range []string{
		"github.event.pull_request.head.repo.clone_url",
		"github.event.pull_request.head.ref",
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
	validationStart := strings.Index(workflow, baseValidation)
	if validationStart == -1 {
		t.Fatal("trusted workflow must validate the checked-out base before using it")
	}
	validationEnd := validationStart + len(baseValidation)
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
	t        *testing.T
	script   string
	upstream string
	pr       string
	base     string
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
	fixture.git("", "init", "-q", "-b", "main", fixture.upstream)
	fixture.configureRepo(fixture.upstream)
	guardWriteFile(t, fixture.upstream, "CHANGELOG.md", "# Changelog\n")
	guardWriteFile(t, fixture.upstream, ".release-please-manifest.json", "{\".\":\"1.0.0\"}\n")
	guardWriteFile(t, fixture.upstream, "source.txt", "base\n")
	fixture.git(fixture.upstream, "add", "--all")
	fixture.git(fixture.upstream, "commit", "-q", "-m", "initial")
	fixture.git(fixture.upstream, "commit", "-q", "--allow-empty", "-m", "base")
	fixture.base = strings.TrimSpace(fixture.git(fixture.upstream, "rev-parse", "HEAD"))
	fixture.git("", "clone", "-q", fixture.upstream, fixture.pr)
	fixture.configureRepo(fixture.pr)
	return fixture
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

func (f *generatedGuardFixture) assertFailsBetween(base, head, upstream string) {
	f.t.Helper()
	output, err := f.runBetween(base, head, upstream)
	if err == nil {
		f.t.Fatalf("guard should fail\n%s", output)
	}
}

func (f *generatedGuardFixture) run(upstream string) (string, error) {
	f.t.Helper()
	head := strings.TrimSpace(f.git(f.pr, "rev-parse", "HEAD"))
	return f.runBetween(f.base, head, upstream)
}

func (f *generatedGuardFixture) runBetween(base, head, upstream string) (string, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", f.script, base, head, upstream)
	cmd.Dir = f.pr
	cmd.Env = guardCommandEnv()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f *generatedGuardFixture) git(dir string, args ...string) string {
	f.t.Helper()
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
		f.t.Fatalf("git %s: %v\n%s", strings.Join(cmdArgs, " "), err, output)
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
