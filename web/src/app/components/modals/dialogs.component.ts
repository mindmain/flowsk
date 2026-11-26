import { ChangeDetectionStrategy, Component, effect, inject, input, output } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  ObjectiveKind,
  ObjectiveModel,
  ObjectiveStatus,
  TaskKind,
  TaskPriority,
  TaskStatus,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

export type ActiveDialogType =
  | null
  | 'new-task'
  | 'new-project'
  | 'project-charter'
  | 'new-objective'
  | 'edit-objective'
  | 'purge-archive'
  | 'prune-orphans';

@Component({
  selector: 'app-dialogs',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (dialog() !== null) {
      <div
        class="fixed inset-0 z-50 bg-black/45 backdrop-blur-[1px] flex items-center justify-center p-4"
        (click)="close.emit()"
      >
        <div
          (click)="$event.stopPropagation()"
          class="w-full max-w-lg rounded-xl bg-white dark:bg-zinc-950 border border-zinc-200 dark:border-zinc-800 shadow-2xl p-5 space-y-4"
        >
          <!-- 1. Create Task -->
          @if (dialog() === 'new-task') {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">New Task</h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400 hover:text-zinc-200">✕</button>
            </div>

            <div class="space-y-3">
              @if (!store.activeProjectId() && store.visibleProjects().length > 1) {
                <select
                  [(ngModel)]="taskProjectId"
                  (ngModelChange)="onTaskProjectChange($event)"
                  class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  @for (p of store.visibleProjects(); track p.id) {
                    <option [value]="p.id">{{ p.keyPrefix }} — {{ p.name }}</option>
                  }
                </select>
              }

              <input
                type="text"
                [(ngModel)]="taskTitle"
                placeholder="Task title (e.g. Implement WAL snapshot compaction)"
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none"
              />

              <div class="grid grid-cols-3 gap-2">
                <select
                  [(ngModel)]="taskStatus"
                  class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  @for (col of statusColumns; track col.status) {
                    <option [value]="col.status">{{ col.label }}</option>
                  }
                </select>

                <select
                  [(ngModel)]="taskPriority"
                  class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="TASK_PRIORITY_URGENT">P0 · Urgent</option>
                  <option value="TASK_PRIORITY_HIGH">P1 · High</option>
                  <option value="TASK_PRIORITY_MEDIUM">P2 · Medium</option>
                  <option value="TASK_PRIORITY_LOW">P3 · Low</option>
                </select>

                <select
                  [(ngModel)]="taskKind"
                  class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="TASK_KIND_FEATURE">Feature</option>
                  <option value="TASK_KIND_TASK">Task</option>
                  <option value="TASK_KIND_BUG">Bug</option>
                  <option value="TASK_KIND_EPIC">Epic</option>
                  <option value="TASK_KIND_ISSUE">Research</option>
                </select>
              </div>

              @if (objectivesForTaskForm().length > 0) {
                <select
                  [(ngModel)]="taskObjectiveId"
                  class="w-full px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="">No linked objective</option>
                  @for (obj of objectivesForTaskForm(); track obj.id) {
                    <option [value]="obj.id">{{ obj.title }}</option>
                  }
                </select>
              }

              <textarea
                rows="3"
                [(ngModel)]="taskDescription"
                placeholder="Short summary..."
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none"
              ></textarea>

              <textarea
                rows="4"
                [(ngModel)]="taskMarkdownDoc"
                placeholder="Optional Task .md document saved in ~/.flowsk/tasks/ (leave empty if not needed)..."
                class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none"
              ></textarea>
            </div>

            <div class="flex justify-end gap-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="submitNewTask()"
                [disabled]="!taskTitle.trim() || store.saving()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
              >
                Create Task
              </button>
            </div>
          }

          <!-- 2. Create Project -->
          @if (dialog() === 'new-project') {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">New Logical Project</h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>

            <div class="space-y-3">
              <div class="grid grid-cols-3 gap-2">
                <input
                  type="text"
                  [(ngModel)]="projKey"
                  placeholder="KEY (e.g. CORE)"
                  maxlength="10"
                  class="px-3 py-2 rounded-md text-xs font-mono uppercase bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                />
                <input
                  type="text"
                  [(ngModel)]="projName"
                  placeholder="Project Name"
                  class="col-span-2 px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
                />
              </div>

              @if (store.visibleProjects().length > 0) {
                <div class="grid grid-cols-2 gap-2">
                  <select
                    [(ngModel)]="projParentId"
                    class="px-2.5 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                  >
                    <option value="">Standalone Root Project</option>
                    @for (p of store.visibleProjects(); track p.id) {
                      <option [value]="p.id">Subproject of {{ p.keyPrefix }} — {{ p.name }}</option>
                    }
                  </select>
                  <input
                    type="text"
                    [(ngModel)]="projScopeSubpath"
                    [disabled]="!projParentId"
                    placeholder="Relative subpath (e.g. apps/web)"
                    class="px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 disabled:opacity-45"
                  />
                </div>
              }

              <textarea
                rows="3"
                [(ngModel)]="projDescription"
                placeholder="Logical project scope (never contains host filesystem paths)..."
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              ></textarea>
            </div>

            <div class="flex justify-end gap-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="submitNewProject()"
                [disabled]="!projName.trim() || store.saving()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
              >
                Create Project
              </button>
            </div>
          }

          <!-- 3. Project Charter (.md in ~/.flowsk/projects) -->
          @if (dialog() === 'project-charter' && store.activeProject(); as proj) {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                Project .md Document — {{ proj.keyPrefix }}
              </h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>
            <p class="text-xs text-zinc-500 dark:text-zinc-400">
              Saved inside <code class="font-mono text-[11px]">~/.flowsk/projects/</code> alongside
              <code class="font-mono text-[11px]">~/.flowsk/tasks/</code>. Clear text and save to remove the file.
            </p>
            <textarea
              rows="12"
              [(ngModel)]="projectCharterMd"
              placeholder="# {{ proj.name }} Charter&#10;&#10;Architecture invariants and goals..."
              class="w-full p-3 rounded-lg text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
            ></textarea>
            <div class="flex justify-end gap-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="submitProjectCharter(proj.id)"
                [disabled]="store.saving()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium"
              >
                Save Project .md
              </button>
            </div>
          }

          <!-- 4. New Objective (Goal / Milestone / Cycle) -->
          @if (dialog() === 'new-objective') {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">New Strategic Goal / Milestone</h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>

            <div class="space-y-3">
              @if (!store.activeProjectId() && store.visibleProjects().length > 1) {
                <select
                  [(ngModel)]="objProjectId"
                  class="w-full px-2.5 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  @for (p of store.visibleProjects(); track p.id) {
                    <option [value]="p.id">{{ p.keyPrefix }} — {{ p.name }}</option>
                  }
                </select>
              }

              <div class="grid grid-cols-3 gap-2">
                <select
                  [(ngModel)]="objKind"
                  class="px-2.5 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="goal">Goal</option>
                  <option value="milestone">Milestone</option>
                  <option value="cycle">Cycle</option>
                </select>

                <select
                  [(ngModel)]="objStatus"
                  class="px-2.5 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="active">Active</option>
                  <option value="planned">Planned</option>
                  <option value="at_risk">At Risk</option>
                </select>

                <input
                  type="date"
                  [(ngModel)]="objTargetDate"
                  class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                />
              </div>

              <input
                type="text"
                [(ngModel)]="objTitle"
                placeholder="Objective headline (e.g. Ship Real-Time Streaming & Structured Goals)"
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              />

              <textarea
                rows="2"
                [(ngModel)]="objSummary"
                placeholder="Strategic context & scope..."
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              ></textarea>

              <textarea
                rows="3"
                [(ngModel)]="objCriteria"
                placeholder="Verifiable success criteria (one per line)..."
                class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              ></textarea>
            </div>

            <div class="flex justify-end gap-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="submitObjective()"
                [disabled]="!objTitle.trim() || store.saving()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
              >
                Create Goal
              </button>
            </div>
          }

          <!-- 4b. Edit Objective -->
          @if (dialog() === 'edit-objective' && editingObjective(); as editObj) {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                Edit {{ editObj.kind }} — {{ editObj.title }}
              </h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>

            <div class="space-y-3">
              <input
                type="text"
                [(ngModel)]="objTitle"
                placeholder="Objective headline"
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              />

              <div class="grid grid-cols-2 gap-2">
                <select
                  [(ngModel)]="objStatus"
                  class="px-2.5 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                >
                  <option value="planned">Planned</option>
                  <option value="active">Active</option>
                  <option value="at_risk">At Risk</option>
                  <option value="achieved">Achieved</option>
                  <option value="dropped">Dropped</option>
                </select>

                <input
                  type="date"
                  [(ngModel)]="objTargetDate"
                  class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200"
                />
              </div>

              <textarea
                rows="3"
                [(ngModel)]="objSummary"
                placeholder="Strategic context & scope..."
                class="w-full px-3 py-2 rounded-md text-xs bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              ></textarea>

              <textarea
                rows="4"
                [(ngModel)]="objCriteria"
                placeholder="Verifiable success criteria (one per line)..."
                class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100"
              ></textarea>
            </div>

            <div class="flex justify-end gap-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="submitEditObjective(editObj.id)"
                [disabled]="!objTitle.trim() || store.saving()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
              >
                Save Changes
              </button>
            </div>
          }

          <!-- 5. Purge Archived Tasks (Global Workspace Hard-Delete) -->
          @if (dialog() === 'purge-archive') {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-rose-600 dark:text-rose-400">
                Permanently Delete All Archived Tasks
              </h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>

            <p class="text-xs text-zinc-600 dark:text-zinc-300 leading-relaxed">
              This will permanently remove all
              <strong class="font-semibold">{{ store.workspaceArchivedCount() }}</strong>
              archived task(s) across the current workspace from the database along with any
              associated <code class="font-mono">.md</code> resource files in
              <code class="font-mono">~/.flowsk/tasks/</code>. This action cannot be undone.
            </p>

            <div class="flex justify-end gap-2 pt-2">
              <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                Cancel
              </button>
              <button
                type="button"
                (click)="confirmPurgeArchive()"
                [disabled]="store.saving()"
                class="px-4 py-1.5 rounded-md bg-rose-600 text-white text-xs font-medium hover:bg-rose-500 disabled:opacity-50"
              >
                Delete All Archived Permanently
              </button>
            </div>
          }

          <!-- 6. Prune Orphan Resources -->
          @if (dialog() === 'prune-orphans') {
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                Memory Integrity & Prune (~/.flowsk)
              </h2>
              <button type="button" (click)="close.emit()" class="text-xs text-zinc-400">✕</button>
            </div>

            <div class="grid grid-cols-2 gap-2 text-xs">
              <div class="p-2.5 rounded-lg bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800">
                <div class="text-zinc-400 text-[11px]">Orphan task .md on disk</div>
                <div class="text-base font-mono font-semibold text-zinc-900 dark:text-zinc-100">
                  {{ store.orphanReport().orphanTaskFilesCount }}
                </div>
              </div>
              <div class="p-2.5 rounded-lg bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800">
                <div class="text-zinc-400 text-[11px]">DB tasks missing .md file</div>
                <div class="text-base font-mono font-semibold text-zinc-900 dark:text-zinc-100">
                  {{ store.orphanReport().missingTaskFilesCount }}
                </div>
              </div>
              <div class="p-2.5 rounded-lg bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800">
                <div class="text-zinc-400 text-[11px]">Orphan project .md on disk</div>
                <div class="text-base font-mono font-semibold text-zinc-900 dark:text-zinc-100">
                  {{ store.orphanReport().orphanProjectFilesCount }}
                </div>
              </div>
              <div class="p-2.5 rounded-lg bg-zinc-50 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800">
                <div class="text-zinc-400 text-[11px]">DB projects missing .md file</div>
                <div class="text-base font-mono font-semibold text-zinc-900 dark:text-zinc-100">
                  {{ store.orphanReport().missingProjectFilesCount }}
                </div>
              </div>
            </div>

            @if (store.orphanReport().issueSummaries.length > 0) {
              <div class="max-h-36 overflow-y-auto p-2.5 rounded-lg bg-amber-500/10 border border-amber-500/30 text-[11px] font-mono text-amber-700 dark:text-amber-300 space-y-1">
                @for (item of store.orphanReport().issueSummaries; track item) {
                  <div>• {{ item }}</div>
                }
              </div>
            } @else {
              <div class="p-3 rounded-lg bg-emerald-500/10 border border-emerald-500/30 text-xs text-emerald-600 dark:text-emerald-300">
                Zero orphan files or broken Markdown references detected.
              </div>
            }

            <div class="flex items-center justify-between pt-2">
              <button
                type="button"
                (click)="runPrune(true)"
                [disabled]="store.saving()"
                class="px-3 py-1.5 rounded-md border border-zinc-200 dark:border-zinc-800 text-xs text-zinc-600 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900"
              >
                Dry-Run Check
              </button>

              <div class="flex gap-2">
                <button type="button" (click)="close.emit()" class="px-3 py-1.5 rounded text-xs text-zinc-500">
                  Close
                </button>
                <button
                  type="button"
                  (click)="runPrune(false)"
                  [disabled]="store.saving() || store.orphanReport().totalIssues === 0"
                  class="px-4 py-1.5 rounded-md bg-amber-500 text-zinc-950 text-xs font-semibold hover:bg-amber-400 disabled:opacity-40"
                >
                  Run Prune Now
                </button>
              </div>
            </div>
          }
        </div>
      </div>
    }
  `,
})
export class DialogsComponent {
  readonly store = inject(FlowskStore);
  readonly dialog = input.required<ActiveDialogType>();
  readonly defaultTaskStatus = input<TaskStatus>('ready');
  readonly presetObjectiveId = input<string>('');
  readonly presetProjectId = input<string>('');
  readonly editingObjective = input<ObjectiveModel | null>(null);
  readonly close = output<void>();

  readonly statusColumns = CANONICAL_STATUS_COLUMNS;

  taskProjectId = '';
  taskTitle = '';
  taskDescription = '';
  taskMarkdownDoc = '';
  taskStatus: TaskStatus = 'ready';
  taskPriority: TaskPriority = 'TASK_PRIORITY_MEDIUM';
  taskKind: TaskKind = 'TASK_KIND_FEATURE';
  taskObjectiveId = '';

  projKey = '';
  projName = '';
  projDescription = '';
  projParentId = '';
  projScopeSubpath = '';
  projectCharterMd = '';

  objProjectId = '';
  objKind: ObjectiveKind = 'goal';
  objStatus: ObjectiveStatus = 'active';
  objTargetDate = '';
  objTitle = '';
  objSummary = '';
  objCriteria = '';

  constructor() {
    effect(() => {
      const current = this.dialog();
      const presetStatus = this.defaultTaskStatus();
      const presetObj = this.presetObjectiveId();
      const presetProj = this.presetProjectId();
      const editObj = this.editingObjective();

      if (current === 'new-task') {
        this.taskStatus = presetStatus;
        this.taskProjectId =
          presetProj || this.store.activeProjectId() || this.store.visibleProjects()[0]?.id || '';
        this.taskObjectiveId = presetObj;
      }
      if (current === 'new-objective') {
        this.objProjectId = this.store.activeProjectId() || this.store.visibleProjects()[0]?.id || '';
        this.objKind = 'goal';
        this.objStatus = 'active';
        this.objTargetDate = '';
        this.objTitle = '';
        this.objSummary = '';
        this.objCriteria = '';
      }
      if (current === 'edit-objective' && editObj) {
        this.objProjectId = editObj.projectId;
        this.objKind = editObj.kind;
        this.objStatus = editObj.status;
        this.objTargetDate = editObj.targetDate ? editObj.targetDate.slice(0, 10) : '';
        this.objTitle = editObj.title;
        this.objSummary = editObj.description;
        this.objCriteria = editObj.successCriteria.join('\n');
      }
      if (current === 'project-charter') {
        this.projectCharterMd = this.store.activeProjectMarkdown();
      }
    });
  }

  objectivesForTaskForm(): ObjectiveModel[] {
    const pid = this.store.activeProjectId() || this.taskProjectId;
    if (!pid) return [...this.store.scopedObjectives()];
    return this.store.objectives().filter((o) => o.projectId === pid);
  }

  onTaskProjectChange(projectId: string): void {
    this.taskProjectId = projectId;
    if (this.taskObjectiveId) {
      const stillMatches = this.store
        .objectives()
        .some((o) => o.id === this.taskObjectiveId && o.projectId === projectId);
      if (!stillMatches) {
        this.taskObjectiveId = '';
      }
    }
  }

  submitNewTask(): void {
    this.store.createTask({
      payload: {
        projectId: this.store.activeProjectId() || this.taskProjectId || undefined,
        title: this.taskTitle,
        description: this.taskDescription,
        acceptanceCriteria: '',
        kind: this.taskKind,
        priority: this.taskPriority,
        initialStatus: this.taskStatus,
        objectiveId: this.taskObjectiveId || undefined,
        markdownDoc: this.taskMarkdownDoc,
      },
      onSuccess: () => {
        this.taskTitle = '';
        this.taskDescription = '';
        this.taskMarkdownDoc = '';
        this.close.emit();
      },
    });
  }

  submitNewProject(): void {
    this.store.createProject({
      payload: {
        keyPrefix: this.projKey || this.projName.slice(0, 4),
        name: this.projName,
        description: this.projDescription,
        parentProjectId: this.projParentId || undefined,
        scopeSubpath: this.projParentId ? this.projScopeSubpath : undefined,
      },
      onSuccess: () => {
        this.projKey = '';
        this.projName = '';
        this.projDescription = '';
        this.projParentId = '';
        this.projScopeSubpath = '';
        this.close.emit();
      },
    });
  }

  submitProjectCharter(projectId: string): void {
    this.store.saveProjectMarkdown({
      payload: { projectId, markdown: this.projectCharterMd },
      onSuccess: () => this.close.emit(),
    });
  }

  submitObjective(): void {
    this.store.createObjective({
      payload: {
        projectId: this.store.activeProjectId() || this.objProjectId || undefined,
        kind: this.objKind,
        status: this.objStatus,
        title: this.objTitle,
        description: this.objSummary,
        successCriteria: this.objCriteria || this.objSummary,
        targetDate: this.objTargetDate || undefined,
      },
      onSuccess: () => {
        this.objTitle = '';
        this.objSummary = '';
        this.objCriteria = '';
        this.objTargetDate = '';
        this.close.emit();
      },
    });
  }

  submitEditObjective(objectiveId: string): void {
    this.store.updateObjective({
      payload: {
        objectiveId,
        title: this.objTitle,
        description: this.objSummary,
        successCriteria: this.objCriteria,
        status: this.objStatus,
        targetDate: this.objTargetDate || undefined,
      },
      onSuccess: () => {
        this.close.emit();
      },
    });
  }

  confirmPurgeArchive(): void {
    this.store.purgeArchivedTasks({
      payload: undefined,
      onSuccess: () => this.close.emit(),
    });
  }

  runPrune(dryRun: boolean): void {
    this.store.pruneOrphans({
      payload: { dryRun },
      onSuccess: () => {
        if (!dryRun) {
          this.close.emit();
        }
      },
    });
  }
}
