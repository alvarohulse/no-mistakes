package daemon

import (
	"crypto/sha256"
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
	command         string
	launcherPath    string
	launcherContent []byte
}

// installWindowsTask registers the daemon as a per-user scheduled task.
//
// Unlike the launchd and systemd paths, it never writes proxy settings into the
// task definition. When NM_REPO_CONFIG is set, the task action points at a
// generated launcher under this NM_HOME that sets only that explicit opt-in.
func installWindowsTask(p *paths.Paths, exe string) error {
	plan, err := buildWindowsTaskInstallPlan(p, exe)
	if err != nil {
		return err
	}
	if plan.launcherPath != "" {
		if err := writeFileAtomic(plan.launcherPath, plan.launcherContent, 0o600); err != nil {
			return fmt.Errorf("write Windows daemon launcher: %w", err)
		}
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
	cleanupStaleWindowsLaunchers(p, plan.launcherPath)
	return nil
}

func buildWindowsTaskInstallPlan(p *paths.Paths, exe string) (windowsTaskInstallPlan, error) {
	directCommand := buildWindowsTaskCommand(exe, p.Root())
	rawPath, set := os.LookupEnv(machineRepoConfigEnv)
	if !set {
		if err := validateWindowsTaskCommandLength(directCommand); err != nil {
			return windowsTaskInstallPlan{}, err
		}
		return windowsTaskInstallPlan{command: directCommand}, nil
	}
	path, err := ValidateMachineRepoConfigPath(rawPath)
	if err != nil {
		return windowsTaskInstallPlan{}, err
	}
	machineConfig, err := powershellSingleQuoted(path)
	if err != nil {
		return windowsTaskInstallPlan{}, fmt.Errorf("encode %s path: %w", machineRepoConfigEnv, err)
	}
	executable, err := powershellSingleQuoted(exe)
	if err != nil {
		return windowsTaskInstallPlan{}, fmt.Errorf("encode daemon executable: %w", err)
	}
	daemonRoot, err := powershellSingleQuoted(p.Root())
	if err != nil {
		return windowsTaskInstallPlan{}, fmt.Errorf("encode daemon root: %w", err)
	}
	script := "$env:" + machineRepoConfigEnv + "=" + machineConfig + "; & " + executable + " 'daemon' 'run' '--root' " + daemonRoot + "; exit $LASTEXITCODE"
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte(script)...)
	digest := sha256.Sum256(content)
	launcherPath := filepath.Join(p.Root(), fmt.Sprintf("%s%x%s", windowsDaemonLauncherPrefix, digest[:6], windowsDaemonLauncherSuffix))
	command := "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File " + quoteWindowsTaskArg(launcherPath)
	if err := validateWindowsTaskCommandLength(command); err != nil {
		return windowsTaskInstallPlan{}, err
	}
	return windowsTaskInstallPlan{command: command, launcherPath: launcherPath, launcherContent: content}, nil
}

func powershellSingleQuoted(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("value contains a control character")
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

func validateWindowsTaskCommandLength(command string) error {
	length := len(utf16.Encode([]rune(command)))
	if length > windowsTaskCommandMaxUTF16 {
		return fmt.Errorf("Windows scheduled-task action is %d UTF-16 code units (maximum %d); move NM_HOME to a shorter path", length, windowsTaskCommandMaxUTF16)
	}
	return nil
}

func cleanupStaleWindowsLaunchers(p *paths.Paths, keep string) {
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
		if launcher == keep {
			continue
		}
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
