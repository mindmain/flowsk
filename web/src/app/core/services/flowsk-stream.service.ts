import { isPlatformBrowser } from '@angular/common';
import { HttpClient } from '@angular/common/http';
import { Injectable, NgZone, PLATFORM_ID, inject } from '@angular/core';
import { Observable, Subscription } from 'rxjs';
import { SanitizedApiError } from '../http/flowsk-api.interceptor';
import { LiveStreamEventModel, RuntimeBootstrapConfig, StreamConnectionState } from '../models/flowsk.models';

/**
 * Union of real-time workspace stream status transitions, mutation frames, and auth challenges.
 */
export type WorkspaceStreamSignal =
  | { readonly type: 'status'; readonly status: StreamConnectionState }
  | { readonly type: 'mutation'; readonly event: LiveStreamEventModel }
  | { readonly type: 'auth_required' };

const INITIAL_RETRY_MS = 1_000;
const MAX_RETRY_MS = 15_000;

/**
 * Platform-safe Server-Sent Events (`EventSource`) transport service connecting to
 * Flowsk's Go channel pub/sub endpoint (`GET /api/events`).
 * Consumed exclusively by FlowskStore; closes connections deterministically on teardown
 * and performs bounded exponential-backoff reconnection when an EventSource closes.
 */
@Injectable({ providedIn: 'root' })
export class FlowskStreamService {
  private readonly platformId = inject(PLATFORM_ID);
  private readonly zone = inject(NgZone);
  private readonly http = inject(HttpClient);

  /**
   * Opens a tenant-scoped SSE subscription for `workspaceId` with bounded reconnect backoff.
   */
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
      const streamUrl = `/api/events?${params.toString()}`;

      let tornDown = false;
      let source: EventSource | null = null;
      let retryTimer: ReturnType<typeof setTimeout> | null = null;
      let authCheckSub: Subscription | null = null;
      let retryAttempt = 0;

      const onReady = (): void => {
        retryAttempt = 0;
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
          retryAttempt = 0;
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

      const cleanupSource = (): void => {
        if (!source) {
          return;
        }
        source.removeEventListener('ready', onReady as EventListener);
        source.removeEventListener('mutation', onMutation as EventListener);
        source.onopen = null;
        source.onerror = null;
        source.close();
        source = null;
      };

      const scheduleReconnect = (): void => {
        if (tornDown || retryTimer !== null) {
          return;
        }
        const delayMs = Math.min(INITIAL_RETRY_MS * Math.pow(2, retryAttempt), MAX_RETRY_MS);
        retryAttempt += 1;
        retryTimer = setTimeout(() => {
          retryTimer = null;
          openEventSource();
        }, delayMs);
      };

      const openEventSource = (): void => {
        if (tornDown) {
          return;
        }
        cleanupSource();
        const current = new EventSource(streamUrl);
        source = current;

        current.onopen = () => {
          retryAttempt = 0;
          this.zone.run(() => {
            subscriber.next({ type: 'status', status: 'live' });
          });
        };

        current.onerror = () => {
          if (tornDown) {
            return;
          }
          this.zone.run(() => {
            subscriber.next({ type: 'status', status: 'reconnecting' });
          });

          if (current.readyState === EventSource.CLOSED) {
            cleanupSource();
            authCheckSub?.unsubscribe();
            authCheckSub = this.http.get<RuntimeBootstrapConfig>('/api/ui-config').subscribe({
              next: (cfg) => {
                if (tornDown) {
                  return;
                }
                if (cfg.authRequired && cfg.authenticated === false) {
                  this.zone.run(() => {
                    subscriber.next({ type: 'auth_required' });
                  });
                  return;
                }
                scheduleReconnect();
              },
              error: (err: unknown) => {
                if (tornDown) {
                  return;
                }
                if (err instanceof SanitizedApiError && err.statusCode === 401) {
                  this.zone.run(() => {
                    subscriber.next({ type: 'auth_required' });
                  });
                  return;
                }
                scheduleReconnect();
              },
            });
          }
        };

        current.addEventListener('ready', onReady as EventListener);
        current.addEventListener('mutation', onMutation as EventListener);
      };

      subscriber.next({ type: 'status', status: 'connecting' });
      openEventSource();

      return () => {
        tornDown = true;
        if (retryTimer !== null) {
          clearTimeout(retryTimer);
          retryTimer = null;
        }
        authCheckSub?.unsubscribe();
        authCheckSub = null;
        cleanupSource();
      };
    });
  }
}
