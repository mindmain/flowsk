import { ChangeDetectionStrategy, Component, inject, signal, viewChild } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { GoalsViewComponent } from './components/goals-view/goals-view.component';
import { KanbanBoardComponent } from './components/kanban-board/kanban-board.component';
import { ActiveDialogType, DialogsComponent } from './components/modals/dialogs.component';
import { ObjectivesStripComponent } from './components/objectives-strip/objectives-strip.component';
import { ProjectsOverviewComponent } from './components/projects-overview/projects-overview.component';
import { SidebarComponent } from './components/sidebar/sidebar.component';
import { TaskDrawerComponent } from './components/task-drawer/task-drawer.component';
import { TaskGridComponent } from './components/task-grid/task-grid.component';
import { TopbarComponent } from './components/topbar/topbar.component';
import { ObjectiveModel, TaskStatus } from './core/models/flowsk.models';
import { FlowskStore } from './core/store/flowsk.store';

/**
 * Root standalone application shell coordinating sidebar navigation, live topbar filters,
 * token session authentication, Kanban/Grid/Goals views, task drawer, and modals.
 */
@Component({
  selector: 'app-root',
  standalone: true,
  imports: [
    FormsModule,
    SidebarComponent,
    TopbarComponent,
    ObjectivesStripComponent,
    ProjectsOverviewComponent,
    GoalsViewComponent,
    KanbanBoardComponent,
    TaskGridComponent,
    TaskDrawerComponent,
    DialogsComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: {
    '(document:keydown.escape)': 'onEscapeKey($event)',
  },
  template: `
    <div class="h-screen w-screen overflow-hidden flex bg-white dark:bg-zinc-950 text-zinc-900 dark:text-zinc-100">
      <app-sidebar
        (openNewProject)="onOpenNewProject()"
        (openPruneModal)="openPruneDialog()"
      />

      <div class="flex-1 min-w-0 flex flex-col h-full">
        <app-topbar
          (openNewTask)="openCreateTask('ready')"
          (openProjectCharter)="openProjectCharter()"
          (openPurgeArchive)="onOpenPurgeArchive()"
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

        @if (store.activeProjectId() !== null && !store.authRequired()) {
          <app-objectives-strip (openNewObjective)="openNewObjectiveModal()" />
        }

        <main class="flex-1 min-h-0 overflow-hidden bg-zinc-50/40 dark:bg-zinc-950">
          @if (store.authRequired()) {
            <div class="h-full flex items-center justify-center p-6">
              <form
                (ngSubmit)="submitAuthToken()"
                class="w-full max-w-sm rounded-xl bg-white dark:bg-zinc-900/80 border border-zinc-200 dark:border-zinc-800 shadow-xl p-6 space-y-4"
              >
                <div class="space-y-1">
                  <div class="flex items-center gap-2">
                    <span class="w-2 h-2 rounded-full bg-amber-400"></span>
                    <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                      Workspace Authentication Required
                    </h2>
                  </div>
                  <p class="text-xs text-zinc-500 dark:text-zinc-400 leading-relaxed">
                    This Flowsk server requires <code class="font-mono text-[11px]">FLOWSK_AUTH_TOKEN</code>. Enter your token to establish a secure local session cookie.
                  </p>
                </div>

                @if (store.authError(); as authErr) {
                  <div
                    class="px-3 py-2 rounded-lg bg-rose-500/10 border border-rose-500/30 text-rose-600 dark:text-rose-300 text-xs"
                  >
                    {{ authErr }}
                  </div>
                }

                <div>
                  <label for="flowsk-auth-token" class="block text-[11px] font-medium text-zinc-500 dark:text-zinc-400 mb-1">
                    Access Token
                  </label>
                  <input
                    id="flowsk-auth-token"
                    name="flowskAuthToken"
                    type="password"
                    autocomplete="current-password"
                    [ngModel]="authToken()"
                    (ngModelChange)="authToken.set($event)"
                    placeholder="Enter FLOWSK_AUTH_TOKEN..."
                    class="w-full px-3 py-2 rounded-md text-xs font-mono bg-zinc-50 dark:bg-zinc-950 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 focus:outline-none focus:border-zinc-400 dark:focus:border-zinc-600"
                  />
                </div>

                <div class="flex justify-end">
                  <button
                    type="submit"
                    [disabled]="!authToken().trim() || store.saving()"
                    class="w-full px-4 py-2 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium hover:bg-zinc-800 dark:hover:bg-zinc-200 disabled:opacity-50 transition-colors"
                  >
                    {{ store.saving() ? 'Verifying...' : 'Unlock Workspace' }}
                  </button>
                </div>
              </form>
            </div>
          } @else if (!store.initialized() && store.loading()) {
            <div class="h-full flex items-center justify-center text-xs text-zinc-400">
              Loading Flowsk workspace...
            </div>
          } @else if (store.projects().length === 0 && Boolean(store.workspaceId()) && !store.authRequired()) {
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
          } @else if (!store.workspaceId()) {
            <div class="h-full flex flex-col items-center justify-center gap-3 p-6 text-center">
              <div class="text-sm font-medium text-zinc-800 dark:text-zinc-200">Workspace connection unavailable</div>
              <p class="text-xs text-zinc-500 max-w-sm">
                Verify that your Flowsk server is running and reachable, then retry loading the workspace.
              </p>
              <button
                type="button"
                (click)="store.bootstrap()"
                [disabled]="store.loading()"
                class="px-4 py-2 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 text-xs font-medium disabled:opacity-50"
              >
                Retry Connection
              </button>
            </div>
          } @else if (store.scopeTab() === 'goals') {
            <app-goals-view
              (openNewObjective)="openNewObjectiveModal()"
              (openEditObjective)="openEditObjectiveModal($event)"
              (openNewTaskForObjective)="openCreateTaskForObjective($event)"
            />
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
        [presetObjectiveId]="presetObjectiveId()"
        [presetProjectId]="presetProjectId()"
        [editingObjective]="editingObjective()"
        (close)="closeDialog()"
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
  readonly topbarRef = viewChild(TopbarComponent);

  readonly activeDialog = signal<ActiveDialogType>(null);
  readonly newTaskStatus = signal<TaskStatus>('ready');
  readonly presetObjectiveId = signal<string>('');
  readonly presetProjectId = signal<string>('');
  readonly editingObjective = signal<ObjectiveModel | null>(null);
  readonly authToken = signal<string>('');

  protected readonly Boolean = Boolean;

  submitAuthToken(): void {
    const token = this.authToken().trim();
    if (!token) {
      return;
    }
    this.store.authenticateSession({
      payload: { token },
      onSuccess: () => {
        this.authToken.set('');
      },
    });
  }

  onOpenNewProject(): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.activeDialog.set('new-project');
  }

  onOpenPurgeArchive(): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.activeDialog.set('purge-archive');
  }

  openCreateTask(status: TaskStatus): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.newTaskStatus.set(status);
    const activeGoalFilter = this.store.objectiveFilterId();
    this.presetObjectiveId.set(activeGoalFilter !== 'all' ? activeGoalFilter : '');
    this.presetProjectId.set(this.store.activeProjectId() ?? '');
    this.activeDialog.set('new-task');
  }

  openCreateTaskForObjective(obj: ObjectiveModel): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.newTaskStatus.set('ready');
    this.presetObjectiveId.set(obj.id);
    this.presetProjectId.set(obj.projectId);
    this.activeDialog.set('new-task');
  }

  openNewObjectiveModal(): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.editingObjective.set(null);
    this.activeDialog.set('new-objective');
  }

  openEditObjectiveModal(obj: ObjectiveModel): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.editingObjective.set(obj);
    this.activeDialog.set('edit-objective');
  }

  openProjectCharter(): void {
    const pid = this.store.activeProjectId();
    if (!pid || this.store.authRequired()) return;
    this.store.loadProjectMarkdown({ projectId: pid });
    this.activeDialog.set('project-charter');
  }

  openPruneDialog(): void {
    if (this.store.authRequired() || !this.store.workspaceId()) return;
    this.store.inspectOrphans();
    this.activeDialog.set('prune-orphans');
  }

  closeDialog(): void {
    this.activeDialog.set(null);
    this.editingObjective.set(null);
  }

  onEscapeKey(event: Event): void {
    if (this.activeDialog() !== null) {
      event.preventDefault();
      this.closeDialog();
      return;
    }
    if (this.topbarRef()?.closeActivityPopover()) {
      event.preventDefault();
      return;
    }
    if (this.store.selectedTaskId() !== null) {
      event.preventDefault();
      this.store.closeTaskInspector();
    }
  }
}
