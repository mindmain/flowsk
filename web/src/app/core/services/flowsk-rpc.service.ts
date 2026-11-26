import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable, map } from 'rxjs';
import {
  ActorModel,
  ObjectiveKind,
  ObjectiveModel,
  ObjectiveStatus,
  OrphanResourcesReport,
  ProjectBindingModel,
  ProjectModel,
  PruneResourcesResult,
  PurgeArchivedResult,
  RuntimeBootstrapConfig,
  TaskCommentModel,
  TaskKind,
  TaskModel,
  TaskPriority,
  TaskRelationModel,
  TaskStatus,
  WorkflowStateModel,
  WorkspaceModel,
} from '../models/flowsk.models';

interface RawWorkspaceDto {
  id?: string;
  slug?: string;
  name?: string;
  description?: string;
}

interface RawActorDto {
  id?: string;
  workspaceId?: string;
  handle?: string;
  displayName?: string;
  kind?: string;
}

interface RawProjectDto {
  id?: string;
  workspaceId?: string;
  keyPrefix?: string;
  slug?: string;
  name?: string;
  description?: string;
  status?: string;
  issueCounter?: string | number;
  hasMarkdownDoc?: boolean;
  updatedAt?: string;
}

interface RawProjectBindingDto {
  id?: string;
  workspaceId?: string;
  parentProjectId?: string;
  childProjectId?: string;
  scopeSubpath?: string;
}

interface RawWorkflowStateDto {
  id?: string;
  projectId?: string;
  name?: string;
  category?: string;
  colorHex?: string;
  position?: number;
  isDefault?: boolean;
}

interface RawObjectiveDto {
  id?: string;
  workspaceId?: string;
  projectId?: string;
  kind?: string;
  status?: string;
  title?: string;
  description?: string;
  successCriteria?: string;
  targetDate?: string;
  closedAt?: string;
  updatedAt?: string;
}

interface RawTaskDto {
  id?: string;
  workspaceId?: string;
  projectId?: string;
  identifier?: string;
  externalId?: string;
  sequenceNumber?: string | number;
  kind?: TaskKind;
  priority?: TaskPriority;
  workflowStateId?: string;
  stateCategory?: string;
  status?: string;
  title?: string;
  description?: string;
  acceptanceCriteria?: string;
  objectiveId?: string;
  parentTaskId?: string;
  assigneeActorId?: string;
  claimedByActorId?: string;
  claimExpiresAt?: string;
  labelIds?: string[];
  hasMarkdownDoc?: boolean;
  createdAt?: string;
  updatedAt?: string;
  completedAt?: string;
}

/**
 * Typed ConnectRPC JSON transport service consumed exclusively by FlowskStore.
 */
@Injectable({ providedIn: 'root' })
export class FlowskRpcService {
  private readonly http = inject(HttpClient);

  getBootstrapConfig(): Observable<RuntimeBootstrapConfig> {
    return this.http.get<RuntimeBootstrapConfig>('/api/ui-config');
  }

  registerActor(workspaceRef: string, handle: string): Observable<ActorModel> {
    return this.http
      .post<{ actor?: RawActorDto }>('/flowsk.workspace.v1.WorkspaceService/RegisterActor', {
        workspaceId: workspaceRef,
        kind: 'ACTOR_KIND_HUMAN',
        handle,
        displayName: handle,
      })
      .pipe(map((res) => mapActor(res.actor)));
  }

  listWorkspaces(): Observable<WorkspaceModel[]> {
    return this.http
      .post<{ workspaces?: RawWorkspaceDto[] }>('/flowsk.workspace.v1.WorkspaceService/ListWorkspaces', {
        pageSize: 100,
      })
      .pipe(map((res) => (res.workspaces ?? []).map(mapWorkspace)));
  }

  listActors(workspaceId: string, actorId: string): Observable<ActorModel[]> {
    return this.http
      .post<{ actors?: RawActorDto[] }>('/flowsk.workspace.v1.WorkspaceService/ListActors', {
        workspaceId,
        actorId,
        pageSize: 100,
      })
      .pipe(map((res) => (res.actors ?? []).map(mapActor)));
  }

  listProjects(workspaceId: string, actorId: string): Observable<ProjectModel[]> {
    return this.http
      .post<{ projects?: RawProjectDto[] }>('/flowsk.project.v1.ProjectService/ListProjects', {
        workspaceId,
        actorId,
        includeArchived: true,
        pageSize: 200,
      })
      .pipe(map((res) => (res.projects ?? []).map(mapProject)));
  }

