package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/spf13/cobra"
)

type doctorAgentCheck struct {
	name     string
	binaries []string
}

// retiredRepoConfigEnv is the retired machine-local repo-config opt-in,
// replaced by the global config's overrides map. Nothing reads it any more, so
// a machine that still exports it would otherwise silently lose the commands,
// hooks, and agent routes it used to supply; doctor reports it as a migration
// signal.
const retiredRepoConfigEnv = "NM_REPO_CONFIG"

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
					gitCmd := exec.Command("git", "--version")
					winproc.Harden(gitCmd)
					out, err := gitCmd.Output()
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
					if detail, present := doctorOverridesDetail(cmd.Context(), p); present {
						ok("repo overrides", detail)
					}
				}

				if _, set := os.LookupEnv(retiredRepoConfigEnv); set {
					warn(retiredRepoConfigEnv, "no longer supported "+sDim.Render("(move its contents into overrides in the global config)"))
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

				if p == nil {
					fail("gate validation", "unavailable: data directory could not be resolved")
					allOK = false
				} else {
					globalCfg, err := config.LoadGlobal(p.ConfigFile())
					if err != nil {
						fail("gate validation", fmt.Sprintf("unavailable: load config (%v)", err))
						allOK = false
					} else {
						cfg := config.Merge(globalCfg, &config.RepoConfig{})
						if err := cfg.ResolveAgent(cmd.Context(), exec.LookPath); err != nil {
							fail("gate validation", err.Error())
							allOK = false
						} else {
							ok("gate validation", fmt.Sprintf("%s is runnable", cfg.Agent))
						}
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

// doctorOverridesDetail summarizes the global config's machine-local repo
// overrides: which <owner>/<repo> keys exist and whether the current
// directory's repository matches one. A config that fails to load is reported
// by the gate-validation check, so this stays silent then; it also stays
// silent when no overrides are configured.
//
// Matching goes through localRepoIdentity, so it answers the question a run
// would answer: the daemon matches the registered upstream URL, and only an
// unregistered repository falls back to the checkout's origin remote.
func doctorOverridesDetail(ctx context.Context, p *paths.Paths) (string, bool) {
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil || len(globalCfg.Overrides) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(globalCfg.Overrides))
	for key := range globalCfg.Overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	detail := strings.Join(keys, ", ")
	local := doctorLocalRepo(p)
	switch identity, found := localRepoIdentity(ctx, local); {
	case local.root == "":
		detail += " " + sDim.Render("(not inside a git repository)")
	case !found:
		detail += " " + sDim.Render("(this repository's remote has no <owner>/<repo> identity, so no override can apply)")
	default:
		if _, key, matched := globalCfg.OverrideForRepoIdentity(identity); matched {
			detail += fmt.Sprintf("; %s applies to this repository", key)
		} else {
			detail += "; none apply to this repository"
		}
	}
	return detail, true
}

// doctorLocalRepo resolves the working directory's repository context for the
// overrides report. The database is opened only when it already exists, so a
// health check never creates the state database the check below reports on.
func doctorLocalRepo(p *paths.Paths) localRepo {
	root, err := git.FindGitRoot(".")
	if err != nil {
		return localRepo{}
	}
	if _, err := os.Stat(p.DB()); err != nil {
		return localRepo{root: root}
	}
	d, err := db.Open(p.DB())
	if err != nil {
		return localRepo{root: root}
	}
	defer d.Close()
	return resolveLocalRepo(d)
}

func doctorAgentChecks() []doctorAgentCheck {
	agents := []doctorAgentCheck{
		{"claude", []string{"claude"}},
		{"codex", []string{"codex"}},
		{"rovodev", []string{"acli"}},
		{"opencode", []string{"opencode"}},
		{"pi", []string{"pi"}},
		{"copilot", []string{"copilot"}},
		{"cursor", []string{"cursor-agent"}},
		{"acpx", []string{"acpx"}},
	}
	for _, registered := range types.RegisteredACPTargets() {
		agents = append(agents, doctorAgentCheck{
			name: "acp:" + registered.Target,
			binaries: []string{
				registered.DefaultCommandBinary(),
				"acpx",
			},
		})
	}
	return agents
}
