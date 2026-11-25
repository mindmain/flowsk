// File install.go implements `flowsk install` with an interactive Lip Gloss +
// Bubble Tea TUI provider multi-selector and confirmation screen for installing
// the embedded Flowsk `SKILL.md` across global AI coding agent skill directories.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mindmain/flowsk/internal/skill"
	"github.com/spf13/cobra"
)

// Lip Gloss palette shared across `flowsk install`, `flowsk doctor`, and `flowsk update`.
var (
	tuiTitleBadgeStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#0F172A")).
				Background(lipgloss.Color("#38BDF8")).
				Padding(0, 1)

	tuiSubtitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#94A3B8"))

	tuiCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#334155")).
			Padding(1, 2)

	tuiOKStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#10B981"))

	tuiWarnStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F59E0B"))

	tuiFailStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#EF4444"))

	tuiInfoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#38BDF8"))

	tuiMutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#64748B"))

	tuiCursorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#38BDF8"))

	tuiActiveBtnStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#0F172A")).
				Background(lipgloss.Color("#10B981")).
				Padding(0, 2)

	tuiActiveCancelBtnStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#F8FAFC")).
				Background(lipgloss.Color("#EF4444")).
				Padding(0, 2)

	tuiInactiveBtnStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#CBD5E1")).
				Background(lipgloss.Color("#1E293B")).
				Padding(0, 2)
)

type installStage int

const (
	installStageSelect installStage = iota
	installStageConfirm
)

// InstallTUIModel implements `tea.Model` for interactive multi-provider skill
// selection and confirmation in `flowsk install`.
type InstallTUIModel struct {
	statuses     []skill.ProviderStatus
	selected     map[string]bool
	cursor       int
	stage        installStage
	confirmFocus int // 0 = Confirm & Install, 1 = Back, 2 = Cancel
	validation   string
	confirmed    bool
	canceled     bool
}

// NewInstallTUIModel constructs a two-stage Bubble Tea model (multi-select +
// confirmation) initialized with detected AI agent providers pre-selected.
func NewInstallTUIModel(statuses []skill.ProviderStatus, preselectedIDs []string) *InstallTUIModel {
	selected := make(map[string]bool, len(statuses))
	if len(preselectedIDs) > 0 {
		for _, id := range preselectedIDs {
			selected[strings.ToLower(strings.TrimSpace(id))] = true
		}
	} else {
		for _, st := range statuses {
			if st.Detected {
				selected[st.Target.ID] = true
			}
		}
		if len(selected) == 0 {
			for _, id := range []string{skill.ProviderClaude, skill.ProviderCodex, skill.ProviderAntigravity, skill.ProviderAgents} {
				selected[id] = true
			}
		}
	}
	return &InstallTUIModel{
		statuses: statuses,
		selected: selected,
		stage:    installStageSelect,
	}
}

// Init implements tea.Model.
func (m *InstallTUIModel) Init() tea.Cmd {
	return nil
}

// Confirmed reports whether the user explicitly confirmed installation.
func (m *InstallTUIModel) Confirmed() bool {
	return m != nil && m.confirmed && !m.canceled
}

// Canceled reports whether the user aborted the interactive installer.
func (m *InstallTUIModel) Canceled() bool {
	return m == nil || m.canceled || !m.confirmed
}

// SelectedTargets returns the ordered slice of ProviderTargets chosen by the user.
func (m *InstallTUIModel) SelectedTargets() []skill.ProviderTarget {
	if m == nil {
		return nil
	}
	var out []skill.ProviderTarget
	for _, st := range m.statuses {
		if m.selected[st.Target.ID] {
			out = append(out, st.Target)
		}
	}
	return out
}

