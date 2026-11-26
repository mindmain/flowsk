// File install.go implements `flowsk install` with an interactive 3-stage Lip Gloss +
// Bubble Tea TUI (Skill targets -> MCP server targets -> Review & Confirm) that installs
// both the embedded Flowsk `SKILL.md` and the `flowsk` stdio MCP server together,
// defaulting to Universal (`~/.agents`) for Skill and Universal (all detected clients) for MCP.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
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
	installStageSkill installStage = iota
	installStageMCP
	installStageConfirm
)

// InstallTUIModel implements `tea.Model` for the 3-stage interactive Skill + MCP
// selector and confirmation wizard in `flowsk install`.
type InstallTUIModel struct {
	statuses      []skill.ProviderStatus
	mcpStatuses   []skill.ProviderStatus
	selectedSkill map[string]bool
	universalMCP  bool
	selectedMCP   map[string]bool
	skillCursor   int
	mcpCursor     int
	stage         installStage
	confirmFocus  int // 0 = Confirm & Install, 1 = Back, 2 = Cancel
	validation    string
	confirmed     bool
	canceled      bool
}

// NewInstallTUIModel constructs a 3-stage Bubble Tea model (Skill -> MCP -> Confirm)
// defaulting to Universal (`~/.agents`) for Skill and Universal (all detected clients) for MCP.
func NewInstallTUIModel(statuses []skill.ProviderStatus, preselectedSkillIDs []string) *InstallTUIModel {
	return NewInstallTUIModelWithMCP(statuses, preselectedSkillIDs, nil)
}

