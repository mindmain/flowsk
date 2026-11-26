// File uninstall.go implements `flowsk uninstall` with an interactive 3-stage
// Lip Gloss + Bubble Tea TUI (Remove Skills -> Remove MCP Server -> Confirm)
// that cleanly removes installed Flowsk `SKILL.md` files and `flowsk` MCP server
// entries without disturbing other skills, MCP servers, comments, or symlinks.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mindmain/flowsk/internal/skill"
	"github.com/spf13/cobra"
)

// UninstallTUIModel implements `tea.Model` for the 3-stage interactive Skill + MCP
// removal wizard in `flowsk uninstall`.
type UninstallTUIModel struct {
	statuses      []skill.ProviderStatus
	mcpStatuses   []skill.ProviderStatus
	selectedSkill map[string]bool
	selectedMCP   map[string]bool
	skillCursor   int
	mcpCursor     int
	stage         installStage
	confirmFocus  int // 0 = Confirm & Uninstall, 1 = Back, 2 = Cancel
	validation    string
	confirmed     bool
	canceled      bool
}

// NewUninstallTUIModel constructs a 3-stage Bubble Tea uninstaller model pre-selecting
// all providers where Flowsk `SKILL.md` or `flowsk` MCP is currently installed.
func NewUninstallTUIModel(statuses []skill.ProviderStatus, preselectedSkillIDs, preselectedMCPIDs []string) *UninstallTUIModel {
	selectedSkill := make(map[string]bool, len(statuses))
	if len(preselectedSkillIDs) > 0 {
		for _, id := range preselectedSkillIDs {
			tok := strings.ToLower(strings.TrimSpace(id))
			if tok == "universal" {
				tok = skill.ProviderAgents
			}
			if tok == "all" {
				for _, st := range statuses {
					selectedSkill[st.Target.ID] = true
				}
			} else if tok != "" {
				selectedSkill[tok] = true
			}
		}
	} else {
		// Default: pre-select every provider that currently has SKILL.md installed.
		for _, st := range statuses {
			if st.Installed {
				selectedSkill[st.Target.ID] = true
			}
		}
	}

	var mcpStatuses []skill.ProviderStatus
	for _, st := range statuses {
		if st.Target.SupportsMCP() {
			mcpStatuses = append(mcpStatuses, st)
		}
	}

	selectedMCP := make(map[string]bool, len(mcpStatuses))
	if len(preselectedMCPIDs) > 0 {
		for _, raw := range preselectedMCPIDs {
			tok := strings.ToLower(strings.TrimSpace(raw))
			if tok == "universal" {
				tok = skill.ProviderAgents
			}
			if tok == "all" {
				for _, st := range mcpStatuses {
					selectedMCP[st.Target.ID] = true
				}
			} else if tok != "" {
				selectedMCP[tok] = true
			}
		}
	} else {
		// Default: pre-select every provider whose MCP config currently mentions flowsk.
		for _, st := range mcpStatuses {
			if st.MCPConfigured {
				selectedMCP[st.Target.ID] = true
			}
		}
	}

	return &UninstallTUIModel{
		statuses:      statuses,
		mcpStatuses:   mcpStatuses,
		selectedSkill: selectedSkill,
		selectedMCP:   selectedMCP,
		stage:         installStageSkill,
	}
}

// Init implements tea.Model.
func (m *UninstallTUIModel) Init() tea.Cmd {
	return nil
}

// Confirmed reports whether the user explicitly confirmed uninstallation.
func (m *UninstallTUIModel) Confirmed() bool {
	return m != nil && m.confirmed && !m.canceled
}

// Canceled reports whether the user aborted the interactive uninstaller.
func (m *UninstallTUIModel) Canceled() bool {
	return m == nil || m.canceled || !m.confirmed
}

// SelectedTargets returns the ordered slice of Skill ProviderTargets chosen for removal.
func (m *UninstallTUIModel) SelectedTargets() []skill.ProviderTarget {
	if m == nil {
		return nil
	}
	var out []skill.ProviderTarget
	for _, st := range m.statuses {
		if m.selectedSkill[st.Target.ID] {
			out = append(out, st.Target)
		}
	}
	return out
}