  createProject(params: {
    workspaceId: string;
    actorId: string;
    keyPrefix: string;
    slug: string;
    name: string;
    description: string;
  }): Observable<ProjectModel> {
    return this.http
      .post<{ project?: RawProjectDto }>('/flowsk.project.v1.ProjectService/CreateProject', {
        ...params,
        status: 'PROJECT_STATUS_ACTIVE',
        seedDefaultWorkflowStates: true,
      })
      .pipe(map((res) => mapProject(res.project)));
  }

  updateProject(params: {
    workspaceId: string;
    actorId: string;
    projectId: string;
    name?: string;
    description?: string;
    status?: string;
  }): Observable<ProjectModel> {
    return this.http
      .post<{ project?: RawProjectDto }>('/flowsk.project.v1.ProjectService/UpdateProject', params)
      .pipe(map((res) => mapProject(res.project)));
  }

  archiveProject(workspaceId: string, actorId: string, projectId: string): Observable<ProjectModel> {
    return this.http
      .post<{ project?: RawProjectDto }>('/flowsk.project.v1.ProjectService/ArchiveProject', {
        workspaceId,
        actorId,
        projectId,
      })
      .pipe(map((res) => mapProject(res.project)));
  }

  listProjectBindings(workspaceId: string, actorId: string): Observable<ProjectBindingModel[]> {
    return this.http
      .post<{ bindings?: RawProjectBindingDto[] }>('/flowsk.project.v1.ProjectService/ListProjectBindings', {
        workspaceId,
        actorId,
      })
      .pipe(map((res) => (res.bindings ?? []).map(mapProjectBinding)));
  }

  bindSubproject(params: {
    workspaceId: string;
    actorId: string;
    parentProjectId: string;
    childProjectId: string;
    scopeSubpath?: string;
  }): Observable<ProjectBindingModel> {
    return this.http
      .post<{ binding?: RawProjectBindingDto }>('/flowsk.project.v1.ProjectService/BindSubproject', {
        workspaceId: params.workspaceId,
        actorId: params.actorId,
        parentProjectId: params.parentProjectId,
        childProjectId: params.childProjectId,
        scopeSubpath: params.scopeSubpath ?? '',
      })
      .pipe(map((res) => mapProjectBinding(res.binding)));
  }

  unbindSubproject(params: {
    workspaceId: string;
    actorId: string;
    parentProjectId: string;
    childProjectId: string;
  }): Observable<boolean> {
    return this.http
      .post<{ unbound?: boolean }>('/flowsk.project.v1.ProjectService/UnbindSubproject', params)
      .pipe(map((res) => Boolean(res.unbound)));
  }

  listWorkflowStates(workspaceId: string, actorId: string, projectId: string): Observable<WorkflowStateModel[]> {
    return this.http
      .post<{ workflowStates?: RawWorkflowStateDto[] }>('/flowsk.project.v1.ProjectService/ListWorkflowStates', {
        workspaceId,
        actorId,
        projectId,
      })
      .pipe(map((res) => (res.workflowStates ?? []).map(mapWorkflowState)));
  }

  getProjectMarkdown(
    workspaceId: string,
    actorId: string,
    projectId: string,
  ): Observable<{ markdown: string; hasMarkdownDoc: boolean }> {
    return this.http
      .post<{ markdown?: string; hasMarkdownDoc?: boolean }>(
        '/flowsk.project.v1.ProjectService/GetProjectMarkdown',
        { workspaceId, actorId, projectId },
      )
      .pipe(
        map((res) => ({
          markdown: res.markdown ?? '',
          hasMarkdownDoc: Boolean(res.hasMarkdownDoc),
        })),
      );
  }

  putProjectMarkdown(
    workspaceId: string,
    actorId: string,
    projectId: string,
    markdown: string,
  ): Observable<ProjectModel> {
    return this.http
      .post<{ project?: RawProjectDto }>('/flowsk.project.v1.ProjectService/PutProjectMarkdown', {
        workspaceId,
        actorId,
        projectId,
        markdown,
      })
      .pipe(map((res) => mapProject(res.project)));
  }