// NewInstallTUIModelWithMCP constructs a 3-stage Bubble Tea model with optional
// pre-selected Skill provider IDs and MCP provider IDs (including `"universal"`).
func NewInstallTUIModelWithMCP(statuses []skill.ProviderStatus, preselectedSkillIDs, preselectedMCPIDs []string) *InstallTUIModel {
	selectedSkill := make(map[string]bool, len(statuses))
	if len(preselectedSkillIDs) > 0 {
		for _, id := range preselectedSkillIDs {
			selectedSkill[strings.ToLower(strings.TrimSpace(id))] = true
		}
	} else {
		// Default: ONLY Universal Agents (~/.agents) is pre-selected; the user opts into extras.
		selectedSkill[skill.ProviderAgents] = true
	}

	var mcpStatuses []skill.ProviderStatus
	for _, st := range statuses {
		if st.Target.SupportsMCP() {
			mcpStatuses = append(mcpStatuses, st)
		}
	}

	selectedMCP := make(map[string]bool, len(mcpStatuses))
	universalMCP := false
	if len(preselectedMCPIDs) > 0 {
		for _, raw := range preselectedMCPIDs {
			token := strings.ToLower(strings.TrimSpace(raw))
			switch token {
			case skill.MCPUniversalID, "detected":
				universalMCP = true
			case "all":
				for _, st := range mcpStatuses {
					selectedMCP[st.Target.ID] = true
				}
			default:
				if token != "" {
					selectedMCP[token] = true
				}
			}
		}
		if universalMCP {
			clear(selectedMCP)
		}
	} else {
		// Default: Universal MCP option (all detected MCP-capable clients) is pre-selected.
		universalMCP = true
	}

	return &InstallTUIModel{
		statuses:      statuses,
		mcpStatuses:   mcpStatuses,
		selectedSkill: selectedSkill,
		universalMCP:  universalMCP,
		selectedMCP:   selectedMCP,
		stage:         installStageSkill,
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

// UniversalMCPSelected reports whether the "Universal (all detected clients)" MCP option is active.
func (m *InstallTUIModel) UniversalMCPSelected() bool {
	return m != nil && m.universalMCP
}

// SelectedTargets returns the ordered slice of Skill ProviderTargets chosen by the user.
func (m *InstallTUIModel) SelectedTargets() []skill.ProviderTarget {
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

// SelectedMCPTargets returns the ordered slice of MCP-capable ProviderTargets chosen by
// the user (resolving the Universal bundle to all detected MCP clients when active).
func (m *InstallTUIModel) SelectedMCPTargets() []skill.ProviderTarget {
	if m == nil {
		return nil
	}
	if m.universalMCP {
		return skill.ResolveUniversalMCPTargets(m.statuses)
	}
	var out []skill.ProviderTarget
	for _, st := range m.mcpStatuses {
		if m.selectedMCP[st.Target.ID] {
			out = append(out, st.Target)
		}
	}
	return out
}

// Update handles keyboard navigation across the Skill step, MCP step, and Confirmation dialog.
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
			if len(m.SelectedTargets()) == 0 {
				m.validation = "Select at least one Skill target with Space (or press q to cancel)."
				return m, nil
			}
			m.validation = ""
			m.stage = installStageMCP
		}

	case installStageMCP:
		totalRows := 1 + len(m.mcpStatuses) // Row 0 = Universal, Rows 1..N = individual MCP clients
		switch key {
		case "q", "esc":
			m.canceled = true
			m.confirmed = false
			return m, tea.Quit
		case "b", "backspace":
			m.validation = ""
			m.stage = installStageSkill
		case "up", "k":
			if totalRows > 0 {
				m.mcpCursor = (m.mcpCursor - 1 + totalRows) % totalRows
			}
		case "down", "j":
			if totalRows > 0 {
				m.mcpCursor = (m.mcpCursor + 1) % totalRows
			}
		case " ", "x":
			m.validation = ""
			if m.mcpCursor == 0 {
				m.universalMCP = !m.universalMCP
				if m.universalMCP {
					clear(m.selectedMCP)
				}
			} else if idx := m.mcpCursor - 1; idx >= 0 && idx < len(m.mcpStatuses) {
				id := m.mcpStatuses[idx].Target.ID
				if m.universalMCP {
					// Switching from Universal bundle to explicit per-client choice.
					m.universalMCP = false
					clear(m.selectedMCP)
					m.selectedMCP[id] = true
				} else {
					m.selectedMCP[id] = !m.selectedMCP[id]
				}
			}
		case "a":
			m.validation = ""
			allIndividual := !m.universalMCP && len(m.SelectedMCPTargets()) == len(m.mcpStatuses)
			m.universalMCP = false
			for _, st := range m.mcpStatuses {
				m.selectedMCP[st.Target.ID] = !allIndividual
			}
		case "enter":
			if len(m.SelectedMCPTargets()) == 0 {
				m.validation = "Select Universal or at least one MCP client with Space (Skill and MCP install together)."
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

// View renders Step 1 (Skill), Step 2 (MCP), or Step 3 (Review & Confirm).
func (m *InstallTUIModel) View() string {
	shortHash := skill.ShortHash(skill.EmbeddedHash())

	switch m.stage {
	case installStageSkill:
		return m.renderSkillStage(shortHash)
	case installStageMCP:
		return m.renderMCPStage()
	default:
		return m.renderConfirmStage(shortHash)
	}
}

func (m *InstallTUIModel) renderSkillStage(shortHash string) string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK INSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 1/3 • Skill Targets (sha256:"+shortHash+")"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")
	body.WriteString("Select where the Flowsk SKILL.md should be installed (Universal is selected by default):\n\n")

	for i, st := range m.statuses {
		cursor := "  "
		name := st.Target.Name
		if i == m.skillCursor {
			cursor = tuiCursorStyle.Render("▸ ")
			name = tuiCursorStyle.Render(name)
		}
		check := tuiMutedStyle.Render("[ ]")
		if m.selectedSkill[st.Target.ID] {
			check = tuiOKStyle.Render("[✓]")
		}
		badge := formatSkillTUIBadge(st)
		fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, name, badge)
		fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render(st.Target.Description))
	}

	if m.validation != "" {
		body.WriteString("\n" + tuiWarnStyle.Render("⚠ "+m.validation) + "\n")
	}
	body.WriteString("\n" + tuiSubtitleStyle.Render("↑/↓ navigate • space toggle • a all/none • enter next (MCP) • q cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func (m *InstallTUIModel) renderMCPStage() string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK INSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 2/3 • Flowsk MCP Server (stdio)"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")
	body.WriteString("Select where the Flowsk MCP server should be registered (Universal is selected by default):\n\n")

	universalResolved := skill.ResolveUniversalMCPTargets(m.statuses)
	universalIDs := make(map[string]bool, len(universalResolved))
	universalNames := make([]string, 0, len(universalResolved))
	for _, t := range universalResolved {
		universalIDs[t.ID] = true
		universalNames = append(universalNames, t.Name)
	}

	// Row 0: Universal MCP option.
	{
		cursor := "  "
		label := "Universal (All Detected Clients)"
		if m.mcpCursor == 0 {
			cursor = tuiCursorStyle.Render("▸ ")
			label = tuiCursorStyle.Render(label)
		}
		check := tuiMutedStyle.Render("[ ]")
		if m.universalMCP {
			check = tuiOKStyle.Render("[✓]")
		}
		badge := tuiInfoStyle.Render(fmt.Sprintf("• %d client(s)", len(universalResolved)))
		fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, label, badge)
		fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render("Covers: "+strings.Join(universalNames, ", ")))
	}

	// Rows 1..N: Individual MCP-capable clients.
	for i, st := range m.mcpStatuses {
		rowIdx := i + 1
		cursor := "  "
		name := st.Target.Name
		if m.mcpCursor == rowIdx {
			cursor = tuiCursorStyle.Render("▸ ")
			name = tuiCursorStyle.Render(name)
		}

		check := tuiMutedStyle.Render("[ ]")
		switch {
		case m.universalMCP && universalIDs[st.Target.ID]:
			check = tuiInfoStyle.Render("[•]")
		case !m.universalMCP && m.selectedMCP[st.Target.ID]:
			check = tuiOKStyle.Render("[✓]")
		}

		badge := formatMCPTUIBadge(st, m.universalMCP && universalIDs[st.Target.ID])
		fmt.Fprintf(&body, "%s%s %-30s %s\n", cursor, check, name, badge)
		fmt.Fprintf(&body, "      %s\n", tuiMutedStyle.Render(shortenHomePath(st.Target.MCPConfigPath)))
	}

	if m.validation != "" {
		body.WriteString("\n" + tuiWarnStyle.Render("⚠ "+m.validation) + "\n")
	}
	body.WriteString("\n" + tuiSubtitleStyle.Render("↑/↓ navigate • space toggle • a all/none • b back (Skill) • enter review • q cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func (m *InstallTUIModel) renderConfirmStage(shortHash string) string {
	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		tuiTitleBadgeStyle.Render("FLOWSK INSTALLER"),
		"  ",
		tuiSubtitleStyle.Render("Step 3/3 • Confirm Skill + MCP"),
	)

	var body strings.Builder
	body.WriteString(header + "\n\n")

	skillTargets := m.SelectedTargets()
	mcpTargets := m.SelectedMCPTargets()

	fmt.Fprintf(&body, "1. Global Skill (SKILL.md sha256:%s) → %d target(s):\n", shortHash, len(skillTargets))
	for _, t := range skillTargets {
		fmt.Fprintf(&body, "   %s %-28s %s\n",
			tuiOKStyle.Render("•"),
			t.Name,
			tuiMutedStyle.Render(t.SkillFilePath),
		)
	}

	mcpModeLabel := ""
	if m.universalMCP {
		mcpModeLabel = " [Universal]"
	}
	fmt.Fprintf(&body, "\n2. Flowsk MCP Server (stdio)%s → %d client config(s):\n", mcpModeLabel, len(mcpTargets))
	for _, t := range mcpTargets {
		fmt.Fprintf(&body, "   %s %-28s %s\n",
			tuiInfoStyle.Render("•"),
			t.Name,
			tuiMutedStyle.Render(t.MCPConfigPath),
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
	body.WriteString("\n" + tuiSubtitleStyle.Render("←/→ select • enter confirm • y install now • b back (MCP) • n/esc cancel"))
	return tuiCardStyle.Render(body.String()) + "\n"
}

func formatSkillTUIBadge(st skill.ProviderStatus) string {
	switch {
	case st.Installed && st.UpToDate:
		return tuiOKStyle.Render("✓ up-to-date (" + skill.ShortHash(st.InstalledHash) + ")")
	case st.Installed && !st.UpToDate:
		return tuiWarnStyle.Render("↻ update (" + skill.ShortHash(st.InstalledHash) + " → " + skill.ShortHash(st.ExpectedHash) + ")")
	case st.Target.ID == skill.ProviderAgents:
		return tuiInfoStyle.Render("★ default universal skill")
	case st.Detected:
		return tuiInfoStyle.Render("• detected on machine")
	default:
		return tuiMutedStyle.Render("· not detected")
	}
}

func formatMCPTUIBadge(st skill.ProviderStatus, viaUniversal bool) string {
	suffix := ""
	if viaUniversal {
		suffix = " " + tuiInfoStyle.Render("(via Universal)")
	}
	switch {
	case st.MCPConfigured:
		return tuiOKStyle.Render("✓ mcp configured") + suffix
	case st.Detected:
		return tuiInfoStyle.Render("• detected on machine") + suffix
	default:
		return tuiMutedStyle.Render("· not detected") + suffix
	}
}

func shortenHomePath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// newInstallCommand constructs `flowsk install`.
func newInstallCommand(opts *GlobalOptions, deps FormatterDependency) *cobra.Command {
	var formatter OutputFormatter
	if deps != nil {
		formatter = deps.OutputFormatter()
	}

	var (
		providers    []string
		mcpProviders []string
		mcpCommand   string
		all          bool
		assumeYes    bool
		homeDir      string
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Interactively install both the embedded Flowsk SKILL.md and Flowsk MCP server",
		Long: `Installs or updates BOTH the embedded Flowsk operational skill ('SKILL.md', verified
by SHA-256 hash) AND the Flowsk stdio MCP server ('flowsk mcp serve --transport stdio')
together across your AI coding agents.

Default selections:
  1. Skill: 'Universal Agents (~/.agents)' (~/.agents/skills/flowsk/SKILL.md) is pre-selected
     by default; you can optionally check additional agent-specific directories.
  2. MCP Server: 'Universal (All Detected Clients)' is pre-selected by default so every
     detected AI client config (~/.claude.json, ~/.codex/config.toml,
     ~/.gemini/antigravity/mcp_config.json, ~/.gemini/settings.json, ~/.cursor/mcp.json)
     receives 'flowsk' non-destructively, or you can pick individual clients.`,
		Example: `  # Launch interactive 3-stage TUI (Skill -> MCP -> Confirm) with Universal defaults
  flowsk install

  # Non-interactive install using Universal defaults (Universal skill + all detected MCP clients)
  flowsk install --yes

  # Explicit Skill and MCP targets for scripts or CI
  flowsk install --providers agents --mcp-providers codex,antigravity --yes`,
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

			preselectedSkill, preselectedMCP, err := resolveInstallPreselections(homeDir, statuses, all, providers, mcpProviders)
			if err != nil {
				return err
			}

			var (
				chosenSkills []skill.ProviderTarget
				chosenMCPs   []skill.ProviderTarget
			)

			if assumeYes {
				model := NewInstallTUIModelWithMCP(statuses, preselectedSkill, preselectedMCP)
				chosenSkills = model.SelectedTargets()
				chosenMCPs = model.SelectedMCPTargets()
			} else if isInteractiveTTY(cmd.InOrStdin(), cmd.ErrOrStderr()) && (opts == nil || opts.Format == "" || opts.Format == FormatAuto || opts.Format == FormatHuman) {
				model := NewInstallTUIModelWithMCP(statuses, preselectedSkill, preselectedMCP)
				prog := tea.NewProgram(model, tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.ErrOrStderr()))
				finalModel, tErr := prog.Run()
				if tErr != nil {
					return fmt.Errorf("interactive installer failed: %w", tErr)
				}
				doneModel, _ := finalModel.(*InstallTUIModel)
				if doneModel == nil || doneModel.Canceled() {
					return errors.New("installation canceled")
				}
				chosenSkills = doneModel.SelectedTargets()
				chosenMCPs = doneModel.SelectedMCPTargets()
			} else {
				model := NewInstallTUIModelWithMCP(statuses, preselectedSkill, preselectedMCP)
				chosenSkills = model.SelectedTargets()
				chosenMCPs = model.SelectedMCPTargets()
				if len(chosenSkills) == 0 || len(chosenMCPs) == 0 {
					return errors.New("both Skill and MCP targets are required (pass --yes or specify --providers / --mcp-providers)")
				}
				var skillNames, mcpNames []string
				for _, t := range chosenSkills {
					skillNames = append(skillNames, t.ID)
				}
				for _, t := range chosenMCPs {
					mcpNames = append(mcpNames, t.ID)
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Install Flowsk SKILL.md [%s] and MCP server [%s]? [y/N]: ",
					strings.Join(skillNames, ", "), strings.Join(mcpNames, ", "))
				ok, pErr := readYesConfirmation(cmd.InOrStdin())
				if pErr != nil {
					return pErr
				}
				if !ok {
					return errors.New("installation canceled (pass --yes to confirm non-interactively)")
				}
			}

			if len(chosenSkills) == 0 {
				return errors.New("no Skill providers selected")
			}
			if len(chosenMCPs) == 0 {
				return errors.New("no MCP client targets selected")
			}

			out := InstallSkillView{
				EmbeddedSHA256: skill.EmbeddedHash(),
				Items:          make([]SkillInstallItemView, 0, len(chosenSkills)),
				MCPItems:       make([]MCPInstallItemView, 0, len(chosenMCPs)),
			}

			// 1. Install Flowsk MCP server into selected client configuration files first
			// so SkillInstallItemView.MCPConfigured reflects the live post-install state.
			mcpConfiguredByID := make(map[string]bool, len(statuses))
			for _, st := range statuses {
				mcpConfiguredByID[st.Target.ID] = st.MCPConfigured
			}
			for _, mcpTarget := range chosenMCPs {
				mRes, mErr := skill.InstallMCPToProvider(mcpTarget, mcpCommand)
				if mErr != nil {
					return mErr
				}
				mcpConfiguredByID[mcpTarget.ID] = true
				switch mRes.Action {
				case "installed":
					out.MCPInstalledCount++
				case "updated":
					out.MCPUpdatedCount++
				case "unchanged":
					out.MCPUnchangedCount++
				}
				out.MCPItems = append(out.MCPItems, MCPInstallItemView{
					ProviderID:   mcpTarget.ID,
					ProviderName: mcpTarget.Name,
					ConfigPath:   mRes.ConfigPath,
					Command:      mRes.Command,
					Args:         mRes.Args,
					Action:       mRes.Action,
				})
			}

			// 2. Install embedded SKILL.md into selected skill directories.
			for _, target := range chosenSkills {
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
					MCPConfigured:  mcpConfiguredByID[target.ID],
					MCPConfigPath:  target.MCPConfigPath,
				})
			}

			return renderPayload(cmd.OutOrStdout(), formatter, opts, out)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceVarP(&providers, "providers", "p", nil, "Comma-separated Skill provider IDs (default: agents; supported: agents, claude, codex, antigravity, gemini, cursor, all)")
	flags.StringSliceVar(&mcpProviders, "mcp-providers", nil, "Comma-separated MCP provider IDs (default: universal; supported: universal, claude, codex, antigravity, gemini, cursor, all)")
	flags.StringVar(&mcpCommand, "mcp-command", "", "Override executable path written into MCP client configs (defaults to resolved flowsk binary path)")
	flags.BoolVar(&all, "all", false, "Install Flowsk SKILL.md and MCP server across all supported AI provider locations")
	flags.BoolVarP(&assumeYes, "yes", "y", false, "Confirm installation without launching interactive prompts")
	flags.StringVar(&homeDir, "home", "", "Override user home directory for global skill and MCP paths")
	_ = flags.MarkHidden("home")
	_ = flags.MarkHidden("mcp-command")
	return cmd
}

func resolveInstallPreselections(homeDir string, statuses []skill.ProviderStatus, all bool, providers, mcpProviders []string) ([]string, []string, error) {
	if all {
		var allSkills []string
		for _, st := range statuses {
			allSkills = append(allSkills, st.Target.ID)
		}
		return allSkills, []string{"all"}, nil
	}

	var preselectedSkill []string
	var resolvedSkillTargets []skill.ProviderTarget
	if len(providers) > 0 {
		resolved, err := skill.ResolveProvidersByIDs(homeDir, providers)
		if err != nil {
			return nil, nil, err
		}
		resolvedSkillTargets = resolved
		for _, t := range resolved {
			preselectedSkill = append(preselectedSkill, t.ID)
		}
	}

	var preselectedMCP []string
	if len(mcpProviders) > 0 {
		hasUniversal := false
		for _, raw := range mcpProviders {
			for _, part := range strings.Split(raw, ",") {
				tok := strings.ToLower(strings.TrimSpace(part))
				if tok == skill.MCPUniversalID || tok == "detected" {
					hasUniversal = true
				}
			}
		}
		resolvedMCP, err := skill.ResolveMCPProvidersByIDs(homeDir, mcpProviders)
		if err != nil {
			return nil, nil, err
		}
		if hasUniversal && len(mcpProviders) == 1 && !strings.Contains(mcpProviders[0], ",") {
			preselectedMCP = []string{skill.MCPUniversalID}
		} else {
			for _, t := range resolvedMCP {
				preselectedMCP = append(preselectedMCP, t.ID)
			}
		}
	} else if len(resolvedSkillTargets) > 0 {
		// When --providers was explicitly passed without --mcp-providers, mirror any
		// MCP-capable providers chosen; if only `agents` was passed, keep Universal MCP.
		for _, t := range resolvedSkillTargets {
			if t.SupportsMCP() {
				preselectedMCP = append(preselectedMCP, t.ID)
			}
		}
	}

	return preselectedSkill, preselectedMCP, nil
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
		tuiTitleBadgeStyle.Render("FLOWSK INSTALLED • SKILL + MCP"),
		"  ",
		tuiSubtitleStyle.Render("sha256:"+skill.ShortHash(v.EmbeddedSHA256)),
	)

	var b strings.Builder
	b.WriteString(header + "\n\n")
	fmt.Fprintf(&b, "Skill Summary: %d installed • %d updated • %d already up-to-date\n",
		v.InstalledCount, v.UpdatedCount, v.UnchangedCount)
	for _, it := range v.Items {
		fmt.Fprintf(&b, "  %s  %-28s  %s\n", renderInstallActionBadge(it.Action), it.ProviderName, tuiMutedStyle.Render(it.SkillPath))
	}

	if len(v.MCPItems) > 0 {
		fmt.Fprintf(&b, "\nMCP Summary:   %d installed • %d updated • %d already up-to-date\n",
			v.MCPInstalledCount, v.MCPUpdatedCount, v.MCPUnchangedCount)
		for _, m := range v.MCPItems {
			fmt.Fprintf(&b, "  %s  %-28s  %s\n", renderInstallActionBadge(m.Action), m.ProviderName, tuiMutedStyle.Render(m.ConfigPath))
		}
	}

	_, err := io.WriteString(w, tuiCardStyle.Render(strings.TrimRight(b.String(), "\n"))+"\n")
	return err
}

func renderInstallActionBadge(action string) string {
	switch action {
	case "installed":
		return tuiOKStyle.Render("✓ INSTALLED")
	case "updated":
		return tuiWarnStyle.Render("↻ UPDATED  ")
	default:
		return tuiInfoStyle.Render("= UNCHANGED")
	}
}
