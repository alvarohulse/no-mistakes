package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func loadCIWorkflowDoc(t *testing.T) *wfDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	var workflow wfDoc
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	for name, job := range workflow.Jobs {
		job.name = name
	}
	return &workflow
}

func ciTestJob(t *testing.T) *wfJob {
	t.Helper()
	job, ok := loadCIWorkflowDoc(t).Jobs["test"]
	if !ok {
		t.Fatal("CI workflow has no test job")
	}
	return job
}

type workflowCommand struct {
	step int
	line int
	name string
	args []string
}

func windowsOnly(condition string) bool {
	condition = strings.TrimSpace(condition)
	condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	for _, part := range strings.Split(condition, "&&") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 3 && fields[0] == "runner.os" && fields[1] == "==" &&
			(fields[2] == "'Windows'" || fields[2] == `"Windows"`) {
			return true
		}
	}
	return false
}

func windowsGoTestCommands(t *testing.T) []workflowCommand {
	t.Helper()
	var tests []workflowCommand
	for _, command := range workflowCommands(ciTestJob(t).Steps) {
		if strings.EqualFold(command.name, "go") && len(command.args) > 0 && command.args[0] == "test" {
			tests = append(tests, command)
		}
	}
	if len(tests) == 0 {
		t.Fatal("CI workflow has no Windows test step")
	}
	return tests
}

func workflowCommands(steps []wfStep) []workflowCommand {
	var commands []workflowCommand
	for stepIndex, step := range steps {
		if !windowsOnly(step.If) {
			continue
		}
		for lineIndex, line := range strings.Split(step.Run, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "$p") || strings.ContainsAny(fields[0], "{}()") {
				continue
			}
			commands = append(commands, workflowCommand{step: stepIndex, line: lineIndex, name: fields[0], args: fields[1:]})
		}
	}
	return commands
}

func goListPackages(t *testing.T, patterns ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", strings.Join(patterns, " "), err)
	}
	var packages []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			packages = append(packages, line)
		}
	}
	slices.Sort(packages)
	return packages
}

func goTestPackagePatterns(command workflowCommand) []string {
	var patterns []string
	for _, arg := range command.args[1:] {
		if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "@") {
			continue
		}
		patterns = append(patterns, arg)
	}
	return patterns
}

func TestCIWorkflow_WindowsShardsCoverEveryPackageWithinTheJobCap(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	if job.TimeoutMinutes != 40 {
		t.Fatalf("test job timeout-minutes = %d, want 40 as a bounded runaway guard", job.TimeoutMinutes)
	}

	windowsMatrix := map[string]string{}
	for _, entry := range job.Strategy.Matrix.Include {
		if entry["os"] == "windows-latest" {
			windowsMatrix[entry["shard"]] = entry["name"]
		}
	}
	if windowsMatrix["core"] != "test (windows-core)" || windowsMatrix["git"] != "test (windows-git)" {
		t.Fatalf("Windows matrix = %v, want stable core and git shard names", windowsMatrix)
	}

	tests := windowsGoTestCommands(t)
	if len(tests) != 2 {
		t.Fatalf("Windows tests must have one core and one git-heavy shard, got %d go test invocations", len(tests))
	}

	jobTimeout := time.Duration(job.TimeoutMinutes) * time.Minute
	var gitCommand workflowCommand
	var coreCommand workflowCommand
	for _, command := range tests {
		goTimeout := goTestTimeout(t, command)
		if goTimeout >= jobTimeout {
			t.Fatalf("go test -timeout is %s and the job cap is %s; a package timeout must produce evidence before job cancellation", goTimeout, jobTimeout)
		}
		patterns := goTestPackagePatterns(command)
		switch {
		case slices.Contains(patterns, "./..."):
			t.Fatalf("Windows shard at step %d still runs ./...", command.step)
		case len(patterns) > 0:
			gitCommand = command
		default:
			coreCommand = command
		}
	}
	if gitCommand.name == "" || coreCommand.name == "" {
		t.Fatal("Windows tests must split explicit git-heavy packages from a go-list remainder")
	}

	exclude := windowsGitExcludePattern(t, job.Steps[coreCommand.step])
	all := goListPackages(t, "./...")
	gitFromFilter := filterPackages(all, exclude, true)
	coreFromFilter := filterPackages(all, exclude, false)
	gitFromArgs := goListPackages(t, goTestPackagePatterns(gitCommand)...)
	if !slices.Equal(gitFromArgs, gitFromFilter) {
		t.Fatalf("git-heavy shard packages %v do not match NM_CI_WINDOWS_GIT_EXCLUDE %q -> %v", gitFromArgs, exclude, gitFromFilter)
	}

	var union []string
	union = append(union, gitFromFilter...)
	union = append(union, coreFromFilter...)
	slices.Sort(union)
	if !slices.Equal(union, all) {
		t.Fatalf("Windows shards must cover every package: union %v, go list ./... %v", union, all)
	}
}

func windowsGitExcludePattern(t *testing.T, step wfStep) *regexp.Regexp {
	t.Helper()
	pattern := step.Env["NM_CI_WINDOWS_GIT_EXCLUDE"]
	if pattern == "" {
		t.Fatal("Windows core shard must define the git-heavy package filter")
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("NM_CI_WINDOWS_GIT_EXCLUDE %q: %v", pattern, err)
	}
	return compiled
}

func filterPackages(packages []string, exclude *regexp.Regexp, wantMatch bool) []string {
	var filtered []string
	for _, pkg := range packages {
		if exclude.MatchString(pkg) == wantMatch {
			filtered = append(filtered, pkg)
		}
	}
	return filtered
}

func goTestTimeout(t *testing.T, command workflowCommand) time.Duration {
	t.Helper()
	for index, arg := range command.args[1:] {
		var value string
		switch {
		case strings.HasPrefix(arg, "-timeout="):
			value = strings.TrimPrefix(arg, "-timeout=")
		case arg == "-timeout" && index+2 < len(command.args):
			value = command.args[index+2]
		default:
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			t.Fatalf("parse go test -timeout %q: %v", value, err)
		}
		return duration
	}
	t.Fatalf("Windows test command must pass an explicit -timeout, got %#v", command.args)
	return 0
}
