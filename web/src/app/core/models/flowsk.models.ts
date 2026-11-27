/**
 * Canonical domain models, view projections, real-time stream contracts, and
 * ConnectRPC wire DTOs for Flowsk Web.
 * Zero host filesystem paths are modeled or transmitted.
 */

export type TaskStatus =
  | 'backlog'
  | 'ready'
  | 'in_progress'
  | 'in_review'
  | 'blocked'
  | 'done'
  | 'cancelled'
  | 'archived';

export type TaskKind = 'TASK_KIND_TASK' | 'TASK_KIND_BUG' | 'TASK_KIND_FEATURE' | 'TASK_KIND_ISSUE' | 'TASK_KIND_EPIC';

export type TaskPriority =
  | 'TASK_PRIORITY_URGENT'
  | 'TASK_PRIORITY_HIGH'
  | 'TASK_PRIORITY_MEDIUM'
  | 'TASK_PRIORITY_LOW'
  | 'TASK_PRIORITY_NONE';

export type ObjectiveKind = 'goal' | 'milestone' | 'cycle';

export type ObjectiveStatus = 'planned' | 'active' | 'at_risk' | 'achieved' | 'dropped';

export type ViewMode = 'kanban' | 'grid';

export type ScopeTab = 'board' | 'goals' | 'backlog' | 'all' | 'archived';

export type StreamConnectionState = 'connecting' | 'live' | 'reconnecting' | 'paused';

export interface LiveStreamEventModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly actorId: string;
  readonly actorHandle?: string;
  readonly actorKind?: string;
  readonly action: string;
  readonly resourceType: string;
  readonly resourceId: string;
  readonly occurredAt: string;
}

export interface RuntimeBootstrapConfig {
  readonly workspaceId?: string;
  readonly workspaceSlug: string;
  readonly projectSlug?: string;
  readonly actorId?: string;
  readonly actorHandle?: string;
  readonly authRequired?: boolean;
  readonly authenticated?: boolean;
}

export interface ProjectBindingModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly parentProjectId: string;
  readonly childProjectId: string;
  readonly scopeSubpath: string;
}

export interface ProjectTaskCounts {
  readonly total: number;
  readonly active: number;
  readonly backlog: number;
  readonly ready: number;
  readonly inProgress: number;
  readonly inReview: number;
  readonly blocked: number;
  readonly done: number;
  readonly archived: number;
}

export interface ProjectHierarchyNode {
  readonly project: ProjectModel;
  readonly lineageKey: string;
  readonly depth: number;
  readonly scopeSubpath: string;
  readonly parentProjectId: string | null;
  readonly parentKeyPrefix: string | null;
  readonly counts: ProjectTaskCounts;
  readonly children: readonly ProjectHierarchyNode[];
}

export interface WorkspaceModel {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  readonly description: string;
}

export interface ActorModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly handle: string;
  readonly displayName: string;
  readonly kind: string;
}

export interface ProjectModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly keyPrefix: string;
  readonly slug: string;
  readonly name: string;
  readonly description: string;
  readonly status: string;
  readonly issueCounter: number;
  readonly hasMarkdownDoc: boolean;
  readonly updatedAt: string;
}

export interface WorkflowStateModel {
  readonly id: string;
  readonly projectId: string;
  readonly name: string;
  readonly statusKey: TaskStatus;
  readonly category: string;
  readonly colorHex: string;
  readonly position: number;
  readonly isDefault: boolean;
}

export interface ObjectiveModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly projectId: string;
  readonly kind: ObjectiveKind;
  readonly status: ObjectiveStatus;
  readonly title: string;
  readonly description: string;
  readonly successCriteria: readonly string[];
  readonly targetDate?: string;
  readonly closedAt?: string;
  readonly updatedAt?: string;
}

export interface ObjectiveProgressView extends ObjectiveModel {
  readonly projectKeyPrefix: string;
  readonly projectName: string;
  readonly tasks: readonly TaskModel[];
  readonly totalCount: number;
  readonly doneCount: number;
  readonly inProgressCount: number;
  readonly inReviewCount: number;
  readonly blockedCount: number;
  readonly todoCount: number;
  readonly backlogCount: number;
  readonly activeClaimsCount: number;
  readonly progressPct: number;
  readonly activeWorkPct: number;
  readonly blockedPct: number;
}