// SelectedMCPTargets returns the ordered slice of MCP ProviderTargets chosen for removal.
func (m *UninstallTUIModel) SelectedMCPTargets() []skill.ProviderTarget {
	if m == nil {
		return nil
	}
	var out []skill.ProviderTarget
	for _, st := range m.mcpStatuses {
		if m.selectedMCP[st.Target.ID] {
			out = append(out, st.Target)
		}
	}
	return out
}

// Update handles keyboard navigation across Skill removal, MCP removal, and Confirmation.
func (m *UninstallTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case installStageSkill:
		switch key {
		case "q", "esc":
			m.canceled = true
			m.confirmed = false
			return m, tea.Quit
		case "up", "k":
			if len(m.statuses) > 0 {
				m.skillCursor = (m.skillCursor - 1 + len(m.statuses)) % len(m.statuses)
			}
		case "down", "j":
			if len(m.statuses) > 0 {
				m.skillCursor = (m.skillCursor + 1) % len(m.statuses)
			}
		case " ", "x":
			if m.skillCursor >= 0 && m.skillCursor < len(m.statuses) {
				id := m.statuses[m.skillCursor].Target.ID
				m.selectedSkill[id] = !m.selectedSkill[id]
				m.validation = ""
			}
		case "a":
			allSelected := len(m.SelectedTargets()) == len(m.statuses)
			for _, st := range m.statuses {
				m.selectedSkill[st.Target.ID] = !allSelected
			}
			m.validation = ""
		case "enter":
			m.validation = ""
			m.stage = installStageMCP
		}

	case installStageMCP:
		switch key {
		case "q", "esc":
			m.canceled = true
			m.confirmed = false
			return m, tea.Quit
		case "b", "backspace":
			m.validation = ""
			m.stage = installStageSkill
		case "up", "k":
			if len(m.mcpStatuses) > 0 {
				m.mcpCursor = (m.mcpCursor - 1 + len(m.mcpStatuses)) % len(m.mcpStatuses)
			}
		case "down", "j":
			if len(m.mcpStatuses) > 0 {
				m.mcpCursor = (m.mcpCursor + 1) % len(m.mcpStatuses)
			}
		case " ", "x":
			if m.mcpCursor >= 0 && m.mcpCursor < len(m.mcpStatuses) {
				id := m.mcpStatuses[m.mcpCursor].Target.ID
				m.selectedMCP[id] = !m.selectedMCP[id]
				m.validation = ""
			}
		case "a":
			allSelected := len(m.SelectedMCPTargets()) == len(m.mcpStatuses)
			for _, st := range m.mcpStatuses {
				m.selectedMCP[st.Target.ID] = !allSelected
			}
			m.validation = ""
		case "enter":
			if len(m.SelectedTargets()) == 0 && len(m.SelectedMCPTargets()) == 0 {
				m.validation = "Select at least one Skill or MCP entry to remove with Space (or press q to cancel)."
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
			m.stage = installStageMCP
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
				m.stage = installStageMCP
			default:
				m.canceled = true
				m.confirmed = false
				return m, tea.Quit
			}
		}
	}

	return m, nil
}

// View renders Step 1 (Remove Skills), Step 2 (Remove MCP), or Step 3 (Confirm Uninstall).
func (m *UninstallTUIModel) View() string {
	switch m.stage {
	case installStageSkill:
		return m.renderSkillStage()
	case installStageMCP:
		return m.renderMCPStage()
	default:
		return m.renderConfirmStage()
	}
}

