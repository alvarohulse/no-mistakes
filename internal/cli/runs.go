package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/spf13/cobra"
)

func newRunsCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List pipeline runs for the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("runs", nil, func() (string, string, error) {
				_, d, err := openResources()
				if err != nil {
					return "", "", err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return "", "", err
				}

				runs, err := d.GetRunsByRepo(repo.ID)
				if err != nil {
					return "", "", fmt.Errorf("list runs: %w", err)
				}

				fingerprint := repo.ID + "|" + renderedRunsFingerprint(runs, limit)

				w := cmd.OutOrStdout()

				if len(runs) == 0 {
					fmt.Fprintf(w, "  %s\n", sDim.Render("no runs yet. Push through the gate to start a pipeline:"))
					fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
					return fingerprint, "", nil
				}

				// Apply limit.
				shown := runs
				if limit > 0 && len(shown) > limit {
					shown = shown[:limit]
				}

				for _, r := range shown {
					printRunLine(w, r)
				}

				if len(runs) > len(shown) {
					fmt.Fprintf(w, "\n  %s\n", sDim.Render(fmt.Sprintf("(%d more runs, use --limit to see more)", len(runs)-len(shown))))
				}

				return fingerprint, "", nil
			})
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "maximum number of runs to display")
	cmd.AddCommand(newRunsPinCmd(true), newRunsPinCmd(false))
	return cmd
}

func newRunsPinCmd(pin bool) *cobra.Command {
	action := "pin"
	resultWord := "pinned"
	if !pin {
		action = "unpin"
		resultWord = "unpinned"
	}
	return &cobra.Command{
		Use:   action + " <run-id>",
		Short: resultWord + " a run for rich-data retention",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("runs "+action, func() error {
				_, database, err := openResources()
				if err != nil {
					return err
				}
				defer database.Close()
				if _, err := database.SetRunPinned(args[0], pin); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s run %s\n", resultWord, args[0])
				return nil
			})
		},
	}
}

func printRunLine(w io.Writer, r *db.Run) {
	ts := time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04")
	sha := r.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	pr := ""
	if r.PRURL != nil {
		pr = fmt.Sprintf("  %s", *r.PRURL)
	}
	pin := ""
	if r.PinnedAt != nil {
		pin = "  pinned"
	}
	fmt.Fprintf(w, "  %-26s %-12s %-20s %s  %s%s%s\n", r.ID, runStatusStyle(r.Status), r.Branch, sDim.Render(sha), sDim.Render(ts), pr, pin)
}
