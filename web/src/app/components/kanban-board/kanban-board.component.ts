import { CdkDrag, CdkDragDrop, CdkDropList, CdkDropListGroup } from '@angular/cdk/drag-drop';
import { ChangeDetectionStrategy, Component, inject, output } from '@angular/core';
import {
  TaskModel,
  TaskStatus,
  formatKindLabel,
  formatShortPriority,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

@Component({
  selector: 'app-kanban-board',
  standalone: true,
  imports: [CdkDropListGroup, CdkDropList, CdkDrag],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div
      cdkDropListGroup
      class="h-full overflow-x-auto overflow-y-hidden px-5 py-4 flex gap-3.5 items-start"
    >
      @for (col of store.kanbanColumns(); track col.status) {
        <div
          class="w-72 shrink-0 max-h-full flex flex-col rounded-xl bg-zinc-100/70 dark:bg-zinc-900/55 border border-zinc-200/80 dark:border-zinc-800/80"
        >
          <!-- Column Header -->
          <div class="px-3 py-2.5 flex items-center justify-between border-b border-zinc-200/70 dark:border-zinc-800/70">
            <div class="flex items-center gap-2">
              <span class="w-2 h-2 rounded-full" [class]="col.dotClass"></span>
              <span class="text-xs font-medium text-zinc-800 dark:text-zinc-200">
                {{ col.label }}
              </span>
              <span class="text-[11px] font-mono text-zinc-400 dark:text-zinc-500">
                {{ col.tasks.length }}
              </span>
            </div>
            @if (store.activeProjectId() && col.status !== 'archived') {
              <button
                type="button"
                (click)="quickCreateInStatus.emit(col.status)"
                class="w-5 h-5 rounded hover:bg-zinc-200 dark:hover:bg-zinc-800 text-zinc-400 hover:text-zinc-800 dark:hover:text-zinc-200 text-xs flex items-center justify-center transition-colors"
                title="Create task in {{ col.label }}"
              >
                +
              </button>
            }
          </div>

          <!-- CDK Drop Zone -->
          <div
            cdkDropList
            [cdkDropListData]="col.status"
            (cdkDropListDropped)="onDrop($event, col.status)"
            class="kanban-drop-list p-2 space-y-2 overflow-y-auto flex-1 min-h-[120px]"
          >
            @for (task of col.tasks; track task.id) {
              <article
                cdkDrag
                [cdkDragData]="task"
                (click)="selectTask(task.id)"
                class="kanban-card group p-3 rounded-lg bg-white dark:bg-zinc-950 border border-zinc-200/90 dark:border-zinc-800/90 hover:border-zinc-400 dark:hover:border-zinc-600 shadow-2xs cursor-pointer transition-colors"
              >
                <div class="flex items-center justify-between gap-2">
                  <div class="flex items-center gap-1.5 min-w-0">
                    <span class="text-[11px] font-mono text-zinc-500 dark:text-zinc-400">
                      {{ task.identifier }}
                    </span>
                    <span
                      class="text-[10px] font-mono px-1 py-0.2 rounded border"
                      [class]="priorityBadgeClass(task)"
                    >
                      {{ shortPriority(task) }}
                    </span>
                    @if (task.hasMarkdownDoc) {
                      <span
                        class="text-[9px] font-mono px-1 rounded bg-sky-500/10 text-sky-600 dark:text-sky-300 border border-sky-500/30"
                        title="Task has extended .md document in ~/.flowsk/tasks"
                      >
                        .md
                      </span>
                    }
                  </div>

                  <div class="flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
                    @if (task.status !== 'archived') {
                      <button
                        type="button"
                        (click)="$event.stopPropagation(); archiveTask(task.id)"
                        class="text-[10px] px-1.5 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 bg-zinc-50 dark:bg-zinc-900"
                        title="Archive task"
                      >
                        Archive
                      </button>
                    } @else {
                      <button
                        type="button"
                        (click)="$event.stopPropagation(); restoreTask(task.id)"
                        class="text-[10px] px-1.5 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 bg-zinc-50 dark:bg-zinc-900"
                        title="Restore task to Backlog"
                      >
                        Restore
                      </button>
                    }
                  </div>
                </div>

                <h3 class="mt-1.5 text-xs font-medium text-zinc-900 dark:text-zinc-100 leading-snug line-clamp-2">
                  {{ task.title }}
                </h3>

                <div class="mt-2.5 flex items-center justify-between gap-2 text-[10px] text-zinc-400 dark:text-zinc-500">
                  <span>{{ kindLabel(task) }}</span>

                  <div class="flex items-center gap-1.5 truncate">
                    @if (task.claimedByActorId; as holderId) {
                      <span
                        class="px-1.5 py-0.5 rounded bg-emerald-500/10 text-emerald-600 dark:text-emerald-300 font-mono truncate max-w-[110px]"
                      >
                        @{{ actorHandle(holderId) }}
                      </span>
                    }
                    @if (task.objectiveId; as objId) {
                      @if (store.objectiveMapById()[objId]; as obj) {
                        <span class="truncate max-w-[100px] text-zinc-500 dark:text-zinc-400">
                          ◎ {{ obj.title }}
                        </span>
                      }
                    }
                  </div>
                </div>
              </article>
            }

            @if (col.tasks.length === 0) {
              <div
                class="py-8 text-center text-[11px] text-zinc-400 dark:text-zinc-600 border border-dashed border-zinc-200/70 dark:border-zinc-800/60 rounded-lg"
              >
                Drop tasks here
              </div>
            }
          </div>
        </div>
      }
    </div>
  `,
})
export class KanbanBoardComponent {
  readonly store = inject(FlowskStore);
  readonly quickCreateInStatus = output<TaskStatus>();

  onDrop(event: CdkDragDrop<TaskStatus, TaskStatus, TaskModel>, targetStatus: TaskStatus): void {
    const dragged = event.item.data;
    if (!dragged || dragged.status === targetStatus) {
      return;
    }
    this.store.moveTaskToStatus({
      payload: {
        taskId: dragged.id,
        targetStatus,
      },
    });
  }

  selectTask(taskId: string): void {
    this.store.loadTaskInspector({ taskId });
  }

  archiveTask(taskId: string): void {
    this.store.moveTaskToStatus({
      payload: {
        taskId,
        targetStatus: 'archived',
      },
    });
  }

  restoreTask(taskId: string): void {
    this.store.moveTaskToStatus({
      payload: {
        taskId,
        targetStatus: 'backlog',
      },
    });
  }

  shortPriority(task: TaskModel): string {
    return formatShortPriority(task.priority);
  }

  kindLabel(task: TaskModel): string {
    return formatKindLabel(task.kind);
  }

  actorHandle(actorId: string): string {
    return this.store.actorMapById()[actorId]?.handle ?? 'agent';
  }

  priorityBadgeClass(task: TaskModel): string {
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