  deleteProjectMarkdown(workspaceId: string, actorId: string, projectId: string): Observable<ProjectModel> {
    return this.http
      .post<{ project?: RawProjectDto }>('/flowsk.project.v1.ProjectService/DeleteProjectMarkdown', {
        workspaceId,
        actorId,
        projectId,
      })
      .pipe(map((res) => mapProject(res.project)));
  }

  listObjectives(workspaceId: string, actorId: string, projectId?: string | null): Observable<ObjectiveModel[]> {
    const body: Record<string, unknown> = {
      workspaceId,
      actorId,
      pageSize: 200,
    };
    if (projectId) {
      body['projectId'] = projectId;
    }
    return this.http
      .post<{ objectives?: RawObjectiveDto[] }>('/flowsk.objective.v1.ObjectiveService/ListObjectives', body)
      .pipe(map((res) => (res.objectives ?? []).map(mapObjective)));
  }

  createObjective(params: {
    workspaceId: string;
    actorId: string;
    projectId: string;
    kind: ObjectiveKind;
    status?: ObjectiveStatus;
    title: string;
    description: string;
    successCriteria?: string;
    targetDate?: string;
  }): Observable<ObjectiveModel> {
    const kindEnum = toObjectiveKindEnum(params.kind);
    const statusEnum = toObjectiveStatusEnum(params.status ?? 'active');
    const body: Record<string, unknown> = {
      workspaceId: params.workspaceId,
      actorId: params.actorId,
      projectId: params.projectId,
      kind: kindEnum,
      status: statusEnum,
      title: params.title,
      description: params.description,
      successCriteria: params.successCriteria?.trim() || params.description || params.title,
    };
    const isoTarget = toProtoTimestampIso(params.targetDate);
    if (isoTarget) {
      body['targetDate'] = isoTarget;
    }
    return this.http
      .post<{ objective?: RawObjectiveDto }>('/flowsk.objective.v1.ObjectiveService/CreateObjective', body)
      .pipe(map((res) => mapObjective(res.objective)));
  }

  updateObjective(params: {
    workspaceId: string;
    actorId: string;
    objectiveId: string;
    title?: string;
    description?: string;
    successCriteria?: string;
    status?: ObjectiveStatus;
    targetDate?: string;
  }): Observable<ObjectiveModel> {
    const body: Record<string, unknown> = {
      workspaceId: params.workspaceId,
      actorId: params.actorId,
      objectiveId: params.objectiveId,
    };
    if (params.title !== undefined) {
      body['title'] = params.title;
    }
    if (params.description !== undefined) {
      body['description'] = params.description;
    }
    if (params.successCriteria !== undefined) {
      body['successCriteria'] = params.successCriteria;
    }
    if (params.status !== undefined) {
      body['status'] = toObjectiveStatusEnum(params.status);
    }
    const isoTarget = toProtoTimestampIso(params.targetDate);
    if (isoTarget) {
      body['targetDate'] = isoTarget;
    }
    return this.http
      .post<{ objective?: RawObjectiveDto }>('/flowsk.objective.v1.ObjectiveService/UpdateObjective', body)
      .pipe(map((res) => mapObjective(res.objective)));
  }

  deleteObjective(workspaceId: string, actorId: string, objectiveId: string): Observable<string> {
    return this.http
      .post<{ deletedObjectiveId?: string }>('/flowsk.objective.v1.ObjectiveService/DeleteObjective', {
        workspaceId,
        actorId,
        objectiveId,
      })
      .pipe(map((res) => res.deletedObjectiveId ?? objectiveId));
  }

  closeObjective(
    workspaceId: string,
    actorId: string,
    objectiveId: string,
    achieved: boolean,
  ): Observable<ObjectiveModel> {
    return this.http
      .post<{ objective?: RawObjectiveDto }>('/flowsk.objective.v1.ObjectiveService/CloseObjective', {
        workspaceId,
        actorId,
        objectiveId,
        terminalStatus: achieved ? 'OBJECTIVE_STATUS_ACHIEVED' : 'OBJECTIVE_STATUS_DROPPED',
        resolutionSummary: achieved ? 'Completed from Flowsk Web UI' : 'Dropped from Flowsk Web UI',
      })
      .pipe(map((res) => mapObjective(res.objective)));
  }

