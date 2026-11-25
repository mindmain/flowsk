import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  CANONICAL_STATUS_COLUMNS,
  TaskModel,
  TaskStatus,
  formatKindLabel,
  formatShortPriority,
} from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

@Component({
  selector: 'app-task-grid',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="h-full overflow-auto px-5 py-4">
      <div
        class="rounded-xl border border-zinc-200 dark:border-zinc-800/90 bg-white dark:bg-zinc-950 overflow-hidden"
      >
        <table class="w-full text-left border-collapse">
          <thead>
            <tr
              class="border-b border-zinc-200 dark:border-zinc-800 bg-zinc-50/90 dark:bg-zinc-900/60 text-[11px] font-medium text-zinc-500 dark:text-zinc-400"
            >
              <th class="py-2.5 px-3 w-24">Key</th>
              <th class="py-2.5 px-3 w-16">Prio</th>
              <th class="py-2.5 px-3">Title</th>
              <th class="py-2.5 px-3 w-36">Status</th>
              <th class="py-2.5 px-3 w-28">Kind</th>
              <th class="py-2.5 px-3 w-36">Objective</th>
              <th class="py-2.5 px-3 w-28">Lease</th>
              <th class="py-2.5 px-3 w-24 text-right">Action</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-200/70 dark:divide-zinc-800/70 text-xs">
            @for (task of store.filteredTasks(); track task.id) {
              <tr
                (click)="store.loadTaskInspector({ taskId: task.id })"
                class="hover:bg-zinc-50 dark:hover:bg-zinc-900/50 cursor-pointer transition-colors"
              >
                <td class="py-2.5 px-3 font-mono text-[11px] text-zinc-500 dark:text-zinc-400">
                  {{ task.identifier }}
                </td>
                <td class="py-2.5 px-3 font-mono text-[11px] text-zinc-700 dark:text-zinc-300">
                  {{ shortPrio(task) }}
                </td>
                <td class="py-2.5 px-3 font-medium text-zinc-900 dark:text-zinc-100">
                  <div class="flex items-center gap-2">
                    <span class="truncate max-w-md">{{ task.title }}</span>
                    @if (task.hasMarkdownDoc) {
                      <span
                        class="text-[10px] font-mono px-1.5 py-0.2 rounded bg-sky-500/10 text-sky-600 dark:text-sky-300 border border-sky-500/30"
                      >
                        .md
                      </span>
                    }
                  </div>
                </td>
                <td class="py-2.5 px-3" (click)="$event.stopPropagation()">
                  <select
                    [ngModel]="task.status"
                    (ngModelChange)="changeStatus(task.id, $event)"
                    class="px-2 py-1 rounded text-[11px] bg-zinc-100 dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-800 dark:text-zinc-200 focus:outline-none"
                  >
                    @for (col of statusColumns; track col.status) {
                      <option [value]="col.status">{{ col.label }}</option>
                    }
                  </select>
                </td>
                <td class="py-2.5 px-3 text-zinc-500 dark:text-zinc-400">
                  {{ kindLabel(task) }}
                </td>
                <td class="py-2.5 px-3 text-zinc-500 dark:text-zinc-400 truncate max-w-[140px]">
                  @if (task.objectiveId; as objId) {
                    {{ store.objectiveMapById()[objId]?.title ?? '—' }}
                  } @else {
                    <span>—</span>
                  }
                </td>
                <td class="py-2.5 px-3 font-mono text-[11px]">
                  @if (task.claimedByActorId; as holderId) {
                    <span class="text-emerald-600 dark:text-emerald-400">
                      @{{ store.actorMapById()[holderId]?.handle ?? 'agent' }}
                    </span>
                  } @else {
                    <span class="text-zinc-400">—</span>
                  }
                </td>
                <td class="py-2.5 px-3 text-right" (click)="$event.stopPropagation()">
                  @if (task.status !== 'archived') {
                    <button
                      type="button"
                      (click)="changeStatus(task.id, 'archived')"
                      class="text-[11px] px-2 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
                    >
                      Archive
                    </button>
                  } @else {
                    <button
                      type="button"
                      (click)="changeStatus(task.id, 'backlog')"
                      class="text-[11px] px-2 py-0.5 rounded border border-zinc-200 dark:border-zinc-800 text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
                    >
                      Restore
                    </button>
                  }
                </td>
              </tr>
            }

            @if (store.filteredTasks().length === 0) {
              <tr>
                <td colspan="8" class="py-12 text-center text-xs text-zinc-400 dark:text-zinc-500">
                  No tasks match the current view and filters.
                </td>
              </tr>
            }
          </tbody>
        </table>
      </div>
    </div>
  `,
})
export class TaskGridComponent {
  readonly store = inject(FlowskStore);
  readonly statusColumns = CANONICAL_STATUS_COLUMNS;

  changeStatus(taskId: string, targetStatus: TaskStatus): void {
    this.store.moveTaskToStatus({
      payload: { taskId, targetStatus },
    });
  }

  shortPrio(task: TaskModel): string {
    return formatShortPriority(task.priority);
  }

  kindLabel(task: TaskModel): string {
    return formatKindLabel(task.kind);
  }
}
