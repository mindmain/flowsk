import { Injectable, signal } from '@angular/core';

/**
 * Holds reactive workspace and actor header values read by flowskApiInterceptor
 * without creating circular DI dependencies with FlowskStore.
 */
@Injectable({ providedIn: 'root' })
export class FlowskHeaderContextService {
  readonly workspaceRef = signal<string>('default');
  readonly actorHandle = signal<string>('web-operator');
  readonly actorId = signal<string>('');

  /**
   * Updates active workspace and actor request header metadata.
   */
  setIdentity(workspaceRef: string, actorHandle: string, actorId?: string): void {
    if (workspaceRef.trim()) {
      this.workspaceRef.set(workspaceRef.trim());
    }
    if (actorHandle.trim()) {
      this.actorHandle.set(actorHandle.trim());
    }
    if (actorId !== undefined) {
      this.actorId.set(actorId.trim());
    }
  }
}
