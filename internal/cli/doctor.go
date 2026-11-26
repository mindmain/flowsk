// File doctor.go implements `flowsk doctor` for verifying ~/.flowsk configuration,
// database connectivity & Goose schema migrations, orphan `.md` prune hygiene,
// AI provider skill installation + SHA-256 hash integrity, and MCP registration.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mindmain/flowsk/internal/skill"
	"github.com/mindmain/flowsk/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type doctorCommandDeps interface {
	WorkspaceDependency
	TaskDependency
	LocalPathDependency
	DatabaseDependency
	FormatterDependency
}

// newDoctorCommand constructs `flowsk doctor`.
func newDoctorCommand(v *viper.Viper, opts *GlobalOptions, deps doctorCommandDeps) *cobra.Command {
	var (
		wsClient   WorkspaceClient
		taskClient TaskClient
		localPaths LocalPathRegistry
		dbAdmin    DatabaseAdmin
		formatter  OutputFormatter
	)
	if deps != nil {
		wsClient = deps.WorkspaceClient()
		taskClient = deps.TaskClient()
		localPaths = deps.LocalPathRegistry()
		dbAdmin = deps.DatabaseAdmin()
		formatter = deps.OutputFormatter()
	}

	var (
		strict  bool
		homeDir string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose Flowsk storage, database migrations, orphan resources, MCP, and AI skill hashes",
		Long: `Runs a comprehensive read-only health check across your Flowsk installation:
  1. Home & Local Bindings (~/.flowsk, config.yaml, local_paths.json stale path check)
  2. Database Connectivity & Goose Schema Migrations (SQLite WAL or remote ConnectRPC)
  3. Resource Hygiene (orphan .md files & broken DB references requiring 'flowsk prune', plus archived tasks)
  4. AI Agent Skills & Hash Verification (checks installed SKILL.md SHA-256 against the current binary)
  5. MCP Registration Hints (checks whether detected AI harnesses reference the 'flowsk' MCP server)`,
		Example: `  flowsk doctor
  flowsk doctor --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("doctor context canceled: %w", err)
			}

			mode := "local"
			if opts != nil && opts.IsRemoteMode() {
				mode = "remote"
			}
			embeddedHash := skill.EmbeddedHash()

			report := DoctorReportView{
				Version:             version.NormalizedTag(),
				Mode:                mode,
				EmbeddedSkillSHA256: embeddedHash,
			}

			addCheck := func(c DoctorCheckView) {
				switch c.Status {
				case "ok":
					report.OKCount++
				case "warn":
					report.WarnCount++
				case "fail":
					report.FailCount++
				}
				report.Checks = append(report.Checks, c)
			}

			// 1. Flowsk Home & Local Path Bindings check.
			cfgDir, _ := DefaultConfigDir()
			cfgFile, _ := resolveActiveConfigPath(opts)
			lpFile, _ := DefaultLocalPathsFile()
			ws := "default"
			if opts != nil && strings.TrimSpace(opts.Workspace) != "" {
				ws = strings.TrimSpace(opts.Workspace)
			}

			var staleDirs []string
			boundCount := 0
			if localPaths != nil {
				if bindings, bErr := localPaths.ListByProject(ctx, ws, ""); bErr == nil {
					boundCount = len(bindings)
					for _, b := range bindings {
						if st, stErr := os.Stat(b.LocalDir); stErr != nil || !st.IsDir() {
							staleDirs = append(staleDirs, fmt.Sprintf("%s (%s)", b.LocalDir, firstNonEmptyCLI(b.ProjectKey, b.ProjectID)))
						}
					}
				}
			}
			if len(staleDirs) > 0 {
				addCheck(DoctorCheckView{
					Name:    "home",
					Status:  "warn",
					Summary: fmt.Sprintf("%s (%d bound dir(s), %d missing on disk)", cfgDir, boundCount, len(staleDirs)),
					Hint:    "Run 'flowsk project unlink <dir>' to remove stale folder bindings from local_paths.json",
					Details: staleDirs,
				})
			} else {
				cfgState := "present"
				if _, stErr := os.Stat(cfgFile); stErr != nil {
					cfgState = "using defaults"
				}
				addCheck(DoctorCheckView{
					Name:    "home",
					Status:  "ok",
					Summary: fmt.Sprintf("%s (config: %s, local bindings: %d, registry: %s)", cfgDir, cfgState, boundCount, lpFile),
				})
			}

			// 2. Database & Migrations check.
			dbHealthy := false
			if mode == "remote" {
				if wsClient == nil {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "skip",
						Summary: fmt.Sprintf("Remote server %s (client port not wired)", opts.ServerURL),
					})
				} else if _, rErr := wsClient.ListWorkspaces(ctx, ListWorkspacesInput{Limit: 1}); rErr != nil {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "fail",
						Summary: fmt.Sprintf("Remote ConnectRPC server %s unreachable: %v", opts.ServerURL, rErr),
						Hint:    "Verify FLOWSK_SERVER_URL / --server-url and ensure 'flowsk serve' is running",
					})
				} else {
					dbHealthy = true
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "ok",
						Summary: fmt.Sprintf("Connected to remote Flowsk server (%s)", opts.ServerURL),
					})
				}
			} else {
				driver, dsn, resolveErr := resolveDoctorDBDriverAndDSN(v, opts)
				if resolveErr != nil {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "fail",
						Summary: fmt.Sprintf("Invalid database configuration: %v", resolveErr),
					})
				} else if dbAdmin == nil {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "skip",
						Summary: fmt.Sprintf("Local %s database %s (admin port not wired)", driver, dsn),
					})
				} else if driver == "sqlite" && !fileExistsOnDisk(dsn) {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "ok",
						Summary: fmt.Sprintf("SQLite database %s not created yet (will initialize automatically on first write)", dsn),
					})
				} else if pingErr := dbAdmin.PingDB(ctx, driver, dsn); pingErr != nil {
					addCheck(DoctorCheckView{
						Name:    "database",
						Status:  "fail",
						Summary: fmt.Sprintf("Database ping failed (%s %s): %v", driver, dsn, pingErr),
					})
				} else {
					dbHealthy = true
					migs, mErr := dbAdmin.MigrateStatus(ctx, driver, dsn)
					if mErr != nil {
						addCheck(DoctorCheckView{
							Name:    "database",
							Status:  "warn",
							Summary: fmt.Sprintf("Database reachable (%s), migration check returned: %v", driver, mErr),
							Hint:    "Run 'flowsk db migrate up' to synchronize schema migrations",
						})
					} else {
						applied, pending := 0, 0
						for _, m := range migs {
							if m.Applied {
								applied++
							} else {
								pending++
							}
						}
						if pending > 0 {
							addCheck(DoctorCheckView{
								Name:    "database",
								Status:  "warn",
								Summary: fmt.Sprintf("%s reachable (%d applied, %d pending migration(s))", driver, applied, pending),
								Hint:    "Run 'flowsk db migrate up' to apply pending Goose migrations",
							})
						} else {
							addCheck(DoctorCheckView{
								Name:    "database",
								Status:  "ok",
								Summary: fmt.Sprintf("%s healthy (%d/%d Goose migrations applied)", driver, applied, len(migs)),
							})
						}
					}
				}
			}

			// 3. Orphan `.md` resources & archived task hygiene (`prune` check).
			if taskClient == nil || !dbHealthy {
				addCheck(DoctorCheckView{
					Name:    "prune",
					Status:  "skip",
					Summary: "Skipped orphan resource inspection (no active database initialized yet)",
				})
			} else {
				orphans, oErr := taskClient.InspectOrphans(ctx, ws)
				if oErr != nil {
					addCheck(DoctorCheckView{
						Name:    "prune",
						Status:  "warn",
						Summary: fmt.Sprintf("Could not inspect orphan Markdown resources: %v", oErr),
					})
				} else if orphans.TotalIssues > 0 {
					addCheck(DoctorCheckView{
						Name:    "prune",
						Status:  "warn",
						Summary: fmt.Sprintf("%d orphan/missing Markdown resource(s) detected in workspace %q", orphans.TotalIssues, ws),
						Hint:    "Run 'flowsk prune' to clean orphan .md files and broken database flags",
						Details: orphans.IssueSummaries,
					})
				} else {
					archivedInfo := ""
					if archList, aErr := taskClient.ListTasks(ctx, ListTasksInput{
						Workspace: ws,
						State:     "archived",
						Limit:     100,
					}); aErr == nil && len(archList.Items) > 0 {
						archivedInfo = fmt.Sprintf(" (%d archived task(s) can be purged with 'flowsk task purge-archived')", len(archList.Items))
					}
					addCheck(DoctorCheckView{
						Name:    "prune",
						Status:  "ok",
						Summary: "Zero orphan .md files or broken database references" + archivedInfo,
					})
				}
			}

			// 4. AI Agent Provider Skills & SHA-256 Hash Check.
			skillStatuses, sErr := skill.InspectAll(homeDir)
			if sErr != nil {
				addCheck(DoctorCheckView{
					Name:    "skills",
					Status:  "fail",
					Summary: fmt.Sprintf("Failed to inspect AI provider skill directories: %v", sErr),
				})
			} else {
				detectedCnt := 0
				upToDateCnt := 0
				outdatedCnt := 0
				missingOnDetectedCnt := 0
				mcpConfiguredCnt := 0
				var skillDetails []string

				for _, st := range skillStatuses {
					report.Skills = append(report.Skills, DoctorSkillView{
						ProviderID:      st.Target.ID,
						ProviderName:    st.Target.Name,
						Detected:        st.Detected,
						Installed:       st.Installed,
						UpToDate:        st.UpToDate,
						InstalledSHA256: st.InstalledHash,
						ExpectedSHA256:  st.ExpectedHash,
						SkillPath:       st.Target.SkillFilePath,
						MCPConfigured:   st.MCPConfigured,
					})

					if !st.Detected {
						continue
					}
					detectedCnt++
					if st.MCPConfigured {
						mcpConfiguredCnt++
					}
					switch {
					case st.Installed && st.UpToDate:
						upToDateCnt++
					case st.Installed && !st.UpToDate:
						outdatedCnt++
						skillDetails = append(skillDetails, fmt.Sprintf("%s: outdated skill hash %s != %s (%s)",
							st.Target.Name, skill.ShortHash(st.InstalledHash), skill.ShortHash(st.ExpectedHash), st.Target.SkillFilePath))
					default:
						missingOnDetectedCnt++
						skillDetails = append(skillDetails, fmt.Sprintf("%s: skill not installed (%s)",
							st.Target.Name, st.Target.SkillFilePath))
					}
				}

				switch {
				case outdatedCnt > 0:
					addCheck(DoctorCheckView{
						Name:    "skills",
						Status:  "warn",
						Summary: fmt.Sprintf("%d outdated and %d missing skill(s) across %d detected AI provider(s) (%d up-to-date)", outdatedCnt, missingOnDetectedCnt, detectedCnt, upToDateCnt),
						Hint:    "Run 'flowsk install' to update SKILL.md to sha256:" + skill.ShortHash(embeddedHash),
						Details: skillDetails,
					})
				case upToDateCnt == 0:
					addCheck(DoctorCheckView{
						Name:    "skills",
						Status:  "warn",
						Summary: fmt.Sprintf("Flowsk SKILL.md is not installed in any AI provider (%d provider(s) detected on machine)", detectedCnt),
						Hint:    "Run 'flowsk install' to install the Flowsk skill into your AI coding agents",
						Details: skillDetails,
					})
				case missingOnDetectedCnt > 0:
					addCheck(DoctorCheckView{
						Name:    "skills",
						Status:  "ok",
						Summary: fmt.Sprintf("%d installed skill(s) verified at sha256:%s (%d other detected provider(s) optional via 'flowsk install')", upToDateCnt, skill.ShortHash(embeddedHash), missingOnDetectedCnt),
					})
				default:
					addCheck(DoctorCheckView{
						Name:    "skills",
						Status:  "ok",
						Summary: fmt.Sprintf("All %d detected AI provider skill(s) verified at sha256:%s", upToDateCnt, skill.ShortHash(embeddedHash)),
					})
				}

				// 5. MCP configuration check across detected providers.
				if detectedCnt > 0 && mcpConfiguredCnt == 0 {
					addCheck(DoctorCheckView{
						Name:    "mcp",
						Status:  "warn",
						Summary: "No detected AI provider config mentions the 'flowsk' MCP server",
						Hint:    "Run 'flowsk install' to install SKILL.md and register the 'flowsk' MCP server",
					})
				} else if mcpConfiguredCnt > 0 {
					addCheck(DoctorCheckView{
						Name:    "mcp",
						Status:  "ok",
						Summary: fmt.Sprintf("Flowsk MCP server detected in %d AI provider configuration(s)", mcpConfiguredCnt),
					})
				}
			}

			if err := renderPayload(cmd.OutOrStdout(), formatter, opts, report); err != nil {
				return err
			}
			if report.FailCount > 0 {
				return fmt.Errorf("flowsk doctor detected %d failing check(s)", report.FailCount)
			}
			if strict && report.WarnCount > 0 {
				return fmt.Errorf("flowsk doctor detected %d warning(s) in --strict mode", report.WarnCount)
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&strict, "strict", false, "Return a non-zero exit code if any warning is reported")
	flags.StringVar(&homeDir, "home", "", "Override user home directory for AI provider inspection")
	_ = flags.MarkHidden("home")
	return cmd
}

func resolveDoctorDBDriverAndDSN(v *viper.Viper, opts *GlobalOptions) (string, string, error) {
	rawDriver := "sqlite"
	if v != nil && strings.TrimSpace(v.GetString("db-driver")) != "" {
		rawDriver = v.GetString("db-driver")
	}
	driver, err := validateDBDriver(rawDriver)
	if err != nil {
		return "", "", err
	}
	var dsn string
	if v != nil {
		dsn = strings.TrimSpace(v.GetString("db-dsn"))
	}
	if dsn == "" && driver == "sqlite" && opts != nil {
		dsn = strings.TrimSpace(opts.DBPath)
	}
	if dsn == "" && driver == "sqlite" {
		dsn, _ = DefaultLocalDBFile()
	}
	if dsn == "" {
		return "", "", fmt.Errorf("database DSN is not configured for driver %q", driver)
	}
	return driver, dsn, nil
}

func resolveActiveConfigPath(opts *GlobalOptions) (string, error) {
	if opts != nil && strings.TrimSpace(opts.ConfigFile) != "" {
		return strings.TrimSpace(opts.ConfigFile), nil
	}
	return DefaultConfigFile()
}

func fileExistsOnDisk(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func renderHumanDoctorReport(w io.Writer, v DoctorReportView) error {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK DOCTOR"),
		"  ",
		tuiSubtitleStyle.Render(fmt.Sprintf("%s • mode:%s • skill sha256:%s", v.Version, v.Mode, skill.ShortHash(v.EmbeddedSkillSHA256))),
	)

	var b strings.Builder
	b.WriteString(header + "\n\n")

	for _, c := range v.Checks {
		var badge string
		switch c.Status {
		case "ok":
			badge = tuiOKStyle.Render("✓ OK  ")
		case "warn":
			badge = tuiWarnStyle.Render("! WARN")
		case "fail":
			badge = tuiFailStyle.Render("✗ FAIL")
		default:
			badge = tuiMutedStyle.Render("· SKIP")
		}
		fmt.Fprintf(&b, "  %s  %-10s  %s\n", badge, c.Name, c.Summary)
		for _, d := range c.Details {
			fmt.Fprintf(&b, "                      %s\n", tuiMutedStyle.Render("• "+d))
		}
		if c.Hint != "" {
			fmt.Fprintf(&b, "                      %s %s\n", tuiInfoStyle.Render("→"), c.Hint)
		}
	}

	if len(v.Skills) > 0 {
		b.WriteString("\n" + lipgloss.NewStyle().Bold(true).Render("AI Provider Skills (expected sha256:"+skill.ShortHash(v.EmbeddedSkillSHA256)+")") + "\n")
		for _, s := range v.Skills {
			var statusLabel string
			switch {
			case s.Installed && s.UpToDate:
				statusLabel = tuiOKStyle.Render("✓ up-to-date (" + skill.ShortHash(s.InstalledSHA256) + ")")
			case s.Installed && !s.UpToDate:
				statusLabel = tuiWarnStyle.Render("! outdated   (" + skill.ShortHash(s.InstalledSHA256) + ")")
			case s.Detected:
				statusLabel = tuiWarnStyle.Render("! not installed")
			default:
				statusLabel = tuiMutedStyle.Render("· not detected ")
			}
			mcpTag := ""
			if s.MCPConfigured {
				mcpTag = " " + tuiOKStyle.Render("[mcp ✓]")
			}
			fmt.Fprintf(&b, "  %-28s  %s%s\n", s.ProviderName, statusLabel, mcpTag)
		}
	}

	b.WriteString("\n" + tuiSubtitleStyle.Render(fmt.Sprintf("Summary: %d ok • %d warning(s) • %d failure(s)", v.OKCount, v.WarnCount, v.FailCount)))
	_, err := io.WriteString(w, tuiCardStyle.Render(b.String())+"\n")
	return err
}