// Update handles keyboard navigation, multi-select toggling, and confirmation keys.
func (m *InstallTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	key := keyMsg.String()
	if key == "ctrl+c" {
		m.canceled = true
		m.confirmed = false
		return m, tea.Quit
	}

	switch m.stage {
	case installStageSelect:
		switch key {
		case "q", "esc":
			m.canceled = true
			m.confirmed = false
			return m, tea.Quit
		case "up", "k":
			if len(m.statuses) > 0 {
				m.cursor = (m.cursor - 1 + len(m.statuses)) % len(m.statuses)
			}
		case "down", "j":
			if len(m.statuses) > 0 {
				m.cursor = (m.cursor + 1) % len(m.statuses)
			}
		case " ", "x":
			if m.cursor >= 0 && m.cursor < len(m.statuses) {
				id := m.statuses[m.cursor].Target.ID
				m.selected[id] = !m.selected[id]
				m.validation = ""
			}
		case "a":
			allSelected := len(m.SelectedTargets()) == len(m.statuses)
			for _, st := range m.statuses {
				m.selected[st.Target.ID] = !allSelected
			}
			m.validation = ""
		case "enter":
			if len(m.SelectedTargets()) == 0 {
				m.validation = "Select at least one AI provider with Space (or press q to cancel)."
				return m, nil
			}
			m.validation = ""
			m.stage = installStageConfirm
			m.confirmFocus = 0
		}

	case installStageConfirm:
		switch key {
		case "left", "h", "shift+tab":
			m.confirmFocus = (m.confirmFocus + 2) % 3
		case "right", "l", "tab":
			m.confirmFocus = (m.confirmFocus + 1) % 3
		case "b", "backspace":
			m.stage = installStageSelect
		case "y", "Y":
			m.confirmed = true
			m.canceled = false
			return m, tea.Quit
		case "n", "N", "q", "esc":
			m.canceled = true
			m.confirmed = false
			return m, tea.Quit
		case "enter":
			switch m.confirmFocus {
			case 0:
				m.confirmed = true
				m.canceled = false
				return m, tea.Quit
			case 1:
				m.stage = installStageSelect
			default:
				m.canceled = true
				m.confirmed = false
				return m, tea.Quit
			}
		}
	}

	return m, nil
}

