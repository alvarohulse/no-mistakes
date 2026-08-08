package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
)

// prBodyRepo is a git repo whose default branch is main, registered in an
// isolated NM_HOME, with the process chdir'd into it.
type prBodyRepo struct {
	root string
	db   *db.DB
	repo *db.Repo
}

func setupPRBodyRepo(t *testing.T) prBodyRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("formatter fixtures are POSIX")
	}

	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")

	root, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		root = repoDir
	}
	chdir(t, root)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, err := database.InsertRepoWithID("repo-1", root, "https://github.com/example/example.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return prBodyRepo{root: root, db: database, repo: repo}
}

// commitRepoConfig writes .no-mistakes.yaml with the given formatter and commits
// it on the current branch.
func commitRepoConfig(t *testing.T, root, hook, message string) {
	t.Helper()
	body := "hooks:\n  pr_body: " + yamlScalar(hook) + "\n"
	if err := os.WriteFile(filepath.Join(root, ".no-mistakes.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "add", ".no-mistakes.yaml")
	run(t, root, "git", "commit", "-m", message)
}

// yamlScalar single-quotes a value so shell metacharacters stay literal YAML.
func yamlScalar(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func runPRBody(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetContext(context.Background())
	cmd.SetArgs(append([]string{"pr-body"}, args...))
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestPRBodyRejectsMoreThanOneContractSource(t *testing.T) {
	_, _, err := runPRBody(t, "", "--sample", "--run", "01SAMPLE")
	if err == nil || !strings.Contains(err.Error(), "pick one of") {
		t.Fatalf("err = %v, want a mutually-exclusive-source error", err)
	}
}

func TestPRBodyPrintsTheSampleContract(t *testing.T) {
	setupPRBodyRepo(t)

	out, _, err := runPRBody(t, "", "--sample", "--print-contract")
	if err != nil {
		t.Fatalf("pr-body: %v", err)
	}
	var contract prbody.Contract
	if err := json.Unmarshal([]byte(out), &contract); err != nil {
		t.Fatalf("printed contract is not valid JSON: %v\n%s", err, out)
	}
	if contract.Version != prbody.Version {
		t.Fatalf("version = %d, want %d", contract.Version, prbody.Version)
	}
	if contract.Sections.Pipeline == nil || len(contract.Sections.Pipeline.Steps) == 0 {
		t.Fatal("printed contract has no pipeline steps")
	}
}

func TestPRBodyRunsTheHookOverride(t *testing.T) {
	setupPRBodyRepo(t)

	out, errOut, err := runPRBody(t, "", "--sample", "--hook", "cat > /dev/null; echo overridden-body")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "overridden-body") {
		t.Fatalf("stdout = %q, want the formatter's body", out)
	}
	if !strings.Contains(errOut, "--hook") {
		t.Fatalf("stderr = %q, want the formatter source reported", errOut)
	}
}

// The security boundary: hooks.pr_body executes arbitrary shell, so a preview
// must read the repo layer from the default branch. Reading the checkout would
// run whatever a contributor's branch declares the moment a reviewer checks it
// out to look at it.
func TestPRBodyRepoFormatterComesFromDefaultBranchNotTheCheckout(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; echo trusted-body", "add trusted formatter")
	run(t, local.root, "git", "checkout", "-b", "contributor")
	commitRepoConfig(t, local.root, "cat > /dev/null; echo hostile-body", "swap the formatter")

	out, errOut, err := runPRBody(t, "", "--sample")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if strings.Contains(out, "hostile-body") || strings.Contains(errOut, "hostile-body") {
		t.Fatalf("the checked-out branch's formatter ran\nstdout: %q\nstderr: %q", out, errOut)
	}
	if !strings.Contains(out, "trusted-body") {
		t.Fatalf("stdout = %q, want the default branch's formatter", out)
	}
}

// The formatter's working directory is the repository root, so a template read
// by relative path resolves the same way it does in a run.
func TestPRBodyRunsTheFormatterFromTheRepositoryRoot(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; pwd", "add formatter")
	sub := filepath.Join(local.root, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)

	out, errOut, err := runPRBody(t, "", "--sample")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	printed, resolveErr := filepath.EvalSymlinks(strings.TrimSpace(out))
	if resolveErr != nil {
		t.Fatalf("formatter printed %q: %v", out, resolveErr)
	}
	if printed != local.root {
		t.Fatalf("formatter ran in %q, want the repository root %q", printed, local.root)
	}
}

// A subdirectory invocation must find the repo's configured formatter, not fall
// through to "no formatter configured" because <cwd>/.no-mistakes.yaml is absent.
func TestPRBodyFindsTheRepoFormatterFromASubdirectory(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; echo subdir-body", "add formatter")
	sub := filepath.Join(local.root, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)

	out, errOut, err := runPRBody(t, "", "--sample")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "subdir-body") {
		t.Fatalf("stdout = %q, want the repo's formatter", out)
	}
}

func TestPRBodyWithoutAnyFormatterConfigured(t *testing.T) {
	setupPRBodyRepo(t)

	_, _, err := runPRBody(t, "", "--sample")
	if err == nil || !strings.Contains(err.Error(), "no formatter configured") {
		t.Fatalf("err = %v, want a missing-formatter error", err)
	}
}

func TestPRBodyReadsAContractFromStdin(t *testing.T) {
	setupPRBodyRepo(t)

	raw, err := json.Marshal(prbody.Sample())
	if err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runPRBody(t, string(raw),
		"--contract-file", "-", "--hook", `grep -o '"run_id":"01SAMPLE0000000000000000000"' && echo piped-body`)
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "piped-body") {
		t.Fatalf("stdout = %q, want the formatter to have seen the piped contract", out)
	}
}

func TestPRBodyRejectsAContractFromAnotherVersion(t *testing.T) {
	setupPRBodyRepo(t)

	path := filepath.Join(t.TempDir(), "contract.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runPRBody(t, "", "--contract-file", path, "--hook", "cat > /dev/null; echo body")
	if err == nil || !strings.Contains(err.Error(), "contract version 1") {
		t.Fatalf("err = %v, want a version rejection", err)
	}
}

// A run id from another repository would otherwise render a contract whose repo
// block is this directory and whose run data belongs somewhere else.
func TestPRBodyRejectsARunFromAnotherRepository(t *testing.T) {
	local := setupPRBodyRepo(t)

	other, err := local.db.InsertRepoWithID("repo-2", filepath.Join(t.TempDir(), "other"), "origin", "main")
	if err != nil {
		t.Fatalf("insert other repo: %v", err)
	}
	otherRun, err := local.db.InsertRun(other.ID, "feature/other", "head", "base")
	if err != nil {
		t.Fatalf("insert other run: %v", err)
	}

	_, _, err = runPRBody(t, "", "--run", otherRun.ID, "--print-contract")
	if err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("err = %v, want a cross-repository rejection", err)
	}
}

// A stacked run's PR targets its parent branch, so its reconstructed contract
// has to as well - the docs promise --run rebuilds everything but what_changed.
func TestPRBodyReconstructsAStackedRunsBaseBranch(t *testing.T) {
	local := setupPRBodyRepo(t)

	stacked, err := local.db.InsertRunWithOptions(local.repo.ID, "feature/child", "head", "base",
		db.RunOptions{StackedOn: "feature/parent"})
	if err != nil {
		t.Fatalf("insert stacked run: %v", err)
	}

	out, errOut, err := runPRBody(t, "", "--run", stacked.ID, "--print-contract")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	var contract prbody.Contract
	if err := json.Unmarshal([]byte(out), &contract); err != nil {
		t.Fatalf("printed contract is not valid JSON: %v\n%s", err, out)
	}
	if contract.BaseBranch != "feature/parent" {
		t.Fatalf("base_branch = %q, want the stacked-on parent", contract.BaseBranch)
	}
}

// A failing formatter must say that a run would fall back, so the fallback path
// is visible here rather than inferred.
func TestPRBodyReportsThatARunWouldFallBack(t *testing.T) {
	setupPRBodyRepo(t)

	_, errOut, err := runPRBody(t, "", "--sample", "--hook", "cat > /dev/null; echo 'no template' >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(errOut, "fall back to the built-in PR body") {
		t.Fatalf("stderr = %q, want the fallback notice", errOut)
	}
	if !strings.Contains(err.Error(), "no template") {
		t.Fatalf("err = %v, want the formatter's own diagnostic", err)
	}
}

// A matching global-config override supplies hooks.pr_body ahead of the
// repo's committed formatter, mirroring the run-time overlay precedence.
func TestPRBodyHookComesFromMatchingGlobalOverride(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; echo repo-body", "add repo formatter")
	globalConfig := "overrides:\n  Example/Example:\n    hooks:\n      pr_body: 'cat > /dev/null; echo override-body'\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("NM_HOME"), "config.yaml"), []byte(globalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runPRBody(t, "", "--sample")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "override-body") || strings.Contains(out, "repo-body") {
		t.Fatalf("stdout = %q, want the override formatter ahead of the repo formatter", out)
	}
	if !strings.Contains(errOut, "global override example/example") {
		t.Fatalf("stderr = %q, want the override source reported", errOut)
	}
}

