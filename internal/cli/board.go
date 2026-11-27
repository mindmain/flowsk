// File board.go implements the root `flowsk board` (aliases: `status`, `dash`)
// command. When executed inside a bound project directory (or with `--project`),
// it renders that project's full state summary, Git worktrees/commits, separate
// child sub-projects, strategic goals, and workflow columns. When executed
// outside any bound directory (or with `--all` / `-A`), it renders a global
// multi-project overview with per-state task counts and Git worktree commits.
package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

type boardCommandDeps interface {
	ProjectDependency
	ObjectiveDependency
	TaskDependency
	LocalPathDependency
	FormatterDependency
}

type boardHandler struct {
	opts       *GlobalOptions
	projects   ProjectClient
	objectives ObjectiveClient
	tasks      TaskClient
	localPaths LocalPathRegistry
	formatter  OutputFormatter
}

// newBoardCommand constructs `flowsk board` (aliases: `status`, `dash`) with
// immediate constructor dependency resolution.
func newBoardCommand(opts *GlobalOptions, deps boardCommandDeps) *cobra.Command {
	var (
		projects   ProjectClient
		objectives ObjectiveClient
		tasks      TaskClient
		localPaths LocalPathRegistry
		formatter  OutputFormatter
	)
	if deps != nil {
		projects = deps.ProjectClient()
		objectives = deps.ObjectiveClient()
		tasks = deps.TaskClient()
		localPaths = deps.LocalPathRegistry()
		formatter = deps.OutputFormatter()
	}

	h := &boardHandler{
		opts:       opts,
		projects:   projects,
		objectives: objectives,
		tasks:      tasks,
		localPaths: localPaths,
		formatter:  formatter,
	}

	var (
		allProjects      bool
		includeCompleted bool
		onlyDone         bool
		onlyInProgress   bool
		onlyTodo         bool
		onlyBlocked      bool
		searchQuery      string
		objectiveFilter  string
		maxTasksPerCol   int
	)

	cmd := &cobra.Command{
		Use:     "board",
		Aliases: []string{"status", "dash"},
		Short:   "Display project task counts, Git worktrees/commits, sub-projects, goals, and tasks (or global overview outside a project)",
		Long: `Renders a terminal status overview:
  - Outside any registered Flowsk directory (or with -A/--all): displays a Global
    Multi-Project Overview across all active projects and nested sub-projects with
    separate per-state task counts (Total, Backlog, Planned, Progress, Review,
    Blocked, Done, Canceled), goal progress, and grouped local Git worktree commits.
  - Inside a bound project directory (or with --project): displays that project's
    full state counters, local Git worktrees & latest commits, child sub-projects
    (with separate counts and subpath commits), active goals, and workflow columns.`,
		Example: `  # Global multi-project status from anywhere (or explicit --all)
  flowsk status
  flowsk status --all

  # Full board for the project (or sub-project) bound to the current folder
  flowsk board

  # Include completed tasks in workflow columns
  flowsk status --include-completed`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("board context canceled: %w", err)
			}
			if h.projects == nil || h.objectives == nil || h.tasks == nil {
				return ErrNotImplemented
			}
			if maxTasksPerCol <= 0 {
				return fmt.Errorf("--limit-per-state must be > 0, got %d", maxTasksPerCol)
			}

			ws, projRef := resolveOptionalProject(ctx, h.opts, h.localPaths)
			if allProjects || projRef == "" {
				overview, err := h.buildWorkspaceOverview(ctx, ws)
				if err != nil {
					return err
				}
				return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, overview)
			}

			projView, err := h.projects.GetProject(ctx, ws, projRef)
			if err != nil {
				return err
			}
			states, err := h.projects.ListStates(ctx, ws, projRef)
			if err != nil {
				return err
			}
			objRes, err := h.objectives.ListObjectives(ctx, ListObjectivesInput{
				Workspace: ws,
				Project:   projView.Key,
				Limit:     200,
			})
			if err != nil {
				return err
			}

			// Fetch unfiltered project tasks first for accurate project-wide state counts.
			allTasksRes, err := h.tasks.ListTasks(ctx, ListTasksInput{
				Workspace:       ws,
				Project:         projView.Key,
				IncludeArchived: true,
				Priority:        -1,
				Limit:           500,
			})
			if err != nil {
				return err
			}
			fullCounts := computeProjectStateCounts(allTasksRes.Items)

			effState := ""
			effShowCompleted := includeCompleted
			switch {
			case onlyDone:
				effState = "done"
				effShowCompleted = true
			case onlyInProgress:
				effState = "in_progress"
			case onlyTodo:
				effState = "todo"
			}
			if strings.Contains(strings.ToLower(searchQuery), "state:done") || strings.Contains(strings.ToLower(searchQuery), "is:done") {
				effShowCompleted = true
			}

			var visibleTasks []TaskSummaryView
			for _, it := range allTasksRes.Items {
				if strings.EqualFold(it.Category, "archived") || strings.EqualFold(it.State, "archived") {
					continue
				}
				visibleTasks = append(visibleTasks, it)
			}
			if effState != "" || onlyBlocked || strings.TrimSpace(searchQuery) != "" || strings.TrimSpace(objectiveFilter) != "" {
				filteredRes, fErr := h.tasks.ListTasks(ctx, ListTasksInput{
					Workspace: ws,
					Project:   projView.Key,
					Objective: strings.TrimSpace(objectiveFilter),
					State:     effState,
					Blocked:   onlyBlocked,
					Query:     strings.TrimSpace(searchQuery),
					Priority:  -1,
					Limit:     500,
				})
				if fErr != nil {
					return fErr
				}
				visibleTasks = filteredRes.Items
			}

			board := assembleBoardView(projView, states, objRes.Items, visibleTasks, effShowCompleted, strings.TrimSpace(objectiveFilter), maxTasksPerCol)
			board.StateCounts = fullCounts
			if orphanRep, oErr := h.tasks.InspectOrphans(ctx, ws); oErr == nil {
				board.OrphanIssuesCount = orphanRep.TotalIssues
			}

			// Enrich with client-local Git worktrees & child/parent sub-project bindings.
			var allLocal []LocalProjectBinding
			if h.localPaths != nil {
				allLocal, _ = h.localPaths.ListByProject(ctx, ws, "")
			}
			allBindings, _ := h.projects.ListProjectBindings(ctx, ListProjectBindingsInput{Workspace: ws})
			var parentKeys []string
			scopeSubpath := ""
			for _, b := range allBindings {
				if strings.EqualFold(b.ChildKey, projView.Key) || b.ChildProjectID == projView.ID {
					if b.ParentKey != "" {
						parentKeys = append(parentKeys, b.ParentKey)
					}
					if scopeSubpath == "" && b.ScopeSubpath != "" {
						scopeSubpath = b.ScopeSubpath
					}
				}
			}
			board.ParentKeys = parentKeys
			board.ScopeSubpath = scopeSubpath
			board.Worktrees = collectProjectWorktrees(ctx, projView.Key, scopeSubpath, parentKeys, allLocal)

			// Populate separate child sub-project status summaries if any exist.
			var childSummaries []ProjectStatusSummaryView
			for _, b := range allBindings {
				if !strings.EqualFold(b.ParentKey, projView.Key) && b.ParentProjectID != projView.ID {
					continue
				}
				childRef := firstNonEmptyCLI(b.ChildKey, b.ChildProjectID)
				childProj, cErr := h.projects.GetProject(ctx, ws, childRef)
				if cErr != nil || childProj.Status == "archived" {
					continue
				}
				sum, sErr := h.buildSingleProjectSummary(ctx, ws, childProj, b.ScopeSubpath, 1, []string{projView.Key}, allLocal)
				if sErr == nil {
					childSummaries = append(childSummaries, sum)
				}
			}
			board.Subprojects = childSummaries

			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, board)
		},
	}

	cmd.Flags().BoolVarP(&allProjects, "all", "A", false, "Display global multi-project overview across all projects and sub-projects")
	cmd.Flags().BoolVar(&includeCompleted, "include-completed", false, "Include completed/canceled tasks and achieved goals in the terminal board")
	cmd.Flags().BoolVar(&onlyDone, "done", false, "Show only completed (Done) tasks on the board")
	cmd.Flags().BoolVar(&onlyInProgress, "in-progress", false, "Show only In Progress tasks on the board")
	cmd.Flags().BoolVar(&onlyTodo, "todo", false, "Show only Todo/Ready tasks on the board")
	cmd.Flags().BoolVar(&onlyBlocked, "blocked", false, "Show only blocked tasks on the board")
	cmd.Flags().StringVarP(&searchQuery, "search", "q", "", "Filter board tasks using GitHub-style query (e.g. 'state:done #storage sqlite')")
	cmd.Flags().StringVar(&objectiveFilter, "objective", "", "Optional goal/objective UUID or slug to focus the board")
	cmd.Flags().IntVar(&maxTasksPerCol, "limit-per-state", 15, "Maximum tasks displayed per workflow column before summarizing")

	return cmd
}