// View renders either the Lip Gloss provider multi-selector or confirmation screen.
func (m *InstallTUIModel) View() string {
	shortHash := skill.ShortHash(skill.EmbeddedHash())
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK SKILL INSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("embedded sha256:"+shortHash),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")

	if m.stage == installStageSelect {
		body.WriteString("Select the AI coding agents where the global Flowsk skill should be installed:\n\n")
		for i, st := range m.statuses {
			cursor := "  "
			name := st.Target.Name
			if i == m.cursor {
				cursor = tuiCursorStyle.Render("▸ ")
				name = tuiCursorStyle.Render(name)
			}
			check := tuiMutedStyle.Render("[ ]")
			if m.selected[st.Target.ID] {
				check = tuiOKStyle.Render("[✓]")
			}
			badge := formatProviderTUIBadge(st)
			fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, name, badge)
			fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render(st.Target.Description))
		}
		if m.validation != "" {
			body.WriteString("\n" + tuiWarnStyle.Render("⚠ "+m.validation) + "\n")
		}
		body.WriteString("\n" + tuiSubtitleStyle.Render("↑/↓ navigate • space toggle • a all/none • enter review • q cancel"))
		return tuiCardStyle.Render(body.String()) + "\n"
	}

	targets := m.SelectedTargets()
	fmt.Fprintf(&body, "Confirm installing Flowsk SKILL.md (sha256:%s) into %d provider(s):\n\n", shortHash, len(targets))
	for _, t := range targets {
		fmt.Fprintf(&body, "  %s %-28s %s\n",
			tuiInfoStyle.Render("•"),
			t.Name,
			tuiMutedStyle.Render(t.SkillFilePath),
		)
	}

	confirmBtn := tuiInactiveBtnStyle.Render("Confirm & Install")
	backBtn := tuiInactiveBtnStyle.Render("Back")
	cancelBtn := tuiInactiveBtnStyle.Render("Cancel")
	switch m.confirmFocus {
	case 0:
		confirmBtn = tuiActiveBtnStyle.Render("Confirm & Install")
	case 1:
		backBtn = tuiTitleBadgeStyle.Render("Back")
	default:
		cancelBtn = tuiActiveCancelBtnStyle.Render("Cancel")
	}

	body.WriteString("\n" + lipgloss.JoinHorizontal(lipgloss.Center, confirmBtn, "  ", backBtn, "  ", cancelBtn) + "\n")
	body.WriteString("\n" + tuiSubtitleStyle.Render("←/→ select • enter confirm • y install now • b back • n/esc cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func formatProviderTUIBadge(st skill.ProviderStatus) string {
	switch {
	case st.Installed && st.UpToDate:
		return tuiOKStyle.Render("✓ up-to-date (" + skill.ShortHash(st.InstalledHash) + ")")
	case st.Installed && !st.UpToDate:
		return tuiWarnStyle.Render("↻ update (" + skill.ShortHash(st.InstalledHash) + " → " + skill.ShortHash(st.ExpectedHash) + ")")
	case st.Detected:
		return tuiInfoStyle.Render("• detected on machine")
	default:
		return tuiMutedStyle.Render("· not detected")
	}
}

// newInstallCommand constructs `flowsk install`.
func newInstallCommand(opts *GlobalOptions, deps FormatterDependency) *cobra.Command {
	var formatter OutputFormatter
	if deps != nil {
		formatter = deps.OutputFormatter()
	}

	var (
		providers []string
		all       bool
		assumeYes bool
		homeDir   string
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Interactively select AI agent providers and install the embedded Flowsk SKILL.md",
		Long: `Installs or updates the embedded Flowsk operational skill ('SKILL.md', verified by
SHA-256 hash) into global skill directories for Claude Code, OpenAI Codex,
Google Antigravity, Gemini CLI, Universal Agents (~/.agents), and Cursor.

In interactive terminals, 'flowsk install' opens a Lip Gloss + Bubble Tea
multi-select picker and confirmation dialog. For scripts or CI, pass
'--providers codex,antigravity --yes' or '--all --yes'.`,
		Example: `  # Launch interactive Lip Gloss TUI provider picker + confirmation
  flowsk install

  # Non-interactive install into Codex and Antigravity
  flowsk install --providers codex,antigravity --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("install context canceled: %w", err)
			}

			statuses, err := skill.InspectAll(homeDir)
			if err != nil {
				return err
			}

			var preselected []string
			if all {
				for _, st := range statuses {
					preselected = append(preselected, st.Target.ID)
				}
			} else if len(providers) > 0 {
				resolved, rErr := skill.ResolveProvidersByIDs(homeDir, providers)
				if rErr != nil {
					return rErr
				}
				for _, t := range resolved {
					preselected = append(preselected, t.ID)
				}
			}

			var chosen []skill.ProviderTarget
			if assumeYes {
				model := NewInstallTUIModel(statuses, preselected)
				chosen = model.SelectedTargets()
			} else if isInteractiveTTY(cmd.InOrStdin(), cmd.ErrOrStderr()) && (opts == nil || opts.Format == "" || opts.Format == FormatAuto || opts.Format == FormatHuman) {
				model := NewInstallTUIModel(statuses, preselected)
				prog := tea.NewProgram(model, tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.ErrOrStderr()))
				finalModel, tErr := prog.Run()
				if tErr != nil {
					return fmt.Errorf("interactive skill installer failed: %w", tErr)
				}
				doneModel, _ := finalModel.(*InstallTUIModel)
				if doneModel == nil || doneModel.Canceled() {
					return errors.New("skill installation canceled")
				}
				chosen = doneModel.SelectedTargets()
			} else {
				model := NewInstallTUIModel(statuses, preselected)
				chosen = model.SelectedTargets()
				if len(chosen) == 0 {
					return errors.New("no AI providers selected (pass --providers or --all)")
				}
				var names []string
				for _, t := range chosen {
					names = append(names, t.ID)
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Install Flowsk SKILL.md (sha256:%s) into [%s]? [y/N]: ",
					skill.ShortHash(skill.EmbeddedHash()), strings.Join(names, ", "))
				ok, pErr := readYesConfirmation(cmd.InOrStdin())
				if pErr != nil {
					return pErr
				}
				if !ok {
					return errors.New("skill installation canceled (pass --yes to confirm non-interactively)")
				}
			}

			if len(chosen) == 0 {
				return errors.New("no AI providers selected")
			}

			mcpByID := make(map[string]bool, len(statuses))
			for _, st := range statuses {
				mcpByID[st.Target.ID] = st.MCPConfigured
			}

			out := InstallSkillView{
				EmbeddedSHA256: skill.EmbeddedHash(),
				Items:          make([]SkillInstallItemView, 0, len(chosen)),
			}
			for _, target := range chosen {
				res, instErr := skill.InstallToProvider(target)
				if instErr != nil {
					return instErr
				}
				switch res.Action {
				case "installed":
					out.InstalledCount++
				case "updated":
					out.UpdatedCount++
				case "unchanged":
					out.UnchangedCount++
				}
				out.Items = append(out.Items, SkillInstallItemView{
					ProviderID:     target.ID,
					ProviderName:   target.Name,
					SkillPath:      target.SkillFilePath,
					Action:         res.Action,
					SHA256:         res.SHA256,
					PreviousSHA256: res.PreviousSHA256,
					MCPConfigured:  mcpByID[target.ID],
					MCPConfigPath:  target.MCPConfigPath,
				})
			}

			return renderPayload(cmd.OutOrStdout(), formatter, opts, out)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceVarP(&providers, "providers", "p", nil, "Comma-separated provider IDs (claude, codex, antigravity, gemini, agents, cursor)")
	flags.BoolVar(&all, "all", false, "Install Flowsk skill across all supported AI provider directories")
	flags.BoolVarP(&assumeYes, "yes", "y", false, "Confirm installation without launching interactive prompts")
	flags.StringVar(&homeDir, "home", "", "Override user home directory for global skill paths")
	_ = flags.MarkHidden("home")
	return cmd
}

func isInteractiveTTY(in io.Reader, out io.Writer) bool {
	inFile, okIn := in.(*os.File)
	outFile, okOut := out.(*os.File)
	if !okIn || !okOut {
		return false
	}
	inStat, errIn := inFile.Stat()
	outStat, errOut := outFile.Stat()
	if errIn != nil || errOut != nil {
		return false
	}
	return (inStat.Mode()&os.ModeCharDevice) != 0 && (outStat.Mode()&os.ModeCharDevice) != 0
}

func renderHumanInstallSkill(w io.Writer, v InstallSkillView) error {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK SKILL INSTALLED"),
		"  ",
		tuiSubtitleStyle.Render("sha256:"+skill.ShortHash(v.EmbeddedSHA256)),
	)

	var b strings.Builder
	b.WriteString(header + "\n\n")
	fmt.Fprintf(&b, "Summary: %d installed • %d updated • %d already up-to-date\n\n",
		v.InstalledCount, v.UpdatedCount, v.UnchangedCount)

	var missingMCP []string
	for _, it := range v.Items {
		var badge string
		switch it.Action {
		case "installed":
			badge = tuiOKStyle.Render("✓ INSTALLED")
		case "updated":
			badge = tuiWarnStyle.Render("↻ UPDATED  ")
		default:
			badge = tuiInfoStyle.Render("= UNCHANGED")
		}
		fmt.Fprintf(&b, "  %s  %-28s  %s\n", badge, it.ProviderName, tuiMutedStyle.Render(it.SkillPath))
		if it.MCPConfigPath != "" && !it.MCPConfigured && !slices.Contains(missingMCP, it.ProviderID) {
			missingMCP = append(missingMCP, it.ProviderID)
		}
	}

	if len(missingMCP) > 0 {
		b.WriteString("\n" + tuiWarnStyle.Render("Tip:") +
			fmt.Sprintf(" Flowsk MCP server was not detected in [%s] config — run 'flowsk mcp guide' to enable MCP tools.\n",
				strings.Join(missingMCP, ", ")))
	}

	_, err := io.WriteString(w, tuiCardStyle.Render(strings.TrimRight(b.String(), "\n"))+"\n")
	return err
}
