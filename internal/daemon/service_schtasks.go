package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const (
	windowsTaskCommandMaxUTF16  = 261
	windowsDaemonLauncherPrefix = "daemon-task-launcher-"
	windowsDaemonLauncherSuffix = ".ps1"
)

type windowsTaskInstallPlan struct {
	command string
}

// installWindowsTask registers the daemon as a per-user scheduled task.
//
// Unlike the launchd and systemd paths, it never writes proxy settings into the
// task definition: the task action invokes the daemon binary directly.
func installWindowsTask(p *paths.Paths, exe string) error {
	plan, err := buildWindowsTaskInstallPlan(p, exe)
	if err != nil {
		return err
	}
	args := []string{
		"/Create",
		"/TN", windowsTaskName(p),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
		"/TR", plan.command,
	}
	if _, err := serviceCommandRunner("schtasks", args...); err != nil {
		return fmt.Errorf("schtasks create: %w", err)
	}
	cleanupLegacyWindowsTask(p)
	cleanupStaleWindowsLaunchers(p)
	return nil
}

func buildWindowsTaskInstallPlan(p *paths.Paths, exe string) (windowsTaskInstallPlan, error) {
	directCommand := buildWindowsTaskCommand(exe, p.Root())
	if err := validateWindowsTaskCommandLength(directCommand); err != nil {
		return windowsTaskInstallPlan{}, err
	}
	return windowsTaskInstallPlan{command: directCommand}, nil
}

func validateWindowsTaskCommandLength(command string) error {
	length := len(utf16.Encode([]rune(command)))
	if length > windowsTaskCommandMaxUTF16 {
		return fmt.Errorf("Windows scheduled-task action is %d UTF-16 code units (maximum %d); move NM_HOME to a shorter path", length, windowsTaskCommandMaxUTF16)
	}
	return nil
}

// cleanupStaleWindowsLaunchers removes launcher scripts written by older
// binaries whose scheduled-task action set the retired machine-local
// repo-config opt-in. The task action now invokes the daemon directly, so
// every remaining launcher under this NM_HOME is stale.
func cleanupStaleWindowsLaunchers(p *paths.Paths) {
	entries, err := os.ReadDir(p.Root())
	if err != nil {
		slog.Warn("clean stale Windows daemon launchers", "root", p.Root(), "error", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, windowsDaemonLauncherPrefix) || !strings.HasSuffix(name, windowsDaemonLauncherSuffix) {
			continue
		}
		launcher := filepath.Join(p.Root(), name)
		if err := os.Remove(launcher); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove stale Windows daemon launcher", "path", launcher, "error", err)
		}
	}
}

func cleanupLegacyWindowsTask(p *paths.Paths) {
	data, err := serviceCommandRunner("schtasks", "/Query", "/TN", legacyWindowsTaskName, "/XML")
	if err != nil || !serviceDefinitionMatchesRoot(data, p) {
		return
	}
	_, _ = serviceCommandRunner("schtasks", "/End", "/TN", legacyWindowsTaskName)
	_, _ = serviceCommandRunner("schtasks", "/Delete", "/TN", legacyWindowsTaskName, "/F")
}

func startWindowsTask(p *paths.Paths) error {
	_, err := serviceCommandRunner("schtasks", "/Run", "/TN", windowsTaskName(p))
	if err != nil {
		return fmt.Errorf("schtasks run: %w", err)
	}
	return nil
}

func stopWindowsTask(p *paths.Paths) error {
	_, err := serviceCommandRunner("schtasks", "/End", "/TN", windowsTaskName(p))
	if err != nil {
		return fmt.Errorf("schtasks end: %w", err)
	}
	return nil
}

type windowsManagedDaemonObservation struct {
	state         int
	runGeneration string
}

func inspectWindowsManagedDaemon(p *paths.Paths) (windowsManagedDaemonObservation, error) {
	taskName := strings.ReplaceAll(windowsTaskName(p), "'", "''")
	command := "$task=Get-ScheduledTask -TaskName '" + taskName + "';$info=Get-ScheduledTaskInfo -TaskName '" + taskName + "';Write-Output \"$([int]$task.State)|$($info.LastRunTime.Ticks)|$($info.LastTaskResult)\""
	output, err := serviceCommandRunner(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		command,
	)
	if err != nil {
		return windowsManagedDaemonObservation{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(fields) != 3 {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task state: unexpected field count %d", len(fields))
	}
	state, err := strconv.Atoi(fields[0])
	if err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task state: %w", err)
	}
	if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task last run: %w", err)
	}
	if _, err := strconv.ParseUint(fields[2], 10, 32); err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task result: %w", err)
	}
	return windowsManagedDaemonObservation{state: state, runGeneration: fields[1]}, nil
}

func windowsManagedDaemonState(p *paths.Paths, launch managedServiceLaunch) (managedServiceState, error) {
	observation, err := inspectWindowsManagedDaemon(p)
	if err != nil {
		return managedServiceUnknown, err
	}
	switch observation.state {
	case 4:
		return managedServiceRunning, nil
	case 1:
		return managedServiceExited, nil
	case 3:
		if launch.windowsRunGeneration != "" && observation.runGeneration != launch.windowsRunGeneration {
			return managedServiceExited, nil
		}
		return managedServiceUnknown, nil
	default:
		return managedServiceUnknown, nil
	}
}

func buildWindowsTaskCommand(exe, root string) string {
	args := []string{quoteWindowsTaskArg(exe), "daemon", "run", "--root", quoteWindowsTaskArg(root)}
	return strings.Join(args, " ")
}

func quoteWindowsTaskArg(arg string) string {
	if !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	return strconv.Quote(arg)
}
