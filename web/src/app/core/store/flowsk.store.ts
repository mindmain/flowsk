import { isPlatformBrowser } from '@angular/common';
import { PLATFORM_ID, computed, inject } from '@angular/core';
import { tapResponse } from '@ngrx/operators';
import { patchState, signalStore, withComputed, withHooks, withMethods, withState } from '@ngrx/signals';
import { rxMethod } from '@ngrx/signals/rxjs-interop';
import { forkJoin, of, pipe, switchMap } from 'rxjs';
import { SanitizedApiError } from '../http/flowsk-api.interceptor';
import {
  ActorModel,
  CANONICAL_STATUS_COLUMNS,
  ObjectiveModel,
  OrphanResourcesReport,
  ProjectBindingModel,
  ProjectHierarchyNode,
  ProjectModel,
  ProjectTaskCounts,
  PruneResourcesResult,
  PurgeArchivedResult,
  ScopeTab,
  TaskCommentModel,
  TaskKind,
  TaskModel,
  TaskPriority,
  TaskRelationModel,
  TaskStatus,
  ViewMode,
  WorkflowStateModel,
  WorkspaceModel,
} from '../models/flowsk.models';
import { FlowskHeaderContextService } from '../services/flowsk-header-context.service';
import { FlowskRpcService } from '../services/flowsk-rpc.service';

export interface UiActionPayload<P, R = void> {
  readonly payload: P;
  readonly onSuccess?: (result: R) => void;
  readonly onError?: (message: string) => void;
}

export interface FlowskState {
  readonly initialized: boolean;
  readonly loading: boolean;
  readonly saving: boolean;
  readonly errorBanner: string | null;
  readonly toastMessage: string | null;
  readonly darkMode: boolean;

  readonly workspaceId: string;
  readonly workspaceSlug: string;
  readonly actorId: string;
  readonly actorHandle: string;

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
  errorBanner: null,
  toastMessage: null,
  darkMode: true,

  workspaceId: '',
  workspaceSlug: 'default',
  actorId: '',
  actorHandle: 'web-operator',

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
 * Root singleton SignalStore orchestrating all Flowsk Web UI state, filtering,
 * drag-and-drop workflow transitions, Markdown document persistence, archive
 * purging, and orphan pruning.
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

    const projectScopedTasks = computed(() => {
      const pid = store.activeProjectId();
      const all = store.workspaceTasks();
      if (store.scopeTab() === 'archived') {
        // Archive tab shows archived tasks (globally across workspace or focused project)
        return all.filter((t) => t.status === 'archived');
      }
      if (!pid) {
        return all.filter((t) => t.status !== 'archived');
      }
      return all.filter((t) => t.projectId === pid);
    });

    const workspaceArchivedCount = computed(() =>
      store.workspaceTasks().filter((t) => t.status === 'archived').length,
    );

    const projectBacklogCount = computed(() => {
      const pid = store.activeProjectId();
      return store
        .workspaceTasks()
        .filter((t) => (!pid || t.projectId === pid) && t.status === 'backlog').length;
    });

    const projectActiveCount = computed(() => {
      const pid = store.activeProjectId();
      return store
        .workspaceTasks()
        .filter(
          (t) =>
            (!pid || t.projectId === pid) &&
            t.status !== 'archived' &&
            t.status !== 'cancelled',
        ).length;
    });

