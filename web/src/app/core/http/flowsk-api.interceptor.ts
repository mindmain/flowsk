import { HttpErrorResponse, HttpInterceptorFn } from '@angular/common/http';
import { inject } from '@angular/core';
import { catchError, throwError } from 'rxjs';
import { FlowskHeaderContextService } from '../services/flowsk-header-context.service';

const UUID_HEADER_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Sanitized HTTP/ConnectRPC error safe for presentation in Angular UI templates.
 */
export class SanitizedApiError extends Error {
  constructor(
    public readonly userMessage: string,
    public readonly statusCode: number,
    public readonly correlationId: string,
  ) {
    super(userMessage);
    this.name = 'SanitizedApiError';
  }
}

/**
 * Centralized HTTP interceptor attaching ConnectRPC protocol headers, tenant
 * identity headers, correlation IDs, and sanitizing backend error payloads.
 */
export const flowskApiInterceptor: HttpInterceptorFn = (req, next) => {
  const headerCtx = inject(FlowskHeaderContextService);
  const correlationId = createCorrelationId();

  const headers: Record<string, string> = {
    'X-Request-Id': correlationId,
  };

  if (req.url.startsWith('/flowsk.')) {
    headers['Connect-Protocol-Version'] = '1';
    headers['X-Flowsk-Client'] = 'webui';
    const ws = headerCtx.workspaceRef();
    const handle = headerCtx.actorHandle();
    const actorId = headerCtx.actorId().trim();
    if (ws) {
      headers['X-Flowsk-Workspace-Id'] = ws;
    }
    if (UUID_HEADER_PATTERN.test(actorId)) {
      headers['X-Flowsk-Actor-Id'] = actorId;
    }
    if (handle) {
      headers['X-Flowsk-Actor-Handle'] = handle;
      headers['X-Flowsk-Actor-Kind'] = 'human';
    }
  }

  const cloned = req.clone({ setHeaders: headers });

  return next(cloned).pipe(
    catchError((err: unknown) => {
      const sanitized = sanitizeHttpError(err, correlationId);
      return throwError(() => sanitized);
    }),
  );
};

function createCorrelationId(): string {
  return `ui-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

function sanitizeHttpError(err: unknown, correlationId: string): SanitizedApiError {
  if (err instanceof HttpErrorResponse) {
    const body = err.error as { message?: unknown; code?: unknown; error?: unknown } | null;
    if (body && typeof body === 'object') {
      const rawMsg =
        typeof body.message === 'string'
          ? body.message
          : typeof body.error === 'string'
            ? body.error
            : '';
      if (rawMsg.trim().length > 0) {
        const cleanMsg = rawMsg.trim().slice(0, 240);
        return new SanitizedApiError(cleanMsg, err.status, correlationId);
      }
    }
    if (err.status === 0) {
      return new SanitizedApiError('Unable to reach local Flowsk server.', 0, correlationId);
    }
    if (err.status === 401 || err.status === 403) {
      return new SanitizedApiError('Access denied for the selected workspace.', err.status, correlationId);
    }
    if (err.status === 404) {
      return new SanitizedApiError('Requested Flowsk resource was not found.', 404, correlationId);
    }
    if (err.status === 409 || err.status === 412) {
      return new SanitizedApiError('Operation conflicted with an active lease or blocker.', err.status, correlationId);
    }
    return new SanitizedApiError(`Request failed (${err.status}).`, err.status, correlationId);
  }
  return new SanitizedApiError('An unexpected communication error occurred.', 500, correlationId);
}
