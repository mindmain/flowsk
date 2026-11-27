import { ChangeDetectionStrategy, Component, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  LiveStreamEventModel,
  ProjectHierarchyNode,
  TaskPriority,
  TaskStatus,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

@Component({
  selector: 'app-topbar',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <header
      class="border-b border-zinc-200 dark:border-zinc-800/90 bg-white/80 dark:bg-zinc-950/90 backdrop-blur px-5 py-2.5 flex flex-col gap-2.5 relative z-20"
    >
      <!-- Top Row: Project Selector, Subproject Breadcrumbs, Live Stream Pill, View Switcher & Primary Actions -->
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex flex-wrap items-center gap-2 min-w-0">
          <!-- Global Project & Subproject Selector -->
          <div class="flex items-center gap-1.5">
            <span class="text-[11px] font-medium text-zinc-400 dark:text-zinc-500 hidden sm:inline">
              Project
            </span>
            <select
              aria-label="Select active project or subproject"
              [ngModel]="store.activeProjectId() ?? ''"
              (ngModelChange)="onProjectChange($event)"
              class="px-2.5 py-1 rounded-md text-xs font-medium bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400 dark:focus:border-zinc-600 max-w-[300px]"
            >
              <option value="">
                All Registered Projects ({{ store.visibleProjects().length }})
              </option>
              @for (node of store.flatProjectHierarchy(); track node.lineageKey) {
                <option [value]="node.project.id">
                  {{ formatSelectorOption(node) }}
                </option>
              }
            </select>
          </div>

          @if (store.scopeTab() === 'archived') {
            <span class="text-xs text-zinc-400">·</span>
            <h1 class="text-xs font-semibold text-zinc-800 dark:text-zinc-200">
              Workspace Archive ({{ store.workspaceArchivedCount() }})
            </h1>
          } @else if (store.activeProject(); as proj) {
            @if (store.activeProjectParentInfo(); as parentInfo) {
              <button
                type="button"
                (click)="store.selectProject(parentInfo.parent.id)"
                class="text-[11px] px-2 py-0.5 rounded bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors font-mono"
                title="Jump to parent project {{ parentInfo.parent.name }}"
              >
                ↑ {{ parentInfo.parent.keyPrefix }}@if (parentInfo.scopeSubpath) { · {{ parentInfo.scopeSubpath }} }
              </button>
            }

            @for (sub of store.activeProjectChildren(); track sub.child.id) {
              <button
                type="button"
                (click)="store.selectProject(sub.child.id)"
                class="text-[11px] px-2 py-0.5 rounded bg-zinc-100/80 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors font-mono"
                title="Open subproject {{ sub.child.name }}{{ sub.scopeSubpath ? ' (' + sub.scopeSubpath + ')' : '' }}"
              >
                └─ {{ sub.child.keyPrefix }}
              </button>
            }

            <button
              type="button"
              (click)="openProjectCharter.emit()"
              class="text-[11px] px-2 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 hover:border-zinc-300 dark:hover:border-zinc-700 transition-colors"
            >
              {{ proj.hasMarkdownDoc ? 'Edit Project .md' : '+ Project .md' }}
            </button>
            @if (proj.status !== 'archived') {
              <button
                type="button"
                (click)="archiveCurrentProject(proj.id)"
                class="text-[11px] text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-300 transition-colors"
                title="Archive project"
              >
                Archive project
              </button>
            }
          }
        </div>

        <div class="flex items-center gap-2">
          <!-- Real-Time Go Channel SSE Stream Status & Activity Feed Popover -->
          <div class="relative">
            <button
              type="button"
              (click)="activityOpen.set(!activityOpen())"
              class="px-2.5 py-1 rounded-md border text-[11px] font-medium flex items-center gap-1.5 transition-colors"
              [class]="streamPillClass()"
              title="Real-time Go channel event stream (click to inspect live agent activity)"
            >
              <span class="w-1.5 h-1.5 rounded-full" [class]="streamDotClass()"></span>
              <span>{{ streamLabel() }}</span>
              @if (store.liveEvents().length > 0) {
                <span class="font-mono text-[10px] opacity-80">· {{ store.liveEvents().length }}</span>
              }
            </button>

            @if (activityOpen()) {
              <div
                class="absolute right-0 mt-1.5 w-84 rounded-xl bg-white dark:bg-zinc-950 border border-zinc-200 dark:border-zinc-800 shadow-2xl p-3 space-y-2.5 z-50"
              >
                <div class="flex items-center justify-between gap-2 pb-1.5 border-b border-zinc-200 dark:border-zinc-800">
                  <div class="text-xs font-semibold text-zinc-900 dark:text-zinc-100">
                    Real-Time Agent & UI Stream
                  </div>
                  <div class="flex items-center gap-1.5">
                    @if (store.liveEvents().length > 0) {
                      <button
                        type="button"
                        (click)="store.clearLiveEvents()"
                        class="text-[10px] px-1.5 py-0.5 rounded text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
                      >
                        Clear
                      </button>
                    }
                    <button
                      type="button"
                      (click)="store.toggleLiveStream()"
                      class="text-[10px] px-2 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900"
                    >
                      {{ store.liveStreamEnabled() ? 'Pause' : 'Resume' }}
                    </button>
                  </div>
                </div>

                @if (store.liveEvents().length === 0) {
                  <div class="py-5 text-center text-[11px] text-zinc-400 dark:text-zinc-500">
                    Listening on Go channel SSE stream. Agent claims, task updates, and goal changes appear here automatically.
                  </div>
                } @else {
                  <div class="max-h-64 overflow-y-auto space-y-1.5">
                    @for (ev of store.liveEvents(); track (ev.id || $index) + '-' + ev.occurredAt) {
                      <div
                        class="p-2 rounded-lg bg-zinc-50 dark:bg-zinc-900/70 border border-zinc-200/70 dark:border-zinc-800/80 text-[11px] space-y-0.5"
                      >
                        <div class="flex items-center justify-between gap-2">
                          <span class="font-mono font-medium text-zinc-800 dark:text-zinc-200">
                            @{{ ev.actorHandle || 'actor' }}
                          </span>
                          <span class="font-mono text-[10px] text-zinc-400">
                            {{ formatEventTime(ev.occurredAt) }}
                          </span>
                        </div>
                        <div class="text-zinc-600 dark:text-zinc-400 truncate">
                          <span class="font-medium text-emerald-600 dark:text-emerald-400">{{ ev.action }}</span>
                          · {{ ev.resourceType }}
                        </div>
                      </div>
                    }
                  </div>
                }
              </div>
            }
          </div>

          @if (store.orphanReport().totalIssues > 0) {
            <button
              type="button"
              (click)="openPruneModal.emit()"
              class="px-2.5 py-1 rounded-md bg-amber-500/15 border border-amber-500/40 text-amber-700 dark:text-amber-300 text-xs font-medium hover:bg-amber-500/25 transition-colors"
            >
              ⚠ {{ store.orphanReport().totalIssues }} orphan .md — Prune
            </button>
          }

          @if (store.scopeTab() !== 'goals' && (store.activeProjectId() !== null || store.scopeTab() !== 'board')) {
            <div
              class="inline-flex p-0.5 rounded-md bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800"
            >
              <button
                type="button"
                (click)="store.setViewMode('kanban')"
                class="px-2.5 py-1 rounded text-xs transition-colors"
                [class]="
                  store.viewMode() === 'kanban'
                    ? 'bg-white dark:bg-zinc-800 text-zinc-900 dark:text-zinc-100 shadow-2xs font-medium'
                    : 'text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-200'
                "
              >
                Kanban
              </button>
              <button
                type="button"
                (click)="store.setViewMode('grid')"
                class="px-2.5 py-1 rounded text-xs transition-colors"
                [class]="
                  store.viewMode() === 'grid'
                    ? 'bg-white dark:bg-zinc-800 text-zinc-900 dark:text-zinc-100 shadow-2xs font-medium'
                    : 'text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-200'
                "
              >
                Grid
              </button>
            </div>
          }

          @if (store.workspaceArchivedCount() > 0) {
            <button
              type="button"
              (click)="openPurgeArchive.emit()"
              class="px-2.5 py-1 rounded-md border border-rose-500/40 bg-rose-500/10 text-rose-600 dark:text-rose-300 hover:bg-rose-500/20 text-xs font-medium transition-colors"
            >
              Purge Archive ({{ store.workspaceArchivedCount() }})
            </button>
          }

          @if (store.visibleProjects().length > 0) {
            <button
              type="button"
              (click)="openNewTask.emit()"
              class="px-3 py-1 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 hover:bg-zinc-800 dark:hover:bg-zinc-200 text-xs font-medium transition-colors"
            >
              + New Task
            </button>
          }

          <button
            type="button"
            (click)="store.refreshProjectContext({ projectId: store.activeProjectId() })"
            class="px-2 py-1 rounded-md text-xs border border-zinc-200 dark:border-zinc-800 text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors"
            title="Manual refresh"
          >
            ↻
          </button>
        </div>
      </div>

      <!-- Second Row: Instant Task Search & Structured Filters (shown on task board/backlog/all/archive) -->
      @if (store.scopeTab() !== 'goals' && (store.activeProjectId() !== null || store.scopeTab() !== 'board')) {
        <div class="flex flex-wrap items-center gap-2">
          <div class="relative flex-1 min-w-[220px]">
            <input
              type="search"
              [ngModel]="store.searchQuery()"
              (ngModelChange)="store.setSearchQuery($event)"
              placeholder="Search tasks (e.g. state:in_progress is:md is:claimed ext:CU-123 @actor keyword)..."
              class="w-full px-3 py-1.5 rounded-md text-xs bg-zinc-100/80 dark:bg-zinc-900/90 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500 focus:outline-none focus:border-zinc-400 dark:focus:border-zinc-600"
            />
          </div>

          <select
            [ngModel]="store.statusFilter()"
            (ngModelChange)="onStatusChange($event)"
            class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-100/80 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 focus:outline-none"
          >
            <option value="all">All Statuses</option>
            @for (col of statusColumns; track col.status) {
              <option [value]="col.status">{{ col.label }}</option>
            }
          </select>

          <select
            [ngModel]="store.priorityFilter()"
            (ngModelChange)="onPriorityChange($event)"
            class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-100/80 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 focus:outline-none"
          >
            <option value="all">All Priorities</option>
            <option value="TASK_PRIORITY_URGENT">P1 · Urgent</option>
            <option value="TASK_PRIORITY_HIGH">P2 · High</option>
            <option value="TASK_PRIORITY_MEDIUM">P3 · Medium</option>
            <option value="TASK_PRIORITY_LOW">P4 · Low</option>
            <option value="TASK_PRIORITY_NONE">Unprioritized</option>
          </select>

          @if (store.scopedObjectives().length > 0) {
            <select
              [ngModel]="store.objectiveFilterId()"
              (ngModelChange)="store.setObjectiveFilter($event)"
              class="px-2.5 py-1.5 rounded-md text-xs bg-zinc-100/80 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-700 dark:text-zinc-300 focus:outline-none max-w-[180px]"
            >
              <option value="all">All Goals</option>
              @for (obj of store.scopedObjectives(); track obj.id) {
                <option [value]="obj.id">{{ obj.title }}</option>
              }
            </select>
          }

          @if (store.scopeTab() === 'board') {
            <button
              type="button"
              (click)="store.toggleCompletedOnBoard()"
              class="px-2.5 py-1.5 rounded-md text-xs border transition-colors"
              [class]="
                store.includeCompletedOnBoard()
                  ? 'border-zinc-300 dark:border-zinc-700 text-zinc-800 dark:text-zinc-200 bg-zinc-100 dark:bg-zinc-900'
                  : 'border-zinc-200 dark:border-zinc-800 text-zinc-400 dark:text-zinc-500'
              "
            >
              Done column
            </button>
          }
        </div>
      }
    </header>
  `,
})
export class TopbarComponent {
  readonly store = inject(FlowskStore);
  readonly statusColumns = CANONICAL_STATUS_COLUMNS;
  readonly activityOpen = signal<boolean>(false);

  readonly openNewTask = output<void>();
  readonly openProjectCharter = output<void>();
  readonly openPurgeArchive = output<void>();
  readonly openPruneModal = output<void>();

  onProjectChange(projectId: string): void {
    this.store.selectProject(projectId ? projectId : null);
  }

  closeActivityPopover(): boolean {
    if (!this.activityOpen()) {
      return false;
    }
    this.activityOpen.set(false);
    return true;
  }

  formatSelectorOption(node: ProjectHierarchyNode): string {
    const indent = node.depth > 0 ? `${'\u00A0\u00A0'.repeat(node.depth)}└─ ` : '';
    const subHint = node.scopeSubpath ? ` [${node.scopeSubpath}]` : '';
    return `${indent}${node.project.keyPrefix} — ${node.project.name}${subHint} (${node.counts.active})`;
  }

  onStatusChange(val: TaskStatus | 'all'): void {
    this.store.setStatusFilter(val);
  }

  onPriorityChange(val: TaskPriority | 'all'): void {
    this.store.setPriorityFilter(val);
  }

  archiveCurrentProject(projectId: string): void {
    this.store.archiveProject({ payload: { projectId } });
  }

  streamLabel(): string {
    switch (this.store.streamStatus()) {
      case 'live':
        return 'Live';
      case 'reconnecting':
        return 'Reconnecting';
      case 'paused':
        return 'Paused';
      default:
        return 'Connecting';
    }
  }

  streamPillClass(): string {
    switch (this.store.streamStatus()) {
      case 'live':
        return 'border-emerald-500/35 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 hover:bg-emerald-500/20';
      case 'reconnecting':
        return 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300';
      default:
        return 'border-zinc-200 dark:border-zinc-800 bg-zinc-100 dark:bg-zinc-900 text-zinc-500 dark:text-zinc-400';
    }
  }

  streamDotClass(): string {
    switch (this.store.streamStatus()) {
      case 'live':
        return 'bg-emerald-400 animate-pulse';
      case 'reconnecting':
        return 'bg-amber-400 animate-pulse';
      default:
        return 'bg-zinc-400';
    }
  }

  formatEventTime(iso: LiveStreamEventModel['occurredAt']): string {
    if (!iso || iso.length < 19) return '';
    return iso.slice(11, 19);
  }
}
