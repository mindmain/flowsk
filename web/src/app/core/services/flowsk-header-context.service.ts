import { Injectable, signal } from '@angular/core';

/**
 * Holds reactive workspace and actor header values read by flowskApiInterceptor
 * without creating circular DI dependencies with FlowskStore.
 */
@Injectable({ providedIn: 'root' })
export class FlowskHeaderContextService {
  readonly workspaceRef = signal<string>('default');
  readonly actorHandle = signal<string>('web-operator');

  setIdentity(workspaceRef: string, actorHandle: string): void {
    if (workspaceRef.trim()) {
      this.workspaceRef.set(workspaceRef.trim());
    }
    if (actorHandle.trim()) {
      this.actorHandle.set(actorHandle.trim());
    }
  }
}
