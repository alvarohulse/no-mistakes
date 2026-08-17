package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/prbody"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/spf13/cobra"
)

// repoConfigFileName is the committed per-repo config a run reads its trusted
// hooks.pr_body from.
const repoConfigFileName = ".no-mistakes.yaml"

func newPRBodyCmd() *cobra.Command {
	var (
		sample        bool
		sampleVersion int
		runID         string
		contractIn    string
		contractOut   bool
		hook          string
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
  --sample-version   which contract version --sample emits
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
			if cmd.Flags().Changed("sample-version") && !sample {
				return fmt.Errorf("--sample-version applies only to --sample")
			}

			ctx := cmd.Context()
			_, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			local := resolveLocalRepo(d)
			contract, err := resolvePRBodyContract(ctx, cmd, d, local, sample, sampleVersion, runID, contractIn)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if contractOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(contract)
			}

			command, origin, err := resolvePRBodyHook(ctx, hook, local)
			if err != nil {
				return err
			}
			if command == "" {
				return fmt.Errorf("no formatter configured: set hooks.pr_body or pass --hook")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "pr-body: running %s formatter: %s\n", origin, command)

			result, err := prbody.RunHook(ctx, prbody.HookOptions{
				Command: command,
				// The repository root, matching a run's worktree root, so a
				// formatter that reads a relative path such as
				// .github/PULL_REQUEST_TEMPLATE.md finds it from any cwd.
				Dir:      local.root,
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
	cmd.Flags().IntVar(&sampleVersion, "sample-version", prbody.Version, "contract version --sample emits (2, 3, or 4)")
	cmd.Flags().StringVar(&runID, "run", "", "use a stored run's contract")
	cmd.Flags().StringVar(&contractIn, "contract-file", "", "read the contract from a JSON file ('-' for stdin)")
	cmd.Flags().BoolVar(&contractOut, "print-contract", false, "print the contract JSON instead of running the formatter")
	cmd.Flags().StringVar(&hook, "hook", "", "formatter command, overriding hooks.pr_body")
	return cmd
}

func resolvePRBodyContract(ctx context.Context, cmd *cobra.Command, d *db.DB, local localRepo, sample bool, sampleVersion int, runID, contractIn string) (*prbody.Contract, error) {
	if sample {
		contract := prbody.SampleForVersion(sampleVersion)
		if contract == nil {
			return nil, fmt.Errorf("unsupported --sample-version %d; supported versions are %s", sampleVersion, joinVersions(prbody.SupportedVersions()))
		}
		return contract, nil
	}
	if contractIn != "" {
		return readContractFile(cmd, contractIn)
	}
	return storedRunContract(ctx, d, local, runID)
}

// localRepo is the repository context this command resolves once: the root a
// formatter runs in, and the registered record the contract and the trusted
// repo config are read from. Both are best-effort, because --sample and
// --contract-file are meant to work anywhere.
type localRepo struct {
	root string
	repo *db.Repo
}

func resolveLocalRepo(d *db.DB) localRepo {
	root, err := git.FindGitRoot(".")
	if err != nil {
		return localRepo{}
	}
	local := localRepo{root: root}
	if repo, err := findRepo(d); err == nil {
		local.repo = repo
	}
	return local
}

func (l localRepo) defaultBranch() string {
	if l.repo == nil {
		return ""
	}
	return strings.TrimSpace(l.repo.DefaultBranch)
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
	// A formatter under a v2-to-v3 rollout has to be testable against both
	// shapes, so a still-supported older contract is read rather than refused.
	if !prbody.IsSupportedVersion(contract.Version) {
		return nil, fmt.Errorf("contract version %d, expected one of %s", contract.Version, joinVersions(prbody.SupportedVersions()))
	}
	return &contract, nil
}

func joinVersions(versions []int) string {
	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, strconv.Itoa(version))
	}
	return strings.Join(parts, ", ")
}