func (h *boardHandler) buildWorkspaceOverview(ctx context.Context, ws string) (WorkspaceOverviewView, error) {
	projRes, err := h.projects.ListProjects(ctx, ListProjectsInput{
		Workspace:       ws,
		IncludeArchived: false,
		Limit:           200,
	})
	if err != nil {
		return WorkspaceOverviewView{}, err
	}
	var allLocal []LocalProjectBinding
	if h.localPaths != nil {
		allLocal, _ = h.localPaths.ListByProject(ctx, ws, "")
	}
	bindings, _ := h.projects.ListProjectBindings(ctx, ListProjectBindingsInput{Workspace: ws})

	projByID := make(map[string]ProjectView, len(projRes.Items))
	projByKey := make(map[string]ProjectView, len(projRes.Items))
	for _, p := range projRes.Items {
		projByID[p.ID] = p
		projByKey[strings.ToUpper(p.Key)] = p
	}

	type edgeInfo struct {
		childKey     string
		scopeSubpath string
	}
	childrenByParentKey := make(map[string][]edgeInfo)
	parentsByChildKey := make(map[string][]string)
	subpathByChildKey := make(map[string]string)

	for _, b := range bindings {
		pProj, okP := projByID[b.ParentProjectID]
		if !okP && b.ParentKey != "" {
			pProj, okP = projByKey[strings.ToUpper(b.ParentKey)]
		}
		cProj, okC := projByID[b.ChildProjectID]
		if !okC && b.ChildKey != "" {
			cProj, okC = projByKey[strings.ToUpper(b.ChildKey)]
		}
		// Only link active projects; if a parent was archived/deleted, child stays a root!
		if !okP || !okC {
			continue
		}
		pKey := strings.ToUpper(pProj.Key)
		cKey := strings.ToUpper(cProj.Key)
		childrenByParentKey[pKey] = append(childrenByParentKey[pKey], edgeInfo{
			childKey:     cKey,
			scopeSubpath: b.ScopeSubpath,
		})
		parentsByChildKey[cKey] = append(parentsByChildKey[cKey], pKey)
		if subpathByChildKey[cKey] == "" && b.ScopeSubpath != "" {
			subpathByChildKey[cKey] = b.ScopeSubpath
		}
	}

	baseSummaries := make(map[string]ProjectStatusSummaryView, len(projRes.Items))
	var workspaceTotals ProjectStateCountsView
	for _, p := range projRes.Items {
		k := strings.ToUpper(p.Key)
		sum, sErr := h.buildSingleProjectSummary(ctx, ws, p, subpathByChildKey[k], 0, parentsByChildKey[k], allLocal)
		if sErr != nil {
			return WorkspaceOverviewView{}, sErr
		}
		baseSummaries[k] = sum
		workspaceTotals = addStateCounts(workspaceTotals, sum.Counts)
	}

	var buildNode func(key, scopeSub string, depth int, trail map[string]bool) ProjectStatusSummaryView
	buildNode = func(key, scopeSub string, depth int, trail map[string]bool) ProjectStatusSummaryView {
		node := baseSummaries[key]
		node.Depth = depth
		if scopeSub != "" {
			node.ScopeSubpath = scopeSub
			node.Worktrees = collectProjectWorktrees(ctx, node.Project.Key, scopeSub, parentsByChildKey[key], allLocal)
		}
		edges := childrenByParentKey[key]
		slices.SortFunc(edges, func(a, b edgeInfo) int {
			return strings.Compare(a.childKey, b.childKey)
		})
		for _, edge := range edges {
			if trail[edge.childKey] {
				continue
			}
			nextTrail := make(map[string]bool, len(trail)+1)
			for tk, tv := range trail {
				nextTrail[tk] = tv
			}
			nextTrail[edge.childKey] = true
			node.Children = append(node.Children, buildNode(edge.childKey, edge.scopeSubpath, depth+1, nextTrail))
		}
		return node
	}

	var roots []ProjectStatusSummaryView
	for _, p := range projRes.Items {
		k := strings.ToUpper(p.Key)
		if len(parentsByChildKey[k]) > 0 {
			continue
		}
		roots = append(roots, buildNode(k, "", 0, map[string]bool{k: true}))
	}
	slices.SortFunc(roots, func(a, b ProjectStatusSummaryView) int {
		return strings.Compare(a.Project.Key, b.Project.Key)
	})

	orphanIssues := 0
	if orphanRep, oErr := h.tasks.InspectOrphans(ctx, ws); oErr == nil {
		orphanIssues = orphanRep.TotalIssues
	}

	return WorkspaceOverviewView{
		Workspace:         ws,
		TotalProjects:     len(projRes.Items),
		TotalSubprojects:  len(parentsByChildKey),
		Counts:            workspaceTotals,
		Projects:          roots,
		OrphanIssuesCount: orphanIssues,
	}, nil
}

