import { isPlatformBrowser } from '@angular/common';
import { DestroyRef, PLATFORM_ID, computed, effect, inject } from '@angular/core';
import { tapResponse } from '@ngrx/operators';
import { patchState, signalStore, withComputed, withHooks, withMethods, withState } from '@ngrx/signals';
import { rxMethod } from '@ngrx/signals/rxjs-interop';
import { EMPTY, catchError, concatMap, debounceTime, forkJoin, of, pipe, switchMap } from 'rxjs';
import { SanitizedApiError } from '../http/flowsk-api.interceptor';
import {
  ActorModel,
  CANONICAL_STATUS_COLUMNS,
  LiveStreamEventModel,
  ObjectiveKind,
  ObjectiveModel,
  ObjectiveProgressView,
  ObjectiveStatus,
  OrphanResourcesReport,
  ProjectBindingModel,
  ProjectHierarchyNode,
  ProjectModel,
  ProjectTaskCounts,
  PruneResourcesResult,
  PurgeArchivedResult,
  ScopeTab,
  StreamConnectionState,
  TaskCommentModel,
  TaskKind,
  TaskModel,
  TaskPriority,
  TaskRelationModel,
  TaskStatus,
  ViewMode,
  WorkflowStateModel,
  WorkspaceModel,
  formatSignalAge,
  isTaskLeaseActive,
  isTaskSignalGlowing,
} from '../models/flowsk.models';
import { FlowskHeaderContextService } from '../services/flowsk-header-context.service';
import { FlowskRpcService } from '../services/flowsk-rpc.service';
import { FlowskStreamService } from '../services/flowsk-stream.service';

/**
 * Standard UI action wrapper supporting completion and error callbacks for modals and drawers.
 */
export interface UiActionPayload<P, R = void> {
  readonly payload: P;
  readonly onSuccess?: (result: R) => void;
  readonly onError?: (message: string) => void;
}

/**
 * Complete immutable state tree managed by FlowskStore.
 */
export interface FlowskState {
  readonly initialized: boolean;
  readonly loading: boolean;
  readonly saving: boolean;
  readonly authRequired: boolean;
  readonly authError: string | null;
  readonly errorBanner: string | null;
  readonly toastMessage: string | null;
  readonly darkMode: boolean;
  readonly nowMs: number;

  readonly workspaceId: string;
  readonly workspaceSlug: string;
  readonly actorId: string;
  readonly actorHandle: string;

  readonly streamStatus: StreamConnectionState;
  readonly liveStreamEnabled: boolean;
  readonly liveEvents: readonly LiveStreamEventModel[];

  readonly workspaces: readonly WorkspaceModel[];
  readonly actors: readonly ActorModel[];
  readonly projects: readonly ProjectModel[];
  readonly projectBindings: readonly ProjectBindingModel[];
  readonly openedFromBoundFolder: boolean;
  readonly activeProjectId: string | null;
  readonly workflowStates: readonly WorkflowStateModel[];
  readonly objectives: readonly ObjectiveModel[];
  readonly workspaceTasks: readonly TaskModel[];

  readonly viewMode: ViewMode;
  readonly scopeTab: ScopeTab;
  readonly searchQuery: string;
  readonly statusFilter: TaskStatus | 'all';
  readonly priorityFilter: TaskPriority | 'all';
  readonly objectiveFilterId: string | 'all';
  readonly includeCompletedOnBoard: boolean;
  readonly showArchivedProjects: boolean;

  readonly selectedTaskId: string | null;
  readonly selectedTaskRelations: readonly TaskRelationModel[];
  readonly selectedTaskComments: readonly TaskCommentModel[];
  readonly selectedTaskMarkdown: string;
  readonly selectedTaskMarkdownLoading: boolean;

  readonly activeProjectMarkdown: string;
  readonly activeProjectMarkdownLoading: boolean;
  readonly orphanReport: OrphanResourcesReport;
  readonly lastPruneResult: PruneResourcesResult | null;
}

const EMPTY_ORPHAN_REPORT: OrphanResourcesReport = {
  orphanTaskFilesCount: 0,
  missingTaskFilesCount: 0,
  orphanProjectFilesCount: 0,
  missingProjectFilesCount: 0,
  totalIssues: 0,
  issueSummaries: [],
};

const INITIAL_STATE: FlowskState = {
  initialized: false,
  loading: false,
  saving: false,
  authRequired: false,
  authError: null,
  errorBanner: null,
  toastMessage: null,
  darkMode: true,
  nowMs: Date.now(),

  workspaceId: '',
  workspaceSlug: 'default',
  actorId: '',
  actorHandle: 'web-operator',

  streamStatus: 'connecting',
  liveStreamEnabled: true,
  liveEvents: [],

  workspaces: [],
  actors: [],
  projects: [],
  projectBindings: [],
  openedFromBoundFolder: false,
  activeProjectId: null,
  workflowStates: [],
  objectives: [],
  workspaceTasks: [],

  viewMode: 'kanban',
  scopeTab: 'board',
  searchQuery: '',
  statusFilter: 'all',
  priorityFilter: 'all',
  objectiveFilterId: 'all',
  includeCompletedOnBoard: true,
  showArchivedProjects: false,

  selectedTaskId: null,
  selectedTaskRelations: [],
  selectedTaskComments: [],
  selectedTaskMarkdown: '',
  selectedTaskMarkdownLoading: false,

  activeProjectMarkdown: '',
  activeProjectMarkdownLoading: false,
  orphanReport: EMPTY_ORPHAN_REPORT,
  lastPruneResult: null,
};

function extractErrorMessage(err: unknown): string {
  if (err instanceof SanitizedApiError) {
    return err.userMessage;
  }
  if (err instanceof Error && err.message) {
    return err.message;
  }
  return 'Operation failed.';
}

const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function isValidUuid(value?: string | null): value is string {
  return typeof value === 'string' && UUID_PATTERN.test(value.trim());
}

function sanitizeActorHandle(raw: string): string {
  const cleaned = raw
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return cleaned || 'web-operator';
}

const CANONICAL_STATE_NAMES: Record<TaskStatus, readonly string[]> = {
  backlog: ['backlog'],
  ready: ['todo', 'ready'],
  in_progress: ['in progress', 'in_progress'],
  in_review: ['in review', 'in_review'],
  blocked: ['blocked'],
  done: ['done', 'completed'],
  cancelled: ['canceled', 'cancelled'],
  archived: ['archived'],
};

function pickWorkflowStateIdForStatus(
  states: readonly WorkflowStateModel[],
  projectId: string,
  targetStatus: TaskStatus,
): string | null {
  const projectStates = states.filter((st) => st.projectId === projectId);
  const preferredNames = CANONICAL_STATE_NAMES[targetStatus] ?? [];
  for (const candidateName of preferredNames) {
    const exact = projectStates.find((st) => st.name.trim().toLowerCase() === candidateName);
    if (exact) {
      return exact.id;
    }
  }
  const fallback = projectStates.find((st) => st.statusKey === targetStatus);
  return fallback?.id ?? null;
}

const ZERO_COUNTS: ProjectTaskCounts = {
  total: 0,
  active: 0,
  backlog: 0,
  ready: 0,
  inProgress: 0,
  inReview: 0,
  blocked: 0,
  done: 0,
  archived: 0,
};

/**
 * Root singleton SignalStore orchestrating all Flowsk Web UI state, session cookie auth,
 * real-time Go channel SSE streaming, structured Goals/Milestones tracking, drag-and-drop
 * Kanban transitions, Markdown document persistence, archive purging, and orphan pruning.
 */