// storedRunContract rebuilds a completed run's contract from the database.
//
// The Summary and What Changed sections are not stored - they are the drafting
// agent's output, which lives only in the assembled body - so they are absent
// here. Every other section is reconstructed exactly as the pr step would have
// built it.
func storedRunContract(ctx context.Context, d *db.DB, local localRepo, runID string) (*prbody.Contract, error) {
	repo := local.repo
	if repo == nil {
		if _, err := git.FindGitRoot("."); err != nil {
			return nil, fmt.Errorf("not in a git repository")
		}
		return nil, fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
	}

	var (
		run *db.Run
		err error
	)
	if runID != "" {
		run, err = d.GetRun(runID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		// Without this, a run id from another repository renders a contract
		// whose repo block is this directory and whose run data is elsewhere.
		if run.RepoID != repo.ID {
			return nil, fmt.Errorf("run %s belongs to another repository", runID)
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

	records := steps.LoadRunRecords(d, run.ID)
	provider := scm.DetectProviderContext(ctx, repo.UpstreamURL)
	in := steps.ContractInput{
		Run:         run,
		Repo:        repo,
		Steps:       records.Steps,
		Rounds:      records.Rounds,
		Invocations: records.Invocations,
		Branch:      run.Branch,
		// The same rule the live step used, so a stacked run reconstructs onto
		// its parent branch rather than the default branch.
		BaseBranch: steps.BaseBranchForRun(run, repo.DefaultBranch),
		BaseSHA:    run.BaseSHA,
		Provider:   string(provider),
		BodyLimit:  scm.MaxPRBodyChars(provider),
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

// resolvePRBodyHook mirrors the run-time precedence: an explicit override,
// then a matching machine-local override from the global config, then the
// repo's own, then the global default. A matching override that explicitly
// declares an empty hooks.pr_body clears the repo layer, exactly as the
// run-time overlay does.
//
// It also mirrors the run-time trust boundary. The repo layer is read from the
// default branch, never from the checkout: hooks.pr_body executes arbitrary
// shell, and a preview that honored the working tree would run whatever a
// contributor's branch declares as soon as someone checked it out to look at it.
func resolvePRBodyHook(ctx context.Context, override string, local localRepo) (command, origin string, err error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed, "--hook", nil
	}

	p, err := paths.New()
	if err != nil {
		return "", "", fmt.Errorf("resolve paths: %w", err)
	}
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return "", "", fmt.Errorf("load global config: %w", err)
	}

	if identity, ok := localRepoIdentity(ctx, local); ok {
		if overrideCfg, key, matched := globalCfg.OverrideForRepoIdentity(identity); matched && overrideCfg.Declares("hooks.pr_body") {
			if hook := strings.TrimSpace(overrideCfg.Hooks.PRBody); hook != "" {
				return hook, "global override " + key, nil
			}
			// Explicitly cleared: the repo layer is displaced, and the global
			// default applies, matching OverlayRepoConfig + Merge at run time.
			return strings.TrimSpace(globalCfg.Hooks.PRBody), "global config", nil
		}
	}

	if ref, ok := trustedRepoConfigRef(ctx, local); ok {
		if data, err := git.ShowFileBytes(ctx, local.root, ref, repoConfigFileName); err == nil {
			repoCfg, err := config.LoadRepoFromBytes(data)
			if err != nil {
				return "", "", fmt.Errorf("parse %s at %s: %w", repoConfigFileName, ref, err)
			}
			if hook := strings.TrimSpace(repoCfg.Hooks.PRBody); hook != "" {
				return hook, "repo config at " + ref, nil
			}
		}
	}

	return strings.TrimSpace(globalCfg.Hooks.PRBody), "global config", nil
}

// localRepoIdentity resolves the repository identity used for global-override
// matching: the registered upstream URL when the repo is initialized, falling
// back to the checkout's origin remote. Best-effort, like the rest of this
// command's repository context.
func localRepoIdentity(ctx context.Context, local localRepo) (string, bool) {
	if local.repo != nil {
		if identity, err := gate.RegisteredRemoteIdentity(local.repo.UpstreamURL); err == nil {
			return identity, true
		}
	}
	if local.root == "" {
		return "", false
	}
	urls, err := git.GetConfiguredRemoteURLs(ctx, local.root, "origin")
	if err != nil || len(urls) != 1 {
		return "", false
	}
	identity, err := gate.RegisteredRemoteIdentity(urls[0])
	if err != nil {
		return "", false
	}
	return identity, true
}

// trustedRepoConfigRef picks the local ref that stands in for the trusted
// default-branch config. A run pins a freshly fetched commit; a preview must not
// fetch, so it prefers the remote-tracking ref and falls back to the local
// branch. Both are refs a contributor cannot move by pushing a feature branch,
// which is the property that matters here. With no known default branch there is
// no trusted copy to read, and the repo layer is skipped rather than guessed.
func trustedRepoConfigRef(ctx context.Context, local localRepo) (string, bool) {
	branch := local.defaultBranch()
	if local.root == "" || branch == "" {
		return "", false
	}
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		if _, err := git.Run(ctx, local.root, "rev-parse", "-q", "--verify", ref+"^{commit}"); err == nil {
			return ref, true
		}
	}
	return "", false
}