// An override that explicitly clears hooks.pr_body displaces the repo's
// committed formatter, exactly as OverlayRepoConfig does at run time.
func TestPRBodyGlobalOverrideExplicitEmptyClearsRepoFormatter(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; echo repo-body", "add repo formatter")
	globalConfig := "overrides:\n  example/example:\n    hooks:\n      pr_body: \"\"\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("NM_HOME"), "config.yaml"), []byte(globalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := runPRBody(t, "", "--sample")
	if err == nil || !strings.Contains(err.Error(), "no formatter configured") {
		t.Fatalf("err = %v, want no-formatter error after the override cleared the repo layer", err)
	}
}

// An override for a different repository must not steer this repository's
// formatter resolution.
func TestPRBodyIgnoresNonMatchingGlobalOverride(t *testing.T) {
	local := setupPRBodyRepo(t)

	commitRepoConfig(t, local.root, "cat > /dev/null; echo repo-body", "add repo formatter")
	globalConfig := "overrides:\n  other/project:\n    hooks:\n      pr_body: 'cat > /dev/null; echo other-body'\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("NM_HOME"), "config.yaml"), []byte(globalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runPRBody(t, "", "--sample")
	if err != nil {
		t.Fatalf("pr-body: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "repo-body") || strings.Contains(out, "other-body") {
		t.Fatalf("stdout = %q, want the repo formatter untouched by the non-matching override", out)
	}
}