func (h *boardHandler) buildSingleProjectSummary(
	ctx context.Context,
	ws string,
	proj ProjectView,
	scopeSubpath string,
	depth int,
	parentKeys []string,
	allLocal []LocalProjectBinding,
) (ProjectStatusSummaryView, error) {
	tRes, err := h.tasks.ListTasks(ctx, ListTasksInput{
		Workspace:       ws,
		Project:         proj.Key,
		IncludeArchived: true,
		Priority:        -1,
		Limit:           500,
	})
	if err != nil {
		return ProjectStatusSummaryView{}, err
	}
	oRes, err := h.objectives.ListObjectives(ctx, ListObjectivesInput{
		Workspace: ws,
		Project:   proj.Key,
		Limit:     200,
	})
	if err != nil {
		return ProjectStatusSummaryView{}, err
	}
	var goalsDone, goalsTotal int
	for _, o := range oRes.Items {
		if strings.EqualFold(o.Status, "canceled") || strings.EqualFold(o.Status, "dropped") {
			continue
		}
		goalsTotal++
		if isTerminalObjectiveStatus(o.Status) {
			goalsDone++
		}
	}
	return ProjectStatusSummaryView{
		Project:      proj,
		ScopeSubpath: scopeSubpath,
		Depth:        depth,
		GoalsDone:    goalsDone,
		GoalsTotal:   goalsTotal,
		Counts:       computeProjectStateCounts(tRes.Items),
		Worktrees:    collectProjectWorktrees(ctx, proj.Key, scopeSubpath, parentKeys, allLocal),
	}, nil
}