    const filteredTasks = computed(() => {
      const scope = store.scopeTab();
      const query = store.searchQuery().trim().toLowerCase();
      const stFilter = store.statusFilter();
      const prioFilter = store.priorityFilter();
      const objFilter = store.objectiveFilterId();
      const actors = actorMapById();
      const objs = objectiveMapById();

      return projectScopedTasks()
        .filter((t) => {
          if (scope === 'archived') {
            if (t.status !== 'archived') return false;
          } else if (scope === 'backlog') {
            if (t.status !== 'backlog') return false;
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
            return matchesSearchExpression(t, query, actors, objs);
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

    const objectivesWithProgress = computed(() => {
      const pid = store.activeProjectId();
      const pTasks = store
        .workspaceTasks()
        .filter((t) => (!pid || t.projectId === pid) && t.status !== 'archived');
      return store.objectives().map((o) => {
        const linked = pTasks.filter((t) => t.objectiveId === o.id);
        const done = linked.filter((t) => t.status === 'done').length;
        const total = linked.length;
        const pct = total > 0 ? Math.round((done * 100) / total) : 0;
        return {
          ...o,
          doneCount: done,
          totalCount: total,
          progressPct: pct,
        };
      });
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
      ): ProjectHierarchyNode => {
        visitedGlobal.add(proj.id);
        const nextPath = new Set(ancestorPath);
        nextPath.add(proj.id);

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
          children.push(buildNode(childProj, depth + 1, edge.scopeSubpath, proj.id, nextPath));
        }

        return {
          project: proj,
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
        buildNode(root, 0, '', null, new Set<string>()),
      );

      // Defensive fallback so any unvisited project is still surfaced cleanly as a root.
      for (const p of visible) {
        if (!visitedGlobal.has(p.id)) {
          roots.push(buildNode(p, 0, '', null, new Set<string>()));
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
      selectedTask,
    };
  }),
  withMethods((store) => {
    const rpc = inject(FlowskRpcService);
    const headerCtx = inject(FlowskHeaderContextService);
    const platformId = inject(PLATFORM_ID);

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

    const resolveStateIdForStatus = (projectId: string, targetStatus: TaskStatus): string | null => {
      const matching = store
        .workflowStates()
        .find((st) => st.projectId === projectId && st.statusKey === targetStatus);
      return matching?.id ?? null;
    };

    const refreshProjectContext = rxMethod<{ projectId: string | null }>(
      pipe(
        switchMap(({ projectId }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          if (!wsId || !actorId) {
            return of(null);
          }
          patchState(store, { loading: true, errorBanner: null });

          const projects$ = rpc.listProjects(wsId, actorId);
          const bindings$ = rpc.listProjectBindings(wsId, actorId);
          const tasks$ = rpc.listTasks(wsId, actorId);
          const orphans$ = rpc.inspectOrphanResources(wsId, actorId);
          const states$ = projectId ? rpc.listWorkflowStates(wsId, actorId, projectId) : of([] as WorkflowStateModel[]);
          const objs$ = projectId ? rpc.listObjectives(wsId, actorId, projectId) : of([] as ObjectiveModel[]);

          return forkJoin({
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
                patchState(store, {
                  loading: false,
                  errorBanner: extractErrorMessage(err),
                });
              },
            }),
          );
        }),
      ),
    );

    const bootstrap = rxMethod<void>(
      pipe(
        switchMap(() => {
          patchState(store, { loading: true, errorBanner: null });
          return rpc.getBootstrapConfig().pipe(
            switchMap((boot) => {
              const wsSlug = (boot.workspaceSlug || 'default').trim();
              const handle = (boot.actorHandle || 'web-operator').trim();
              headerCtx.setIdentity(boot.workspaceId || wsSlug, handle);

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
                      headerCtx.setIdentity(wsId, actor.handle);
                      return forkJoin({
                        actor: of(actor),
                        boot: of(boot),
                        resolvedSlug: of(wsInfo.wsSlug),
                        workspaces: wsInfo.cachedWorkspaces ? of(wsInfo.cachedWorkspaces) : rpc.listWorkspaces(),
                        actors: rpc.listActors(wsId, actorId),
                        projects: rpc.listProjects(wsId, actorId),
                        bindings: rpc.listProjectBindings(wsId, actorId),
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
                  objectives: [] as ObjectiveModel[],
                });
              }
              return forkJoin({
                states: rpc.listWorkflowStates(ctx.actor.workspaceId, ctx.actor.id, chosen.id),
                objectives: rpc.listObjectives(ctx.actor.workspaceId, ctx.actor.id, chosen.id),
              }).pipe(
                switchMap((projBundle) =>
                  of({
                    ...ctx,
                    chosenId: chosen.id,
                    openedFromBoundFolder: true,
                    states: projBundle.states,
                    objectives: projBundle.objectives,
                  }),
                ),
              );
            }),
            tapResponse({
              next: (res) => {
                patchState(store, {
                  initialized: true,
                  loading: false,
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
              },
              error: (err: unknown) => {
                patchState(store, {
                  initialized: true,
                  loading: false,
                  errorBanner: extractErrorMessage(err),
                });
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
          patchState(store, {
            selectedTaskId: taskId,
            selectedTaskMarkdownLoading: true,
          });
          return forkJoin({
            detail: rpc.getTaskDetail(wsId, actorId, taskId),
            comments: rpc.listTaskComments(wsId, actorId, taskId),
            md: rpc.getTaskMarkdown(wsId, actorId, taskId),
          }).pipe(
            tapResponse({
              next: (res) => {
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
                patchState(store, {
                  selectedTaskMarkdownLoading: false,
                  errorBanner: extractErrorMessage(err),
                });
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
        switchMap(({ payload, onSuccess, onError }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const existing = store.workspaceTasks().find((t) => t.id === payload.taskId);
          if (!existing || !wsId || !actorId) {
            return of(null);
          }
          if (existing.status === payload.targetStatus) {
            return of(null);
          }

          const previousTasks = store.workspaceTasks();
          // Optimistic patch for snappy Kanban drag & drop UX
          patchState(store, {
            workspaceTasks: previousTasks.map((t) =>
              t.id === payload.taskId ? { ...t, status: payload.targetStatus } : t,
            ),
            errorBanner: null,
          });

          const knownStateId = resolveStateIdForStatus(existing.projectId, payload.targetStatus);
          const stateId$ = knownStateId
            ? of(knownStateId)
            : rpc
                .listWorkflowStates(wsId, actorId, existing.projectId)
                .pipe(switchMap((sts) => of(sts.find((s) => s.statusKey === payload.targetStatus)?.id ?? '')));

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
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === updated.id ? updated : t)),
                  toastMessage: `${updated.identifier} moved to ${payload.targetStatus.replace('_', ' ')}`,
                });
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, {
                  workspaceTasks: previousTasks,
                  errorBanner: msg,
                });
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
        switchMap(({ payload, onSuccess, onError }) => {
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
                  return of({ created, binding: null as ProjectBindingModel | null });
                }
                return rpc
                  .bindSubproject({
                    workspaceId: wsId,
                    actorId,
                    parentProjectId: parentId,
                    childProjectId: created.id,
                    scopeSubpath: (payload.scopeSubpath ?? '').trim(),
                  })
                  .pipe(switchMap((binding) => of({ created, binding })));
              }),
              tapResponse({
                next: ({ created, binding }) => {
                  patchState(store, {
                    saving: false,
                    projects: [...store.projects(), created],
                    projectBindings: binding ? [...store.projectBindings(), binding] : store.projectBindings(),
                    activeProjectId: created.id,
                    scopeTab: 'board',
                    toastMessage: binding
                      ? `Subproject ${created.keyPrefix} created and bound`
                      : `Project ${created.keyPrefix} created`,
                  });
                  refreshProjectContext({ projectId: created.id });
                  onSuccess?.(created);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  onError?.(msg);
                },
              }),
            );
        }),
      ),
    );

    const archiveProject = rxMethod<UiActionPayload<{ projectId: string }, ProjectModel>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
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
                refreshProjectContext({ projectId: nextActive });
                onSuccess?.(archived);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { saving: false, errorBanner: msg });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const loadProjectMarkdown = rxMethod<{ projectId: string }>(
      pipe(
        switchMap(({ projectId }) =>
          rpc.getProjectMarkdown(store.workspaceId(), store.actorId(), projectId).pipe(
            tapResponse({
              next: (res) => {
                patchState(store, { activeProjectMarkdown: res.markdown });
              },
              error: (err: unknown) => {
                patchState(store, { errorBanner: extractErrorMessage(err) });
              },
            }),
          ),
        ),
      ),
    );

    const saveProjectMarkdown = rxMethod<
      UiActionPayload<{ projectId: string; markdown: string }, ProjectModel>
    >(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
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
                  projects: store.projects().map((p) => (p.id === proj.id ? proj : p)),
                  toastMessage: `Project Markdown saved in ~/.flowsk/projects`,
                });
                onSuccess?.(proj);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { saving: false, errorBanner: msg });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const createObjective = rxMethod<
      UiActionPayload<{ title: string; kind: 'goal' | 'milestone' | 'cycle'; description: string }, ObjectiveModel>
    >(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
          const pid = store.activeProjectId();
          if (!pid) return of(null);
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .createObjective({
              workspaceId: store.workspaceId(),
              actorId: store.actorId(),
              projectId: pid,
              kind: payload.kind,
              title: payload.title.trim(),
              description: payload.description.trim(),
            })
            .pipe(
              tapResponse({
                next: (created) => {
                  patchState(store, {
                    saving: false,
                    objectives: [...store.objectives(), created],
                    toastMessage: `Objective '${created.title}' added`,
                  });
                  onSuccess?.(created);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  onError?.(msg);
                },
              }),
            );
        }),
      ),
    );

    const closeObjective = rxMethod<UiActionPayload<{ objectiveId: string; achieved: boolean }, ObjectiveModel>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) =>
          rpc.closeObjective(store.workspaceId(), store.actorId(), payload.objectiveId, payload.achieved).pipe(
            tapResponse({
              next: (closed) => {
                patchState(store, {
                  objectives: store.objectives().map((o) => (o.id === closed.id ? closed : o)),
                  toastMessage: `Objective closed`,
                });
                onSuccess?.(closed);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { errorBanner: msg });
                onError?.(msg);
              },
            }),
          ),
        ),
      ),
    );

    const createTask = rxMethod<
      UiActionPayload<
        {
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
        switchMap(({ payload, onSuccess, onError }) => {
          const pid = store.activeProjectId();
          if (!pid) return of(null);
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const stateId = resolveStateIdForStatus(pid, payload.initialStatus) ?? undefined;
          patchState(store, { saving: true, errorBanner: null });
          return rpc
            .createTask({
              workspaceId: wsId,
              actorId,
              projectId: pid,
              title: payload.title.trim(),
              description: payload.description.trim(),
              acceptanceCriteria: payload.acceptanceCriteria.trim(),
              kind: payload.kind,
              priority: payload.priority,
              workflowStateId: stateId,
              objectiveId: payload.objectiveId || undefined,
            })
            .pipe(
              switchMap((created) => {
                const md = (payload.markdownDoc ?? '').trim();
                if (!md) {
                  return of(created);
                }
                return rpc.putTaskMarkdown(wsId, actorId, created.id, md);
              }),
              tapResponse({
                next: (created) => {
                  patchState(store, {
                    saving: false,
                    workspaceTasks: [created, ...store.workspaceTasks()],
                    toastMessage: `Created ${created.identifier}`,
                  });
                  onSuccess?.(created);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  onError?.(msg);
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
        switchMap(({ payload, onSuccess, onError }) => {
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
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  onError?.(msg);
                },
              }),
            );
        }),
      ),
    );

    const saveTaskMarkdown = rxMethod<UiActionPayload<{ taskId: string; markdown: string }, TaskModel>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
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
                  onSuccess?.(updated);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { saving: false, errorBanner: msg });
                  onError?.(msg);
                },
              }),
            );
        }),
      ),
    );

    const deleteTaskMarkdown = rxMethod<UiActionPayload<{ taskId: string }, TaskModel>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
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
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { saving: false, errorBanner: msg });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const toggleTaskClaim = rxMethod<UiActionPayload<{ taskId: string; claim: boolean }, TaskModel>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
          const wsId = store.workspaceId();
          const actorId = store.actorId();
          const req$ = payload.claim
            ? rpc.claimTask(wsId, actorId, payload.taskId)
            : rpc.releaseTaskClaim(wsId, actorId, payload.taskId, '');
          return req$.pipe(
            tapResponse({
              next: (updated) => {
                patchState(store, {
                  workspaceTasks: store
                    .workspaceTasks()
                    .map((t) => (t.id === updated.id ? updated : t)),
                  toastMessage: payload.claim
                    ? `Claimed ${updated.identifier}`
                    : `Released lease on ${updated.identifier}`,
                });
                onSuccess?.(updated);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { errorBanner: msg });
                onError?.(msg);
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
        switchMap(({ payload, onSuccess, onError }) =>
          rpc
            .addTaskComment(store.workspaceId(), store.actorId(), payload.taskId, payload.kind, payload.body)
            .pipe(
              tapResponse({
                next: (comment) => {
                  patchState(store, {
                    selectedTaskComments: [...store.selectedTaskComments(), comment],
                  });
                  onSuccess?.(comment);
                },
                error: (err: unknown) => {
                  const msg = extractErrorMessage(err);
                  patchState(store, { errorBanner: msg });
                  onError?.(msg);
                },
              }),
            ),
        ),
      ),
    );

    const purgeArchivedTasks = rxMethod<UiActionPayload<void, PurgeArchivedResult>>(
      pipe(
        switchMap(({ onSuccess, onError }) => {
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
                onSuccess?.(res);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { saving: false, errorBanner: msg });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    const inspectOrphans = rxMethod<void>(
      pipe(
        switchMap(() =>
          rpc.inspectOrphanResources(store.workspaceId(), store.actorId()).pipe(
            tapResponse({
              next: (report) => patchState(store, { orphanReport: report }),
              error: (err: unknown) => patchState(store, { errorBanner: extractErrorMessage(err) }),
            }),
          ),
        ),
      ),
    );

    const pruneOrphans = rxMethod<UiActionPayload<{ dryRun: boolean }, PruneResourcesResult>>(
      pipe(
        switchMap(({ payload, onSuccess, onError }) => {
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
                onSuccess?.(pruneRes);
              },
              error: (err: unknown) => {
                const msg = extractErrorMessage(err);
                patchState(store, { saving: false, errorBanner: msg });
                onError?.(msg);
              },
            }),
          );
        }),
      ),
    );

    return {
      bootstrap,
      refreshProjectContext,
      loadTaskInspector,
      moveTaskToStatus,
      createProject,
      archiveProject,
      loadProjectMarkdown,
      saveProjectMarkdown,
      createObjective,
      closeObjective,
      createTask,
      updateTaskDetails,
      saveTaskMarkdown,
      deleteTaskMarkdown,
      toggleTaskClaim,
      addComment,
      purgeArchivedTasks,
      inspectOrphans,
      pruneOrphans,

      selectProject(projectId: string | null): void {
        const nextTab = store.scopeTab() === 'archived' ? 'board' : store.scopeTab();
        patchState(store, {
          activeProjectId: projectId,
          scopeTab: nextTab,
          objectiveFilterId: 'all',
          selectedTaskId: null,
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
        patchState(store, { selectedTaskId: null });
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
      store.initThemeFromPlatform();
      store.bootstrap();
    },
  }),
);

function matchesSearchExpression(
  task: TaskModel,
  rawQuery: string,
  actors: Record<string, ActorModel>,
  objectives: Record<string, ObjectiveModel>,
): boolean {
  const tokens = rawQuery.split(/\s+/).filter(Boolean);
  const holder = task.claimedByActorId ? actors[task.claimedByActorId]?.handle ?? '' : '';
  const assignee = task.assigneeActorId ? actors[task.assigneeActorId]?.handle ?? '' : '';
  const objTitle = task.objectiveId ? objectives[task.objectiveId]?.title ?? '' : '';
  const haystack = `${task.identifier} ${task.title} ${task.description} ${task.acceptanceCriteria} ${objTitle} ${holder} ${assignee}`.toLowerCase();

  for (const tok of tokens) {
    if (tok.startsWith('state:') || tok.startsWith('status:')) {
      const val = tok.slice(tok.indexOf(':') + 1).replace('-', '_');
      if (val && task.status !== val && !(val === 'todo' && task.status === 'ready')) {
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
      if (!task.claimedByActorId) return false;
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