  listTasks(workspaceId: string, actorId: string, projectId?: string): Observable<TaskModel[]> {
    const body: Record<string, unknown> = {
      workspaceId,
      actorId,
      pageSize: 200,
    };
    if (projectId) {
      body['projectId'] = projectId;
    }
    return this.http
      .post<{ tasks?: RawTaskDto[] }>('/flowsk.task.v1.TaskService/ListTasks', body)
      .pipe(map((res) => (res.tasks ?? []).map(mapTask)));
  }

  getTaskDetail(
    workspaceId: string,
    actorId: string,
    taskId: string,
  ): Observable<{ task: TaskModel; relations: TaskRelationModel[] }> {
    return this.http
      .post<{
        task?: RawTaskDto;
        relations?: Array<{ id?: string; sourceTaskId?: string; targetTaskId?: string; relationType?: string }>;
      }>('/flowsk.task.v1.TaskService/GetTask', {
        workspaceId,
        actorId,
        taskId,
      })
      .pipe(
        map((res) => ({
          task: mapTask(res.task),
          relations: (res.relations ?? []).map((r) => ({
            id: r.id ?? '',
            sourceTaskId: r.sourceTaskId ?? '',
            targetTaskId: r.targetTaskId ?? '',
            relationType: r.relationType ?? '',
          })),
        })),
      );
  }

  createTask(params: {
    workspaceId: string;
    actorId: string;
    projectId: string;
    title: string;
    description: string;
    acceptanceCriteria: string;
    kind: TaskKind;
    priority: TaskPriority;
    workflowStateId?: string;
    objectiveId?: string;
  }): Observable<TaskModel> {
    const body: Record<string, unknown> = {
      workspaceId: params.workspaceId,
      actorId: params.actorId,
      projectId: params.projectId,
      title: params.title,
      description: params.description,
      acceptanceCriteria: params.acceptanceCriteria,
      kind: params.kind,
      priority: params.priority,
    };
    if (params.workflowStateId) {
      body['workflowStateId'] = params.workflowStateId;
    }
    if (params.objectiveId) {
      body['objectiveId'] = params.objectiveId;
    }
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/CreateTask', body)
      .pipe(map((res) => mapTask(res.task)));
  }

