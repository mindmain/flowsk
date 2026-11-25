import { ChangeDetectionStrategy, Component, computed, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ProjectHierarchyNode } from '../../core/models/flowsk.models';
import { FlowskStore } from '../../core/store/flowsk.store';

/**
 * Minimalist workspace-wide projects & subprojects selector overview rendered when
 * `flowsk ui` / `flowsk web` is opened outside a bound project folder or when the
 * operator selects "All Registered Projects".
 */
@Component({
  selector: 'app-projects-overview',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="h-full overflow-y-auto px-6 py-6">
      <div class="max-w-5xl mx-auto space-y-5">
        <!-- Invitation & Workspace Summary Card -->
        <div
          class="rounded-xl bg-white dark:bg-zinc-900/70 border border-zinc-200 dark:border-zinc-800/90 p-5 flex flex-wrap items-center justify-between gap-4"
        >
          <div class="space-y-1">
            <div class="flex items-center gap-2">
              <span class="w-2 h-2 rounded-full bg-emerald-400"></span>
              <h2 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                Select a Project to Inspect
              </h2>
              <span
                class="text-[11px] font-mono px-1.5 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 text-zinc-600 dark:text-zinc-300"
              >
                {{ store.visibleProjects().length }} registered
              </span>
              @if (store.totalSubprojectsCount() > 0) {
                <span
                  class="text-[11px] font-mono px-1.5 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 text-zinc-500 dark:text-zinc-400"
                >
                  {{ store.totalSubprojectsCount() }} subproject(s)
                </span>
              }
            </div>
            <p class="text-xs text-zinc-500 dark:text-zinc-400 max-w-2xl leading-relaxed">
              @if (!store.openedFromBoundFolder()) {
                Opened from a folder that is not bound to a Flowsk project. Choose any project or nested subproject below — or use the project selector in the top bar — to open its Kanban board, backlog, goals, and Markdown docs.
              } @else {
                Browse all registered projects and nested subproject hierarchies in workspace
                <span class="font-mono text-zinc-700 dark:text-zinc-300">{{ store.workspaceSlug() }}</span>.
              }
            </p>
          </div>

          <div class="flex items-center gap-2">
            <button
              type="button"
              (click)="store.setScopeTab('all')"
              class="px-3 py-1.5 rounded-md border border-zinc-200 dark:border-zinc-800 text-xs text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800 transition-colors"
            >
              Combined Tasks ({{ store.projectActiveCount() }})
            </button>
            <button
              type="button"
              (click)="openNewProject.emit()"
              class="px-3 py-1.5 rounded-md bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-950 hover:bg-zinc-800 dark:hover:bg-zinc-200 text-xs font-medium transition-colors"
            >
              + New Project
            </button>
          </div>
        </div>

        <!-- Quick Filter Bar -->
        <div class="flex items-center justify-between gap-3">
          <input
            type="search"
            [ngModel]="filterQuery()"
            (ngModelChange)="filterQuery.set($event)"
            placeholder="Filter registered projects or subprojects by key, name, or subpath..."
            class="w-full max-w-md px-3 py-1.5 rounded-md text-xs bg-white dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800 text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500 focus:outline-none focus:border-zinc-400 dark:focus:border-zinc-600"
          />
          <div class="text-[11px] text-zinc-400 dark:text-zinc-500 hidden sm:block">
            Tip: running <code class="font-mono">flowsk ui</code> inside a bound project folder auto-selects it
          </div>
        </div>

        <!-- Hierarchical Project Cards -->
        @if (filteredRoots().length === 0) {
          <div
            class="rounded-xl border border-dashed border-zinc-300 dark:border-zinc-800 p-8 text-center text-xs text-zinc-500"
          >
            No projects match "{{ filterQuery() }}".
          </div>
        } @else {
          <div class="grid grid-cols-1 md:grid-cols-2 gap-3.5">
            @for (root of filteredRoots(); track root.project.id) {
              <div
                class="rounded-xl bg-white dark:bg-zinc-900/60 border border-zinc-200 dark:border-zinc-800/90 hover:border-zinc-300 dark:hover:border-zinc-700 transition-colors flex flex-col overflow-hidden"
              >
                <!-- Root Project Card Body -->
                <button
                  type="button"
                  (click)="store.selectProject(root.project.id)"
                  class="w-full text-left p-4 hover:bg-zinc-50/80 dark:hover:bg-zinc-900 transition-colors space-y-2.5"
                >
                  <div class="flex items-start justify-between gap-2">
                    <div class="flex items-center gap-2 min-w-0">
                      <span
                        class="text-[11px] font-mono font-medium px-1.5 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 border border-zinc-200 dark:border-zinc-700 text-zinc-800 dark:text-zinc-200 shrink-0"
                      >
                        {{ root.project.keyPrefix }}
                      </span>
                      <span class="text-sm font-semibold text-zinc-900 dark:text-zinc-100 truncate">
                        {{ root.project.name }}
                      </span>
                      @if (root.project.hasMarkdownDoc) {
                        <span
                          class="text-[9px] font-mono px-1 rounded border border-zinc-300 dark:border-zinc-700 text-zinc-500 dark:text-zinc-400 shrink-0"
                          title="Has Project .md charter"
                        >
                          MD
                        </span>
                      }
                    </div>
                    <span class="text-[11px] text-zinc-400 dark:text-zinc-500 shrink-0">Open →</span>
                  </div>

                  @if (root.project.description) {
                    <p class="text-xs text-zinc-500 dark:text-zinc-400 line-clamp-2">
                      {{ root.project.description }}
                    </p>
                  }

                  <!-- Status Pill Breakdown -->
                  <div class="flex flex-wrap items-center gap-1.5 pt-0.5 text-[11px] tabular-nums">
                    <span
                      class="px-1.5 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800/80 text-zinc-700 dark:text-zinc-300"
                    >
                      {{ root.counts.active }} active
                    </span>
                    @if (root.counts.inProgress > 0) {
                      <span
                        class="px-1.5 py-0.5 rounded bg-amber-500/10 text-amber-700 dark:text-amber-300"
                      >
                        {{ root.counts.inProgress }} in progress
                      </span>
                    }
                    @if (root.counts.ready > 0) {
                      <span class="px-1.5 py-0.5 rounded bg-sky-500/10 text-sky-700 dark:text-sky-300">
                        {{ root.counts.ready }} todo
                      </span>
                    }
                    @if (root.counts.backlog > 0) {
                      <span class="px-1.5 py-0.5 rounded bg-zinc-500/10 text-zinc-600 dark:text-zinc-400">
                        {{ root.counts.backlog }} backlog
                      </span>
                    }
                    @if (root.counts.blocked > 0) {
                      <span class="px-1.5 py-0.5 rounded bg-rose-500/10 text-rose-600 dark:text-rose-300">
                        {{ root.counts.blocked }} blocked
                      </span>
                    }
                    @if (root.counts.done > 0) {
                      <span
                        class="px-1.5 py-0.5 rounded bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
                      >
                        {{ root.counts.done }} done
                      </span>
                    }
                  </div>
                </button>

                <!-- Nested Subprojects Hierarchy -->
                @if (flattenDescendants(root).length > 0) {
                  <div
                    class="border-t border-zinc-200/80 dark:border-zinc-800/80 bg-zinc-50/70 dark:bg-zinc-950/60 px-3 py-2 space-y-1"
                  >
                    <div
                      class="px-1 text-[10px] font-medium uppercase tracking-wider text-zinc-400 dark:text-zinc-500"
                    >
                      Subprojects ({{ flattenDescendants(root).length }})
                    </div>
                    @for (sub of flattenDescendants(root); track sub.project.id + '-' + sub.depth) {
                      <button
                        type="button"
                        (click)="store.selectProject(sub.project.id)"
                        class="w-full px-2 py-1.5 rounded-md hover:bg-zinc-200/60 dark:hover:bg-zinc-800/70 transition-colors flex items-center justify-between gap-2 text-left text-xs"
                        [style.padding-left.px]="8 + (sub.depth - 1) * 14"
                      >
                        <div class="flex items-center gap-1.5 min-w-0">
                          <span class="font-mono text-zinc-400 dark:text-zinc-500 shrink-0">└─</span>
                          <span
                            class="text-[10px] font-mono px-1 py-0.5 rounded bg-zinc-200/80 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-300 shrink-0"
                          >
                            {{ sub.project.keyPrefix }}
                          </span>
                          <span class="text-zinc-800 dark:text-zinc-200 font-medium truncate">
                            {{ sub.project.name }}
                          </span>
                          @if (sub.scopeSubpath) {
                            <span
                              class="text-[10px] font-mono text-zinc-400 dark:text-zinc-500 truncate"
                              title="Scoped subpath within parent repository"
                            >
                              · {{ sub.scopeSubpath }}
                            </span>
                          }
                        </div>
                        <div class="flex items-center gap-1.5 shrink-0 text-[11px] tabular-nums">
                          @if (sub.counts.inProgress > 0) {
                            <span class="text-amber-600 dark:text-amber-400">
                              {{ sub.counts.inProgress }} active
                            </span>
                          } @else {
                            <span class="text-zinc-400 dark:text-zinc-500">
                              {{ sub.counts.active }} tasks
                            </span>
                          }
                          <span class="text-zinc-400">→</span>
                        </div>
                      </button>
                    }
                  </div>
                }
              </div>
            }
          </div>
        }
      </div>
    </div>
  `,
})
export class ProjectsOverviewComponent {
  readonly store = inject(FlowskStore);
  readonly openNewProject = output<void>();
  readonly filterQuery = signal('');

  readonly filteredRoots = computed(() => {
    const q = this.filterQuery().trim().toLowerCase();
    const roots = this.store.projectTree();
    if (!q) {
      return roots;
    }
    return roots
      .map((node) => filterHierarchyNode(node, q))
      .filter((node): node is ProjectHierarchyNode => node !== null);
  });

  flattenDescendants(root: ProjectHierarchyNode): ProjectHierarchyNode[] {
    const out: ProjectHierarchyNode[] = [];
    const visit = (nodes: readonly ProjectHierarchyNode[]): void => {
      for (const n of nodes) {
        out.push(n);
        if (n.children.length > 0) {
          visit(n.children);
        }
      }
    };
    visit(root.children);
    return out;
  }
}

function filterHierarchyNode(node: ProjectHierarchyNode, q: string): ProjectHierarchyNode | null {
  const matchedChildren = node.children
    .map((child) => filterHierarchyNode(child, q))
    .filter((child): child is ProjectHierarchyNode => child !== null);

  const selfHaystack = `${node.project.keyPrefix} ${node.project.slug} ${node.project.name} ${node.project.description} ${node.scopeSubpath}`.toLowerCase();
  if (selfHaystack.includes(q)) {
    return node;
  }
  if (matchedChildren.length > 0) {
    return {
      ...node,
      children: matchedChildren,
    };
  }
  return null;
}
