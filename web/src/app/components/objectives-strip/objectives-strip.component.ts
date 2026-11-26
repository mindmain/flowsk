import { ChangeDetectionStrategy, Component, inject, output, signal } from '@angular/core';
import { FlowskStore } from '../../core/store/flowsk.store';

@Component({
  selector: 'app-objectives-strip',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (store.activeProjectId() && store.scopeTab() === 'board') {
      <section class="px-5 py-2 border-b border-zinc-200 dark:border-zinc-800/80 bg-zinc-50/50 dark:bg-zinc-950/50">
        <div class="flex items-center justify-between gap-2">
          <div class="flex items-center gap-3">
            <button
              type="button"
              (click)="expanded.set(!expanded())"
              class="text-[11px] font-medium text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-200 flex items-center gap-1.5"
            >
              <span>{{ expanded() ? '▾' : '▸' }}</span>
              <span>Goals & Milestones ({{ store.objectivesWithProgress().length }})</span>
            </button>

            @if (store.objectiveFilterId() !== 'all') {
              <button
                type="button"
                (click)="store.setObjectiveFilter('all')"
                class="text-[10px] px-1.5 py-0.5 rounded bg-zinc-200/80 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-300"
              >
                Clear goal filter ✕
              </button>
            }
          </div>

          <div class="flex items-center gap-3">
            <button
              type="button"
              (click)="store.setScopeTab('goals')"
              class="text-[11px] text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors"
            >
              Structured Roadmap →
            </button>
            <button
              type="button"
              (click)="openNewObjective.emit()"
              class="text-[11px] font-medium text-zinc-600 dark:text-zinc-300 hover:text-zinc-900 dark:hover:text-zinc-100 transition-colors"
            >
              + Add Goal
            </button>
          </div>
        </div>

        @if (expanded() && store.objectivesWithProgress().length > 0) {
          <div class="mt-2 flex gap-2.5 overflow-x-auto pb-1">
            @for (obj of store.objectivesWithProgress(); track obj.id) {
              <div
                class="min-w-[225px] max-w-[270px] p-2.5 rounded-lg border transition-colors cursor-pointer"
                [class]="
                  store.objectiveFilterId() === obj.id
                    ? 'border-zinc-900 dark:border-zinc-300 bg-white dark:bg-zinc-900'
                    : 'border-zinc-200 dark:border-zinc-800/90 bg-white/70 dark:bg-zinc-900/50 hover:border-zinc-300 dark:hover:border-zinc-700'
                "
                (click)="toggleFilter(obj.id)"
              >
                <div class="flex items-center justify-between gap-2 text-[10px] uppercase tracking-wider text-zinc-400">
                  <span class="flex items-center gap-1.5">
                    <span>{{ obj.kind }}</span>
                    <span>·</span>
                    <span
                      [class]="
                        obj.status === 'at_risk'
                          ? 'text-amber-500 font-semibold'
                          : obj.status === 'achieved'
                            ? 'text-emerald-500'
                            : ''
                      "
                    >
                      {{ obj.status.replace('_', ' ') }}
                    </span>
                  </span>
                  @if (obj.status !== 'achieved' && obj.status !== 'dropped') {
                    <button
                      type="button"
                      (click)="$event.stopPropagation(); markDone(obj.id)"
                      class="text-[10px] text-zinc-400 hover:text-emerald-500"
                      title="Mark objective achieved"
                    >
                      ✓ Close
                    </button>
                  }
                </div>
                <div class="mt-1 text-xs font-medium text-zinc-900 dark:text-zinc-100 truncate">
                  {{ obj.title }}
                </div>
                <div class="mt-2 flex items-center gap-2">
                  <div class="flex-1 h-1.5 rounded-full bg-zinc-200 dark:bg-zinc-800 overflow-hidden flex">
                    @if (obj.progressPct > 0) {
                      <div class="h-full bg-emerald-500 transition-all" [style.width.%]="obj.progressPct"></div>
                    }
                    @if (obj.activeWorkPct > 0) {
                      <div class="h-full bg-amber-400 transition-all" [style.width.%]="obj.activeWorkPct"></div>
                    }
                    @if (obj.blockedPct > 0) {
                      <div class="h-full bg-rose-500 transition-all" [style.width.%]="obj.blockedPct"></div>
                    }
                  </div>
                  <span class="text-[10px] font-mono text-zinc-500 dark:text-zinc-400">
                    {{ obj.doneCount }}/{{ obj.totalCount }}
                  </span>
                </div>
              </div>
            }
          </div>
        }
      </section>
    }
  `,
})
export class ObjectivesStripComponent {
  readonly store = inject(FlowskStore);
  readonly openNewObjective = output<void>();
  readonly expanded = signal<boolean>(true);

  toggleFilter(objectiveId: string): void {
    const next = this.store.objectiveFilterId() === objectiveId ? 'all' : objectiveId;
    this.store.setObjectiveFilter(next);
  }

  markDone(objectiveId: string): void {
    this.store.closeObjective({ payload: { objectiveId, achieved: true } });
  }
}