  updateTask(params: {
    workspaceId: string;
    actorId: string;
    taskId: string;
    title?: string;
    description?: string;
    acceptanceCriteria?: string;
    kind?: TaskKind;
    priority?: TaskPriority;
    objectiveId?: string;
    clearObjective?: boolean;
  }): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/UpdateTask', params)
      .pipe(map((res) => mapTask(res.task)));
  }

  transitionTaskState(params: {
    workspaceId: string;
    actorId: string;
    taskId: string;
    workflowStateId: string;
    resolutionNote?: string;
    releaseClaim?: boolean;
  }): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/TransitionTaskState', {
        workspaceId: params.workspaceId,
        actorId: params.actorId,
        taskId: params.taskId,
        workflowStateId: params.workflowStateId,
        resolutionNote: params.resolutionNote ?? '',
        releaseClaim: Boolean(params.releaseClaim),
      })
      .pipe(map((res) => mapTask(res.task)));
  }

  claimTask(workspaceId: string, actorId: string, taskId: string): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/ClaimTask', {
        workspaceId,
        actorId,
        taskId,
        leaseTtlSeconds: 1800,
        forceTakeover: true,
      })
      .pipe(map((res) => mapTask(res.task)));
  }

  releaseTaskClaim(workspaceId: string, actorId: string, taskId: string, handoffNote: string): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/ReleaseTaskClaim', {
        workspaceId,
        actorId,
        taskId,
        handoffNote,
      })
      .pipe(map((res) => mapTask(res.task)));
  }

  listTaskComments(workspaceId: string, actorId: string, taskId: string): Observable<TaskCommentModel[]> {
    return this.http
      .post<{
        comments?: Array<{
          id?: string;
          taskId?: string;
          authorActorId?: string;
          kind?: string;
          body?: string;
          createdAt?: string;
        }>;
      }>('/flowsk.task.v1.TaskService/ListTaskComments', {
        workspaceId,
        actorId,
        taskId,
        pageSize: 100,
      })
      .pipe(
        map((res) =>
          (res.comments ?? []).map((c) => ({
            id: c.id ?? '',
            taskId: c.taskId ?? taskId,
            authorActorId: c.authorActorId ?? '',
            kind: c.kind ?? 'TASK_COMMENT_KIND_COMMENT',
            body: c.body ?? '',
            createdAt: c.createdAt ?? '',
          })),
        ),
      );
  }

  addTaskComment(
    workspaceId: string,
    actorId: string,
    taskId: string,
    kind: string,
    body: string,
  ): Observable<TaskCommentModel> {
    return this.http
      .post<{
        comment?: {
          id?: string;
          taskId?: string;
          authorActorId?: string;
          kind?: string;
          body?: string;
          createdAt?: string;
        };
      }>('/flowsk.task.v1.TaskService/AddTaskComment', {
        workspaceId,
        actorId,
        taskId,
        kind,
        body,
      })
      .pipe(
        map((res) => ({
          id: res.comment?.id ?? '',
          taskId: res.comment?.taskId ?? taskId,
          authorActorId: res.comment?.authorActorId ?? actorId,
          kind: res.comment?.kind ?? kind,
          body: res.comment?.body ?? body,
          createdAt: res.comment?.createdAt ?? new Date().toISOString(),
        })),
      );
  }

  getTaskMarkdown(
    workspaceId: string,
    actorId: string,
    taskId: string,
  ): Observable<{ markdown: string; hasMarkdownDoc: boolean }> {
    return this.http
      .post<{ markdown?: string; hasMarkdownDoc?: boolean }>('/flowsk.task.v1.TaskService/GetTaskMarkdown', {
        workspaceId,
        actorId,
        taskId,
      })
      .pipe(
        map((res) => ({
          markdown: res.markdown ?? '',
          hasMarkdownDoc: Boolean(res.hasMarkdownDoc),
        })),
      );
  }

  putTaskMarkdown(workspaceId: string, actorId: string, taskId: string, markdown: string): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/PutTaskMarkdown', {
        workspaceId,
        actorId,
        taskId,
        markdown,
      })
      .pipe(map((res) => mapTask(res.task)));
  }

  deleteTaskMarkdown(workspaceId: string, actorId: string, taskId: string): Observable<TaskModel> {
    return this.http
      .post<{ task?: RawTaskDto }>('/flowsk.task.v1.TaskService/DeleteTaskMarkdown', {
        workspaceId,
        actorId,
        taskId,
      })
      .pipe(map((res) => mapTask(res.task)));
  }

  purgeArchivedTasks(workspaceId: string, actorId: string): Observable<PurgeArchivedResult> {
    return this.http
      .post<{
        deletedTaskCount?: number;
        deletedMarkdownCount?: number;
        deletedTaskKeys?: string[];
      }>('/flowsk.task.v1.TaskService/PurgeArchivedTasks', {
        workspaceId,
        actorId,
      })
      .pipe(
        map((res) => ({
          deletedTaskCount: res.deletedTaskCount ?? 0,
          deletedMarkdownCount: res.deletedMarkdownCount ?? 0,
          deletedTaskKeys: res.deletedTaskKeys ?? [],
        })),
      );
  }

  inspectOrphanResources(workspaceId: string, actorId: string): Observable<OrphanResourcesReport> {
    return this.http
      .post<{
        orphanTaskFilesCount?: number;
        missingTaskFilesCount?: number;
        orphanProjectFilesCount?: number;
        missingProjectFilesCount?: number;
        totalIssues?: number;
        issueSummaries?: string[];
      }>('/flowsk.task.v1.TaskService/InspectOrphanResources', {
        workspaceId,
        actorId,
      })
      .pipe(
        map((res) => ({
          orphanTaskFilesCount: res.orphanTaskFilesCount ?? 0,
          missingTaskFilesCount: res.missingTaskFilesCount ?? 0,
          orphanProjectFilesCount: res.orphanProjectFilesCount ?? 0,
          missingProjectFilesCount: res.missingProjectFilesCount ?? 0,
          totalIssues: res.totalIssues ?? 0,
          issueSummaries: res.issueSummaries ?? [],
        })),
      );
  }

  pruneOrphanResources(workspaceId: string, actorId: string, dryRun: boolean): Observable<PruneResourcesResult> {
    return this.http
      .post<{
        removedTaskFilesCount?: number;
        clearedTaskRefsCount?: number;
        removedProjectFilesCount?: number;
        clearedProjectRefsCount?: number;
        totalCleaned?: number;
        dryRun?: boolean;
        cleanedSummaries?: string[];
      }>('/flowsk.task.v1.TaskService/PruneOrphanResources', {
        workspaceId,
        actorId,
        dryRun,
      })
      .pipe(
        map((res) => ({
          removedTaskFilesCount: res.removedTaskFilesCount ?? 0,
          clearedTaskRefsCount: res.clearedTaskRefsCount ?? 0,
          removedProjectFilesCount: res.removedProjectFilesCount ?? 0,
          clearedProjectRefsCount: res.clearedProjectRefsCount ?? 0,
          totalCleaned: res.totalCleaned ?? 0,
          dryRun: Boolean(res.dryRun),
          cleanedSummaries: res.cleanedSummaries ?? [],
        })),
      );
  }
}

