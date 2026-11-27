import { ChangeDetectionStrategy, Component, computed, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  ObjectiveKind,
  ObjectiveModel,
  ObjectiveProgressView,
  ObjectiveStatus,
  TaskModel,
  TaskStatus,
  formatKindLabel,
  formatObjectiveKindLabel,
  formatShortPriority,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

type GoalStatusFilter = 'all' | 'open' | 'at_risk' | 'achieved' | 'dropped';

/**
 * Structured Goals, Milestones & Cycles Roadmap View.
 * Displays macro-objectives, multi-segment live task progress bars, verifiable
 * success criteria, active @agent leases, and linked tasks with inline actions.
 */
@Component({
  selector: 'app-goals-view',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="h-full overflow-y-auto px-6 py-5">
      <div class="max-w-6xl mx-auto space-y-5">
        <!-- Macro KPI & Roadmap Header -->
        <div
          class="rounded-xl bg-white dark:bg-zinc-900/70 border border-zinc-200 dark:border-zinc-800/90 p-5 space-y-4"
        >
          <div class="flex flex-wrap items-start justify-between gap-4">
            <div class="space-y-1">
              <div class="flex items-center gap-2.5">
                <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                  Strategic Goals, Milestones & Cycles
                </h2>
                <span
                  class="text-[11px] font-mono px-2 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-300"
                >
                  {{ summary().totalObjectives }} total
                </span>
                @if (summary().activeAgentClaims > 0) {
                  <span
                    class="text-[11px] font-mono px-2 py-0.5 rounded bg-emerald-500/15 border border-emerald-500/30 text-emerald-700 dark:text-emerald-300 flex items-center gap-1.5"
                  >
                    <span class="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse"></span>
                    <span>{{ summary().activeAgentClaims }} active lease(s)</span>
                  </span>
                }
              </div>
              <p class="text-xs text-zinc-500 dark:text-zinc-400 max-w-2xl">
                Track macro-objectives, verifiable success criteria, and live task execution progress in real time as agents claim, write <code class="font-mono">.md</code> specs, and complete tasks.
              </p>
            </div>

            <button
              type="button"
              (click)="openNewObjective.emit()"
              class="px-3.5 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 hover:bg-zinc-800 dark:hover:bg-zinc-200 text-xs font-medium transition-colors"
            >
              + New Goal / Milestone
            </button>
          </div>

          <!-- KPI Strip -->
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 pt-1">
            <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-950/70 border border-zinc-200/80 dark:border-zinc-800/80">
              <div class="text-[11px] text-zinc-500 dark:text-zinc-400">Open Goals & Milestones</div>
              <div class="mt-0.5 text-lg font-mono font-semibold text-zinc-900 dark:text-zinc-100">
                {{ summary().openObjectives }}
              </div>
            </div>

            <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-950/70 border border-zinc-200/80 dark:border-zinc-800/80">
              <div class="text-[11px] text-zinc-500 dark:text-zinc-400">At Risk</div>
              <div
                class="mt-0.5 text-lg font-mono font-semibold"
                [class]="summary().atRiskObjectives > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-zinc-900 dark:text-zinc-100'"
              >
                {{ summary().atRiskObjectives }}
              </div>
            </div>

            <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-950/70 border border-zinc-200/80 dark:border-zinc-800/80">
              <div class="text-[11px] text-zinc-500 dark:text-zinc-400">Achieved</div>
              <div class="mt-0.5 text-lg font-mono font-semibold text-emerald-600 dark:text-emerald-400">
                {{ summary().achievedObjectives }}
              </div>
            </div>

            <div class="p-3 rounded-lg bg-zinc-50 dark:bg-zinc-950/70 border border-zinc-200/80 dark:border-zinc-800/80">
              <div class="flex items-center justify-between text-[11px] text-zinc-500 dark:text-zinc-400">
                <span>Linked Tasks Progress</span>
                <span class="font-mono">{{ summary().doneTasks }}/{{ summary().totalTasks }}</span>
              </div>
              <div class="mt-2 flex items-center gap-2">
                <div class="flex-1 h-2 rounded-full bg-zinc-200 dark:bg-zinc-800 overflow-hidden">
                  <div
                    class="h-full bg-emerald-500 transition-all duration-300"
                    [style.width.%]="summary().overallPct"
                  ></div>
                </div>
                <span class="text-xs font-mono font-semibold text-zinc-800 dark:text-zinc-200">
                  {{ summary().overallPct }}%
                </span>
              </div>
            </div>
          </div>

          <!-- Filter Controls -->
          <div class="flex flex-wrap items-center justify-between gap-2.5 pt-1 border-t border-zinc-200/70 dark:border-zinc-800/70">
            <div class="flex flex-wrap items-center gap-1.5">
              <button
                type="button"
                (click)="statusFilter.set('open')"
                class="px-2.5 py-1 rounded-md text-xs transition-colors"
                [class]="filterBtnClass(statusFilter() === 'open')"
              >
                Active & Planned ({{ summary().openObjectives }})
              </button>
              <button
                type="button"
                (click)="statusFilter.set('at_risk')"
                class="px-2.5 py-1 rounded-md text-xs transition-colors"
                [class]="filterBtnClass(statusFilter() === 'at_risk')"
              >
                At Risk ({{ summary().atRiskObjectives }})
              </button>
              <button
                type="button"
                (click)="statusFilter.set('achieved')"
                class="px-2.5 py-1 rounded-md text-xs transition-colors"
                [class]="filterBtnClass(statusFilter() === 'achieved')"
              >
                Achieved ({{ summary().achievedObjectives }})
              </button>
              <button
                type="button"
                (click)="statusFilter.set('dropped')"
                class="px-2.5 py-1 rounded-md text-xs transition-colors"
                [class]="filterBtnClass(statusFilter() === 'dropped')"
              >
                Dropped ({{ summary().droppedObjectives }})
              </button>
              <button
                type="button"
                (click)="statusFilter.set('all')"
                class="px-2.5 py-1 rounded-md text-xs transition-colors"
                [class]="filterBtnClass(statusFilter() === 'all')"
              >
                All ({{ summary().totalObjectives }})
              </button>
            </div>

            <div class="flex items-center gap-2">
              <select
                aria-label="Filter objectives by kind"
                [ngModel]="kindFilter()"
                (ngModelChange)="kindFilter.set($event)"
                class="px-2.5 py-1 rounded-md text-xs bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 focus:outline-none"
              >
                <option value="all">All Kinds</option>
                <option value="goal">Goals</option>
                <option value="milestone">Milestones</option>
                <option value="cycle">Cycles</option>
              </select>

              <input
                type="search"
                [ngModel]="query()"
                (ngModelChange)="query.set($event)"
                placeholder="Filter goals or linked tasks..."
                class="px-3 py-1 rounded-md text-xs bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 focus:outline-none w-52"
              />
            </div>
          </div>
        </div>

        <!-- Empty State -->
        @if (visibleObjectives().length === 0) {
          <div
            class="rounded-xl border border-dashed border-zinc-300 dark:border-zinc-800 p-10 text-center space-y-3 bg-white/50 dark:bg-zinc-900/30"
          >
            <div class="text-sm font-medium text-zinc-800 dark:text-zinc-200">
              @if (store.objectivesWithProgress().length === 0) {
                No strategic goals or milestones defined yet
              } @else {
                No goals match the active filter
              }
            </div>
            <p class="text-xs text-zinc-500 max-w-md mx-auto">
              Define macro-objectives, milestones, or cycles and link backlog/board tasks to watch completion progress in real time.
            </p>
            @if (store.objectivesWithProgress().length === 0) {
              <button
                type="button"
                (click)="openNewObjective.emit()"
                class="px-4 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium"
              >
                + Create First Goal
              </button>
            }
          </div>
        }

        <!-- Structured Goal / Milestone Cards -->
        <div class="space-y-4">
          @for (obj of visibleObjectives(); track obj.id) {
            <section
              class="rounded-xl bg-white dark:bg-zinc-900/65 border border-zinc-200 dark:border-zinc-800/90 overflow-hidden shadow-2xs"
            >
              <!-- Card Header -->
              <div class="p-4 sm:px-5 space-y-3">
                <div class="flex flex-wrap items-start justify-between gap-3">
                  <div class="space-y-1 min-w-0 flex-1">
                    <div class="flex flex-wrap items-center gap-2">
                      <button
                        type="button"
                        (click)="toggleCollapsed(obj.id)"
                        class="text-xs font-mono text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
                        title="Expand or collapse goal tasks"
                      >
                        {{ isCollapsed(obj.id) ? '▸' : '▾' }}
                      </button>

                      @if (!store.activeProjectId()) {
                        <span
                          class="text-[10px] font-mono px-1.5 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 border border-zinc-200 dark:border-zinc-700 text-zinc-700 dark:text-zinc-300"
                        >
                          {{ obj.projectKeyPrefix }}
                        </span>
                      }

                      <span
                        class="text-[10px] font-mono uppercase tracking-wider px-1.5 py-0.5 rounded border"
                        [class]="kindBadgeClass(obj.kind)"
                      >
                        {{ kindLabel(obj.kind) }}
                      </span>

                      <h3
                        (click)="toggleCollapsed(obj.id)"
                        class="text-sm font-semibold text-zinc-900 dark:text-zinc-100 cursor-pointer truncate"
                      >
                        {{ obj.title }}
                      </h3>

                      @if (obj.targetDate) {
                        <span class="text-[11px] font-mono text-zinc-500 dark:text-zinc-400">
                          Target: {{ formatDate(obj.targetDate) }}
                        </span>
                      }

                      @if (obj.activeClaimsCount > 0) {
                        <span
                          class="text-[10px] font-mono px-1.5 py-0.5 rounded bg-emerald-500/10 text-emerald-600 dark:text-emerald-300 border border-emerald-500/30"
                        >
                          ● {{ obj.activeClaimsCount }} agent working
                        </span>
                      }
                    </div>

                    @if (obj.description) {
                      <p class="text-xs text-zinc-600 dark:text-zinc-400 leading-relaxed pl-5">
                        {{ obj.description }}
                      </p>
                    }
                  </div>

                  <!-- Status Selector & Goal Actions -->
                  <div class="flex flex-wrap items-center gap-2 shrink-0">
                    <select
                      aria-label="Objective status"
                      [ngModel]="obj.status"
                      (ngModelChange)="changeObjectiveStatus(obj, $event)"
                      class="px-2 py-1 rounded-md text-xs font-medium border focus:outline-none"
                      [class]="statusSelectClass(obj.status)"
                    >
                      <option value="planned">Planned</option>
                      <option value="active">Active</option>
                      <option value="at_risk">At Risk</option>
                      <option value="achieved">Achieved</option>
                      <option value="dropped">Dropped</option>
                    </select>

                    <button
                      type="button"
                      (click)="openNewTaskForObjective.emit(obj)"
                      class="px-2.5 py-1 rounded-md text-xs bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 hover:bg-zinc-800 dark:hover:bg-zinc-200 font-medium transition-colors"
                    >
                      + Add Task
                    </button>

                    <button
                      type="button"
                      (click)="filterBoardByGoal(obj)"
                      class="px-2 py-1 rounded-md text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 transition-colors"
                      title="Open Kanban board filtered to this goal"
                    >
                      Board →
                    </button>

                    <button
                      type="button"
                      (click)="openEditObjective.emit(obj)"
                      class="px-2 py-1 rounded-md text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors"
                    >
                      Edit
                    </button>

                    <button
                      type="button"
                      (click)="removeObjective(obj)"
                      class="px-2 py-1 rounded-md text-xs border border-rose-500/30 text-rose-600 dark:text-rose-400 hover:bg-rose-500/10 transition-colors"
                      title="Delete objective (keeps tasks intact)"
                    >
                      Delete
                    </button>
                  </div>
                </div>

                <!-- Multi-Segment Progress Bar & Breakdown Pills -->
                <div class="pl-5 space-y-1.5">
                  <div class="flex items-center gap-3">
                    <div class="flex-1 h-2 rounded-full bg-zinc-200/90 dark:bg-zinc-800 overflow-hidden flex">
                      @if (obj.progressPct > 0) {
                        <div
                          class="h-full bg-emerald-500 transition-all duration-300"
                          [style.width.%]="obj.progressPct"
                          title="Completed: {{ obj.doneCount }}"
                        ></div>
                      }
                      @if (obj.activeWorkPct > 0) {
                        <div
                          class="h-full bg-amber-400 transition-all duration-300"
                          [style.width.%]="obj.activeWorkPct"
                          title="In Progress / Review: {{ obj.inProgressCount + obj.inReviewCount }}"
                        ></div>
                      }
                      @if (obj.blockedPct > 0) {
                        <div
                          class="h-full bg-rose-500 transition-all duration-300"
                          [style.width.%]="obj.blockedPct"
                          title="Blocked: {{ obj.blockedCount }}"
                        ></div>
                      }
                    </div>
                    <span class="text-xs font-mono font-semibold text-zinc-800 dark:text-zinc-200 tabular-nums w-20 text-right">
                      {{ obj.doneCount }}/{{ obj.totalCount }} ({{ obj.progressPct }}%)
                    </span>
                  </div>

                  <div class="flex flex-wrap items-center gap-3 text-[11px] text-zinc-500 dark:text-zinc-400">
                    <span class="flex items-center gap-1">
                      <span class="w-1.5 h-1.5 rounded-full bg-emerald-500"></span>
                      <span>{{ obj.doneCount }} done</span>
                    </span>
                    <span class="flex items-center gap-1">
                      <span class="w-1.5 h-1.5 rounded-full bg-amber-400"></span>
                      <span>{{ obj.inProgressCount + obj.inReviewCount }} in progress/review</span>
                    </span>
                    @if (obj.blockedCount > 0) {
                      <span class="flex items-center gap-1 text-rose-600 dark:text-rose-400 font-medium">
                        <span class="w-1.5 h-1.5 rounded-full bg-rose-500"></span>
                        <span>{{ obj.blockedCount }} blocked</span>
                      </span>
                    }
                    <span class="flex items-center gap-1">
                      <span class="w-1.5 h-1.5 rounded-full bg-sky-400"></span>
                      <span>{{ obj.todoCount }} todo</span>
                    </span>
                    @if (obj.backlogCount > 0) {
                      <span class="flex items-center gap-1">
                        <span class="w-1.5 h-1.5 rounded-full bg-zinc-400"></span>
                        <span>{{ obj.backlogCount }} backlog</span>
                      </span>
                    }
                  </div>
                </div>
              </div>

              <!-- Expandable Body: Success Criteria + Linked Tasks Table + Quick Attach -->
              @if (!isCollapsed(obj.id)) {
                <div class="border-t border-zinc-200/80 dark:border-zinc-800/80 bg-zinc-50/50 dark:bg-zinc-950/45">
                  @if (obj.successCriteria.length > 0 && !(obj.successCriteria.length === 1 && obj.successCriteria[0] === obj.description)) {
                    <div class="px-5 py-3 border-b border-zinc-200/70 dark:border-zinc-800/70 space-y-1.5">
                      <div class="text-[10px] font-medium uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
                        Success Criteria
                      </div>
                      <ul class="space-y-1 text-xs text-zinc-700 dark:text-zinc-300">
                        @for (criterion of obj.successCriteria; track $index) {
                          <li class="flex items-start gap-2">
                            <span
                              class="font-mono text-[11px] mt-0.5"
                              [class]="obj.status === 'achieved' ? 'text-emerald-500' : 'text-zinc-400'"
                            >
                              {{ obj.status === 'achieved' ? '✓' : '◎' }}
                            </span>
                            <span>{{ criterion }}</span>
                          </li>
                        }
                      </ul>
                    </div>
                  }

                  <!-- Linked Tasks List -->
                  @if (obj.tasks.length > 0) {
                    <div class="divide-y divide-zinc-200/70 dark:divide-zinc-800/70">
                      @for (task of obj.tasks; track task.id) {
                        <div
                          (click)="store.loadTaskInspector({ taskId: task.id })"
                          [class.task-signal-row-glow]="store.isTaskGlowing(task)"
                          class="px-5 py-2.5 flex flex-wrap items-center justify-between gap-2.5 hover:bg-zinc-100/70 dark:hover:bg-zinc-900/70 cursor-pointer transition-colors text-xs"
                        >
                          <div class="flex items-center gap-2.5 min-w-0 flex-1">
                            <span class="font-mono text-[11px] text-zinc-500 dark:text-zinc-400 w-20 shrink-0 flex items-center gap-1.5">
                              @if (store.isTaskGlowing(task)) {
                                <span
                                  class="relative flex h-2 w-2 shrink-0"
                                  [title]="'Active agent signal · ' + store.taskSignalAgeLabel(task)"
                                >
                                  <span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
                                  <span class="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
                                </span>
                              }
                              <span>{{ task.identifier }}</span>
                            </span>
                            <span
                              class="text-[10px] font-mono px-1.5 py-0.5 rounded border shrink-0"
                              [class]="priorityClass(task)"
                            >
                              {{ shortPrio(task) }}
                            </span>
                            <span
                              class="truncate font-medium"
                              [class]="
                                task.status === 'done'
                                  ? 'line-through text-zinc-400 dark:text-zinc-500'
                                  : 'text-zinc-900 dark:text-zinc-100'
                              "
                            >
                              {{ task.title }}
                            </span>
                            @if (task.hasMarkdownDoc) {
                              <span
                                class="text-[10px] font-mono px-1.5 py-0.5 rounded bg-sky-500/10 text-sky-600 dark:text-sky-300 border border-sky-500/30 shrink-0"
                                title="Extended Task .md spec stored in ~/.flowsk/tasks"
                              >
                                .md
                              </span>
                            }
                            <span class="text-[11px] text-zinc-400 dark:text-zinc-500 hidden md:inline shrink-0">
                              {{ taskKindLabel(task) }}
                            </span>
                          </div>

                          <div class="flex items-center gap-2 shrink-0" (click)="$event.stopPropagation()">
                            @if (store.activeClaimHolderId(task); as holderId) {
                              <span
                                class="px-1.5 py-0.5 rounded bg-emerald-500/10 border border-emerald-500/30 text-emerald-600 dark:text-emerald-300 font-mono text-[11px] flex items-center gap-1"
                              >
                                <span class="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse"></span>
                                <span>@{{ actorHandle(holderId) }}</span>
                              </span>
                            }

                            <select
                              aria-label="Task workflow status"
                              [ngModel]="task.status"
                              (ngModelChange)="changeTaskStatus(task.id, $event)"
                              class="px-2 py-1 rounded text-[11px] bg-white dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200 focus:outline-none"
                            >
                              @for (col of statusColumns; track col.status) {
                                <option [value]="col.status">{{ col.label }}</option>
                              }
                            </select>

                            <button
                              type="button"
                              (click)="detachTask(task.id)"
                              class="px-1.5 py-0.5 rounded text-[11px] text-zinc-400 hover:text-rose-500 transition-colors"
                              title="Detach task from this goal"
                            >
                              ✕
                            </button>
                          </div>
                        </div>
                      }
                    </div>
                  } @else {
                    <div class="px-5 py-4 text-xs text-zinc-400 dark:text-zinc-500">
                      No tasks linked to this {{ obj.kind }} yet. Add a new task or attach an existing unlinked task below.
                    </div>
                  }

                  <!-- Footer: Quick Attach Existing Task in Same Project -->
                  @if (unlinkedTasksForProject(obj.projectId).length > 0) {
                    <div
                      class="px-5 py-2.5 border-t border-zinc-200/70 dark:border-zinc-800/70 flex flex-wrap items-center justify-between gap-2"
                    >
                      <span class="text-[11px] text-zinc-500 dark:text-zinc-400">
                        Attach existing project task to this {{ obj.kind }}:
                      </span>
                      <select
                        aria-label="Attach existing task to goal"
                        [ngModel]="''"
                        (ngModelChange)="attachExistingTask(obj.id, $event)"
                        class="px-2.5 py-1 rounded-md text-xs bg-white dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 focus:outline-none max-w-xs"
                      >
                        <option value="">+ Select unlinked task ({{ unlinkedTasksForProject(obj.projectId).length }})...</option>
                        @for (cand of unlinkedTasksForProject(obj.projectId); track cand.id) {
                          <option [value]="cand.id">{{ cand.identifier }} — {{ cand.title }}</option>
                        }
                      </select>
                    </div>
                  }
                </div>
              }
            </section>
          }
        </div>
      </div>
    </div>
  `,
})
export class GoalsViewComponent {
  readonly store = inject(FlowskStore);
  readonly statusColumns = CANONICAL_STATUS_COLUMNS;

  readonly openNewObjective = output<void>();
  readonly openEditObjective = output<ObjectiveModel>();
  readonly openNewTaskForObjective = output<ObjectiveModel>();

  readonly statusFilter = signal<GoalStatusFilter>('open');
  readonly kindFilter = signal<ObjectiveKind | 'all'>('all');
  readonly query = signal<string>('');
  private readonly collapsedIds = signal<ReadonlySet<string>>(new Set<string>());

  readonly summary = computed(() => {
    const all = this.store.objectivesWithProgress();
    let openObjectives = 0;
    let atRiskObjectives = 0;
    let achievedObjectives = 0;
    let droppedObjectives = 0;
    let totalTasks = 0;
    let doneTasks = 0;
    let activeAgentClaims = 0;

    for (const o of all) {
      if (o.status === 'achieved') {
        achievedObjectives++;
      } else if (o.status === 'dropped') {
        droppedObjectives++;
      } else {
        openObjectives++;
      }
      if (o.status === 'at_risk') {
        atRiskObjectives++;
      }
      totalTasks += o.totalCount;
      doneTasks += o.doneCount;
      activeAgentClaims += o.activeClaimsCount;
    }
    const overallPct = totalTasks > 0 ? Math.round((doneTasks * 100) / totalTasks) : 0;

    return {
      totalObjectives: all.length,
      openObjectives,
      atRiskObjectives,
      achievedObjectives,
      droppedObjectives,
      totalTasks,
      doneTasks,
      overallPct,
      activeAgentClaims,
    };
  });

  readonly visibleObjectives = computed<readonly ObjectiveProgressView[]>(() => {
    const sf = this.statusFilter();
    const kf = this.kindFilter();
    const q = this.query().trim().toLowerCase();

    return this.store.objectivesWithProgress().filter((o) => {
      if (sf === 'open' && (o.status === 'achieved' || o.status === 'dropped')) {
        return false;
      }
      if (sf === 'at_risk' && o.status !== 'at_risk') {
        return false;
      }
      if (sf === 'achieved' && o.status !== 'achieved') {
        return false;
      }
      if (sf === 'dropped' && o.status !== 'dropped') {
        return false;
      }
      if (kf !== 'all' && o.kind !== kf) {
        return false;
      }
      if (q.length > 0) {
        const taskHaystack = o.tasks.map((t) => `${t.identifier} ${t.title}`).join(' ');
        const hay = `${o.projectKeyPrefix} ${o.title} ${o.description} ${o.successCriteria.join(' ')} ${taskHaystack}`.toLowerCase();
        if (!hay.includes(q)) {
          return false;
        }
      }
      return true;
    });
  });

  isCollapsed(id: string): boolean {
    return this.collapsedIds().has(id);
  }

  toggleCollapsed(id: string): void {
    const next = new Set(this.collapsedIds());
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    this.collapsedIds.set(next);
  }

  changeObjectiveStatus(obj: ObjectiveModel, nextStatus: ObjectiveStatus): void {
    if (obj.status === nextStatus) return;
    if (nextStatus === 'achieved' || nextStatus === 'dropped') {
      this.store.closeObjective({
        payload: { objectiveId: obj.id, achieved: nextStatus === 'achieved' },
      });
      return;
    }
    this.store.updateObjective({
      payload: { objectiveId: obj.id, status: nextStatus },
    });
  }

  removeObjective(obj: ObjectiveModel): void {
    this.store.deleteObjective({ payload: { objectiveId: obj.id } });
  }

  filterBoardByGoal(obj: ObjectiveModel): void {
    if (this.store.activeProjectId() !== obj.projectId) {
      this.store.selectProject(obj.projectId);
    }
    this.store.setScopeTab('board');
    this.store.setObjectiveFilter(obj.id);
  }

  changeTaskStatus(taskId: string, targetStatus: TaskStatus): void {
    this.store.moveTaskToStatus({ payload: { taskId, targetStatus } });
  }

  attachExistingTask(objectiveId: string, taskId: string): void {
    if (!taskId) return;
    this.store.assignTaskToObjective({ payload: { taskId, objectiveId } });
  }

  detachTask(taskId: string): void {
    this.store.assignTaskToObjective({ payload: { taskId, objectiveId: null } });
  }

  unlinkedTasksForProject(projectId: string): TaskModel[] {
    return this.store
      .workspaceTasks()
      .filter(
        (t) =>
          t.projectId === projectId &&
          !t.objectiveId &&
          t.status !== 'archived' &&
          t.status !== 'cancelled',
      )
      .slice()
      .sort((a, b) => a.sequenceNumber - b.sequenceNumber);
  }

  kindLabel(kind: ObjectiveKind): string {
    return formatObjectiveKindLabel(kind);
  }

  taskKindLabel(task: TaskModel): string {
    return formatKindLabel(task.kind);
  }

  shortPrio(task: TaskModel): string {
    return formatShortPriority(task.priority);
  }

  actorHandle(actorId: string): string {
    return this.store.actorMapById()[actorId]?.handle ?? 'agent';
  }

  formatDate(iso: string): string {
    return iso.slice(0, 10);
  }

  filterBtnClass(active: boolean): string {
    return active
      ? 'bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 font-medium'
      : 'bg-zinc-100 dark:bg-zinc-800/80 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-200';
  }

  kindBadgeClass(kind: ObjectiveKind): string {
    switch (kind) {
      case 'milestone':
        return 'border-violet-500/40 bg-violet-500/10 text-violet-600 dark:text-violet-300';
      case 'cycle':
        return 'border-sky-500/40 bg-sky-500/10 text-sky-600 dark:text-sky-300';
      default:
        return 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300';
    }
  }

  statusSelectClass(status: ObjectiveStatus): string {
    switch (status) {
      case 'at_risk':
        return 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300';
      case 'achieved':
        return 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300';
      case 'dropped':
        return 'border-zinc-300 dark:border-zinc-700 bg-zinc-100 dark:bg-zinc-900 text-zinc-500';
      case 'planned':
        return 'border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-300';
      default:
        return 'border-zinc-300 dark:border-zinc-700 bg-zinc-50 dark:bg-zinc-900 text-zinc-800 dark:text-zinc-200';
    }
  }

  priorityClass(task: TaskModel): string {
    switch (task.priority) {
      case 'TASK_PRIORITY_URGENT':
        return 'border-rose-500/40 text-rose-600 dark:text-rose-400 bg-rose-500/10';
      case 'TASK_PRIORITY_HIGH':
        return 'border-amber-500/40 text-amber-600 dark:text-amber-400 bg-amber-500/10';
      case 'TASK_PRIORITY_LOW':
        return 'border-zinc-300 dark:border-zinc-800 text-zinc-400';
      default:
        return 'border-zinc-300 dark:border-zinc-700 text-zinc-600 dark:text-zinc-300';
    }
  }
}
