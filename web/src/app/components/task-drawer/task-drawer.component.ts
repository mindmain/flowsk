import { ChangeDetectionStrategy, Component, effect, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  TaskKind,
  TaskPriority,
  TaskStatus,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

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
            <div class="flex items-center gap-2.5 min-w-0">
              <span class="font-mono text-xs px-2 py-0.5 rounded bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300">
                {{ task.identifier }}
              </span>
              @if (task.hasMarkdownDoc) {
                <span class="text-[11px] font-mono px-1.5 py-0.5 rounded bg-sky-500/10 text-sky-600 dark:text-sky-300 border border-sky-500/30">
                  ~/.flowsk/tasks .md
                </span>
              }
            </div>

            <div class="flex items-center gap-2">
              @if (task.claimedByActorId) {
                <button
                  type="button"
                  (click)="store.toggleTaskClaim({ payload: { taskId: task.id, claim: false } })"
                  class="px-2.5 py-1 rounded text-xs border border-amber-500/40 text-amber-600 dark:text-amber-300 hover:bg-amber-500/10"
                >
                  Release Lease
                </button>
              } @else {
                <button
                  type="button"
                  (click)="store.toggleTaskClaim({ payload: { taskId: task.id, claim: true } })"
                  class="px-2.5 py-1 rounded text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900"
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
              class="py-2.5 border-b-2 transition-colors"
              [class]="
                activeTab() === 'details'
                  ? 'border-zinc-900 dark:border-zinc-100 text-zinc-900 dark:text-zinc-100 font-medium'
                  : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-300'
              "
            >
              Overview
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
              @if (task.hasMarkdownDoc) {
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
                  [(ngModel)]="draftTitle"
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
                    [(ngModel)]="draftPriority"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    <option value="TASK_PRIORITY_URGENT">P0 · Urgent</option>
                    <option value="TASK_PRIORITY_HIGH">P1 · High</option>
                    <option value="TASK_PRIORITY_MEDIUM">P2 · Medium</option>
                    <option value="TASK_PRIORITY_LOW">P3 · Low</option>
                  </select>
                </div>

                <div>
                  <label class="block text-[11px] text-zinc-500 mb-1">Kind</label>
                  <select
                    [(ngModel)]="draftKind"
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
                    [(ngModel)]="draftObjectiveId"
                    class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                  >
                    <option value="">None</option>
                    @for (obj of store.objectives(); track obj.id) {
                      <option [value]="obj.id">{{ obj.title }}</option>
                    }
                  </select>
                </div>
              </div>

              <div>
                <label class="block text-[11px] font-medium text-zinc-500 dark:text-zinc-400 mb-1">
                  Summary / Description
                </label>
                <textarea
                  rows="5"
                  [(ngModel)]="draftDescription"
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
                  [(ngModel)]="draftAcceptance"
                  placeholder="Deterministic verification checkpoints..."
                  class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                ></textarea>
              </div>

              <div class="flex justify-end pt-2">
                <button
                  type="button"
                  (click)="saveOverview(task.id)"
                  [disabled]="store.saving()"
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

                <textarea
                  rows="16"
                  [(ngModel)]="draftMarkdown"
                  placeholder="# Task Specification&#10;&#10;Write architectural notes, rollout checklists, or agent context..."
                  class="w-full p-3 rounded-lg text-xs font-mono leading-relaxed bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400"
                ></textarea>

                <div class="flex items-center justify-between pt-1">
                  @if (task.hasMarkdownDoc) {
                    <button
                      type="button"
                      (click)="removeMarkdown(task.id)"
                      class="px-3 py-1.5 rounded-md border border-rose-500/40 text-rose-600 dark:text-rose-300 text-xs hover:bg-rose-500/10"
                    >
                      Delete .md Resource
                    </button>
                  } @else {
                    <span></span>
                  }

                  <button
                    type="button"
                    (click)="saveMarkdown(task.id)"
                    [disabled]="store.saving() || !draftMarkdown.trim()"
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
                      <div class="flex items-center justify-between text-[11px] text-zinc-500">
                        <span class="font-mono">{{ c.kind.replace('TASK_COMMENT_KIND_', '') }}</span>
                        <span>{{ c.createdAt }}</span>
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
                      [(ngModel)]="commentKind"
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
                    [(ngModel)]="commentBody"
                    placeholder="Add timeline comment or handoff note..."
                    class="w-full p-2.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none"
                  ></textarea>
                  <div class="flex justify-end">
                    <button
                      type="button"
                      (click)="postComment(task.id)"
                      [disabled]="!commentBody.trim()"
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
  readonly statusColumns = CANONICAL_STATUS_COLUMNS;
  readonly activeTab = signal<'details' | 'markdown' | 'comments'>('details');

  draftTitle = '';
  draftDescription = '';
  draftAcceptance = '';
  draftPriority: TaskPriority = 'TASK_PRIORITY_MEDIUM';
  draftKind: TaskKind = 'TASK_KIND_FEATURE';
  draftObjectiveId = '';
  draftMarkdown = '';

  commentKind = 'TASK_COMMENT_KIND_COMMENT';
  commentBody = '';

  constructor() {
    effect(() => {
      const t = this.store.selectedTask();
      const md = this.store.selectedTaskMarkdown();
      if (t) {
        this.draftTitle = t.title;
        this.draftDescription = t.description;
        this.draftAcceptance = t.acceptanceCriteria;
        this.draftPriority = t.priority;
        this.draftKind = t.kind;
        this.draftObjectiveId = t.objectiveId ?? '';
      }
      this.draftMarkdown = md;
    });
  }

  setStatus(taskId: string, targetStatus: TaskStatus): void {
    this.store.moveTaskToStatus({ payload: { taskId, targetStatus } });
  }

  saveOverview(taskId: string): void {
    if (!this.draftTitle.trim()) return;
    this.store.updateTaskDetails({
      payload: {
        taskId,
        title: this.draftTitle,
        description: this.draftDescription,
        acceptanceCriteria: this.draftAcceptance,
        kind: this.draftKind,
        priority: this.draftPriority,
        objectiveId: this.draftObjectiveId,
      },
    });
  }

  saveMarkdown(taskId: string): void {
    this.store.saveTaskMarkdown({
      payload: { taskId, markdown: this.draftMarkdown },
    });
  }

  removeMarkdown(taskId: string): void {
    this.store.deleteTaskMarkdown({
      payload: { taskId },
      onSuccess: () => {
        this.draftMarkdown = '';
      },
    });
  }

  postComment(taskId: string): void {
    const body = this.commentBody.trim();
    if (!body) return;
    this.store.addComment({
      payload: { taskId, kind: this.commentKind, body },
      onSuccess: () => {
        this.commentBody = '';
      },
    });
  }
}