export interface TaskModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly projectId: string;
  readonly identifier: string;
  readonly externalId: string;
  readonly sequenceNumber: number;
  readonly kind: TaskKind;
  readonly priority: TaskPriority;
  readonly priorityRank: number;
  readonly workflowStateId: string;
  readonly status: TaskStatus;
  readonly title: string;
  readonly description: string;
  readonly acceptanceCriteria: string;
  readonly objectiveId?: string;
  readonly parentTaskId?: string;
  readonly assigneeActorId?: string;
  readonly claimedByActorId?: string;
  readonly claimExpiresAt?: string;
  readonly labelIds: readonly string[];
  readonly tags: readonly string[];
  readonly hasMarkdownDoc: boolean;
  readonly createdAt: string;
  readonly updatedAt: string;
  readonly completedAt?: string;
  readonly lastSignalAt?: string;
}

export interface TaskRelationModel {
  readonly id: string;
  readonly sourceTaskId: string;
  readonly targetTaskId: string;
  readonly relationType: string;
}

export interface TaskCommentModel {
  readonly id: string;
  readonly taskId: string;
  readonly authorActorId: string;
  readonly kind: string;
  readonly body: string;
  readonly createdAt: string;
}

export interface OrphanResourcesReport {
  readonly orphanTaskFilesCount: number;
  readonly missingTaskFilesCount: number;
  readonly orphanProjectFilesCount: number;
  readonly missingProjectFilesCount: number;
  readonly totalIssues: number;
  readonly issueSummaries: readonly string[];
}

export interface PruneResourcesResult {
  readonly removedTaskFilesCount: number;
  readonly clearedTaskRefsCount: number;
  readonly removedProjectFilesCount: number;
  readonly clearedProjectRefsCount: number;
  readonly totalCleaned: number;
  readonly dryRun: boolean;
  readonly cleanedSummaries: readonly string[];
}

export interface PurgeArchivedResult {
  readonly deletedTaskCount: number;
  readonly deletedMarkdownCount: number;
  readonly deletedTaskKeys: readonly string[];
}

export interface StatusColumnSpec {
  readonly status: TaskStatus;
  readonly label: string;
  readonly dotClass: string;
  readonly showOnActiveBoard: boolean;
}

export const CANONICAL_STATUS_COLUMNS: readonly StatusColumnSpec[] = [
  { status: 'backlog', label: 'Backlog', dotClass: 'bg-zinc-400', showOnActiveBoard: true },
  { status: 'ready', label: 'Todo', dotClass: 'bg-sky-400', showOnActiveBoard: true },
  { status: 'in_progress', label: 'In Progress', dotClass: 'bg-amber-400', showOnActiveBoard: true },
  { status: 'in_review', label: 'In Review', dotClass: 'bg-violet-400', showOnActiveBoard: true },
  { status: 'blocked', label: 'Blocked', dotClass: 'bg-rose-500', showOnActiveBoard: true },
  { status: 'done', label: 'Done', dotClass: 'bg-emerald-400', showOnActiveBoard: true },
  { status: 'cancelled', label: 'Canceled', dotClass: 'bg-zinc-500', showOnActiveBoard: false },
  { status: 'archived', label: 'Archived', dotClass: 'bg-slate-500', showOnActiveBoard: false },
];

export function formatPriorityLabel(priority: TaskPriority): string {
  switch (priority) {
    case 'TASK_PRIORITY_URGENT':
      return 'P0 · Urgent';
    case 'TASK_PRIORITY_HIGH':
      return 'P1 · High';
    case 'TASK_PRIORITY_MEDIUM':
      return 'P2 · Medium';
    case 'TASK_PRIORITY_LOW':
      return 'P3 · Low';
    case 'TASK_PRIORITY_NONE':
      return 'Unprioritized';
    default:
      return 'P2 · Normal';
  }
}

