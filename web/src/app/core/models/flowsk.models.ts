/**
 * Canonical domain models, view projections, and ConnectRPC wire DTOs for Flowsk Web.
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

export type ViewMode = 'kanban' | 'grid';

export type ScopeTab = 'board' | 'backlog' | 'all' | 'archived';

export interface RuntimeBootstrapConfig {
  readonly workspaceId?: string;
  readonly workspaceSlug: string;
  readonly projectSlug?: string;
  readonly actorId?: string;
  readonly actorHandle?: string;
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
  readonly kind: 'goal' | 'milestone' | 'cycle';
  readonly status: string;
  readonly title: string;
  readonly description: string;
  readonly targetDate?: string;
}

export interface TaskModel {
  readonly id: string;
  readonly workspaceId: string;
  readonly projectId: string;
  readonly identifier: string;
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
  readonly hasMarkdownDoc: boolean;
  readonly createdAt: string;
  readonly updatedAt: string;
  readonly completedAt?: string;
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
    default:
      return 'P2';
  }
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
