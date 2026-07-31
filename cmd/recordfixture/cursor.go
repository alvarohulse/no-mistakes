package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func recordCursor(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "cursor-agent")
	if err := captureCursor(
		ctx,
		bin,
		forward,
		`Reply with ONLY this JSON literal and nothing else: {"findings":[],"risk_level":"low","risk_rationale":"no risks","summary":"ok"}`,
		filepath.Join(out, "structured.jsonl"),
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := captureCursor(
		ctx,
		bin,
		forward,
		"Reply with the literal word OK and nothing else.",
		filepath.Join(out, "plain.jsonl"),
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "cursor fixtures written to %s\n", out)
	return 0
}

func captureCursor(ctx context.Context, bin string, forward []string, prompt, outPath string) error {
	root, err := os.MkdirTemp("", "recordcursor-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(root)
	workspace := filepath.Join(root, "workspace")
	repo := filepath.Join(root, "repo")
	for _, dir := range []string{workspace, repo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	probePath := filepath.Join(repo, "probe.txt")
	if err := os.WriteFile(probePath, []byte("FIXTURE_REPO_ACCESS\n"), 0o644); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	prompt = fmt.Sprintf("First read %s with the file-read tool. After reading it, %s", probePath, prompt)

	cmdArgs := append([]string(nil), forward...)
	cmdArgs = append(cmdArgs,
		"-p",
		"--output-format", "stream-json",
		"--workspace", workspace,
		"--add-dir", repo,
		"--force",
		"--trust",
	)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = workspace
	cmd.Stdin = strings.NewReader(prompt)
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "recording cursor → %s\n", outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run cursor: %w", err)
	}
	if err := scrubCursorFile(outPath); err != nil {
		return fmt.Errorf("scrub %s: %w", outPath, err)
	}
	if err := validateCursorFixture(outPath); err != nil {
		return fmt.Errorf("validate %s: %w", outPath, err)
	}
	return nil
}

func validateCursorFixture(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var initSeen, completedReadSeen, resultSeen bool
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type     string                     `json:"type"`
			Subtype  string                     `json:"subtype"`
			ToolCall map[string]json.RawMessage `json:"tool_call"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		switch event.Type {
		case "system":
			initSeen = initSeen || event.Subtype == "init"
		case "tool_call":
			_, readCall := event.ToolCall["readToolCall"]
			completedReadSeen = completedReadSeen || event.Subtype == "completed" && readCall
		case "result":
			resultSeen = true
		}
	}
	if !initSeen || !completedReadSeen || !resultSeen {
		return fmt.Errorf("required events missing (init=%t completed_read=%t result=%t)", initSeen, completedReadSeen, resultSeen)
	}
	return nil
}
