import { isPlatformBrowser } from '@angular/common';
import { Injectable, NgZone, PLATFORM_ID, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { LiveStreamEventModel, StreamConnectionState } from '../models/flowsk.models';

export type WorkspaceStreamSignal =
  | { readonly type: 'status'; readonly status: StreamConnectionState }
  | { readonly type: 'mutation'; readonly event: LiveStreamEventModel };

/**
 * Platform-safe Server-Sent Events (`EventSource`) transport service connecting to
 * Flowsk's Go channel pub/sub endpoint (`GET /api/events`).
 * Consumed exclusively by FlowskStore; closes connections deterministically on teardown.
 */
@Injectable({ providedIn: 'root' })
export class FlowskStreamService {
  private readonly platformId = inject(PLATFORM_ID);
  private readonly zone = inject(NgZone);

  connectWorkspace(workspaceId: string, actorHandle: string): Observable<WorkspaceStreamSignal> {
    return new Observable<WorkspaceStreamSignal>((subscriber) => {
      if (!isPlatformBrowser(this.platformId) || typeof EventSource === 'undefined' || !workspaceId.trim()) {
        subscriber.complete();
        return undefined;
      }

      const params = new URLSearchParams({
        workspaceId: workspaceId.trim(),
      });
      if (actorHandle.trim()) {
        params.set('actorHandle', actorHandle.trim());
      }

      subscriber.next({ type: 'status', status: 'connecting' });
      const source = new EventSource(`/api/events?${params.toString()}`);

      const onReady = (): void => {
        this.zone.run(() => {
          subscriber.next({ type: 'status', status: 'live' });
        });
      };

      const onMutation = (ev: MessageEvent<string>): void => {
        if (!ev.data) {
          return;
        }
        try {
          const parsed = JSON.parse(ev.data) as Partial<LiveStreamEventModel>;
          if (!parsed || typeof parsed !== 'object' || !parsed.action) {
            return;
          }
          const mapped: LiveStreamEventModel = {
            id: String(parsed.id ?? ''),
            workspaceId: String(parsed.workspaceId ?? workspaceId),
            actorId: String(parsed.actorId ?? ''),
            actorHandle: parsed.actorHandle ? String(parsed.actorHandle) : undefined,
            actorKind: parsed.actorKind ? String(parsed.actorKind) : undefined,
            action: String(parsed.action),
            resourceType: String(parsed.resourceType ?? 'entity'),
            resourceId: String(parsed.resourceId ?? ''),
            occurredAt: String(parsed.occurredAt ?? new Date().toISOString()),
          };
          this.zone.run(() => {
            subscriber.next({ type: 'status', status: 'live' });
            subscriber.next({ type: 'mutation', event: mapped });
          });
        } catch {
          // Ignore malformed SSE frames safely
        }
      };

      source.onopen = () => {
        this.zone.run(() => {
          subscriber.next({ type: 'status', status: 'live' });
        });
      };

      source.onerror = () => {
        this.zone.run(() => {
          subscriber.next({ type: 'status', status: 'reconnecting' });
        });
      };

      source.addEventListener('ready', onReady as EventListener);
      source.addEventListener('mutation', onMutation as EventListener);

      return () => {
        source.removeEventListener('ready', onReady as EventListener);
        source.removeEventListener('mutation', onMutation as EventListener);
        source.close();
      };
    });
  }
}
