import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { KanbanBoardComponent } from './components/kanban-board/kanban-board.component';
import { ActiveDialogType, DialogsComponent } from './components/modals/dialogs.component';
import { ObjectivesStripComponent } from './components/objectives-strip/objectives-strip.component';
import { ProjectsOverviewComponent } from './components/projects-overview/projects-overview.component';
import { SidebarComponent } from './components/sidebar/sidebar.component';
import { TaskDrawerComponent } from './components/task-drawer/task-drawer.component';
import { TaskGridComponent } from './components/task-grid/task-grid.component';
import { TopbarComponent } from './components/topbar/topbar.component';
import { TaskStatus } from './core/models/flowsk.models';
import { FlowskStore } from './core/store/flowsk.store';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [
    SidebarComponent,
    TopbarComponent,
    ObjectivesStripComponent,
    ProjectsOverviewComponent,
    KanbanBoardComponent,
    TaskGridComponent,
    TaskDrawerComponent,
    DialogsComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="h-screen w-screen overflow-hidden flex bg-white dark:bg-zinc-950 text-zinc-900 dark:text-zinc-100">
      <app-sidebar
        (openNewProject)="activeDialog.set('new-project')"
        (openPruneModal)="openPruneDialog()"
      />

      <div class="flex-1 min-w-0 flex flex-col h-full">
        <app-topbar
          (openNewTask)="openCreateTask('ready')"
          (openProjectCharter)="openProjectCharter()"
          (openPurgeArchive)="activeDialog.set('purge-archive')"
          (openPruneModal)="openPruneDialog()"
        />

        @if (store.errorBanner(); as err) {
          <div
            class="px-5 py-2 bg-rose-500/10 border-b border-rose-500/30 text-rose-600 dark:text-rose-300 text-xs flex items-center justify-between"
          >
            <span>{{ err }}</span>
            <button type="button" (click)="store.dismissError()" class="text-xs underline">Dismiss</button>
          </div>
        }

        @if (store.activeProjectId() !== null) {
          <app-objectives-strip (openNewObjective)="activeDialog.set('new-objective')" />
        }

        <main class="flex-1 min-h-0 overflow-hidden bg-zinc-50/40 dark:bg-zinc-950">
          @if (!store.initialized() && store.loading()) {
            <div class="h-full flex items-center justify-center text-xs text-zinc-400">
              Loading Flowsk workspace...
            </div>
          } @else if (store.projects().length === 0) {
            <div class="h-full flex flex-col items-center justify-center gap-3 p-6 text-center">
              <div class="text-sm font-medium text-zinc-800 dark:text-zinc-200">No projects in this workspace yet</div>
              <p class="text-xs text-zinc-500 max-w-sm">
                Create your first logical project to manage Kanban pipelines, backlog tasks, strategic goals, and Markdown specs.
              </p>
              <button
                type="button"
                (click)="activeDialog.set('new-project')"
                class="px-4 py-2 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium"
              >
                + Create First Project
              </button>
            </div>
          } @else if (store.activeProjectId() === null && store.scopeTab() === 'board') {
            <app-projects-overview (openNewProject)="activeDialog.set('new-project')" />
          } @else if (store.viewMode() === 'kanban') {
            <app-kanban-board (quickCreateInStatus)="openCreateTask($event)" />
          } @else {
            <app-task-grid />
          }
        </main>
      </div>

      <app-task-drawer />

      <app-dialogs
        [dialog]="activeDialog()"
        [defaultTaskStatus]="newTaskStatus()"
        (close)="activeDialog.set(null)"
      />

      @if (store.toastMessage(); as toast) {
        <div
          (click)="store.dismissToast()"
          class="fixed bottom-4 right-4 z-50 px-3.5 py-2 rounded-lg bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium shadow-lg cursor-pointer"
        >
          {{ toast }}
        </div>
      }
    </div>
  `,
})
export class App {
  readonly store = inject(FlowskStore);
  readonly activeDialog = signal<ActiveDialogType>(null);
  readonly newTaskStatus = signal<TaskStatus>('ready');

  openCreateTask(status: TaskStatus): void {
    this.newTaskStatus.set(status);
    this.activeDialog.set('new-task');
  }

  openProjectCharter(): void {
    const pid = this.store.activeProjectId();
    if (!pid) return;
    this.store.loadProjectMarkdown({ projectId: pid });
    this.activeDialog.set('project-charter');
  }

  openPruneDialog(): void {
    this.store.inspectOrphans();
    this.activeDialog.set('prune-orphans');
  }
}
