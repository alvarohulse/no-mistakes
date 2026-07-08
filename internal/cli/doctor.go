package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

type doctorAgentCheck struct {
	name     string
	binaries []string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommandStatus("doctor", func() (string, error) {
				w := cmd.OutOrStdout()
				allOK := true

				ok := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sGreen.Render("✓"), sDim.Render(label), detail)
				}
				warn := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sYellow.Render("–"), sDim.Render(label), detail)
				}
				fail := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sRed.Render("✗"), sDim.Render(label), detail)
				}

				fmt.Fprintf(w, "  %s\n", sCyan.Render("System"))

				if _, err := exec.LookPath("git"); err != nil {
					fail("git           ", "not found")
					allOK = false
				} else {
					out, err := exec.Command("git", "--version").Output()
					if err != nil {
						fail("git           ", fmt.Sprintf("error (%v)", err))
						allOK = false
					} else {
						ok("git           ", strings.TrimSpace(string(out)))
					}
				}

				if _, err := exec.LookPath("gh"); err != nil {
					warn("gh            ", "not found "+sDim.Render("(optional, needed for PR/CI)"))
				} else {
					ok("gh            ", "ok")
				}

				if _, err := exec.LookPath("az"); err != nil {
					warn("az            ", "not found "+sDim.Render("(optional, needed for Azure DevOps PR/CI)"))
				} else {
					ok("az            ", "ok")
				}

				p, err := paths.New()
				if err != nil {
					fail("data directory", fmt.Sprintf("error resolving paths (%v)", err))
					allOK = false
				} else if _, err := os.Stat(p.Root()); os.IsNotExist(err) {
					fail("data directory", fmt.Sprintf("not found (%s)", p.Root()))
					allOK = false
				} else {
					ok("data directory", p.Root())
				}

				if p != nil {
					if _, err := os.Stat(p.DB()); os.IsNotExist(err) {
						warn("database      ", "not found "+sDim.Render("(will be created on first use)"))
					} else {
						d, err := db.Open(p.DB())
						if err != nil {
							fail("database      ", fmt.Sprintf("error (%v)", err))
							allOK = false
						} else {
							d.Close()
							ok("database      ", "ok")
						}
					}
				}

				if p != nil {
					alive, _ := daemon.IsRunning(p)
					if alive {
						ok("daemon        ", "running")
					} else {
						warn("daemon        ", "stopped")
					}
				}

				agents := doctorAgentChecks()
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Agents"))
				for _, a := range agents {
					label := fmt.Sprintf("%-14s", a.name)
					var found, missing []string
					for _, bin := range a.binaries {
						if path, err := exec.LookPath(bin); err != nil {
							missing = append(missing, bin)
						} else {
							found = append(found, path)
						}
					}
					switch {
					case len(missing) == 0:
						ok(label, strings.Join(found, ", "))
					case len(a.binaries) > 1:
						warn(label, "not found ("+strings.Join(missing, ", ")+")")
					default:
						warn(label, "not found")
					}
				}

				if !allOK {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sRed.Render("some checks failed"))
					return "error", nil
				}

				return "success", nil
			})
		},
	}
}

func doctorAgentChecks() []doctorAgentCheck {
	agents := []doctorAgentCheck{
		{"claude", []string{"claude"}},
		{"codex", []string{"codex"}},
		{"rovodev", []string{"acli"}},
		{"opencode", []string{"opencode"}},
		{"pi", []string{"pi"}},
		{"copilot", []string{"copilot"}},
	}
	for _, alias := range types.ACPAliases() {
		agents = append(agents, doctorAgentCheck{
			name: string(alias.Name),
			binaries: []string{
				alias.DefaultCommandBinary(),
				"acpx",
			},
		})
	}
	return agents
}