function mapWorkspace(raw?: RawWorkspaceDto): WorkspaceModel {
  return {
    id: raw?.id ?? '',
    slug: raw?.slug ?? 'default',
    name: raw?.name ?? 'Default Workspace',
    description: raw?.description ?? '',
  };
}

function mapActor(raw?: RawActorDto): ActorModel {
  return {
    id: raw?.id ?? '',
    workspaceId: raw?.workspaceId ?? '',
    handle: raw?.handle ?? 'operator',
    displayName: raw?.displayName ?? raw?.handle ?? 'Operator',
    kind: raw?.kind ?? 'ACTOR_KIND_HUMAN',
  };
}

function mapProject(raw?: RawProjectDto): ProjectModel {
  const statusRaw = (raw?.status ?? 'PROJECT_STATUS_ACTIVE').replace('PROJECT_STATUS_', '').toLowerCase();
  return {
    id: raw?.id ?? '',
    workspaceId: raw?.workspaceId ?? '',
    keyPrefix: raw?.keyPrefix ?? 'PRJ',
    slug: raw?.slug ?? '',
    name: raw?.name ?? 'Untitled Project',
    description: raw?.description ?? '',
    status: statusRaw || 'active',
    issueCounter: Number(raw?.issueCounter ?? 0),
    hasMarkdownDoc: Boolean(raw?.hasMarkdownDoc),
    updatedAt: raw?.updatedAt ?? '',
  };
}

function mapProjectBinding(raw?: RawProjectBindingDto): ProjectBindingModel {
  return {
    id: raw?.id ?? '',
    workspaceId: raw?.workspaceId ?? '',
    parentProjectId: raw?.parentProjectId ?? '',
    childProjectId: raw?.childProjectId ?? '',
    scopeSubpath: raw?.scopeSubpath ?? '',
  };
}

function mapWorkflowState(raw?: RawWorkflowStateDto): WorkflowStateModel {
  const name = raw?.name ?? 'Todo';
  const cat = raw?.category ?? '';
  return {
    id: raw?.id ?? '',
    projectId: raw?.projectId ?? '',
    name,
    statusKey: inferTaskStatus(name, cat, ''),
    category: cat,
    colorHex: raw?.colorHex ?? '#64748B',
    position: Number(raw?.position ?? 0),
    isDefault: Boolean(raw?.isDefault),
  };
}

function toObjectiveKindEnum(kind: ObjectiveKind): string {
  switch (kind) {
    case 'milestone':
      return 'OBJECTIVE_KIND_MILESTONE';
    case 'cycle':
      return 'OBJECTIVE_KIND_CYCLE';
    default:
      return 'OBJECTIVE_KIND_GOAL';
  }
}

function toObjectiveStatusEnum(status: ObjectiveStatus): string {
  switch (status) {
    case 'planned':
      return 'OBJECTIVE_STATUS_PLANNED';
    case 'at_risk':
      return 'OBJECTIVE_STATUS_AT_RISK';
    case 'achieved':
      return 'OBJECTIVE_STATUS_ACHIEVED';
    case 'dropped':
      return 'OBJECTIVE_STATUS_DROPPED';
    default:
      return 'OBJECTIVE_STATUS_ACTIVE';
  }
}

function toProtoTimestampIso(dateInput?: string): string | undefined {
  const trimmed = (dateInput ?? '').trim();
  if (!trimmed) {
    return undefined;
  }
  if (/^\d{4}-\d{2}-\d{2}$/.test(trimmed)) {
    return `${trimmed}T00:00:00Z`;
  }
  const parsed = new Date(trimmed);
  return Number.isNaN(parsed.getTime()) ? undefined : parsed.toISOString();
}

