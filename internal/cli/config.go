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
	var format string
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Resolve the policy for the current gate branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}
