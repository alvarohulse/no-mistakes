package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect effective no-mistakes configuration",
	}
	cmd.AddCommand(newConfigExplainCmd())
	return cmd
}

func newConfigExplainCmd() *cobra.Command {
	var format, runID string
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Resolve current policy or read a stored run configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			runID = strings.TrimSpace(runID)
			if runID != "" {
				if cmd.Flags().Changed("format") {
					return fmt.Errorf("--format cannot be used with --run; stored run configuration is YAML")
				}
				return trackCommand("config-explain", func() error {
					p, database, err := openResources()
					if err != nil {
						return err
					}
					defer database.Close()
					run, err := database.GetRun(runID)
					if err != nil {
						return fmt.Errorf("get run %q: %w", runID, err)
					}
					if run == nil {
						receipt, receiptErr := database.GetRunMetricReceipt(runID)
						if receiptErr != nil {
							return fmt.Errorf("get archived run %q: %w", runID, receiptErr)
						}
						if receipt != nil {
							return fmt.Errorf("effective config unavailable for archived run %q", runID)
						}
						return fmt.Errorf("run %q not found", runID)
					}
					artifact, _, err := daemon.ReadEffectiveConfigForRun(p, run)
					if err != nil {
						return fmt.Errorf("read stored effective config for run %q: %w", runID, err)
					}
					if artifact == nil {
						return fmt.Errorf("effective config unavailable for legacy run %q", runID)
					}
					_, err = cmd.OutOrStdout().Write(artifact.YAML)
					return err
				})
			}
			format = strings.ToLower(strings.TrimSpace(format))
			if format != "text" && format != "json" {
				return fmt.Errorf("unsupported config explain format %q (expected text or json)", format)
			}
			return trackCommand("config-explain", func() error {
				p, database, err := openResources()
				if err != nil {
					return err
				}
				defer database.Close()

				repo, err := findRepo(database)
				if err != nil {
					return err
				}
				branch, err := git.CurrentBranch(cmd.Context(), ".")
				if err != nil {
					return fmt.Errorf("get current branch: %w", err)
				}
				if branch == "HEAD" {
					return fmt.Errorf("cannot explain config from a detached HEAD")
				}
				if err := daemon.EnsureDaemon(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					return fmt.Errorf("connect to daemon: %w", err)
				}
				defer client.Close()
				var result ipc.ConfigExplainResult
				if err := client.Call(ipc.MethodConfigExplain, &ipc.ConfigExplainParams{RepoID: repo.ID, Branch: branch, Format: format}, &result); err != nil {
					return fmt.Errorf("resolve config for gate branch %q: %w", branch, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSuffix(result.Output, "\n"))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().StringVar(&runID, "run", "", "read the immutable YAML stored for a run ID")
	return cmd
}