func computeProjectStateCounts(items []TaskSummaryView) ProjectStateCountsView {
	var c ProjectStateCountsView
	for _, t := range items {
		c.Total++
		if t.Holder != "" {
			c.ActiveClaims++
		}
		cat := strings.ToLower(strings.TrimSpace(t.Category))
		st := strings.ToLower(strings.TrimSpace(t.State))
		if cat == "archived" || st == "archived" {
			c.Total--
			c.Archived++
			continue
		}
		switch {
		case cat == "done" || st == "done" || st == "completed":
			c.Done++
		case cat == "canceled" || cat == "cancelled" || st == "canceled" || st == "cancelled":
			c.Canceled++
		case cat == "blocked" || st == "blocked":
			c.Blocked++
		case cat == "backlog" || cat == "triage" || st == "backlog" || st == "triage":
			c.Backlog++
		case cat == "in_progress" || st == "in progress" || st == "in_progress":
			c.InProgress++
		case cat == "review" || cat == "in_review" || st == "in review" || st == "review":
			c.InReview++
		default:
			c.Planned++
		}
	}
	return c
}

func addStateCounts(a, b ProjectStateCountsView) ProjectStateCountsView {
	return ProjectStateCountsView{
		Total:        a.Total + b.Total,
		Backlog:      a.Backlog + b.Backlog,
		Planned:      a.Planned + b.Planned,
		InProgress:   a.InProgress + b.InProgress,
		InReview:     a.InReview + b.InReview,
		Blocked:      a.Blocked + b.Blocked,
		Done:         a.Done + b.Done,
		Canceled:     a.Canceled + b.Canceled,
		Archived:     a.Archived + b.Archived,
		ActiveClaims: a.ActiveClaims + b.ActiveClaims,
	}
}