func (m *UninstallTUIModel) renderSkillStage() string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiActiveCancelBtnStyle.Render("FLOWSK UNINSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 1/3 • Remove Global Skills"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")
	body.WriteString("Select the Flowsk SKILL.md installations to remove (currently installed ones are pre-selected):\n\n")

	for i, st := range m.statuses {
		cursor := "  "
		name := st.Target.Name
		if i == m.skillCursor {
			cursor = tuiCursorStyle.Render("▸ ")
			name = tuiCursorStyle.Render(name)
		}
		check := tuiMutedStyle.Render("[ ]")
		if m.selectedSkill[st.Target.ID] {
			check = tuiFailStyle.Render("[✓]")
		}
		var badge string
		if st.Installed {
			badge = tuiWarnStyle.Render("• installed (" + skill.ShortHash(st.InstalledHash) + ")")
		} else {
			badge = tuiMutedStyle.Render("· not installed")
		}
		fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, name, badge)
		fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render(st.Target.Description))
	}

	if m.validation != "" {
		body.WriteString("\n" + tuiWarnStyle.Render("⚠ "+m.validation) + "\n")
	}
	body.WriteString("\n" + tuiSubtitleStyle.Render("↑/↓ navigate • space toggle • a all/none • enter next (MCP) • q cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func (m *UninstallTUIModel) renderMCPStage() string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiActiveCancelBtnStyle.Render("FLOWSK UNINSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 2/3 • Remove Flowsk MCP Server"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")
	body.WriteString("Select the Flowsk MCP server entries to remove (currently configured ones are pre-selected):\n\n")

	for i, st := range m.mcpStatuses {
		cursor := "  "
		name := st.Target.Name
		if i == m.mcpCursor {
			cursor = tuiCursorStyle.Render("▸ ")
			name = tuiCursorStyle.Render(name)
		}
		check := tuiMutedStyle.Render("[ ]")
		if m.selectedMCP[st.Target.ID] {
			check = tuiFailStyle.Render("[✓]")
		}
		var badge string
		if st.MCPConfigured {
			badge = tuiWarnStyle.Render("• mcp configured")
		} else {
			badge = tuiMutedStyle.Render("· not configured")
		}
		fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, name, badge)
		fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render(shortenHomePath(st.Target.MCPConfigPath)))
	}

	if m.validation != "" {
		body.WriteString("\n" + tuiWarnStyle.Render("⚠ "+m.validation) + "\n")
	}
	body.WriteString("\n" + tuiSubtitleStyle.Render("↑/↓ navigate • space toggle • a all/none • b back (Skill) • enter review • q cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func (m *UninstallTUIModel) renderConfirmStage() string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiActiveCancelBtnStyle.Render("FLOWSK UNINSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 3/3 • Confirm Removal"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")

	skillTargets := m.SelectedTargets()
	mcpTargets := m.SelectedMCPTargets()

	fmt.Fprintf(&body, "1. Remove Global Skill (SKILL.md) → %d target(s):\n", len(skillTargets))
	if len(skillTargets) == 0 {
		fmt.Fprintf(&body, "   %s\n", tuiMutedStyle.Render("(none selected)"))
	}
	for _, t := range skillTargets {
		fmt.Fprintf(&body, "   %s %-28s %s\n",
			tuiFailStyle.Render("•"),
			t.Name,
			tuiMutedStyle.Render(t.SkillFilePath),
		)
	}

	fmt.Fprintf(&body, "\n2. Remove Flowsk MCP Server → %d client config(s):\n", len(mcpTargets))
	if len(mcpTargets) == 0 {
		fmt.Fprintf(&body, "   %s\n", tuiMutedStyle.Render("(none selected)"))
	}
	for _, t := range mcpTargets {
		fmt.Fprintf(&body, "   %s %-28s %s\n",
			tuiFailStyle.Render("•"),
			t.Name,
			tuiMutedStyle.Render(t.MCPConfigPath),
		)
	}

	confirmBtn := tuiInactiveBtnStyle.Render("Confirm & Uninstall")
	backBtn := tuiInactiveBtnStyle.Render("Back")
	cancelBtn := tuiInactiveBtnStyle.Render("Cancel")
	switch m.confirmFocus {
	case 0:
		confirmBtn = tuiActiveCancelBtnStyle.Render("Confirm & Uninstall")
	case 1:
		backBtn = tuiTitleBadgeStyle.Render("Back")
	default:
		cancelBtn = tuiActiveBtnStyle.Render("Cancel")
	}

	body.WriteString("\n" + lipgloss.JoinHorizontal(lipgloss.Center, confirmBtn, "  ", backBtn, "  ", cancelBtn) + "\n")
	body.WriteString("\n" + tuiSubtitleStyle.Render("←/→ select • enter confirm • y uninstall now • b back (MCP) • n/esc cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

// newUninstallCommand constructs `flowsk uninstall`.
func newUninstallCommand(opts *GlobalOptions, deps FormatterDependency) *cobra.Command {
	var formatter OutputFormatter
	if deps != nil {
		formatter = deps.OutputFormatter()
	}

	var (
		providers    []string
		mcpProviders []string
		all          bool
		assumeYes    bool
		homeDir      string
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Interactively remove installed Flowsk SKILL.md files and Flowsk MCP server entries",
		Long: `Removes installed Flowsk operational skills ('SKILL.md') and deregisters the
'flowsk' stdio MCP server from AI client configurations without modifying any
other skills, MCP servers, user settings, comments, or symbolic links.

By default, 'flowsk uninstall' pre-selects every provider where Flowsk SKILL.md
or Flowsk MCP is currently installed.`,
		Example: `  # Launch interactive 3-stage TUI pre-selecting all currently installed Skills & MCPs
  flowsk uninstall

  # Non-interactively remove all currently installed Flowsk Skills & MCP entries
  flowsk uninstall --yes

  # Remove MCP from specific clients while keeping Universal (~/.agents)
  flowsk uninstall --mcp-providers claude,codex,antigravity,gemini,cursor --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("uninstall context canceled: %w", err)
			}

			statuses, err := skill.InspectAll(homeDir)
			if err != nil {
				return err
			}

			explicitFlags := all || len(providers) > 0 || len(mcpProviders) > 0
			var preselectedSkill, preselectedMCP []string
			if all {
				for _, st := range statuses {
					preselectedSkill = append(preselectedSkill, st.Target.ID)
					if st.Target.SupportsMCP() {
						preselectedMCP = append(preselectedMCP, st.Target.ID)
					}
				}
			} else {
				if len(providers) > 0 {
					resolved, rErr := skill.ResolveProvidersByIDs(homeDir, providers)
					if rErr != nil {
						return rErr
					}
					for _, t := range resolved {
						preselectedSkill = append(preselectedSkill, t.ID)
					}
				}
				if len(mcpProviders) > 0 {
					resolvedMCP, rErr := skill.ResolveMCPProvidersByIDs(homeDir, mcpProviders)
					if rErr != nil {
						return rErr
					}
					for _, t := range resolvedMCP {
						preselectedMCP = append(preselectedMCP, t.ID)
					}
				}
			}

			model := NewUninstallTUIModel(statuses, preselectedSkill, preselectedMCP)
			// When only --providers or only --mcp-providers was passed explicitly, do not auto-select the unspecified group.
			if len(providers) > 0 && len(mcpProviders) == 0 && !all {
				model = NewUninstallTUIModel(statuses, preselectedSkill, nil)
				clear(model.selectedMCP)
			} else if len(mcpProviders) > 0 && len(providers) == 0 && !all {
				model = NewUninstallTUIModel(statuses, nil, preselectedMCP)
				clear(model.selectedSkill)
			}

			// If nothing is installed anywhere and no explicit flags were passed, succeed cleanly.
			if !explicitFlags && len(model.SelectedTargets()) == 0 && len(model.SelectedMCPTargets()) == 0 {
				return renderPayload(cmd.OutOrStdout(), formatter, opts, UninstallSkillView{})
			}

			var (
				chosenSkills []skill.ProviderTarget
				chosenMCPs   []skill.ProviderTarget
			)

			if assumeYes {
				chosenSkills = model.SelectedTargets()
				chosenMCPs = model.SelectedMCPTargets()
			} else if isInteractiveTTY(cmd.InOrStdin(), cmd.ErrOrStderr()) && (opts == nil || opts.Format == "" || opts.Format == FormatAuto || opts.Format == FormatHuman) {
				prog := tea.NewProgram(model, tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.ErrOrStderr()))
				finalModel, tErr := prog.Run()
				if tErr != nil {
					return fmt.Errorf("interactive uninstaller failed: %w", tErr)
				}
				doneModel, _ := finalModel.(*UninstallTUIModel)
				if doneModel == nil || doneModel.Canceled() {
					return errors.New("uninstallation canceled")
				}
				chosenSkills = doneModel.SelectedTargets()
				chosenMCPs = doneModel.SelectedMCPTargets()
			} else {
				chosenSkills = model.SelectedTargets()
				chosenMCPs = model.SelectedMCPTargets()
				if len(chosenSkills) == 0 && len(chosenMCPs) == 0 {
					return renderPayload(cmd.OutOrStdout(), formatter, opts, UninstallSkillView{})
				}
				var skillNames, mcpNames []string
				for _, t := range chosenSkills {
					skillNames = append(skillNames, t.ID)
				}
				for _, t := range chosenMCPs {
					mcpNames = append(mcpNames, t.ID)
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Remove Flowsk SKILL.md [%s] and MCP server [%s]? [y/N]: ",
					strings.Join(skillNames, ", "), strings.Join(mcpNames, ", "))
				ok, pErr := readYesConfirmation(cmd.InOrStdin())
				if pErr != nil {
					return pErr
				}
				if !ok {
					return errors.New("uninstallation canceled (pass --yes to confirm non-interactively)")
				}
			}

			out := UninstallSkillView{
				SkillItems: make([]SkillUninstallItemView, 0, len(chosenSkills)),
				MCPItems:   make([]MCPUninstallItemView, 0, len(chosenMCPs)),
			}

			for _, target := range chosenSkills {
				res, uErr := skill.UninstallFromProvider(target)
				if uErr != nil {
					return uErr
				}
				if res.Action == "removed" {
					out.RemovedSkillCount++
				} else {
					out.NotFoundSkillCount++
				}
				out.SkillItems = append(out.SkillItems, SkillUninstallItemView{
					ProviderID:   target.ID,
					ProviderName: target.Name,
					SkillPath:    target.SkillFilePath,
					Action:       res.Action,
				})
			}

			for _, mcpTarget := range chosenMCPs {
				mRes, mErr := skill.UninstallMCPFromProvider(mcpTarget)
				if mErr != nil {
					return mErr
				}
				if mRes.Action == "removed" {
					out.RemovedMCPCount++
				} else {
					out.NotFoundMCPCount++
				}
				out.MCPItems = append(out.MCPItems, MCPUninstallItemView{
					ProviderID:   mcpTarget.ID,
					ProviderName: mcpTarget.Name,
					ConfigPath:   mRes.ConfigPath,
					Action:       mRes.Action,
				})
			}

			return renderPayload(cmd.OutOrStdout(), formatter, opts, out)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceVarP(&providers, "providers", "p", nil, "Comma-separated Skill provider IDs to remove (agents, claude, codex, antigravity, gemini, cursor, all)")
	flags.StringSliceVar(&mcpProviders, "mcp-providers", nil, "Comma-separated MCP provider IDs to remove (agents, claude, codex, antigravity, gemini, cursor, all)")
	flags.BoolVar(&all, "all", false, "Check and remove Flowsk SKILL.md and MCP server across all supported providers")
	flags.BoolVarP(&assumeYes, "yes", "y", false, "Confirm removal without launching interactive prompts")
	flags.StringVar(&homeDir, "home", "", "Override user home directory for global skill and MCP paths")
	_ = flags.MarkHidden("home")
	return cmd
}

func renderHumanUninstallSkill(w io.Writer, v UninstallSkillView) error {
	header := tuiActiveCancelBtnStyle.Render("FLOWSK UNINSTALLED • SKILL + MCP")

	var b strings.Builder
	b.WriteString(header + "\n\n")

	if len(v.SkillItems) == 0 && len(v.MCPItems) == 0 {
		b.WriteString(tuiInfoStyle.Render("• CLEAN") + "  No Flowsk SKILL.md or MCP server entries were installed.\n")
		_, err := io.WriteString(w, tuiCardStyle.Render(strings.TrimRight(b.String(), "\n"))+"\n")
		return err
	}

	if len(v.SkillItems) > 0 {
		fmt.Fprintf(&b, "Skill Removal: %d removed • %d already absent\n", v.RemovedSkillCount, v.NotFoundSkillCount)
		for _, it := range v.SkillItems {
			fmt.Fprintf(&b, "  %s  %-28s  %s\n", renderUninstallActionBadge(it.Action), it.ProviderName, tuiMutedStyle.Render(it.SkillPath))
		}
	}

	if len(v.MCPItems) > 0 {
		if len(v.SkillItems) > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "MCP Removal:   %d removed • %d already absent\n", v.RemovedMCPCount, v.NotFoundMCPCount)
		for _, m := range v.MCPItems {
			fmt.Fprintf(&b, "  %s  %-28s  %s\n", renderUninstallActionBadge(m.Action), m.ProviderName, tuiMutedStyle.Render(m.ConfigPath))
		}
	}

	_, err := io.WriteString(w, tuiCardStyle.Render(strings.TrimRight(b.String(), "\n"))+"\n")
	return err
}

func renderUninstallActionBadge(action string) string {
	if action == "removed" {
		return tuiOKStyle.Render("✓ REMOVED  ")
	}
	return tuiMutedStyle.Render("· ABSENT   ")
}
