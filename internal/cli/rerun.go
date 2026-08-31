package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func newRerunCmd() *cobra.Command {
	var refreshStrategyValue string
	var stackedOn string
	var metadata string
	var intent string
	var publishEffectiveConfig bool
	var noPublishEffectiveConfig bool
	cmd := &cobra.Command{
		Use:   "rerun",
		Short: "Rerun the pipeline for the current branch",
		Long:  "Rerun the pipeline for the current branch. By default, an explicit intent from the selected prior run is inherited; otherwise intent is inferred afresh. Use --intent to replace either with a new explicit intent.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			effectiveConfigPublish, err := effectiveConfigPublishOverride(cmd, publishEffectiveConfig, noPublishEffectiveConfig)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("intent") && strings.TrimSpace(intent) == "" {
				return fmt.Errorf("--intent must not be empty")
			}
			return trackCommand("rerun", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				branch, err := git.CurrentBranch(cmd.Context(), ".")
				if err != nil {
					return fmt.Errorf("get current branch: %w", err)
				}
				if branch == "HEAD" {
					return fmt.Errorf("not on a branch")
				}
				strategy, err := types.ParseRefreshStrategy(refreshStrategyValue)
				if err != nil {
					return err
				}
				if cmd.Flags().Changed("refresh-strategy") && strategy == "" {
					return fmt.Errorf("--refresh-strategy requires rebase or merge")
				}
				stackedOn = strings.TrimSpace(stackedOn)
				if cmd.Flags().Changed("stacked-on") && stackedOn == "" {
					return fmt.Errorf("--stacked-on requires a branch")
				}
				if stackedOn == branch {
					return fmt.Errorf("--stacked-on cannot name the branch being rerun")
				}
				if stackedOn != "" {
					if err := git.ValidateBranchName(cmd.Context(), ".", stackedOn); err != nil {
						return fmt.Errorf("invalid --stacked-on branch: %w", err)
					}
				}
				var metadataValue *string
				if cmd.Flags().Changed("metadata") {
					if err := validateMetadata(metadata); err != nil {
						return err
					}
					metadataValue = &metadata
				}

				if err := daemon.EnsureDaemon(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}

				client, err := ipc.Dial(p.Socket())
				if err != nil {
					return fmt.Errorf("connect to daemon: %w", err)
				}
				defer client.Close()

				var result ipc.RerunResult
				if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
					RepoID:                 repo.ID,
					Branch:                 branch,
					RefreshStrategy:        strategy,
					StackedOn:              stackedOn,
					Intent:                 intent,
					Metadata:               metadataValue,
					EffectiveConfigPublish: effectiveConfigPublish,
				}, &result); err != nil {
					return fmt.Errorf("rerun pipeline: %w", err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "  %s Rerun started for %s %s\n", sGreen.Render("✓"), branch, sDim.Render(result.RunID))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&refreshStrategyValue, "refresh-strategy", "", "refresh strategy: rebase or merge (inherited from the prior run when omitted)")
	cmd.Flags().StringVar(&stackedOn, "stacked-on", "", "branch this change is stacked on; inherited from the prior run when omitted")
	cmd.Flags().StringVar(&metadata, "metadata", "", "replace inherited opaque run metadata; pass an empty value to clear it")
	cmd.Flags().StringVar(&intent, "intent", "", "explicit intent for this rerun (overrides inherited intent or fresh inference)")
	cmd.Flags().BoolVar(&publishEffectiveConfig, "publish-effective-config", false, "publish the complete stored effective configuration in the built-in GitHub PR body for this rerun")
	cmd.Flags().BoolVar(&noPublishEffectiveConfig, "no-publish-effective-config", false, "do not publish the stored effective configuration in the built-in GitHub PR body for this rerun")
	return cmd
}