export const FlowskStore = signalStore(
  { providedIn: 'root' },
  withState(INITIAL_STATE),
  withComputed((store) => {
    const visibleProjects = computed(() => {
      const all = store.projects();
      return store.showArchivedProjects() ? all : all.filter((p) => p.status !== 'archived');
    });

    const activeProject = computed(() => {
      const id = store.activeProjectId();
      return store.projects().find((p) => p.id === id) ?? null;
    });

    const actorMapById = computed(() => {
      const map: Record<string, ActorModel> = {};
      for (const a of store.actors()) {
        map[a.id] = a;
      }
      return map;
    });

    const projectMapById = computed(() => {
      const map: Record<string, ProjectModel> = {};
      for (const p of store.projects()) {
        map[p.id] = p;
      }
      return map;
    });

    const objectiveMapById = computed(() => {
      const map: Record<string, ObjectiveModel> = {};
      for (const o of store.objectives()) {
        map[o.id] = o;
      }
      return map;
    });

    const taskMapById = computed(() => {
      const map: Record<string, TaskModel> = {};
      for (const t of store.workspaceTasks()) {
        map[t.id] = t;
      }
      return map;
    });

    const scopedObjectives = computed(() => {
      const pid = store.activeProjectId();
      const visibleIds = new Set(visibleProjects().map((p) => p.id));
      return store.objectives().filter((o) => {
        if (pid) {
          return o.projectId === pid;
        }
        return visibleIds.has(o.projectId);
      });
    });

    const activeObjectivesCount = computed(() =>
      scopedObjectives().filter((o) => o.status !== 'achieved' && o.status !== 'dropped').length,
    );

    const isTaskInVisibleProjectScope = (
      task: TaskModel,
      pid: string | null,
      showArchivedProj: boolean,
      byProj: Record<string, ProjectModel>,
    ): boolean => {
      if (pid) {
        return task.projectId === pid;
      }
      if (showArchivedProj) {
        return true;
      }
      return byProj[task.projectId]?.status !== 'archived';
    };

    const projectScopedTasks = computed(() => {
      const pid = store.activeProjectId();
      const all = store.workspaceTasks();
      if (store.scopeTab() === 'archived') {
        return all.filter((t) => t.status === 'archived');
      }
      if (!pid) {
        const showArch = store.showArchivedProjects();
        const byProj = projectMapById();
        return all.filter(
          (t) => t.status !== 'archived' && isTaskInVisibleProjectScope(t, null, showArch, byProj),
        );
      }
      return all.filter((t) => t.projectId === pid);
    });

    const workspaceArchivedCount = computed(() =>
      store.workspaceTasks().filter((t) => t.status === 'archived').length,
    );

    const projectBacklogCount = computed(() => {
      const pid = store.activeProjectId();
      const showArch = store.showArchivedProjects();
      const byProj = projectMapById();
      return store
        .workspaceTasks()
        .filter((t) => isTaskInVisibleProjectScope(t, pid, showArch, byProj) && t.status === 'backlog')
        .length;
    });

    const projectActiveCount = computed(() => {
      const pid = store.activeProjectId();
      const showArch = store.showArchivedProjects();
      const byProj = projectMapById();
      return store
        .workspaceTasks()
        .filter(
          (t) =>
            isTaskInVisibleProjectScope(t, pid, showArch, byProj) &&
            t.status !== 'archived' &&
            t.status !== 'cancelled',
        ).length;
    });

    const filteredTasks = computed(() => {
      const scope = store.scopeTab();
      const viewMode = store.viewMode();
      const query = store.searchQuery().trim().toLowerCase();
      const stFilter = store.statusFilter();
      const prioFilter = store.priorityFilter();
      const objFilter = store.objectiveFilterId();
      const actors = actorMapById();
      const objs = objectiveMapById();
      const currentNowMs = store.nowMs();

      return projectScopedTasks()
        .filter((t) => {
          if (scope === 'archived') {
            if (t.status !== 'archived') return false;
          } else if (scope === 'backlog') {
            const allowTodoColumn = viewMode === 'kanban' && t.status === 'ready';
            if (t.status !== 'backlog' && !allowTodoColumn) return false;
          } else if (scope === 'board') {
            if (t.status === 'archived' || t.status === 'cancelled') return false;
            if (!store.includeCompletedOnBoard() && t.status === 'done') return false;
          } else {
            if (t.status === 'archived') return false;
          }

          if (stFilter !== 'all' && t.status !== stFilter) {
            return false;
          }
          if (prioFilter !== 'all' && t.priority !== prioFilter) {
            return false;
          }
          if (objFilter !== 'all' && t.objectiveId !== objFilter) {
            return false;
          }

          if (query.length > 0) {
            return matchesSearchExpression(t, query, actors, objs, currentNowMs);
          }
          return true;
        })
        .slice()
        .sort((a, b) => {
          if (a.priorityRank !== b.priorityRank) {
            return a.priorityRank - b.priorityRank;
          }
          return b.sequenceNumber - a.sequenceNumber;
        });
    });

    const kanbanColumns = computed(() => {
      const scope = store.scopeTab();
      const tasks = filteredTasks();
      const includeDone = store.includeCompletedOnBoard();

      const specs = CANONICAL_STATUS_COLUMNS.filter((col) => {
        if (scope === 'archived') {
          return col.status === 'archived';
        }
        if (scope === 'backlog') {
          return col.status === 'backlog' || col.status === 'ready';
        }
        if (scope === 'all') {
          return col.status !== 'archived';
        }
        if (col.status === 'done') {
          return includeDone;
        }
        return col.showOnActiveBoard;
      });

      return specs.map((spec) => ({
        ...spec,
        tasks: tasks.filter((t) => t.status === spec.status),
      }));
    });

    const objectivesWithProgress = computed<readonly ObjectiveProgressView[]>(() => {
      const byProj = projectMapById();
      const currentNowMs = store.nowMs();
      const allNonArchived = store
        .workspaceTasks()
        .filter((t) => t.status !== 'archived' && t.status !== 'cancelled');

      return scopedObjectives().map((o) => {
        const linked = allNonArchived
          .filter((t) => t.objectiveId === o.id)
          .slice()
          .sort((a, b) => {
            const rankA = statusProgressSortOrder(a.status);
            const rankB = statusProgressSortOrder(b.status);
            if (rankA !== rankB) return rankA - rankB;
            if (a.priorityRank !== b.priorityRank) return a.priorityRank - b.priorityRank;
            return a.sequenceNumber - b.sequenceNumber;
          });

        const total = linked.length;
        const done = linked.filter((t) => t.status === 'done').length;
        const inProgress = linked.filter((t) => t.status === 'in_progress').length;
        const inReview = linked.filter((t) => t.status === 'in_review').length;
        const blocked = linked.filter((t) => t.status === 'blocked').length;
        const todo = linked.filter((t) => t.status === 'ready').length;
        const backlog = linked.filter((t) => t.status === 'backlog').length;
        const claims = linked.filter((t) => isTaskLeaseActive(t, currentNowMs)).length;

        const progressPct = total > 0 ? Math.round((done * 100) / total) : 0;
        const activeWorkPct = total > 0 ? Math.round(((inProgress + inReview) * 100) / total) : 0;
        const blockedPct = total > 0 ? Math.round((blocked * 100) / total) : 0;

        const proj = byProj[o.projectId];
        return {
          ...o,
          projectKeyPrefix: proj?.keyPrefix ?? 'PRJ',
          projectName: proj?.name ?? 'Project',
          tasks: linked,
          totalCount: total,
          doneCount: done,
          inProgressCount: inProgress,
          inReviewCount: inReview,
          blockedCount: blocked,
          todoCount: todo,
          backlogCount: backlog,
          activeClaimsCount: claims,
          progressPct,
          activeWorkPct,
          blockedPct,
        };
      });
    });

    const unlinkedProjectTasks = computed(() => {
      const pid = store.activeProjectId();
      const showArch = store.showArchivedProjects();
      const byProj = projectMapById();
      return store
        .workspaceTasks()
        .filter(
          (t) =>
            isTaskInVisibleProjectScope(t, pid, showArch, byProj) &&
            !t.objectiveId &&
            t.status !== 'archived' &&
            t.status !== 'cancelled',
        )
        .slice()
        .sort((a, b) => a.sequenceNumber - b.sequenceNumber);
    });

    const selectedTask = computed(() => {
      const tid = store.selectedTaskId();
      if (!tid) return null;
      return store.workspaceTasks().find((t) => t.id === tid) ?? null;
    });

    const projectCountsById = computed(() => {
      const countsMap: Record<string, ProjectTaskCounts> = {};
      for (const p of store.projects()) {
        countsMap[p.id] = { ...ZERO_COUNTS };
      }
      for (const t of store.workspaceTasks()) {
        const cur = countsMap[t.projectId] ?? { ...ZERO_COUNTS };
        const isActive = t.status !== 'archived' && t.status !== 'cancelled';
        countsMap[t.projectId] = {
          total: cur.total + 1,
          active: cur.active + (isActive ? 1 : 0),
          backlog: cur.backlog + (t.status === 'backlog' ? 1 : 0),
          ready: cur.ready + (t.status === 'ready' ? 1 : 0),
          inProgress: cur.inProgress + (t.status === 'in_progress' ? 1 : 0),
          inReview: cur.inReview + (t.status === 'in_review' ? 1 : 0),
          blocked: cur.blocked + (t.status === 'blocked' ? 1 : 0),
          done: cur.done + (t.status === 'done' ? 1 : 0),
          archived: cur.archived + (t.status === 'archived' ? 1 : 0),
        };
      }
      return countsMap;
    });

    const projectTree = computed<readonly ProjectHierarchyNode[]>(() => {
      const visible = visibleProjects();
      const visibleIds = new Set(visible.map((p) => p.id));
      const byId = projectMapById();
      const counts = projectCountsById();
      const validBindings = store
        .projectBindings()
        .filter((b) => visibleIds.has(b.parentProjectId) && visibleIds.has(b.childProjectId));

      const childrenByParent = new Map<string, ProjectBindingModel[]>();
      const hasVisibleParent = new Set<string>();
      for (const b of validBindings) {
        const list = childrenByParent.get(b.parentProjectId) ?? [];
        list.push(b);
        childrenByParent.set(b.parentProjectId, list);
        hasVisibleParent.add(b.childProjectId);
      }

      const visitedGlobal = new Set<string>();
      const buildNode = (
        proj: ProjectModel,
        depth: number,
        scopeSubpath: string,
        parentId: string | null,
        ancestorPath: Set<string>,
        ancestorChain: readonly string[],
      ): ProjectHierarchyNode => {
        visitedGlobal.add(proj.id);
        const nextPath = new Set(ancestorPath);
        nextPath.add(proj.id);
        const nextChain = [...ancestorChain, proj.id];

        const rawEdges = (childrenByParent.get(proj.id) ?? []).slice().sort((a, b) => {
          const pa = byId[a.childProjectId]?.keyPrefix ?? '';
          const pb = byId[b.childProjectId]?.keyPrefix ?? '';
          return pa.localeCompare(pb);
        });

        const children: ProjectHierarchyNode[] = [];
        for (const edge of rawEdges) {
          if (nextPath.has(edge.childProjectId)) {
            continue;
          }
          const childProj = byId[edge.childProjectId];
          if (!childProj || !visibleIds.has(childProj.id)) {
            continue;
          }
          children.push(
            buildNode(childProj, depth + 1, edge.scopeSubpath, proj.id, nextPath, nextChain),
          );
        }

        return {
          project: proj,
          lineageKey: nextChain.join('>'),
          depth,
          scopeSubpath,
          parentProjectId: parentId,
          parentKeyPrefix: parentId ? (byId[parentId]?.keyPrefix ?? null) : null,
          counts: counts[proj.id] ?? ZERO_COUNTS,
          children,
        };
      };

      const sortedRoots = visible
        .filter((p) => !hasVisibleParent.has(p.id))
        .slice()
        .sort((a, b) => a.keyPrefix.localeCompare(b.keyPrefix));

      const roots: ProjectHierarchyNode[] = sortedRoots.map((root) =>
        buildNode(root, 0, '', null, new Set<string>(), []),
      );

      for (const p of visible) {
        if (!visitedGlobal.has(p.id)) {
          roots.push(buildNode(p, 0, '', null, new Set<string>(), []));
        }
      }
      return roots;
    });

    const flatProjectHierarchy = computed<readonly ProjectHierarchyNode[]>(() => {
      const out: ProjectHierarchyNode[] = [];
      const walk = (nodes: readonly ProjectHierarchyNode[]): void => {
        for (const n of nodes) {
          out.push(n);
          if (n.children.length > 0) {
            walk(n.children);
          }
        }
      };
      walk(projectTree());
      return out;
    });

    const totalSubprojectsCount = computed(() => {
      const visibleIds = new Set(visibleProjects().map((p) => p.id));
      const subIds = new Set<string>();
      for (const b of store.projectBindings()) {
        if (visibleIds.has(b.parentProjectId) && visibleIds.has(b.childProjectId)) {
          subIds.add(b.childProjectId);
        }
      }
      return subIds.size;
    });

    const activeProjectParentInfo = computed(() => {
      const pid = store.activeProjectId();
      if (!pid) return null;
      const byId = projectMapById();
      const edge = store.projectBindings().find((b) => b.childProjectId === pid && Boolean(byId[b.parentProjectId]));
      if (!edge) return null;
      const parent = byId[edge.parentProjectId];
      return parent ? { parent, scopeSubpath: edge.scopeSubpath } : null;
    });

    const activeProjectChildren = computed(() => {
      const pid = store.activeProjectId();
      if (!pid) return [];
      const byId = projectMapById();
      return store
        .projectBindings()
        .filter((b) => b.parentProjectId === pid && Boolean(byId[b.childProjectId]))
        .map((b) => ({
          child: byId[b.childProjectId]!,
          scopeSubpath: b.scopeSubpath,
        }))
        .sort((a, b) => a.child.keyPrefix.localeCompare(b.child.keyPrefix));
    });

    return {
      visibleProjects,
      activeProject,
      actorMapById,
      projectMapById,
      objectiveMapById,
      taskMapById,
      scopedObjectives,
      activeObjectivesCount,
      projectCountsById,
      projectTree,
      flatProjectHierarchy,
      totalSubprojectsCount,
      activeProjectParentInfo,
      activeProjectChildren,
      projectScopedTasks,
      workspaceArchivedCount,
      projectBacklogCount,
      projectActiveCount,
      filteredTasks,
      kanbanColumns,
      objectivesWithProgress,
      unlinkedProjectTasks,
      selectedTask,
    };
  }),
  withMethods((store) => {
    const rpc = inject(FlowskRpcService);
    const stream = inject(FlowskStreamService);
    const headerCtx = inject(FlowskHeaderContextService);
    const platformId = inject(PLATFORM_ID);

    let pendingStreamSync = false;
    let pendingStreamEvent: LiveStreamEventModel | null = null;

    const applyThemeToDocument = (dark: boolean): void => {
      if (!isPlatformBrowser(platformId)) {
        return;
      }
      const rootEl = document.documentElement;
      if (dark) {
        rootEl.classList.add('dark');
        rootEl.style.colorScheme = 'dark';
      } else {
        rootEl.classList.remove('dark');
        rootEl.style.colorScheme = 'light';
      }
      try {
        localStorage.setItem('flowsk.theme', dark ? 'dark' : 'light');
      } catch {
        // Ignore storage restrictions cleanly
      }
    };

    const resolveStateIdForStatus = (projectId: string, targetStatus: TaskStatus): string | null =>
      pickWorkflowStateIdForStatus(store.workflowStates(), projectId, targetStatus);

    const connectLiveStream = rxMethod<{ workspaceId: string; actorHandle: string; enabled: boolean }>(
      pipe(
        switchMap(({ workspaceId, actorHandle, enabled }) => {
          if (!enabled || !workspaceId || store.authRequired()) {
            patchState(store, { streamStatus: 'paused' });
            return EMPTY;
          }
          return stream.connectWorkspace(workspaceId, actorHandle).pipe(
            tapResponse({
              next: (sig) => {
                if (sig.type === 'auth_required') {
                  patchState(store, {
                    initialized: true,
                    loading: false,
                    saving: false,
                    authRequired: true,
                    authError: 'Session authentication required. Enter your Flowsk token to continue.',
                    streamStatus: 'paused',
                  });
                  return;
                }
                if (sig.type === 'status') {
                  patchState(store, { streamStatus: sig.status });
                  return;
                }
                const existing = store.liveEvents();
                if (sig.event.id && existing.some((e) => e.id === sig.event.id)) {
                  return;
                }
                patchState(store, {
                  liveEvents: [sig.event, ...existing].slice(0, 30),
                });
                silentSyncFromStream(sig.event);
              },
              error: () => {
                patchState(store, { streamStatus: 'reconnecting' });
              },
            }),
          );
        }),
      ),
    );

    const handlePotentialAuthError = (err: unknown): boolean => {
      if (err instanceof SanitizedApiError && err.statusCode === 401) {
        pendingStreamSync = false;
        pendingStreamEvent = null;
        patchState(store, {
          initialized: true,
          loading: false,
          saving: false,
          authRequired: true,
          authError: 'Authentication required. Enter your Flowsk server token to continue.',
          streamStatus: 'paused',
        });
        connectLiveStream({ workspaceId: '', actorHandle: '', enabled: false });
        return true;
      }
      return false;
    };

    const silentSyncFromStream = rxMethod<LiveStreamEventModel>(
      pipe(
        debounceTime(180),
        switchMap((lastEvent) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          if (!wsId || !actorId || store.authRequired()) {
            return of(null);
          }
          if (store.saving()) {
            pendingStreamSync = true;
            const currentOpenId = store.selectedTaskId();
            if (
              pendingStreamEvent &&
              currentOpenId &&
              pendingStreamEvent.resourceId === currentOpenId &&
              lastEvent.resourceId !== currentOpenId
            ) {
              // Preserve the open task resourceId so its markdown refreshes once save finishes
            } else {
              pendingStreamEvent = lastEvent;
            }
            return of(null);
          }
          const projectId = store.activeProjectId();
          const openTaskId = store.selectedTaskId();
          const shouldFetchOpenTaskMd = Boolean(openTaskId && lastEvent.resourceId === openTaskId);

          return forkJoin({
            actors: rpc.listActors(wsId, actorId),
            projects: rpc.listProjects(wsId, actorId),
            bindings: rpc.listProjectBindings(wsId, actorId),
            objectives: rpc.listObjectives(wsId, actorId),
            tasks: rpc.listTasks(wsId, actorId),
            orphans: rpc.inspectOrphanResources(wsId, actorId),
            states: projectId
              ? rpc.listWorkflowStates(wsId, actorId, projectId)
              : of(store.workflowStates() as WorkflowStateModel[]),
            inspector: openTaskId
              ? forkJoin({
                  comments: rpc.listTaskComments(wsId, actorId, openTaskId),
                  md: shouldFetchOpenTaskMd
                    ? rpc.getTaskMarkdown(wsId, actorId, openTaskId)
                    : of(null),
                  detail: rpc.getTaskDetail(wsId, actorId, openTaskId),
                })
              : of(null),
            trigger: of(lastEvent),
          }).pipe(
            tapResponse({
              next: (bundle) => {
                const inspectorPatch: Partial<FlowskState> =
                  bundle.inspector && store.selectedTaskId() === openTaskId
                    ? {
                        selectedTaskComments: bundle.inspector.comments,
                        selectedTaskRelations: bundle.inspector.detail.relations,
                        ...(bundle.inspector.md !== null
                          ? { selectedTaskMarkdown: bundle.inspector.md.markdown }
                          : {}),
                      }
                    : {};
                patchState(store, {
                  nowMs: Date.now(),
                  actors: bundle.actors,
                  projects: bundle.projects,
                  projectBindings: bundle.bindings,
                  objectives: bundle.objectives,
                  workspaceTasks: bundle.tasks,
                  orphanReport: bundle.orphans,
                  workflowStates: bundle.states,
                  ...inspectorPatch,
                });
              },
              error: (err: unknown) => {
                handlePotentialAuthError(err);
              },
            }),
          );
        }),
      ),
    );

    const flushPendingStreamSync = (): void => {
      if (!store.saving() && pendingStreamSync) {
        const queuedEvent =
          pendingStreamEvent ?? {
            id: '',
            workspaceId: store.workspaceId(),
            actorId: store.actorId(),
            action: 'sync',
            resourceType: 'workspace',
            resourceId: '',
            occurredAt: new Date().toISOString(),
          };
        pendingStreamSync = false;
        pendingStreamEvent = null;
        silentSyncFromStream(queuedEvent);
      }
    };

    const refreshProjectContext = rxMethod<{ projectId: string | null; preserveErrorBanner?: boolean }>(
      pipe(
        switchMap(({ projectId, preserveErrorBanner }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          if (!wsId || !actorId || store.authRequired()) {
            return of(null);
          }
          patchState(store, {
            loading: true,
            ...(preserveErrorBanner ? {} : { errorBanner: null }),
          });

          const actors$ = rpc.listActors(wsId, actorId);
          const projects$ = rpc.listProjects(wsId, actorId);
          const bindings$ = rpc.listProjectBindings(wsId, actorId);
          const tasks$ = rpc.listTasks(wsId, actorId);
          const orphans$ = rpc.inspectOrphanResources(wsId, actorId);
          const states$ = projectId ? rpc.listWorkflowStates(wsId, actorId, projectId) : of([] as WorkflowStateModel[]);
          const objs$ = rpc.listObjectives(wsId, actorId);

          return forkJoin({
            actors: actors$,
            projects: projects$,
            bindings: bindings$,
            tasks: tasks$,
            orphans: orphans$,
            states: states$,
            objectives: objs$,
          }).pipe(
            tapResponse({
              next: (bundle) => {
                patchState(store, {
                  nowMs: Date.now(),
                  actors: bundle.actors,
                  projects: bundle.projects,
                  projectBindings: bundle.bindings,
                  workspaceTasks: bundle.tasks,
                  orphanReport: bundle.orphans,
                  workflowStates: bundle.states,
                  objectives: bundle.objectives,
                  loading: false,
                });
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  patchState(store, {
                    loading: false,
                    errorBanner: extractErrorMessage(err),
                  });
                }
              },
            }),
          );
        }),
      ),
    );

    const bootstrap = rxMethod<void>(
      pipe(
        switchMap(() => {
          patchState(store, { loading: true, errorBanner: null, authError: null });
          return rpc.getBootstrapConfig().pipe(
            switchMap((boot) => {
              const wsSlug = (boot.workspaceSlug || 'default').trim();
              const handle = sanitizeActorHandle(boot.actorHandle || 'web-operator');

              if (boot.authRequired && boot.authenticated === false) {
                patchState(store, {
                  initialized: true,
                  loading: false,
                  authRequired: true,
                  authError: null,
                  workspaceSlug: wsSlug,
                  actorHandle: handle,
                  streamStatus: 'paused',
                });
                connectLiveStream({ workspaceId: '', actorHandle: '', enabled: false });
                return EMPTY;
              }

              patchState(store, { authRequired: false, authError: null });
              headerCtx.setIdentity(boot.workspaceId || wsSlug, handle, boot.actorId ?? '');

              const resolveWorkspace$ = isValidUuid(boot.workspaceId)
                ? of({
                    wsId: boot.workspaceId.trim(),
                    wsSlug,
                    cachedWorkspaces: null as WorkspaceModel[] | null,
                  })
                : rpc.listWorkspaces().pipe(
                    switchMap((workspaces) => {
                      const matched =
                        workspaces.find(
                          (w) =>
                            w.slug.toLowerCase() === wsSlug.toLowerCase() ||
                            w.id.toLowerCase() === wsSlug.toLowerCase(),
                        ) ?? workspaces[0];
                      if (!matched?.id) {
                        throw new Error(`Workspace '${wsSlug}' could not be resolved.`);
                      }
                      return of({
                        wsId: matched.id,
                        wsSlug: matched.slug || wsSlug,
                        cachedWorkspaces: workspaces,
                      });
                    }),
                  );

              return resolveWorkspace$.pipe(
                switchMap((wsInfo) => {
                  headerCtx.setIdentity(wsInfo.wsId, handle);
                  return rpc.registerActor(wsInfo.wsId, handle).pipe(
                    switchMap((actor) => {
                      const wsId = actor.workspaceId || wsInfo.wsId;
                      const actorId = actor.id;
                      headerCtx.setIdentity(wsId, actor.handle, actorId);
                      return forkJoin({
                        actor: of(actor),
                        boot: of(boot),
                        resolvedSlug: of(wsInfo.wsSlug),
                        workspaces: wsInfo.cachedWorkspaces ? of(wsInfo.cachedWorkspaces) : rpc.listWorkspaces(),
                        actors: rpc.listActors(wsId, actorId),
                        projects: rpc.listProjects(wsId, actorId),
                        bindings: rpc.listProjectBindings(wsId, actorId),
                        objectives: rpc.listObjectives(wsId, actorId),
                        tasks: rpc.listTasks(wsId, actorId),
                        orphans: rpc.inspectOrphanResources(wsId, actorId),
                      });
                    }),
                  );
                }),
              );
            }),
            switchMap((ctx) => {
              const preferredRef = (ctx.boot.projectSlug ?? '').trim().toLowerCase();
              const chosen = preferredRef
                ? (ctx.projects.find(
                    (p) =>
                      p.id.toLowerCase() === preferredRef ||
                      p.slug.toLowerCase() === preferredRef ||
                      p.keyPrefix.toLowerCase() === preferredRef,
                  ) ?? null)
                : null;

              if (!chosen) {
                return of({
                  ...ctx,
                  chosenId: null as string | null,
                  openedFromBoundFolder: false,
                  states: [] as WorkflowStateModel[],
                });
              }
              return rpc.listWorkflowStates(ctx.actor.workspaceId, ctx.actor.id, chosen.id).pipe(
                switchMap((states) =>
                  of({
                    ...ctx,
                    chosenId: chosen.id,
                    openedFromBoundFolder: true,
                    states,
                  }),
                ),
              );
            }),
            tapResponse({
              next: (res) => {
                patchState(store, {
                  initialized: true,
                  loading: false,
                  authRequired: false,
                  authError: null,
                  nowMs: Date.now(),
                  workspaceId: res.actor.workspaceId,
                  workspaceSlug: res.resolvedSlug || 'default',
                  actorId: res.actor.id,
                  actorHandle: res.actor.handle,
                  workspaces: res.workspaces,
                  actors: res.actors,
                  projects: res.projects,
                  projectBindings: res.bindings,
                  openedFromBoundFolder: res.openedFromBoundFolder,
                  activeProjectId: res.chosenId,
                  workflowStates: res.states,
                  objectives: res.objectives,
                  workspaceTasks: res.tasks,
                  orphanReport: res.orphans,
                });
                connectLiveStream({
                  workspaceId: res.actor.workspaceId,
                  actorHandle: res.actor.handle,
                  enabled: store.liveStreamEnabled(),
                });
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  patchState(store, {
                    initialized: true,
                    loading: false,
                    errorBanner: extractErrorMessage(err),
                  });
                }
              },
            }),
          );
        }),
      ),
    );

    const authenticateSession = rxMethod<UiActionPayload<{ token: string }, void>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const trimmed = payload.token.trim();
          if (!trimmed) {
            patchState(store, { authError: 'Please enter an authentication token.' });
            return of(null);
          }
          patchState(store, { saving: true, authError: null, errorBanner: null });
          return rpc.authenticateSession(trimmed).pipe(
            tapResponse({
              next: () => {
                patchState(store, {
                  saving: false,
                  authRequired: false,
                  authError: null,
                });
                onSuccess?.();
                bootstrap();
              },
              error: (err: unknown) => {
                const msg =
                  err instanceof SanitizedApiError && err.statusCode === 401
                    ? 'Invalid authentication token. Verify FLOWSK_AUTH_TOKEN and try again.'
                    : extractErrorMessage(err);
                patchState(store, {
                  saving: false,
                  authError: msg,
                });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const loadTaskInspector = rxMethod<{ taskId: string }>(
      pipe(
        switchMap(({ taskId }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          if (!wsId || !actorId) return of(null);
          const isDifferentTask = store.selectedTaskId() !== taskId;
          patchState(store, {
            selectedTaskId: taskId,
            selectedTaskMarkdownLoading: true,
            ...(isDifferentTask
              ? {
                  selectedTaskComments: [],
                  selectedTaskRelations: [],
                  selectedTaskMarkdown: '',
                }
              : {}),
          });
          return forkJoin({
            detail: rpc.getTaskDetail(wsId, actorId, taskId),
            comments: rpc.listTaskComments(wsId, actorId, taskId),
            md: rpc.getTaskMarkdown(wsId, actorId, taskId),
          }).pipe(
            tapResponse({
              next: (res) => {
                if (store.selectedTaskId() !== taskId) {
                  return;
                }
                patchState(store, {
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === res.detail.task.id ? res.detail.task : t)),
                  selectedTaskRelations: res.detail.relations,
                  selectedTaskComments: res.comments,
                  selectedTaskMarkdown: res.md.markdown,
                  selectedTaskMarkdownLoading: false,
                });
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  patchState(store, {
                    selectedTaskMarkdownLoading: false,
                    errorBanner: extractErrorMessage(err),
                  });
                }
              },
            }),
          );
        }),
      ),
    );

    const moveTaskToStatus = rxMethod<
      UiActionPayload<{ taskId: string; targetStatus: TaskStatus; note?: string }, TaskModel>
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const existing = store.workspaceTasks().find((t) => t.id === payload.taskId);
          if (!existing || !wsId || !actorId) {
            return of(null);
          }
          if (existing.status === payload.targetStatus) {
            return of(null);
          }

          patchState(store, {
            saving: true,
            workspaceTasks: store
              .workspaceTasks()
              .map((t) => (t.id === payload.taskId ? { ...t, status: payload.targetStatus } : t)),
            errorBanner: null,
          });

          const knownStateId = resolveStateIdForStatus(existing.projectId, payload.targetStatus);
          const stateId$ = knownStateId
            ? of(knownStateId)
            : rpc
                .listWorkflowStates(wsId, actorId, existing.projectId)
                .pipe(
                  switchMap((sts) =>
                    of(pickWorkflowStateIdForStatus(sts, existing.projectId, payload.targetStatus) ?? ''),
                  ),
                );

          return stateId$.pipe(
            switchMap((workflowStateId) => {
              if (!workflowStateId) {
                throw new Error(`Workflow state '${payload.targetStatus}' is unavailable for this project.`);
              }
              const isTerminal =
                payload.targetStatus === 'done' ||
                payload.targetStatus === 'cancelled' ||
                payload.targetStatus === 'archived';
              return rpc.transitionTaskState({
                workspaceId: wsId,
                actorId,
                taskId: payload.taskId,
                workflowStateId,
                resolutionNote: payload.note,
                releaseClaim: isTerminal,
              });
            }),
            tapResponse({
              next: (updated) => {
                patchState(store, {
                  saving: false,
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === updated.id ? updated : t)),
                  toastMessage: `${updated.identifier} moved to ${payload.targetStatus.replace('_', ' ')}`,
                });
                flushPendingStreamSync();
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                const rolledBack = store
                  .workspaceTasks()
                  .map((t) => (t.id === existing.id ? existing : t));
                if (handlePotentialAuthError(err)) {
                  patchState(store, { workspaceTasks: rolledBack });
                  return;
                }
                const msg = extractErrorMessage(err);
                patchState(store, {
                  saving: false,
                  workspaceTasks: rolledBack,
                  errorBanner: msg,
                });
                flushPendingStreamSync();
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const createProject = rxMethod<
      UiActionPayload<
        {
          keyPrefix: string;
          name: string;
          description: string;
          parentProjectId?: string;
          scopeSubpath?: string;
        },
        ProjectModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const key = payload.keyPrefix.trim().toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 10);
          const slug = (payload.name || key)
            .trim()
            .toLowerCase()
            .replace(/[^a-z0-9]+/g, '-')
            .replace(/^-|-$/g, '')
            .slice(0, 60);
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .createProject({
              workspaceId: wsId,
              actorId,
              keyPrefix: key.length >= 2 ? key : 'PRJ',
              slug: slug.length >= 2 ? slug : `proj-${Date.now().toString(36)}`,
              name: payload.name.trim(),
              description: payload.description.trim(),
            })
            .pipe(
              switchMap((created) => {
                const parentId = (payload.parentProjectId ?? '').trim();
                if (!parentId) {
                  return of({
                    created,
                    binding: null as ProjectBindingModel | null,
                    bindError: null as string | null,
                  });
                }
                return rpc
                  .bindSubproject({
                    workspaceId: wsId,
                    actorId,
                    parentProjectId: parentId,
                    childProjectId: created.id,
                    scopeSubpath: (payload.scopeSubpath ?? '').trim(),
                  })
                  .pipe(
                    switchMap((binding) =>
                      of({
                        created,
                        binding,
                        bindError: null as string | null,
                      }),
                    ),
                    catchError((bindErr: unknown) =>
                      of({
                        created,
                        binding: null as ProjectBindingModel | null,
                        bindError: extractErrorMessage(bindErr),
                      }),
                    ),
                  );
              }),
              tapResponse({
                next: ({ created, binding, bindError }) => {
                  patchState(store, {
                    saving: false,
                    projects: [...store.projects(), created],
                    projectBindings: binding ? [...store.projectBindings(), binding] : store.projectBindings(),
                    activeProjectId: created.id,
                    activeProjectMarkdown: '',
                    activeProjectMarkdownLoading: false,
                    scopeTab: 'board',
                    errorBanner: bindError
                      ? `Project ${created.keyPrefix} was created, but subproject binding failed: ${bindError}`
                      : null,
                    toastMessage: bindError
                      ? `Project ${created.keyPrefix} created (binding failed)`
                      : binding
                        ? `Subproject ${created.keyPrefix} created and bound`
                        : `Project ${created.keyPrefix} created`,
                  });
                  flushPendingStreamSync();
                  refreshProjectContext({
                    projectId: created.id,
                    preserveErrorBanner: Boolean(bindError),
                  });
                  onSuccess?.(created);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const archiveProject = rxMethod<UiActionPayload<{ projectId: string }, ProjectModel>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.archiveProject(store.workspaceId(), store.actorId(), payload.projectId).pipe(
            tapResponse({
              next: (archived) => {
                const updatedList = store.projects().map((p) => (p.id === archived.id ? archived : p));
                const nextActive = store.activeProjectId() === archived.id ? null : store.activeProjectId();
                patchState(store, {
                  saving: false,
                  projects: updatedList,
                  activeProjectId: nextActive,
                  toastMessage: `Project ${archived.keyPrefix} archived`,
                });
                flushPendingStreamSync();
                refreshProjectContext({ projectId: nextActive });
                onSuccess?.(archived);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const loadProjectMarkdown = rxMethod<{ projectId: string }>(
      pipe(
        switchMap(({ projectId }) => {
          patchState(store, {
            activeProjectMarkdown: '',
            activeProjectMarkdownLoading: true,
          });
          return rpc.getProjectMarkdown(store.workspaceId(), store.actorId(), projectId).pipe(
            tapResponse({
              next: (res) => {
                patchState(store, {
                  activeProjectMarkdown: res.markdown,
                  activeProjectMarkdownLoading: false,
                });
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  patchState(store, {
                    activeProjectMarkdownLoading: false,
                    errorBanner: extractErrorMessage(err),
                  });
                }
              },
            }),
          );
        }),
      ),
    );

    const saveProjectMarkdown = rxMethod<
      UiActionPayload<{ projectId: string; markdown: string }, ProjectModel>
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          const req$ = payload.markdown.trim()
            ? rpc.putProjectMarkdown(store.workspaceId(), store.actorId(), payload.projectId, payload.markdown)
            : rpc.deleteProjectMarkdown(store.workspaceId(), store.actorId(), payload.projectId);
          return req$.pipe(
            tapResponse({
              next: (proj) => {
                patchState(store, {
                  saving: false,
                  activeProjectMarkdown: payload.markdown.trim() ? payload.markdown : '',
                  activeProjectMarkdownLoading: false,
                  projects: store.projects().map((p) => (p.id === proj.id ? proj : p)),
                  toastMessage: `Project Markdown saved in ~/.flowsk/projects`,
                });
                flushPendingStreamSync();
                onSuccess?.(proj);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const createObjective = rxMethod<
      UiActionPayload<
        {
          projectId?: string;
          title: string;
          kind: ObjectiveKind;
          status?: ObjectiveStatus;
          description: string;
          successCriteria?: string;
          targetDate?: string;
        },
        ObjectiveModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const pid = payload.projectId || store.activeProjectId() || store.visibleProjects()[0]?.id;
          if (!pid) return of(null);
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .createObjective({
              workspaceId: store.workspaceId(),
              actorId: store.actorId(),
              projectId: pid,
              kind: payload.kind,
              status: payload.status ?? 'active',
              title: payload.title.trim(),
              description: payload.description.trim(),
              successCriteria: payload.successCriteria?.trim(),
              targetDate: payload.targetDate?.trim(),
            })
            .pipe(
              tapResponse({
                next: (created) => {
                  patchState(store, {
                    saving: false,
                    objectives: [...store.objectives(), created],
                    toastMessage: `${created.kind} '${created.title}' added`,
                  });
                  flushPendingStreamSync();
                  onSuccess?.(created);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const updateObjective = rxMethod<
      UiActionPayload<
        {
          objectiveId: string;
          title?: string;
          description?: string;
          successCriteria?: string;
          status?: ObjectiveStatus;
          targetDate?: string;
          clearTargetDate?: boolean;
        },
        ObjectiveModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          const shouldClearDate = Boolean(payload.clearTargetDate) || payload.targetDate === '';
          return rpc
            .updateObjective({
              workspaceId: store.workspaceId(),
              actorId: store.actorId(),
              objectiveId: payload.objectiveId,
              title: payload.title?.trim(),
              description: payload.description,
              successCriteria: payload.successCriteria,
              status: payload.status,
              targetDate: shouldClearDate ? '' : payload.targetDate,
              clearTargetDate: shouldClearDate,
            })
            .pipe(
              tapResponse({
                next: (updated) => {
                  patchState(store, {
                    saving: false,
                    objectives: store.objectives().map((o) => (o.id === updated.id ? updated : o)),
                    toastMessage: `Updated '${updated.title}'`,
                  });
                  flushPendingStreamSync();
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const closeObjective = rxMethod<UiActionPayload<{ objectiveId: string; achieved: boolean }, ObjectiveModel>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.closeObjective(store.workspaceId(), store.actorId(), payload.objectiveId, payload.achieved).pipe(
            tapResponse({
              next: (closed) => {
                patchState(store, {
                  saving: false,
                  objectives: store.objectives().map((o) => (o.id === closed.id ? closed : o)),
                  toastMessage: payload.achieved ? `Marked '${closed.title}' achieved` : `Dropped '${closed.title}'`,
                });
                flushPendingStreamSync();
                onSuccess?.(closed);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const deleteObjective = rxMethod<UiActionPayload<{ objectiveId: string }, string>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.deleteObjective(store.workspaceId(), store.actorId(), payload.objectiveId).pipe(
            tapResponse({
              next: (removedId) => {
                patchState(store, {
                  saving: false,
                  objectives: store.objectives().filter((o) => o.id !== removedId),
                  objectiveFilterId: store.objectiveFilterId() === removedId ? 'all' : store.objectiveFilterId(),
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.objectiveId === removedId ? { ...t, objectiveId: undefined } : t)),
                  toastMessage: `Objective deleted`,
                });
                flushPendingStreamSync();
                onSuccess?.(removedId);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const assignTaskToObjective = rxMethod<
      UiActionPayload<{ taskId: string; objectiveId: string | null }, TaskModel>
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const targetObjId = payload.objectiveId?.trim() || '';
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .updateTask({
              workspaceId: store.workspaceId(),
              actorId: store.actorId(),
              taskId: payload.taskId,
              objectiveId: targetObjId || undefined,
              clearObjective: !targetObjId,
            })
            .pipe(
              tapResponse({
                next: (updated) => {
                  patchState(store, {
                    saving: false,
                    workspaceTasks: store
                      .workspaceTasks()
                      .map((t) => (t.id === updated.id ? updated : t)),
                    toastMessage: targetObjId
                      ? `${updated.identifier} linked to goal`
                      : `${updated.identifier} detached from goal`,
                  });
                  flushPendingStreamSync();
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const createTask = rxMethod<
      UiActionPayload<
        {
          projectId?: string;
          title: string;
          description: string;
          acceptanceCriteria: string;
          kind: TaskKind;
          priority: TaskPriority;
          initialStatus: TaskStatus;
          objectiveId?: string;
          markdownDoc?: string;
        },
        TaskModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const linkedObj = payload.objectiveId ? store.objectiveMapById()[payload.objectiveId] : undefined;
          const pid =
            payload.projectId || linkedObj?.projectId || store.activeProjectId() || store.visibleProjects()[0]?.id;
          if (!pid) return of(null);
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const knownStateId = resolveStateIdForStatus(pid, payload.initialStatus);
          const stateId$ = knownStateId
            ? of(knownStateId)
            : rpc
                .listWorkflowStates(wsId, actorId, pid)
                .pipe(
                  switchMap((sts) => of(pickWorkflowStateIdForStatus(sts, pid, payload.initialStatus) ?? undefined)),
                );

          patchState(store, { saving: true, errorBanner: null });
          return stateId$.pipe(
            switchMap((stateId) =>
              rpc.createTask({
                workspaceId: wsId,
                actorId,
                projectId: pid,
                title: payload.title.trim(),
                description: payload.description.trim(),
                acceptanceCriteria: payload.acceptanceCriteria.trim(),
                kind: payload.kind,
                priority: payload.priority,
                workflowStateId: stateId ?? undefined,
                objectiveId: payload.objectiveId || undefined,
              }),
            ),
            switchMap((created) => {
              const md = (payload.markdownDoc ?? '').trim();
              if (!md) {
                return of({ task: created, mdError: null as string | null });
              }
              return rpc.putTaskMarkdown(wsId, actorId, created.id, md).pipe(
                switchMap((updatedWithMd) => of({ task: updatedWithMd, mdError: null as string | null })),
                catchError((mdErr: unknown) =>
                  of({
                    task: created,
                    mdError: extractErrorMessage(mdErr),
                  }),
                ),
              );
            }),
            tapResponse({
              next: ({ task, mdError }) => {
                patchState(store, {
                  saving: false,
                  workspaceTasks: [task, ...store.workspaceTasks()],
                  errorBanner: mdError
                    ? `Task ${task.identifier} was created, but saving its .md document failed: ${mdError}`
                    : null,
                  toastMessage: mdError
                    ? `Created ${task.identifier} (.md save failed)`
                    : `Created ${task.identifier}`,
                });
                flushPendingStreamSync();
                onSuccess?.(task);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const updateTaskDetails = rxMethod<
      UiActionPayload<
        {
          taskId: string;
          title: string;
          description: string;
          acceptanceCriteria: string;
          kind: TaskKind;
          priority: TaskPriority;
          objectiveId: string;
        },
        TaskModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .updateTask({
              workspaceId: store.workspaceId(),
              actorId: store.actorId(),
              taskId: payload.taskId,
              title: payload.title.trim(),
              description: payload.description,
              acceptanceCriteria: payload.acceptanceCriteria,
              kind: payload.kind,
              priority: payload.priority,
              objectiveId: payload.objectiveId || undefined,
              clearObjective: !payload.objectiveId,
            })
            .pipe(
              tapResponse({
                next: (updated) => {
                  patchState(store, {
                    saving: false,
                    workspaceTasks: store
                      .workspaceTasks()
                      .map((t) => (t.id === updated.id ? updated : t)),
                    toastMessage: `${updated.identifier} saved`,
                  });
                  flushPendingStreamSync();
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const saveTaskMarkdown = rxMethod<UiActionPayload<{ taskId: string; markdown: string }, TaskModel>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .putTaskMarkdown(store.workspaceId(), store.actorId(), payload.taskId, payload.markdown)
            .pipe(
              tapResponse({
                next: (updated) => {
                  patchState(store, {
                    saving: false,
                    selectedTaskMarkdown: payload.markdown,
                    workspaceTasks: store
                      .workspaceTasks()
                      .map((t) => (t.id === updated.id ? updated : t)),
                    toastMessage: `${updated.identifier} Markdown saved in ~/.flowsk/tasks`,
                  });
                  flushPendingStreamSync();
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const deleteTaskMarkdown = rxMethod<UiActionPayload<{ taskId: string }, TaskModel>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.deleteTaskMarkdown(store.workspaceId(), store.actorId(), payload.taskId).pipe(
            tapResponse({
              next: (updated) => {
                patchState(store, {
                  saving: false,
                  selectedTaskMarkdown: '',
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === updated.id ? updated : t)),
                  toastMessage: `Removed Markdown document for ${updated.identifier}`,
                });
                flushPendingStreamSync();
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const toggleTaskClaim = rxMethod<
      UiActionPayload<
        { taskId: string; claim: boolean; forceTakeover?: boolean; force?: boolean },
        TaskModel
      >
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          patchState(store, { saving: true, errorBanner: null });
          const req$ = payload.claim
            ? rpc.claimTask(wsId, actorId, payload.taskId, Boolean(payload.forceTakeover))
            : rpc.releaseTaskClaim(wsId, actorId, payload.taskId, '', Boolean(payload.force));
          return req$.pipe(
            tapResponse({
              next: (updated) => {
                patchState(store, {
                  saving: false,
                  nowMs: Date.now(),
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === updated.id ? updated : t)),
                  toastMessage: payload.claim
                    ? payload.forceTakeover
                      ? `Took over lease on ${updated.identifier}`
                      : `Claimed ${updated.identifier}`
                    : `Released lease on ${updated.identifier}`,
                });
                flushPendingStreamSync();
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const addComment = rxMethod<
      UiActionPayload<{ taskId: string; kind: string; body: string }, TaskCommentModel>
    >(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .addTaskComment(store.workspaceId(), store.actorId(), payload.taskId, payload.kind, payload.body)
            .pipe(
              tapResponse({
                next: (comment) => {
                  patchState(store, {
                    saving: false,
                    selectedTaskComments: [...store.selectedTaskComments(), comment],
                  });
                  flushPendingStreamSync();
                  onSuccess?.(comment);
                },
                error: (err: unknown) => {
                  if (!handlePotentialAuthError(err)) {
                    const msg = extractErrorMessage(err);
                    patchState(store, { saving: false, errorBanner: msg });
                    flushPendingStreamSync();
                    onError?.(msg);
                  }
                },
              }),
            );
        }),
      ),
    );

    const purgeArchivedTasks = rxMethod<UiActionPayload<void, PurgeArchivedResult>>(
      pipe(
        concatMap(({ onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.purgeArchivedTasks(store.workspaceId(), store.actorId()).pipe(
            switchMap((res) =>
              rpc.inspectOrphanResources(store.workspaceId(), store.actorId()).pipe(
                switchMap((orphans) => of({ res, orphans })),
              ),
            ),
            tapResponse({
              next: ({ res, orphans }) => {
                const selected = store.selectedTask();
                const clearSelected = selected?.status === 'archived' ? null : store.selectedTaskId();
                patchState(store, {
                  saving: false,
                  selectedTaskId: clearSelected,
                  workspaceTasks: store.workspaceTasks().filter((t) => t.status !== 'archived'),
                  orphanReport: orphans,
                  toastMessage: `Purged ${res.deletedTaskCount} archived task(s) and ${res.deletedMarkdownCount} .md file(s)`,
                });
                flushPendingStreamSync();
                onSuccess?.(res);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    const inspectOrphans = rxMethod<void>(
      pipe(
        switchMap(() => {
          patchState(store, { lastPruneResult: null });
          return rpc.inspectOrphanResources(store.workspaceId(), store.actorId()).pipe(
            tapResponse({
              next: (report) => patchState(store, { orphanReport: report }),
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  patchState(store, { errorBanner: extractErrorMessage(err) });
                }
              },
            }),
          );
        }),
      ),
    );

    const pruneOrphans = rxMethod<UiActionPayload<{ dryRun: boolean }, PruneResourcesResult>>(
      pipe(
        concatMap(({ payload, onSuccess, onError }) => {
          patchState(store, { saving: true, errorBanner: null });
          return rpc.pruneOrphanResources(store.workspaceId(), store.actorId(), payload.dryRun).pipe(
            switchMap((pruneRes) =>
              forkJoin({
                pruneRes: of(pruneRes),
                orphans: rpc.inspectOrphanResources(store.workspaceId(), store.actorId()),
                tasks: rpc.listTasks(store.workspaceId(), store.actorId()),
              }),
            ),
            tapResponse({
              next: ({ pruneRes, orphans, tasks }) => {
                patchState(store, {
                  saving: false,
                  lastPruneResult: pruneRes,
                  orphanReport: orphans,
                  workspaceTasks: tasks,
                  toastMessage: pruneRes.dryRun
                    ? `Dry-run: ${pruneRes.totalCleaned} orphan item(s) would be cleaned`
                    : `Pruned ${pruneRes.totalCleaned} orphan resource(s)`,
                });
                flushPendingStreamSync();
                onSuccess?.(pruneRes);
              },
              error: (err: unknown) => {
                if (!handlePotentialAuthError(err)) {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  flushPendingStreamSync();
                  onError?.(msg);
                }
              },
            }),
          );
        }),
      ),
    );

    return {
      bootstrap,
      authenticateSession,
      refreshProjectContext,
      loadTaskInspector,
      moveTaskToStatus,
      createProject,
      archiveProject,
      loadProjectMarkdown,
      saveProjectMarkdown,
      createObjective,
      updateObjective,
      closeObjective,
      deleteObjective,
      assignTaskToObjective,
      createTask,
      updateTaskDetails,
      saveTaskMarkdown,
      deleteTaskMarkdown,
      toggleTaskClaim,
      addComment,
      purgeArchivedTasks,
      inspectOrphans,
      pruneOrphans,
      pruneOrphanResources: pruneOrphans,

      isTaskClaimActive(task: TaskModel): boolean {
        return isTaskLeaseActive(task, store.nowMs());
      },

      activeClaimHolderId(task: TaskModel): string | null {
        return isTaskLeaseActive(task, store.nowMs()) ? (task.claimedByActorId ?? null) : null;
      },

      isTaskGlowing(task: TaskModel): boolean {
        return isTaskSignalGlowing(task, store.nowMs());
      },

      taskSignalAgeLabel(task: TaskModel): string {
        return formatSignalAge(task.lastSignalAt, store.nowMs());
      },

      tickNow(): void {
        patchState(store, { nowMs: Date.now() });
      },

      toggleLiveStream(): void {
        const nextEnabled = !store.liveStreamEnabled();
        patchState(store, {
          liveStreamEnabled: nextEnabled,
          streamStatus: nextEnabled && !store.authRequired() ? 'connecting' : 'paused',
        });
        connectLiveStream({
          workspaceId: store.workspaceId(),
          actorHandle: store.actorHandle(),
          enabled: nextEnabled && !store.authRequired(),
        });
      },

      clearLiveEvents(): void {
        patchState(store, { liveEvents: [] });
      },

      selectProject(projectId: string | null): void {
        const nextTab = store.scopeTab() === 'archived' ? 'board' : store.scopeTab();
        patchState(store, {
          activeProjectId: projectId,
          activeProjectMarkdown: '',
          activeProjectMarkdownLoading: false,
          scopeTab: nextTab,
          objectiveFilterId: 'all',
          selectedTaskId: null,
          selectedTaskComments: [],
          selectedTaskRelations: [],
          selectedTaskMarkdown: '',
          selectedTaskMarkdownLoading: false,
        });
        refreshProjectContext({ projectId });
      },

      setViewMode(viewMode: ViewMode): void {
        patchState(store, { viewMode });
      },

      setScopeTab(scopeTab: ScopeTab): void {
        patchState(store, { scopeTab, statusFilter: 'all' });
      },

      setSearchQuery(searchQuery: string): void {
        patchState(store, { searchQuery });
      },

      setStatusFilter(statusFilter: TaskStatus | 'all'): void {
        patchState(store, { statusFilter });
      },

      setPriorityFilter(priorityFilter: TaskPriority | 'all'): void {
        patchState(store, { priorityFilter });
      },

      setObjectiveFilter(objectiveFilterId: string | 'all'): void {
        patchState(store, { objectiveFilterId });
      },

      toggleCompletedOnBoard(): void {
        patchState(store, { includeCompletedOnBoard: !store.includeCompletedOnBoard() });
      },

      toggleArchivedProjects(): void {
        patchState(store, { showArchivedProjects: !store.showArchivedProjects() });
      },

      closeTaskInspector(): void {
        patchState(store, {
          selectedTaskId: null,
          selectedTaskComments: [],
          selectedTaskRelations: [],
          selectedTaskMarkdown: '',
          selectedTaskMarkdownLoading: false,
        });
      },

      dismissError(): void {
        patchState(store, { errorBanner: null });
      },

      dismissToast(): void {
        patchState(store, { toastMessage: null });
      },

      toggleDarkMode(): void {
        const next = !store.darkMode();
        patchState(store, { darkMode: next });
        applyThemeToDocument(next);
      },

      initThemeFromPlatform(): void {
        if (!isPlatformBrowser(platformId)) {
          return;
        }
        let prefersDark = true;
        try {
          const saved = localStorage.getItem('flowsk.theme');
          if (saved === 'light') prefersDark = false;
          else if (saved === 'dark') prefersDark = true;
          else prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
        } catch {
          prefersDark = true;
        }
        patchState(store, { darkMode: prefersDark });
        applyThemeToDocument(prefersDark);
      },
    };
  }),
  withHooks({
    onInit(store) {
      const platformId = inject(PLATFORM_ID);
      const destroyRef = inject(DestroyRef);

      if (isPlatformBrowser(platformId)) {
        const leaseClockId = setInterval(() => {
          store.tickNow();
        }, 15_000);

        let toastTimer: ReturnType<typeof setTimeout> | null = null;
        effect(() => {
          const currentToast = store.toastMessage();
          if (toastTimer !== null) {
            clearTimeout(toastTimer);
            toastTimer = null;
          }
          if (currentToast) {
            toastTimer = setTimeout(() => {
              toastTimer = null;
              store.dismissToast();
            }, 4_000);
          }
        });

        destroyRef.onDestroy(() => {
          clearInterval(leaseClockId);
          if (toastTimer !== null) {
            clearTimeout(toastTimer);
            toastTimer = null;
          }
        });
      }

      store.initThemeFromPlatform();
      store.bootstrap();
    },
  }),
);

function statusProgressSortOrder(status: TaskStatus): number {
  switch (status) {
    case 'in_progress':
      return 0;
    case 'in_review':
      return 1;
    case 'blocked':
      return 2;
    case 'ready':
      return 3;
    case 'backlog':
      return 4;
    case 'done':
      return 5;
    default:
      return 6;
  }
}

function matchesSearchExpression(
  task: TaskModel,
  rawQuery: string,
  actors: Record<string, ActorModel>,
  objectives: Record<string, ObjectiveModel>,
  nowMs: number,
): boolean {
  const tokens = rawQuery.split(/\s+/).filter(Boolean);
  const leaseActive = isTaskLeaseActive(task, nowMs);
  const holder = leaseActive && task.claimedByActorId ? (actors[task.claimedByActorId]?.handle ?? '') : '';
  const assignee = task.assigneeActorId ? (actors[task.assigneeActorId]?.handle ?? '') : '';
  const objTitle = task.objectiveId ? (objectives[task.objectiveId]?.title ?? '') : '';
  const tagsJoined = task.tags.join(' ');
  const haystack = `${task.identifier} ${task.externalId} ${task.title} ${task.description} ${task.acceptanceCriteria} ${objTitle} ${holder} ${assignee} ${tagsJoined}`.toLowerCase();

  for (const tok of tokens) {
    if (tok.startsWith('state:') || tok.startsWith('status:')) {
      const rawVal = tok.slice(tok.indexOf(':') + 1).replace(/-/g, '_');
      const val = rawVal === 'canceled' ? 'cancelled' : rawVal === 'todo' ? 'ready' : rawVal;
      if (val && task.status !== val) {
        return false;
      }
      continue;
    }
    if (tok.startsWith('ext:')) {
      const extVal = tok.slice(4).toLowerCase();
      if (extVal && task.externalId.toLowerCase() !== extVal) {
        return false;
      }
      continue;
    }
    if (tok === 'is:md' || tok === 'has:md') {
      if (!task.hasMarkdownDoc) return false;
      continue;
    }
    if (tok === 'is:blocked') {
      if (task.status !== 'blocked') return false;
      continue;
    }
    if (tok === 'is:claimed') {
      if (!leaseActive) return false;
      continue;
    }
    if (tok === 'is:glowing' || tok === 'is:signal') {
      if (!isTaskSignalGlowing(task, nowMs)) return false;
      continue;
    }
    if (tok.startsWith('@') && tok.length > 1) {
      const actorTerm = tok.slice(1);
      if (!holder.toLowerCase().includes(actorTerm) && !assignee.toLowerCase().includes(actorTerm)) {
        return false;
      }
      continue;
    }
    if (!haystack.includes(tok.replace(/^#/, ''))) {
      return false;
    }
  }
  return true;
}
