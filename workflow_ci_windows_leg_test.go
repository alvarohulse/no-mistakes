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
	value, ok := workflowConditionValue(condition, "runner.os")
	return ok && value == "Windows"
}

func workflowConditionValue(condition, variable string) (string, bool) {
	condition = strings.TrimSpace(condition)
	condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	for _, part := range strings.Split(condition, "&&") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 3 && fields[0] == variable && fields[1] == "==" {
			return strings.Trim(fields[2], `'"`), true
		}
	}
	return "", false
}

func windowsShard(condition string) string {
	shard, _ := workflowConditionValue(condition, "matrix.shard")
	return shard
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

func TestCIWorkflow_TestJobChecksOutFullHistory(t *testing.T) {
	t.Parallel()

	for _, step := range ciTestJob(t).Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			if step.With["fetch-depth"] != "0" {
				t.Fatalf("test job checkout fetch-depth = %q, want 0 for historical guard fixtures", step.With["fetch-depth"])
			}
			return
		}
	}
	t.Fatal("CI test job has no checkout step")
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
	commandsByShard := map[string]workflowCommand{}
	for _, command := range tests {
		goTimeout := goTestTimeout(t, command)
		if goTimeout >= jobTimeout {
			t.Fatalf("go test -timeout is %s and the job cap is %s; a package timeout must produce evidence before job cancellation", goTimeout, jobTimeout)
		}
		shard := windowsShard(job.Steps[command.step].If)
		if shard != "core" && shard != "git" {
			t.Fatalf("Windows test step %d is not routed to exactly one known shard: %q", command.step, job.Steps[command.step].If)
		}
		if _, exists := commandsByShard[shard]; exists {
			t.Fatalf("Windows shard %q has more than one go test invocation", shard)
		}
		commandsByShard[shard] = command

		patterns := goTestPackagePatterns(command)
		if slices.Contains(patterns, "./...") {
			t.Fatalf("Windows shard at step %d still runs ./...", command.step)
		}
	}
	gitCommand, hasGitCommand := commandsByShard["git"]
	coreCommand, hasCoreCommand := commandsByShard["core"]
	if !hasGitCommand || !hasCoreCommand {
		t.Fatalf("Windows tests must have one invocation per shard, got %v", commandsByShard)
	}
	if len(goTestPackagePatterns(gitCommand)) == 0 {
		t.Fatal("Windows git shard must test explicit package patterns")
	}
	if !slices.Contains(coreCommand.args, "@pkgs") {
		t.Fatalf("Windows core shard must test the filtered @pkgs list, got %#v", coreCommand.args)
	}

	coreStep := job.Steps[coreCommand.step]
	assertWindowsCorePackageEnumeration(t, coreStep)
	exclude := windowsGitExcludePattern(t, coreStep)
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

func assertWindowsCorePackageEnumeration(t *testing.T, step wfStep) {
	t.Helper()
	goList := strings.Index(step.Run, "$pkgs = go list ./...")
	exitCode := strings.Index(step.Run, "$goListExitCode = $LASTEXITCODE")
	failureCheck := strings.Index(step.Run, "if ($goListExitCode -ne 0)")
	failureExit := strings.Index(step.Run, "exit $goListExitCode")
	filter := strings.Index(step.Run, "$pkgs = $pkgs | Where-Object { $_ -notmatch $env:NM_CI_WINDOWS_GIT_EXCLUDE }")
	goTest := strings.Index(step.Run, "go test -v -timeout=15m @pkgs")
	if goList == -1 || exitCode < goList || failureCheck < exitCode || failureExit < failureCheck || filter < failureExit || goTest < filter {
		t.Fatalf("Windows core shard must fail closed on go list before filtering packages:\n%s", step.Run)
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