function mapObjective(raw?: RawObjectiveDto): ObjectiveModel {
  const kindRaw = (raw?.kind ?? '').toUpperCase();
  const kind: ObjectiveKind = kindRaw.includes('MILESTONE')
    ? 'milestone'
    : kindRaw.includes('CYCLE')
      ? 'cycle'
      : 'goal';
  const statusNorm = (raw?.status ?? 'OBJECTIVE_STATUS_ACTIVE').replace('OBJECTIVE_STATUS_', '').toLowerCase();
  let status: ObjectiveStatus = 'active';
  switch (statusNorm) {
    case 'planned':
    case 'draft':
      status = 'planned';
      break;
    case 'at_risk':
      status = 'at_risk';
      break;
    case 'achieved':
      status = 'achieved';
      break;
    case 'dropped':
      status = 'dropped';
      break;
    default:
      status = 'active';
      break;
  }
  const criteria = (raw?.successCriteria ?? '')
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
  return {
    id: raw?.id ?? '',
    workspaceId: raw?.workspaceId ?? '',
    projectId: raw?.projectId ?? '',
    kind,
    status,
    title: raw?.title ?? '',
    description: raw?.description ?? '',
    successCriteria: criteria,
    targetDate: raw?.targetDate,
    closedAt: raw?.closedAt,
    updatedAt: raw?.updatedAt,
  };
}

function mapTask(raw?: RawTaskDto): TaskModel {
  const priority = raw?.priority ?? 'TASK_PRIORITY_MEDIUM';
  const status = inferTaskStatus(raw?.status ?? '', raw?.stateCategory ?? '', raw?.status ?? '');
  return {
    id: raw?.id ?? '',
    workspaceId: raw?.workspaceId ?? '',
    projectId: raw?.projectId ?? '',
    identifier: raw?.identifier ?? 'TSK-0',
    externalId: raw?.externalId ?? '',
    sequenceNumber: Number(raw?.sequenceNumber ?? 0),
    kind: raw?.kind ?? 'TASK_KIND_TASK',
    priority,
    priorityRank: priorityOrder(priority),
    workflowStateId: raw?.workflowStateId ?? '',
    status,
    title: raw?.title ?? '',
    description: raw?.description ?? '',
    acceptanceCriteria: raw?.acceptanceCriteria ?? '',
    objectiveId: raw?.objectiveId,
    parentTaskId: raw?.parentTaskId,
    assigneeActorId: raw?.assigneeActorId,
    claimedByActorId: raw?.claimedByActorId,
    claimExpiresAt: raw?.claimExpiresAt,
    labelIds: raw?.labelIds ?? [],
    hasMarkdownDoc: Boolean(raw?.hasMarkdownDoc),
    createdAt: raw?.createdAt ?? '',
    updatedAt: raw?.updatedAt ?? '',
    completedAt: raw?.completedAt,
  };
}

function priorityOrder(p: TaskPriority): number {
  switch (p) {
    case 'TASK_PRIORITY_URGENT':
      return 0;
    case 'TASK_PRIORITY_HIGH':
      return 1;
    case 'TASK_PRIORITY_MEDIUM':
      return 2;
    case 'TASK_PRIORITY_LOW':
      return 3;
    default:
      return 2;
  }
}

function inferTaskStatus(nameOrStatus: string, category: string, explicitStatus: string): TaskStatus {
  const norm = (explicitStatus || nameOrStatus).trim().toLowerCase().replace(/[\s-]+/g, '_');
  switch (norm) {
    case 'backlog':
    case 'triage':
      return 'backlog';
    case 'ready':
    case 'todo':
    case 'unstarted':
      return 'ready';
    case 'in_progress':
    case 'started':
      return 'in_progress';
    case 'in_review':
    case 'review':
      return 'in_review';
    case 'blocked':
      return 'blocked';
    case 'done':
    case 'completed':
      return 'done';
    case 'cancelled':
    case 'canceled':
      return 'cancelled';
    case 'archived':
      return 'archived';
  }
  const cat = category.toUpperCase();
  if (cat.includes('ARCHIVED')) return 'archived';
  if (cat.includes('BACKLOG') || cat.includes('TRIAGE')) return 'backlog';
  if (cat.includes('UNSTARTED')) return 'ready';
  if (cat.includes('STARTED')) return 'in_progress';
  if (cat.includes('COMPLETED')) return 'done';
  if (cat.includes('CANCELED')) return 'cancelled';
  return 'ready';
}