export function formatShortPriority(priority: TaskPriority): string {
  switch (priority) {
    case 'TASK_PRIORITY_URGENT':
      return 'P0';
    case 'TASK_PRIORITY_HIGH':
      return 'P1';
    case 'TASK_PRIORITY_MEDIUM':
      return 'P2';
    case 'TASK_PRIORITY_LOW':
      return 'P3';
    case 'TASK_PRIORITY_NONE':
      return '—';
    default:
      return 'P2';
  }
}

/**
 * Duration (10 minutes in milliseconds) during which a recent agent/CLI/MCP signal
 * illuminates the pulsing green task glow on active workflow stages (`ready`, `in_progress`, `in_review`).
 */
export const TASK_SIGNAL_GLOW_WINDOW_MS = 10 * 60 * 1000;

/**
 * Returns true when a task carries an active, unexpired actor lease at `nowMs`.
 */
export function isTaskLeaseActive(
  task: Pick<TaskModel, 'claimedByActorId' | 'claimExpiresAt'>,
  nowMs: number,
): boolean {
  if (!task.claimedByActorId) {
    return false;
  }
  if (!task.claimExpiresAt) {
    return true;
  }
  const expiresMs = Date.parse(task.claimExpiresAt);
  return Number.isNaN(expiresMs) ? true : expiresMs > nowMs;
}

/**
 * Returns true strictly when `task.status` is in an active pre-completion stage
 * (`ready`, `in_progress`, or `in_review` — never `backlog`, `blocked`, `done`, `cancelled`,
 * or `archived`) AND `task.lastSignalAt` occurred within the last 10 minutes of `nowMs`.
 */
export function isTaskSignalGlowing(
  task: Pick<TaskModel, 'status' | 'lastSignalAt'>,
  nowMs: number,
): boolean {
  if (
    task.status !== 'ready' &&
    task.status !== 'in_progress' &&
    task.status !== 'in_review'
  ) {
    return false;
  }
  if (!task.lastSignalAt) {
    return false;
  }
  const signalMs = Date.parse(task.lastSignalAt);
  if (Number.isNaN(signalMs)) {
    return false;
  }
  return nowMs - signalMs <= TASK_SIGNAL_GLOW_WINDOW_MS;
}

/**
 * Formats a concise human-readable relative age for `lastSignalAt` relative to `nowMs`.
 */
export function formatSignalAge(lastSignalAt: string | undefined, nowMs: number): string {
  if (!lastSignalAt) {
    return '';
  }
  const signalMs = Date.parse(lastSignalAt);
  if (Number.isNaN(signalMs)) {
    return '';
  }
  const diffSec = Math.max(0, Math.floor((nowMs - signalMs) / 1000));
  if (diffSec < 45) {
    return 'just now';
  }
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) {
    return `${diffMin}m ago`;
  }
  const diffHours = Math.floor(diffMin / 60);
  if (diffHours < 24) {
    return `${diffHours}h ago`;
  }
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d ago`;
}

export function formatKindLabel(kind: TaskKind): string {
  switch (kind) {
    case 'TASK_KIND_EPIC':
      return 'Epic';
    case 'TASK_KIND_FEATURE':
      return 'Feature';
    case 'TASK_KIND_BUG':
      return 'Bug';
    case 'TASK_KIND_ISSUE':
      return 'Research';
    default:
      return 'Task';
  }
}

export function formatStatusLabel(status: TaskStatus): string {
  const found = CANONICAL_STATUS_COLUMNS.find((c) => c.status === status);
  return found ? found.label : status;
}

export function formatObjectiveKindLabel(kind: ObjectiveKind): string {
  switch (kind) {
    case 'milestone':
      return 'Milestone';
    case 'cycle':
      return 'Cycle';
    default:
      return 'Goal';
  }
}

export function formatObjectiveStatusLabel(status: ObjectiveStatus): string {
  switch (status) {
    case 'planned':
      return 'Planned';
    case 'active':
      return 'Active';
    case 'at_risk':
      return 'At Risk';
    case 'achieved':
      return 'Achieved';
    case 'dropped':
      return 'Dropped';
    default:
      return 'Active';
  }
}