func assembleBoardView(
	proj ProjectView,
	states []WorkflowStateView,
	objectives []ObjectiveView,
	tasks []TaskSummaryView,
	includeCompleted bool,
	objectiveFilter string,
	maxPerCol int,
) BoardView {
	var filteredObjs []ObjectiveView
	for _, o := range objectives {
		if objectiveFilter != "" && !strings.EqualFold(o.Key, objectiveFilter) && o.ID != objectiveFilter {
			continue
		}
		if !includeCompleted && isTerminalObjectiveStatus(o.Status) {
			continue
		}
		filteredObjs = append(filteredObjs, o)
	}

	slices.SortFunc(states, func(a, b WorkflowStateView) int {
		return a.Position - b.Position
	})

	colIndex := make(map[string]int)
	var cols []BoardColumnView
	for _, st := range states {
		if strings.EqualFold(st.Category, "archived") || strings.EqualFold(st.Name, "archived") {
			continue
		}
		if !includeCompleted && isTerminalTaskCategory(st.Category, st.Name) {
			continue
		}
		colIndex[strings.ToLower(st.Name)] = len(cols)
		cols = append(cols, BoardColumnView{State: st.Name, Category: st.Category})
	}

	var activeClaims, blockedCount, totalTasks int
	for _, t := range tasks {
		if !includeCompleted && isTerminalTaskCategory(t.Category, t.State) {
			continue
		}
		totalTasks++
		if t.Holder != "" {
			activeClaims++
		}
		if t.Blocked {
			blockedCount++
		}
		key := strings.ToLower(t.State)
		idx, ok := colIndex[key]
		if !ok {
			idx = len(cols)
			colIndex[key] = idx
			cols = append(cols, BoardColumnView{State: t.State, Category: t.Category})
		}
		cols[idx].TotalCount++
		if len(cols[idx].Tasks) < maxPerCol {
			cols[idx].Tasks = append(cols[idx].Tasks, t)
		} else {
			cols[idx].OmittedCount++
		}
	}

	return BoardView{
		Project:      proj,
		Objectives:   filteredObjs,
		Columns:      cols,
		ActiveClaims: activeClaims,
		BlockedCount: blockedCount,
		TotalTasks:   totalTasks,
	}
}

func isTerminalObjectiveStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "achieved", "canceled", "cancelled", "dropped", "superseded":
		return true
	default:
		return false
	}
}

func isTerminalTaskCategory(category, state string) bool {
	for _, v := range []string{category, state} {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "done", "canceled", "cancelled", "archived":
			return true
		}
	}
	return false
}
