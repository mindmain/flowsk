import { ChangeDetectionStrategy, Component, inject, output } from '@angular/core';
import { ScopeTab } from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

@Component({
  selector: 'app-sidebar',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <aside
      class="w-64 shrink-0 border-r border-zinc-200 dark:border-zinc-800/90 bg-zinc-50/80 dark:bg-zinc-950 flex flex-col h-full select-none"
    >
      <!-- Brand & Workspace Header -->
      <div class="h-13 px-4 border-b border-zinc-200 dark:border-zinc-800/80 flex items-center justify-between">
        <div class="flex items-center gap-2.5 min-w-0">
          <div
            class="w-6 h-6 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 flex items-center justify-center text-xs font-semibold tracking-tight"
          >
            F
          </div>
          <div class="truncate">
            <div class="text-xs font-semibold text-zinc-900 dark:text-zinc-100 leading-tight truncate">
              Flowsk
            </div>
            <div class="text-[11px] text-zinc-500 dark:text-zinc-400 truncate">
              {{ store.workspaceSlug() }} · @{{ store.actorHandle() }}
            </div>
          </div>
        </div>
        <button
          type="button"
          (click)="store.toggleDarkMode()"
          class="p-1.5 rounded-md text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100 hover:bg-zinc-200/60 dark:hover:bg-zinc-800/70 transition-colors text-xs"
          title="Toggle light/dark theme"
        >
          @if (store.darkMode()) {
            <span>☀</span>
          } @else {
            <span>☾</span>
          }
        </button>
      </div>

      <!-- Primary Views -->
      <div class="p-2.5 space-y-0.5">
        <div class="px-2 pb-1 text-[10px] font-medium uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
          Views
        </div>

        <button
          type="button"
          (click)="selectTab('board')"
          class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
          [class]="tabClass('board')"
        >
          <span class="flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-emerald-400"></span>
            <span>Active Board</span>
          </span>
          <span class="text-[11px] tabular-nums opacity-70">{{ store.projectActiveCount() }}</span>
        </button>

        <button
          type="button"
          (click)="selectTab('goals')"
          class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
          [class]="tabClass('goals')"
        >
          <span class="flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-violet-400"></span>
            <span>Goals & Roadmap</span>
          </span>
          <span class="text-[11px] tabular-nums opacity-70">{{ store.activeObjectivesCount() }}</span>
        </button>

        <button
          type="button"
          (click)="selectTab('backlog')"
          class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
          [class]="tabClass('backlog')"
        >
          <span class="flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-zinc-400"></span>
            <span>Backlog & Triage</span>
          </span>
          <span class="text-[11px] tabular-nums opacity-70">{{ store.projectBacklogCount() }}</span>
        </button>

        <button
          type="button"
          (click)="selectTab('all')"
          class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
          [class]="tabClass('all')"
        >
          <span class="flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-sky-400"></span>
            <span>All States</span>
          </span>
        </button>

        <button
          type="button"
          (click)="selectTab('archived')"
          class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
          [class]="tabClass('archived')"
        >
          <span class="flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-slate-500"></span>
            <span>Archive</span>
          </span>
          <span class="text-[11px] tabular-nums opacity-70">{{ store.workspaceArchivedCount() }}</span>
        </button>
      </div>

      <!-- Projects Section -->
      <div class="px-2.5 pt-3 pb-2 flex-1 overflow-y-auto min-h-0">
        <div class="px-2 pb-1.5 flex items-center justify-between">
          <span class="text-[10px] font-medium uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
            Projects
          </span>
          <button
            type="button"
            (click)="openNewProject.emit()"
            class="text-[11px] px-1.5 py-0.5 rounded text-zinc-600 dark:text-zinc-300 hover:bg-zinc-200/70 dark:hover:bg-zinc-800 transition-colors"
          >
            + New
          </button>
        </div>

        <div class="space-y-0.5">
          <button
            type="button"
            (click)="store.selectProject(null)"
            class="w-full px-2.5 py-1.5 rounded-md text-xs flex items-center justify-between transition-colors"
            [class]="
              store.activeProjectId() === null
                ? 'bg-zinc-200/80 dark:bg-zinc-800/90 text-zinc-900 dark:text-zinc-100 font-medium'
                : 'text-zinc-600 dark:text-zinc-400 hover:bg-zinc-200/40 dark:hover:bg-zinc-900'
            "
          >
            <span class="truncate">All Registered Projects</span>
            <span class="text-[10px] font-mono opacity-70 tabular-nums">
              {{ store.visibleProjects().length }}
            </span>
          </button>

          @for (node of store.flatProjectHierarchy(); track node.project.id + '-' + node.depth) {
            <div class="group flex items-center gap-1">
              <button
                type="button"
                (click)="store.selectProject(node.project.id)"
                class="flex-1 min-w-0 py-1.5 pr-2.5 rounded-md text-xs flex items-center justify-between transition-colors"
                [style.padding-left.px]="10 + node.depth * 14"
                [class]="
                  store.activeProjectId() === node.project.id
                    ? 'bg-zinc-200/80 dark:bg-zinc-800/90 text-zinc-900 dark:text-zinc-100 font-medium'
                    : 'text-zinc-600 dark:text-zinc-400 hover:bg-zinc-200/40 dark:hover:bg-zinc-900'
                "
                [title]="
                  node.scopeSubpath
                    ? node.project.name + ' · subpath: ' + node.scopeSubpath
                    : node.project.name
                "
              >
                <span class="flex items-center gap-1.5 min-w-0">
                  @if (node.depth > 0) {
                    <span class="text-[10px] font-mono text-zinc-400 dark:text-zinc-500 shrink-0">└─</span>
                  }
                  <span
                    class="text-[10px] font-mono px-1 py-0.5 rounded bg-zinc-200/70 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-300 shrink-0"
                  >
                    {{ node.project.keyPrefix }}
                  </span>
                  <span class="truncate">{{ node.project.name }}</span>
                </span>
                <span class="flex items-center gap-1 shrink-0 ml-1">
                  @if (node.project.hasMarkdownDoc) {
                    <span
                      class="text-[9px] font-mono px-1 rounded border border-zinc-300 dark:border-zinc-700 text-zinc-500 dark:text-zinc-400"
                      title="Project has Markdown charter in ~/.flowsk/projects"
                    >
                      MD
                    </span>
                  }
                  <span class="text-[10px] tabular-nums opacity-65">{{ node.counts.active }}</span>
                </span>
              </button>
            </div>
          }
        </div>

        <div class="mt-3 px-2">
          <button
            type="button"
            (click)="store.toggleArchivedProjects()"
            class="text-[11px] text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-300 transition-colors"
          >
            {{ store.showArchivedProjects() ? 'Hide archived projects' : 'Show archived projects' }}
          </button>
        </div>
      </div>

      <!-- Footer: Memory & Prune Health -->
      <div class="p-3 border-t border-zinc-200 dark:border-zinc-800/80 space-y-2">
        @if (store.orphanReport().totalIssues > 0) {
          <button
            type="button"
            (click)="openPruneModal.emit()"
            class="w-full px-2.5 py-2 rounded-md bg-amber-500/15 border border-amber-500/40 text-amber-700 dark:text-amber-300 text-left text-[11px] hover:bg-amber-500/20 transition-colors"
          >
            <div class="font-medium flex items-center justify-between">
              <span>⚠ Prune needed</span>
              <span class="font-mono">{{ store.orphanReport().totalIssues }}</span>
            </div>
            <div class="opacity-80 mt-0.5">Orphan or missing .md refs detected</div>
          </button>
        } @else {
          <button
            type="button"
            (click)="openPruneModal.emit()"
            class="w-full px-2.5 py-1.5 rounded-md border border-zinc-200 dark:border-zinc-800 text-[11px] text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-200 hover:border-zinc-300 dark:hover:border-zinc-700 flex items-center justify-between transition-colors"
          >
            <span>Memory ~/.flowsk clean</span>
            <span class="font-mono text-[10px] text-emerald-500">OK</span>
          </button>
        }
      </div>
    </aside>
  `,
})
export class SidebarComponent {
  readonly store = inject(FlowskStore);
  readonly openNewProject = output<void>();
  readonly openPruneModal = output<void>();

  selectTab(tab: ScopeTab): void {
    this.store.setScopeTab(tab);
  }

  tabClass(tab: ScopeTab): string {
    return this.store.scopeTab() === tab
      ? 'bg-zinc-200/80 dark:bg-zinc-800/90 text-zinc-900 dark:text-zinc-100 font-medium'
      : 'text-zinc-600 dark:text-zinc-400 hover:bg-zinc-200/40 dark:hover:bg-zinc-900';
  }
}
