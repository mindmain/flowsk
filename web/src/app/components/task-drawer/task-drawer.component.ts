import { isPlatformBrowser } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  PLATFORM_ID,
  computed,
  effect,
  inject,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  TaskKind,
  TaskPriority,
  TaskRelationModel,
  TaskStatus,
  formatStatusLabel,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

interface ResolvedTaskRelationView {
  readonly id: string;
  readonly directionLabel: string;
  readonly isBlocker: boolean;
  readonly otherTaskId: string;
  readonly otherIdentifier: string;
  readonly otherTitle: string;
  readonly otherStatusLabel: string;
}

/**
 * Slide-over task inspector drawer with dirty-draft protection against background SSE
 * updates, project-scoped objective selection, lease ownership & takeover controls,
 * task relation links, Markdown spec editor, and actor-attributed timeline comments.
 */
@Component({
  selector: 'app-task-drawer',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (store.selectedTask(); as task) {
      <div
        class="fixed inset-0 z-40 bg-black/35 backdrop-blur-[1px] flex justify-end"
        (click)="store.closeTaskInspector()"
      >
        <aside
          (click)="$event.stopPropagation()"
          class="w-full max-w-2xl h-full bg-white dark:bg-zinc-950 border-l border-zinc-200 dark:border-zinc-800 shadow-2xl flex flex-col"
        >
          <!-- Header -->
          <div class="px-5 py-3.5 border-b border-zinc-200 dark:border-zinc-800 flex items-center justify-between gap-3">
            <div class="flex items-center gap-2 min-w-0 flex-wrap">
              <span class="font-mono text-xs px-2 py-0.5 rounded bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300">
                {{ task.identifier }}
              </span>
              @if (task.hasMarkdownDoc) {
                <span class="text-[11px] font-mono px-1.5 py-0.5 rounded bg-sky-500/10 text-sky-600 dark:text-sky-300 border border-sky-500/30">
                  ~/.flowsk/tasks .md
                </span>
              }
              @if (store.isTaskGlowing(task)) {
                <span
                  class="text-[11px] font-mono px-2 py-0.5 rounded bg-emerald-500/15 text-emerald-700 dark:text-emerald-300 border border-emerald-500/40 flex items-center gap-1.5"
                  [title]="'Agent/CLI/MCP signal received ' + store.taskSignalAgeLabel(task)"
                >
                  <span class="relative flex h-2 w-2 shrink-0">
                    <span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
                    <span class="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
                  </span>
                  <span>Signal · {{ store.taskSignalAgeLabel(task) }}</span>
                </span>
              }
              @if (store.activeClaimHolderId(task); as holderId) {
                <span
                  class="text-[11px] font-mono px-2 py-0.5 rounded bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 border border-emerald-500/30 flex items-center gap-1.5"
                >
                  <span class="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse"></span>
                  <span>@{{ actorHandle(holderId) }}</span>
                </span>
              }
            </div>

            <div class="flex items-center gap-2 shrink-0">
              @if (store.isTaskGlowing(task)) {
                <span
                  class="text-[11px] font-mono px-2 py-0.5 rounded bg-emerald-500/15 text-emerald-700 dark:text-emerald-300 border border-emerald-500/40 flex items-center gap-1.5"
                  [title]="'Agent/CLI/MCP signal received ' + store.taskSignalAgeLabel(task)"
                >
                  <span class="relative flex h-2 w-2 shrink-0">
                    <span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
                    <span class="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
                  </span>
                  <span>Signal · {{ store.taskSignalAgeLabel(task) }}</span>
                </span>
              }
              @if (store.activeClaimHolderId(task); as holderId) {
                @if (holderId === store.actorId()) {
                  <button
                    type="button"
                    (click)="releaseOwnLease(task.id)"
                    [disabled]="store.saving()"
                    class="px-2.5 py-1 rounded text-xs border border-amber-500/40 text-amber-600 dark:text-amber-300 hover:bg-amber-500/10 disabled:opacity-50"
                  >
                    Release Lease
                  </button>
                } @else {
                  <button
                    type="button"
                    (click)="takeOverLease(task.id, task.identifier, holderId)"
                    [disabled]="store.saving()"
                    class="px-2.5 py-1 rounded text-xs border border-amber-500/40 text-amber-700 dark:text-amber-300 hover:bg-amber-500/10 disabled:opacity-50"
                  >
                    Take Over Lease
                  </button>
                }
              } @else {
                <button
                  type="button"
                  (click)="claimUnleasedTask(task.id)"
                  [disabled]="store.saving()"
                  class="px-2.5 py-1 rounded text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900 disabled:opacity-50"
                >
                  Claim Lease
                </button>
              }

              @if (task.status !== 'archived') {
                <button
                  type="button"
                  (click)="setStatus(task.id, 'archived')"
                  class="px-2.5 py-1 rounded text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
                >
                  Archive
                </button>
              } @else {
                <button
                  type="button"
                  (click)="setStatus(task.id, 'backlog')"
                  class="px-2.5 py-1 rounded text-xs border border-emerald-500/40 text-emerald-600 dark:text-emerald-300"
                >
                  Restore
                </button>
              }

              <button
                type="button"
                (click)="store.closeTaskInspector()"
                class="px-2 py-1 rounded text-sm text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
              >
                ✕
              </button>
            </div>
          </div>

          <!-- Sub-navigation Tabs -->
          <div class="px-5 border-b border-zinc-200 dark:border-zinc-800 flex gap-5 text-xs">
            <button
              type="button"
              (click)="activeTab.set('details')"
              class="py-2.5 border-b-2 transition-colors flex items-center gap-1.5"
              [class]="
                activeTab() === 'details'
                  ? 'border-zinc-900 dark:border-zinc-100 text-zinc-900 dark:text-zinc-100 font-medium'
                  : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-300'
              "
            >
              <span>Overview</span>
              @if (detailsDirty()) {
                <span class="w-1.5 h-1.5 rounded-full bg-amber-400" title="Unsaved overview edits"></span>
              }
            </button>
            <button
              type="button"
              (click)="activeTab.set('markdown')"
              class="py-2.5 border-b-2 transition-colors flex items-center gap-1.5"
              [class]="
                activeTab() === 'markdown'
                  ? 'border-zinc-900 dark:border-zinc-100 text-zinc-900 dark:text-zinc-100 font-medium'
                  : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-300'
              "
            >
              <span>Task .md Resource</span>
              @if (markdownDirty()) {
                <span class="w-1.5 h-1.5 rounded-full bg-amber-400" title="Unsaved Markdown edits"></span>
              } @else if (task.hasMarkdownDoc) {
                <span class="w-1.5 h-1.5 rounded-full bg-sky-400"></span>
              }
            </button>
            <button
              type="button"
              (click)="activeTab.set('comments')"
              class="py-2.5 border-b-2 transition-colors"
              [class]="
                activeTab() === 'comments'
                  ? 'border-zinc-900 dark:border-zinc-100 text-zinc-900 dark:text-zinc-100 font-medium'
                  : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-300'
              "
            >
              Timeline ({{ store.selectedTaskComments().length }})
            </button>
          </div>

          <!-- Drawer Body -->
          <div class="p-5 flex-1 overflow-y-auto space-y-4">
            @if (activeTab() === 'details') {
              <div>
                <label class="block text-[11px] font-medium text-zinc-500 dark:text-zinc-400 mb-1">Title</label>
                <input
                  type="text"
                  [ngModel]="draftTitle()"
                  (ngModelChange)="onDetailsFieldChange('title', $event)"
                  class="w-full px-3 py-2 rounded-md text-sm font-medium bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                />
              </div>

              <div class="grid grid-cols-2 sm:grid-cols-4 gap-2.5">
                <div>
                  <label class="block text-[11px] text-zinc-500 mb-1">Status</label>
                  <select
                    [ngModel]="task.status"
                    (ngModelChange)="setStatus(task.id, $event)"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    @for (col of statusColumns; track col.status) {
                      <option [value]="col.status">{{ col.label }}</option>
                    }
                  </select>
                </div>

                <div>
                  <label class="block text-[11px] text-zinc-500 mb-1">Priority</label>
                  <select
                    [ngModel]="draftPriority()"
                    (ngModelChange)="onPriorityChange($event)"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    <option value="TASK_PRIORITY_URGENT">P0 · Urgent</option>
                    <option value="TASK_PRIORITY_HIGH">P1 · High</option>
                    <option value="TASK_PRIORITY_MEDIUM">P2 · Medium</option>
                    <option value="TASK_PRIORITY_LOW">P3 · Low</option>
                    <option value="TASK_PRIORITY_NONE">Unprioritized</option>
                  </select>
                </div>

                <div>
                  <label class="block text-[11px] text-zinc-500 mb-1">Kind</label>
                  <select
                    [ngModel]="draftKind()"
                    (ngModelChange)="onKindChange($event)"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    <option value="TASK_KIND_FEATURE">Feature</option>
                    <option value="TASK_KIND_TASK">Task</option>
                    <option value="TASK_KIND_BUG">Bug</option>
                    <option value="TASK_KIND_EPIC">Epic</option>
                    <option value="TASK_KIND_ISSUE">Research</option>
                  </select>
                </div>

                <div>
                  <label class="block text-[11px] text-zinc-500 mb-1">Objective</label>
                  <select
                    [ngModel]="draftObjectiveId()"
                    (ngModelChange)="onDetailsFieldChange('objectiveId', $event)"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    <option value="">None</option>
                    @for (obj of projectObjectives(); track obj.id) {
                      <option [value]="obj.id">{{ obj.title }}</option>
                    }
                  </select>
                </div>
              </div>

              @if (task.lastSignalAt) {
                <div class="text-[11px] font-mono text-zinc-500 dark:text-zinc-400 flex items-center gap-2">
                  <span>Last agent/CLI/MCP signal:</span>
                  <span class="text-zinc-700 dark:text-zinc-300">{{ formatTimestamp(task.lastSignalAt) }}</span>
                  @if (store.taskSignalAgeLabel(task); as ageLabel) {
                    <span class="text-zinc-400">({{ ageLabel }})</span>
                  }
                </div>
              }

              @if (task.tags.length > 0) {
                <div class="flex flex-wrap items-center gap-1.5">
                  @for (tag of task.tags; track $index) {
                    <span
                      class="text-[10px] font-mono px-2 py-0.5 rounded bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400"
                    >
                      #{{ tag }}
                    </span>
                  }
                </div>
              }

              @if (resolvedRelations().length > 0) {
                <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-900/60 border border-zinc-200 dark:border-zinc-800 space-y-2">
                  <div class="text-[11px] font-medium text-zinc-500 dark:text-zinc-400">
                    Dependencies & Relations ({{ resolvedRelations().length }})
                  </div>
                  <div class="space-y-1.5">
                    @for (rel of resolvedRelations(); track rel.id) {
                      <button
                        type="button"
                        (click)="inspectRelatedTask(rel.otherTaskId)"
                        class="w-full px-2.5 py-1.5 rounded-md bg-white dark:bg-zinc-950 border border-zinc-200/80 dark:border-zinc-800 hover:border-zinc-400 dark:hover:border-zinc-600 flex items-center justify-between gap-2 text-left text-xs transition-colors"
                      >
                        <div class="flex items-center gap-2 min-w-0">
                          <span
                            class="text-[10px] font-mono uppercase px-1.5 py-0.5 rounded border shrink-0"
                            [class]="
                              rel.isBlocker
                                ? 'border-rose-500/40 bg-rose-500/10 text-rose-600 dark:text-rose-300'
                                : 'border-zinc-300 dark:border-zinc-700 text-zinc-600 dark:text-zinc-400'
                            "
                          >
                            {{ rel.directionLabel }}
                          </span>
                          <span class="font-mono text-[11px] text-zinc-500 dark:text-zinc-400 shrink-0">
                            {{ rel.otherIdentifier }}
                          </span>
                          <span class="truncate text-zinc-800 dark:text-zinc-200">
                            {{ rel.otherTitle }}
                          </span>
                        </div>
                        <span class="text-[10px] font-mono text-zinc-400 shrink-0">
                          {{ rel.otherStatusLabel }}
                        </span>
                      </button>
                    }
                  </div>
                </div>
              }

              <div>
                <label class="block text-[11px] font-medium text-zinc-500 dark:text-zinc-400 mb-1">
                  Summary / Description
                </label>
                <textarea
                  rows="5"
                  [ngModel]="draftDescription()"
                  (ngModelChange)="onDetailsFieldChange('description', $event)"
                  placeholder="Concise implementation context..."
                  class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                ></textarea>
              </div>

              <div>
                <label class="block text-[11px] font-medium text-zinc-500 dark:text-zinc-400 mb-1">
                  Acceptance Criteria (one per line)
                </label>
                <textarea
                  rows="4"
                  [ngModel]="draftAcceptance()"
                  (ngModelChange)="onDetailsFieldChange('acceptance', $event)"
                  placeholder="Deterministic verification checkpoints..."
                  class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                ></textarea>
              </div>

              <div class="flex justify-end pt-2">
                <button
                  type="button"
                  (click)="saveOverview(task.id)"
                  [disabled]="store.saving() || !draftTitle().trim()"
                  class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium hover:bg-zinc-800 dark:hover:bg-zinc-200 disabled:opacity-50"
                >
                  Save Changes
                </button>
              </div>
            }

            @if (activeTab() === 'markdown') {
              <div class="space-y-3">
                <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-900/70 border border-zinc-200 dark:border-zinc-800 text-xs text-zinc-600 dark:text-zinc-400">
                  Extended Markdown specification stored globally inside Flowsk's memory folder
                  (<code class="font-mono text-[11px]">~/.flowsk/tasks/</code>). Deleting archived tasks or running
                  <code class="font-mono text-[11px]">flowsk prune</code> keeps these resources synchronized with the DB.
                </div>

                @if (store.selectedTaskMarkdownLoading() && !markdownDirty()) {
                  <div class="py-10 text-center text-xs font-mono text-zinc-400">
                    Loading Task .md document...
                  </div>
                } @else {
                  <textarea
                    rows="16"
                    [ngModel]="draftMarkdown()"
                    (ngModelChange)="onMarkdownChange($event)"
                    placeholder="# Task Specification&#10;&#10;Write architectural notes, rollout checklists, or agent context..."
                    class="w-full p-3 rounded-lg text-xs font-mono leading-relaxed bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                  ></textarea>
                }

                <div class="flex items-center justify-between pt-1">
                  @if (task.hasMarkdownDoc) {
                    <button
                      type="button"
                      (click)="removeMarkdown(task.id)"
                      [disabled]="store.saving() || store.selectedTaskMarkdownLoading()"
                      class="px-3 py-1.5 rounded-md border border-rose-500/40 text-rose-600 dark:text-rose-300 text-xs hover:bg-rose-500/10 disabled:opacity-50"
                    >
                      Delete .md Resource
                    </button>
                  } @else {
                    <span></span>
                  }

                  <button
                    type="button"
                    (click)="saveMarkdown(task.id)"
                    [disabled]="store.saving() || store.selectedTaskMarkdownLoading() || !draftMarkdown().trim()"
                    class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium hover:bg-zinc-800 dark:hover:bg-zinc-200 disabled:opacity-50"
                  >
                    Save Task .md
                  </button>
                </div>
              </div>
            }

            @if (activeTab() === 'comments') {
              <div class="space-y-3">
                <div class="space-y-2">
                  @for (c of store.selectedTaskComments(); track c.id) {
                    <div class="p-3 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-zinc-50/70 dark:bg-zinc-900/50">
                      <div class="flex items-center justify-between gap-2 text-[11px] text-zinc-500">
                        <div class="flex items-center gap-2 min-w-0">
                          <span class="font-mono font-medium text-zinc-800 dark:text-zinc-200 truncate">
                            @{{ actorHandle(c.authorActorId) }}
                          </span>
                          <span
                            class="font-mono text-[10px] px-1.5 py-0.5 rounded bg-zinc-200/70 dark:bg-zinc-800 text-zinc-600 dark:text-zinc-300"
                          >
                            {{ c.kind.replace('TASK_COMMENT_KIND_', '') }}
                          </span>
                        </div>
                        <span class="font-mono text-[10px] text-zinc-400 shrink-0">
                          {{ formatTimestamp(c.createdAt) }}
                        </span>
                      </div>
                      <p class="mt-1.5 text-xs text-zinc-800 dark:text-zinc-200 whitespace-pre-wrap">{{ c.body }}</p>
                    </div>
                  }
                  @if (store.selectedTaskComments().length === 0) {
                    <div class="py-6 text-center text-xs text-zinc-400">No comments yet.</div>
                  }
                </div>

                <div class="pt-3 border-t border-zinc-200 dark:border-zinc-800 space-y-2">
                  <div class="flex items-center gap-2">
                    <select
                      [ngModel]="commentKind()"
                      (ngModelChange)="commentKind.set($event)"
                      class="px-2.5 py-1 rounded text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300"
                    >
                      <option value="TASK_COMMENT_KIND_COMMENT">Note / Progress</option>
                      <option value="TASK_COMMENT_KIND_HANDOFF">Agent Handoff</option>
                      <option value="TASK_COMMENT_KIND_BLOCKER_REPORT">Blocker Report</option>
                      <option value="TASK_COMMENT_KIND_RESOLUTION">Resolution</option>
                    </select>
                  </div>
                  <textarea
                    rows="3"
                    [ngModel]="commentBody()"
                    (ngModelChange)="commentBody.set($event)"
                    placeholder="Add timeline comment or handoff note..."
                    class="w-full p-2.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none"
                  ></textarea>
                  <div class="flex justify-end">
                    <button
                      type="button"
                      (click)="postComment(task.id)"
                      [disabled]="!commentBody().trim() || store.saving()"
                      class="px-3 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
                    >
                      Post Comment
                    </button>
                  </div>
                </div>
              </div>
            }
          </div>
        </aside>
      </div>
    }
  `,
})
export class TaskDrawerComponent {
  readonly store = inject(FlowskStore);
  private readonly platformId = inject(PLATFORM_ID);
  readonly statusColumns = CANONICAL_STATUS_COLUMNS;
  readonly activeTab = signal<'details' | 'markdown' | 'comments'>('details');

  readonly draftTitle = signal<string>('');
  readonly draftDescription = signal<string>('');
  readonly draftAcceptance = signal<string>('');
  readonly draftPriority = signal<TaskPriority>('TASK_PRIORITY_MEDIUM');
  readonly draftKind = signal<TaskKind>('TASK_KIND_FEATURE');
  readonly draftObjectiveId = signal<string>('');
  readonly draftMarkdown = signal<string>('');

  readonly commentKind = signal<string>('TASK_COMMENT_KIND_COMMENT');
  readonly commentBody = signal<string>('');

  readonly detailsDirty = signal<boolean>(false);
  readonly markdownDirty = signal<boolean>(false);

  private hydratedTaskId: string | null = null;

  readonly projectObjectives = computed(() => {
    const task = this.store.selectedTask();
    if (!task) {
      return [];
    }
    return this.store.objectives().filter((o) => o.projectId === task.projectId);
  });

  readonly resolvedRelations = computed<readonly ResolvedTaskRelationView[]>(() => {
    const task = this.store.selectedTask();
    if (!task) {
      return [];
    }
    const byId = this.store.taskMapById();
    return this.store.selectedTaskRelations().map((rel, idx) =>
      formatRelationForTask(rel, task.id, byId, idx),
    );
  });

  constructor() {
    effect(() => {
      const t = this.store.selectedTask();
      const md = this.store.selectedTaskMarkdown();
      const mdLoading = this.store.selectedTaskMarkdownLoading();

      if (!t) {
        this.hydratedTaskId = null;
        this.detailsDirty.set(false);
        this.markdownDirty.set(false);
        return;
      }

      if (this.hydratedTaskId !== t.id) {
        this.hydratedTaskId = t.id;
        this.activeTab.set('details');
        this.detailsDirty.set(false);
        this.markdownDirty.set(false);
        this.commentKind.set('TASK_COMMENT_KIND_COMMENT');
        this.commentBody.set('');

        this.draftTitle.set(t.title);
        this.draftDescription.set(t.description);
        this.draftAcceptance.set(t.acceptanceCriteria);
        this.draftPriority.set(t.priority);
        this.draftKind.set(t.kind);
        this.draftObjectiveId.set(t.objectiveId ?? '');
        this.draftMarkdown.set(md);
        return;
      }

      if (!untracked(() => this.detailsDirty())) {
        this.draftTitle.set(t.title);
        this.draftDescription.set(t.description);
        this.draftAcceptance.set(t.acceptanceCriteria);
        this.draftPriority.set(t.priority);
        this.draftKind.set(t.kind);
        this.draftObjectiveId.set(t.objectiveId ?? '');
      }

      if (!mdLoading && !untracked(() => this.markdownDirty())) {
        this.draftMarkdown.set(md);
      }
    });
  }

  onDetailsFieldChange(field: 'title' | 'description' | 'acceptance' | 'objectiveId', value: string): void {
    this.detailsDirty.set(true);
    switch (field) {
      case 'title':
        this.draftTitle.set(value);
        break;
      case 'description':
        this.draftDescription.set(value);
        break;
      case 'acceptance':
        this.draftAcceptance.set(value);
        break;
      case 'objectiveId':
        this.draftObjectiveId.set(value);
        break;
    }
  }

  onPriorityChange(priority: TaskPriority): void {
    this.detailsDirty.set(true);
    this.draftPriority.set(priority);
  }

  onKindChange(kind: TaskKind): void {
    this.detailsDirty.set(true);
    this.draftKind.set(kind);
  }

  onMarkdownChange(value: string): void {
    this.markdownDirty.set(true);
    this.draftMarkdown.set(value);
  }

  actorHandle(actorId: string): string {
    if (!actorId) return 'actor';
    return this.store.actorMapById()[actorId]?.handle ?? 'actor';
  }

  formatTimestamp(iso: string): string {
    if (!iso) return '';
    const parsed = new Date(iso);
    if (Number.isNaN(parsed.getTime())) {
      return iso;
    }
    const yyyy = parsed.getFullYear();
    const mm = String(parsed.getMonth() + 1).padStart(2, '0');
    const dd = String(parsed.getDate()).padStart(2, '0');
    const hh = String(parsed.getHours()).padStart(2, '0');
    const min = String(parsed.getMinutes()).padStart(2, '0');
    return `${yyyy}-${mm}-${dd} ${hh}:${min}`;
  }

  inspectRelatedTask(otherTaskId: string): void {
    if (!otherTaskId) return;
    this.store.loadTaskInspector({ taskId: otherTaskId });
  }

  claimUnleasedTask(taskId: string): void {
    this.store.toggleTaskClaim({
      payload: { taskId, claim: true, forceTakeover: false },
    });
  }

  releaseOwnLease(taskId: string): void {
    this.store.toggleTaskClaim({
      payload: { taskId, claim: false },
    });
  }

  takeOverLease(taskId: string, identifier: string, holderId: string): void {
    const holder = this.actorHandle(holderId);
    if (isPlatformBrowser(this.platformId)) {
      const confirmed = window.confirm(
        `Task ${identifier} is currently leased by @${holder}. Take over this lease?`,
      );
      if (!confirmed) {
        return;
      }
    }
    this.store.toggleTaskClaim({
      payload: { taskId, claim: true, forceTakeover: true },
    });
  }

  setStatus(taskId: string, targetStatus: TaskStatus): void {
    this.store.moveTaskToStatus({ payload: { taskId, targetStatus } });
  }

  saveOverview(taskId: string): void {
    const title = this.draftTitle().trim();
    if (!title) return;
    this.store.updateTaskDetails({
      payload: {
        taskId,
        title,
        description: this.draftDescription(),
        acceptanceCriteria: this.draftAcceptance(),
        kind: this.draftKind(),
        priority: this.draftPriority(),
        objectiveId: this.draftObjectiveId(),
      },
      onSuccess: (updated) => {
        this.detailsDirty.set(false);
        this.draftTitle.set(updated.title);
        this.draftDescription.set(updated.description);
        this.draftAcceptance.set(updated.acceptanceCriteria);
        this.draftPriority.set(updated.priority);
        this.draftKind.set(updated.kind);
        this.draftObjectiveId.set(updated.objectiveId ?? '');
      },
    });
  }

  saveMarkdown(taskId: string): void {
    this.store.saveTaskMarkdown({
      payload: { taskId, markdown: this.draftMarkdown() },
      onSuccess: () => {
        this.markdownDirty.set(false);
      },
    });
  }

  removeMarkdown(taskId: string): void {
    this.store.deleteTaskMarkdown({
      payload: { taskId },
      onSuccess: () => {
        this.markdownDirty.set(false);
        this.draftMarkdown.set('');
      },
    });
  }

  postComment(taskId: string): void {
    const body = this.commentBody().trim();
    if (!body) return;
    this.store.addComment({
      payload: { taskId, kind: this.commentKind(), body },
      onSuccess: () => {
        this.commentBody.set('');
      },
    });
  }
}

function formatRelationForTask(
  rel: TaskRelationModel,
  currentTaskId: string,
  taskMap: Record<string, { identifier: string; title: string; status: TaskStatus }>,
  index: number,
): ResolvedTaskRelationView {
  const normType = rel.relationType.replace('TASK_RELATION_TYPE_', '').toUpperCase();
  const isSource = rel.sourceTaskId === currentTaskId;
  const otherTaskId = isSource ? rel.targetTaskId : rel.sourceTaskId;
  const other = taskMap[otherTaskId];

  let directionLabel = normType.toLowerCase().replace(/_/g, ' ') || 'related';
  let isBlocker = false;

  if (normType === 'BLOCKS') {
    if (isSource) {
      directionLabel = 'Blocks';
    } else {
      directionLabel = 'Blocked by';
      isBlocker = true;
    }
  } else if (normType === 'BLOCKED_BY') {
    if (isSource) {
      directionLabel = 'Blocked by';
      isBlocker = true;
    } else {
      directionLabel = 'Blocks';
    }
  }

  return {
    id: rel.id || `${rel.sourceTaskId}-${rel.targetTaskId}-${normType}-${index}`,
    directionLabel,
    isBlocker,
    otherTaskId,
    otherIdentifier: other?.identifier ?? otherTaskId.slice(0, 8),
    otherTitle: other?.title ?? 'Linked task',
    otherStatusLabel: other ? formatStatusLabel(other.status) : 'Unknown',
  };
}
