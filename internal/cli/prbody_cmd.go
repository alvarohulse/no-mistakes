package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/spf13/cobra"
)

func newPRBodyCmd() *cobra.Command {
	var (
		sample      bool
		runID       string
		contractIn  string
		contractOut bool
		hook        string
	)

	cmd := &cobra.Command{
		Use:   "pr-body",
		Short: "Render a pull request body through the configured formatter",
		Long: `Render a pull request body through hooks.pr_body and print it.

This never creates or updates a pull request. Generation returns a string;
publication is the pr step's job, and conflating the two is what makes a
formatter impossible to iterate on.

Pick one source of contract data:

  --sample           a built-in contract that exercises every section
  --run <id>         a stored run's real contract
  --contract-file    a contract JSON file, or - for stdin

With no source, the latest run for the current repository is used.

  no-mistakes pr-body --sample --print-contract > sample.json
  no-mistakes pr-body --sample
  no-mistakes pr-body --run 01K... --hook ~/scripts/no-mistakes/format-pr`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sources := 0
			for _, set := range []bool{sample, runID != "", contractIn != ""} {
				if set {
					sources++
				}
			}
			if sources > 1 {
				return fmt.Errorf("pick one of --sample, --run, or --contract-file")
			}

			contract, err := resolvePRBodyContract(cmd.Context(), cmd, sample, runID, contractIn)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if contractOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(contract)
			}

			command, origin, err := resolvePRBodyHook(hook)
			if err != nil {
				return err
			}
			if command == "" {
				return fmt.Errorf("no formatter configured: set hooks.pr_body or pass --hook")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "pr-body: running %s formatter: %s\n", origin, command)

			result, err := prbody.RunHook(cmd.Context(), prbody.HookOptions{
				Command:  command,
				Dir:      workingDirOrEmpty(),
				Contract: contract,
			})
			if err != nil {
				// A run would fall back to the built-in body here. Say so, so
				// a failing formatter is never mistaken for a formatter that
				// simply produced nothing.
				fmt.Fprintln(cmd.ErrOrStderr(),
					"pr-body: a run would fall back to the built-in PR body and report this")
				return err
			}
			if result.Diagnostics != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", result.Diagnostics)
			}
			fmt.Fprintln(out, result.Body)
			return nil
		},
	}

	cmd.Flags().BoolVar(&sample, "sample", false, "use the built-in sample contract")
	cmd.Flags().StringVar(&runID, "run", "", "use a stored run's contract")
	cmd.Flags().StringVar(&contractIn, "contract-file", "", "read the contract from a JSON file ('-' for stdin)")
	cmd.Flags().BoolVar(&contractOut, "print-contract", false, "print the contract JSON instead of running the formatter")
	cmd.Flags().StringVar(&hook, "hook", "", "formatter command, overriding hooks.pr_body")
	return cmd
}

func resolvePRBodyContract(ctx context.Context, cmd *cobra.Command, sample bool, runID, contractIn string) (*prbody.Contract, error) {
	if sample {
		return prbody.Sample(), nil
	}
	if contractIn != "" {
		return readContractFile(cmd, contractIn)
	}
	return storedRunContract(ctx, runID)
}

func readContractFile(cmd *cobra.Command, path string) (*prbody.Contract, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read contract: %w", err)
	}
	var contract prbody.Contract
	if err := json.Unmarshal(data, &contract); err != nil {
		return nil, fmt.Errorf("parse contract: %w", err)
	}
	if contract.Version != prbody.Version {
		return nil, fmt.Errorf("contract version %d, expected %d", contract.Version, prbody.Version)
	}
	return &contract, nil
}

// storedRunContract rebuilds a completed run's contract from the database.
//
// The What Changed section is not stored - it is the drafting agent's output,
// which lives only in the assembled body - so it is absent here. Every other
// section is reconstructed exactly as the pr step would have built it.
func storedRunContract(ctx context.Context, runID string) (*prbody.Contract, error) {
	_, d, err := openResources()
	if err != nil {
		return nil, err
	}
	defer d.Close()

	repo, err := findRepo(d)
	if err != nil {
		return nil, err
	}

	var run *db.Run
	if runID != "" {
		run, err = d.GetRun(runID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("run %s not found", runID)
		}
	} else {
		runs, err := d.GetRunsByRepo(repo.ID)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		if len(runs) == 0 {
			return nil, fmt.Errorf("no runs yet; try --sample")
		}
		run = runs[0]
	}

	stepResults, rounds, invocations := steps.LoadRunRecords(d, run.ID)
	provider := scm.DetectProviderContext(ctx, repo.UpstreamURL)
	in := steps.ContractInput{
		Run:         run,
		Repo:        repo,
		Steps:       stepResults,
		Rounds:      rounds,
		Invocations: invocations,
		Branch:      run.Branch,
		BaseBranch:  repo.DefaultBranch,
		BaseSHA:     run.BaseSHA,
		Provider:    string(provider),
		BodyLimit:   scm.MaxPRBodyChars(provider),
	}
	if run.Intent != nil {
		in.Intent = strings.TrimSpace(*run.Intent)
	}
	if run.IntentSource != nil {
		in.IntentSource = *run.IntentSource
		in.IntentAuthoritative = *run.IntentSource == db.RunIntentSourceAgent
	}
	if run.PRNote != nil {
		in.Note = strings.TrimSpace(*run.PRNote)
	}
	return steps.BuildContract(in), nil
}

// resolvePRBodyHook mirrors the run-time precedence: an explicit override, then
// the machine-local repo config, then the repo's own, then the global default.
func resolvePRBodyHook(override string) (command, origin string, err error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed, "--hook", nil
	}

	if rawPath, set := os.LookupEnv("NM_REPO_CONFIG"); set {
		path, err := daemon.ValidateMachineRepoConfigPath(rawPath)
		if err != nil {
			return "", "", fmt.Errorf("NM_REPO_CONFIG: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("read NM_REPO_CONFIG: %w", err)
		}
		repoCfg, err := config.LoadRepoFromBytes(data)
		if err != nil {
			return "", "", fmt.Errorf("parse NM_REPO_CONFIG: %w", err)
		}
		if hook := strings.TrimSpace(repoCfg.Hooks.PRBody); hook != "" {
			return hook, "NM_REPO_CONFIG", nil
		}
	}

	if workDir := workingDirOrEmpty(); workDir != "" {
		if repoCfg, err := config.LoadRepo(workDir); err == nil {
			if hook := strings.TrimSpace(repoCfg.Hooks.PRBody); hook != "" {
				return hook, "repo config", nil
			}
		}
	}

	p, err := paths.New()
	if err != nil {
		return "", "", fmt.Errorf("resolve paths: %w", err)
	}
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return "", "", fmt.Errorf("load global config: %w", err)
	}
	return strings.TrimSpace(globalCfg.Hooks.PRBody), "global config", nil
}

func workingDirOrEmpty() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}
