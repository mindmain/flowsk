// File stream.go implements a concurrency-safe, tenant-isolated Go channel
// pub/sub hub (`EventBroker`), a ConnectRPC mutation wake interceptor, a
// cross-process audit-log tail relay, and a Server-Sent Events (`GET /api/events`)
// HTTP endpoint so the Flowsk Web UI updates in real time whenever autonomous
// agents or human operators create, claim, comment on, or transition tasks and goals.
package connectrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	flowsk_task_pb "github.com/mindmain/flowsk/gen/go/flowsk/task/v1"
	flowsk_workspace_pb "github.com/mindmain/flowsk/gen/go/flowsk/workspace/v1"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

const (
	// EventsPath is the Server-Sent Events (`text/event-stream`) endpoint mounted
	// alongside the embedded Web UI and ConnectRPC services.
	EventsPath = "/api/events"

	// DefaultSubscriberBuffer bounds per-subscriber Go channel capacity so slow
	// browser tabs never block RPC mutations or audit relay workers.
	DefaultSubscriberBuffer = 64

	// DefaultStreamHeartbeat is the keep-alive comment interval emitted on idle SSE connections.
	DefaultStreamHeartbeat = 15 * time.Second

	// DefaultAuditPollInterval is the background audit-log tail interval used when
	// at least one browser subscriber is connected, catching mutations committed by
	// standalone local CLI or `stdio` MCP agent processes sharing `~/.flowsk/flowsk.db`.
	DefaultAuditPollInterval = 750 * time.Millisecond

	auditRelayBatchLimit        = 200
	maxSeenAuditIDsPerWorkspace = 512
	streamWriteFrameTimeout     = 30 * time.Second
)

// StreamEvent represents a single tenant-scoped real-time mutation notification
// delivered over Go channels to subscribed Web UI clients.
//
// Zero Host Path Invariant:
//   - StreamEvent carries strictly logical UUIDs, handles, actions, and entity types;
//     host filesystem paths are never included.
type StreamEvent struct {
	// ID is the unique event or audit record UUID.
	ID string `json:"id"`

	// WorkspaceID is the owning tenant workspace UUID.
	WorkspaceID string `json:"workspaceId"`

	// ActorID is the principal UUID that triggered the mutation.
	ActorID string `json:"actorId"`

	// ActorHandle is the compact display handle of the actor (e.g., "claude" or "web-operator").
	ActorHandle string `json:"actorHandle,omitempty"`

	// ActorKind classifies the principal ("human" or "agent").
	ActorKind string `json:"actorKind,omitempty"`

	// Action is the canonical AuditAction name ("Create", "Update", "Delete", "Transition", "Claim", "Release", "BatchExec").
	Action string `json:"action"`

	// ResourceType identifies the mutated domain entity ("task", "project", "objective", "task_comment", "codemode_script", etc.).
	ResourceType string `json:"resourceType"`

	// ResourceID is the canonical UUID or identifier of the mutated resource.
	ResourceID string `json:"resourceId"`

	// OccurredAt is the RFC3339Nano UTC timestamp when the mutation committed.
	OccurredAt string `json:"occurredAt"`
}

// StreamEventFromAuditRecord converts an immutable domain `audit.Record` into a wire `StreamEvent`.
func StreamEventFromAuditRecord(rec audit.Record) StreamEvent {
	ts := rec.OccurredAt.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return StreamEvent{
		ID:           rec.ID.String(),
		WorkspaceID:  rec.WorkspaceID.String(),
		ActorID:      rec.ActorID.String(),
		ActorHandle:  rec.ActorHandle,
		ActorKind:    string(rec.ActorKind),
		Action:       string(rec.Action),
		ResourceType: rec.ResourceType,
		ResourceID:   rec.ResourceID,
		OccurredAt:   ts.Format(time.RFC3339Nano),
	}
}

// StreamAuthResolver authenticates an inbound `GET /api/events` HTTP request and
// returns the caller's authorized `authctx.Identity` together with the target
// workspace UUID to subscribe to.
type StreamAuthResolver func(ctx context.Context, r *http.Request) (authctx.Identity, uuid.UUID, error)

// WorkspaceIdentityProvider mints an internal read-only `authctx.Identity` for a
// workspace that currently has active subscribers so the background audit relay
// can query `audit.Service` with strict workspace ownership checks.
type WorkspaceIdentityProvider func(ctx context.Context, workspaceID uuid.UUID) (authctx.Identity, bool)

// EventBroker is a concurrency-safe, multi-tenant Go channel fan-out broker for
// real-time workspace mutation streaming.
type EventBroker struct {
	bufSize int
	wakeCh  chan uuid.UUID

	mu           sync.RWMutex
	subs         map[uuid.UUID]map[chan StreamEvent]struct{}
	initialized  map[uuid.UUID]bool
	subscribedAt map[uuid.UUID]time.Time
	lastSince    map[uuid.UUID]time.Time
	seenSet      map[uuid.UUID]map[uuid.UUID]struct{}
	seenRing     map[uuid.UUID][]uuid.UUID
}

// NewEventBroker constructs a tenant-isolated Go channel `EventBroker`.
func NewEventBroker(subscriberBuffer int) *EventBroker {
	if subscriberBuffer <= 0 {
		subscriberBuffer = DefaultSubscriberBuffer
	}
	return &EventBroker{
		bufSize:      subscriberBuffer,
		wakeCh:       make(chan uuid.UUID, 64),
		subs:         make(map[uuid.UUID]map[chan StreamEvent]struct{}),
		initialized:  make(map[uuid.UUID]bool),
		subscribedAt: make(map[uuid.UUID]time.Time),
		lastSince:    make(map[uuid.UUID]time.Time),
		seenSet:      make(map[uuid.UUID]map[uuid.UUID]struct{}),
		seenRing:     make(map[uuid.UUID][]uuid.UUID),
	}
}

// Subscribe registers a buffered Go channel subscribed exclusively to mutation
// events belonging to `workspaceID`, returning the read-only channel and an
// idempotent `unsubscribe` cleanup callback.
func (b *EventBroker) Subscribe(workspaceID uuid.UUID) (<-chan StreamEvent, func()) {
	if b == nil || workspaceID == uuid.Nil {
		closed := make(chan StreamEvent)
		close(closed)
		return closed, func() {}
	}
	ch := make(chan StreamEvent, b.bufSize)

	b.mu.Lock()
	wsSubs := b.subs[workspaceID]
	if wsSubs == nil {
		wsSubs = make(map[chan StreamEvent]struct{})
		b.subs[workspaceID] = wsSubs
	}
	wsSubs[ch] = struct{}{}
	needInit := !b.initialized[workspaceID]
	if needInit && b.subscribedAt[workspaceID].IsZero() {
		now := nextWallClockTickUTC()
		b.subscribedAt[workspaceID] = now
		b.lastSince[workspaceID] = now.Add(-200 * time.Millisecond)
	}
	b.mu.Unlock()

	if needInit {
		b.NotifyWorkspace(workspaceID)
	}

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[workspaceID]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(b.subs, workspaceID)
					delete(b.initialized, workspaceID)
					delete(b.subscribedAt, workspaceID)
					delete(b.lastSince, workspaceID)
					delete(b.seenSet, workspaceID)
					delete(b.seenRing, workspaceID)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, unsubscribe
}

// Publish fans out `evt` non-blockingly onto every subscriber Go channel registered
// for `workspaceID`, never crossing tenant boundaries or blocking callers.
func (b *EventBroker) Publish(workspaceID uuid.UUID, evt StreamEvent) int {
	if b == nil || workspaceID == uuid.Nil {
		return 0
	}
	if evt.WorkspaceID == "" {
		evt.WorkspaceID = workspaceID.String()
	}
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if evt.OccurredAt == "" {
		evt.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	b.mu.RLock()
	wsSubs := b.subs[workspaceID]
	if len(wsSubs) == 0 {
		b.mu.RUnlock()
		return 0
	}
	targets := make([]chan StreamEvent, 0, len(wsSubs))
	for ch := range wsSubs {
		targets = append(targets, ch)
	}
	b.mu.RUnlock()

	delivered := 0
	for _, ch := range targets {
		select {
		case ch <- evt:
			delivered++
		default:
			// Subscriber buffer is full; drop oldest queued event or skip without blocking.
		}
	}
	return delivered
}

// NotifyWorkspace wakes the audit relay worker immediately after an in-process
// mutation commits in `workspaceID`.
func (b *EventBroker) NotifyWorkspace(workspaceID uuid.UUID) {
	if b == nil || workspaceID == uuid.Nil {
		return
	}
	select {
	case b.wakeCh <- workspaceID:
	default:
	}
}

// ActiveWorkspaces returns a defensive snapshot of workspace UUIDs that currently
// have at least one connected subscriber channel.
func (b *EventBroker) ActiveWorkspaces() []uuid.UUID {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.subs) == 0 {
		return nil
	}
	out := make([]uuid.UUID, 0, len(b.subs))
	for wsID, set := range b.subs {
		if len(set) > 0 {
			out = append(out, wsID)
		}
	}
	return out
}

// RunAuditRelay tails newly committed `audit.Record` rows for active workspaces,
// combining sub-millisecond in-process `NotifyWorkspace` wakeups with periodic
// polling (catching external CLI / `stdio` MCP agent mutations on shared SQLite WAL
// or PostgreSQL databases) and broadcasting deduplicated events over Go channels.
//
// Concurrency Invariant:
//   - Terminates deterministically as soon as `ctx` is canceled.
//   - Performs zero database queries while no Web UI subscribers are connected.
func (b *EventBroker) RunAuditRelay(
	ctx context.Context,
	auditSvc audit.Service,
	idProvider WorkspaceIdentityProvider,
	pollInterval time.Duration,
) {
	if b == nil || auditSvc == nil || idProvider == nil {
		return
	}
	if pollInterval <= 0 {
		pollInterval = DefaultAuditPollInterval
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case wsID := <-b.wakeCh:
			b.syncWorkspaceAudit(ctx, auditSvc, idProvider, wsID)
		case <-ticker.C:
			for _, wsID := range b.ActiveWorkspaces() {
				if ctx.Err() != nil {
					return
				}
				b.syncWorkspaceAudit(ctx, auditSvc, idProvider, wsID)
			}
		}
	}
}

func (b *EventBroker) syncWorkspaceAudit(
	ctx context.Context,
	auditSvc audit.Service,
	idProvider WorkspaceIdentityProvider,
	workspaceID uuid.UUID,
) {
	if workspaceID == uuid.Nil || ctx.Err() != nil {
		return
	}

	b.mu.RLock()
	hasSubs := len(b.subs[workspaceID]) > 0
	isInit := b.initialized[workspaceID]
	subAt := b.subscribedAt[workspaceID]
	since := b.lastSince[workspaceID]
	b.mu.RUnlock()

	if !hasSubs {
		return
	}

	ident, ok := idProvider(ctx, workspaceID)
	if !ok || ident.WorkspaceID != workspaceID {
		return
	}
	queryCtx := authctx.WithIdentity(ctx, ident)

	if !isInit {
		// Baseline seed: mark pre-existing audit records (committed at or before
		// Subscribe's synchronous baseline timestamp) as seen while immediately
		// relaying any mutations committed after Subscribe() returned.
		records, err := auditSvc.QueryRecords(queryCtx, audit.QueryFilter{
			WorkspaceID: workspaceID,
			Limit:       auditRelayBatchLimit,
		})
		if err != nil {
			return
		}
		var fresh []audit.Record
		b.mu.Lock()
		if len(b.subs[workspaceID]) == 0 {
			b.mu.Unlock()
			return
		}
		if !b.initialized[workspaceID] {
			baseline := b.subscribedAt[workspaceID]
			if baseline.IsZero() {
				baseline = subAt
			}
			if baseline.IsZero() {
				baseline = time.Now().UTC()
			}
			latest := baseline
			for i := len(records) - 1; i >= 0; i-- {
				r := records[i]
				if r.WorkspaceID != workspaceID {
					continue
				}
				b.markSeenLocked(workspaceID, r.ID)
				if r.OccurredAt.After(latest) {
					latest = r.OccurredAt.UTC()
				}
				if !r.OccurredAt.Before(baseline) {
					fresh = append(fresh, r)
				}
			}
			b.lastSince[workspaceID] = latest
			b.initialized[workspaceID] = true
		}
		b.mu.Unlock()

		for _, rec := range fresh {
			b.Publish(workspaceID, StreamEventFromAuditRecord(rec))
		}
		return
	}

	windowSince := since.Add(-2 * time.Second)
	records, err := auditSvc.QueryRecords(queryCtx, audit.QueryFilter{
		WorkspaceID: workspaceID,
		Since:       &windowSince,
		Limit:       auditRelayBatchLimit,
	})
	if err != nil || len(records) == 0 {
		return
	}

	var fresh []audit.Record
	b.mu.Lock()
	if len(b.subs[workspaceID]) == 0 {
		b.mu.Unlock()
		return
	}
	latest := b.lastSince[workspaceID]
	// Iterate oldest-to-newest (QueryRecords returns newest-first).
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if rec.WorkspaceID != workspaceID {
			continue
		}
		if b.isSeenLocked(workspaceID, rec.ID) {
			continue
		}
		b.markSeenLocked(workspaceID, rec.ID)
		if rec.OccurredAt.After(latest) {
			latest = rec.OccurredAt.UTC()
		}
		fresh = append(fresh, rec)
	}
	if latest.After(b.lastSince[workspaceID]) {
		b.lastSince[workspaceID] = latest
	}
	b.mu.Unlock()

	for _, rec := range fresh {
		b.Publish(workspaceID, StreamEventFromAuditRecord(rec))
	}
}

func (b *EventBroker) isSeenLocked(workspaceID, recordID uuid.UUID) bool {
	set := b.seenSet[workspaceID]
	if set == nil {
		return false
	}
	_, ok := set[recordID]
	return ok
}

func (b *EventBroker) markSeenLocked(workspaceID, recordID uuid.UUID) {
	if recordID == uuid.Nil {
		return
	}
	set := b.seenSet[workspaceID]
	if set == nil {
		set = make(map[uuid.UUID]struct{})
		b.seenSet[workspaceID] = set
	}
	if _, exists := set[recordID]; exists {
		return
	}
	set[recordID] = struct{}{}
	ring := append(b.seenRing[workspaceID], recordID)
	if len(ring) > maxSeenAuditIDsPerWorkspace {
		evict := ring[0]
		ring = ring[1:]
		delete(set, evict)
	}
	b.seenRing[workspaceID] = ring
}

// NewEventBroadcastInterceptor returns a Connect unary interceptor that signals
// `broker.NotifyWorkspace` immediately whenever a mutating ConnectRPC call succeeds,
// delivering sub-millisecond real-time updates to connected Web UI clients.
func NewEventBroadcastInterceptor(broker *EventBroker) connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err != nil || broker == nil {
				return resp, err
			}
			isSignalRead := isTaskSignalReadProcedure(req.Spec().Procedure) && authctx.ShouldEmitTaskSignal(ctx)
			if isReadOnlyProcedure(req.Spec().Procedure) && !isSignalRead {
				return resp, nil
			}
			if ident, idErr := authctx.FromContext(ctx); idErr == nil && ident.WorkspaceID != uuid.Nil {
				if isSignalRead && resp != nil {
					if resID, ok := extractSignaledTaskIDFromResponse(req.Any(), resp.Any()); ok {
						broker.Publish(ident.WorkspaceID, StreamEvent{
							ID:           uuid.NewString(),
							WorkspaceID:  ident.WorkspaceID.String(),
							ActorID:      ident.ActorID.String(),
							ActorHandle:  ident.Handle,
							ActorKind:    string(ident.Kind),
							Action:       "Signal",
							ResourceType: "task",
							ResourceID:   resID,
							OccurredAt:   time.Now().UTC().Format(time.RFC3339Nano),
						})
					}
				}
				broker.NotifyWorkspace(ident.WorkspaceID)
				return resp, nil
			}
			if resp != nil {
				if wsID := extractWorkspaceIDFromResponse(resp.Any()); wsID != uuid.Nil {
					broker.NotifyWorkspace(wsID)
				}
			}
			return resp, nil
		})
	})
}

func extractWorkspaceIDFromResponse(raw any) uuid.UUID {
	switch msg := raw.(type) {
	case *flowsk_workspace_pb.CreateWorkspaceResponse:
		if ws := msg.GetWorkspace(); ws != nil {
			if id, err := uuid.Parse(strings.TrimSpace(ws.GetId())); err == nil {
				return id
			}
		}
	case *flowsk_workspace_pb.RegisterActorResponse:
		if actor := msg.GetActor(); actor != nil {
			if id, err := uuid.Parse(strings.TrimSpace(actor.GetWorkspaceId())); err == nil {
				return id
			}
		}
	}
	return uuid.Nil
}

func isReadOnlyProcedure(proc string) bool {
	idx := strings.LastIndexByte(proc, '/')
	method := proc
	if idx >= 0 && idx+1 < len(proc) {
		method = proc[idx+1:]
	}
	return strings.HasPrefix(method, "Get") ||
		strings.HasPrefix(method, "List") ||
		strings.HasPrefix(method, "Inspect") ||
		strings.HasPrefix(method, "Validate")
}

func isTaskSignalReadProcedure(proc string) bool {
	switch proc {
	case "/flowsk.task.v1.TaskService/GetTask",
		"/flowsk.task.v1.TaskService/GetTaskByIdentifier",
		"/flowsk.task.v1.TaskService/GetTaskByExternalId",
		"/flowsk.task.v1.TaskService/GetTaskMarkdown":
		return true
	default:
		return false
	}
}

func nextWallClockTickUTC() time.Time {
	t0 := time.Now().UTC()
	deadline := time.Now().Add(50 * time.Microsecond)
	for time.Now().Before(deadline) {
		now := time.Now().UTC()
		if now.After(t0) {
			return now
		}
	}
	return time.Now().UTC()
}

func extractSignaledTaskIDFromResponse(rawReq, rawResp any) (string, bool) {
	switch msg := rawResp.(type) {
	case *flowsk_task_pb.GetTaskResponse:
		if t := msg.GetTask(); t != nil && t.GetLastSignalAt() != nil && t.GetId() != "" {
			return t.GetId(), true
		}
	case *flowsk_task_pb.GetTaskByIdentifierResponse:
		if t := msg.GetTask(); t != nil && t.GetLastSignalAt() != nil && t.GetId() != "" {
			return t.GetId(), true
		}
	case *flowsk_task_pb.GetTaskByExternalIdResponse:
		if t := msg.GetTask(); t != nil && t.GetLastSignalAt() != nil && t.GetId() != "" {
			return t.GetId(), true
		}
	case *flowsk_task_pb.GetTaskMarkdownResponse:
		if mdReq, ok := rawReq.(*flowsk_task_pb.GetTaskMarkdownRequest); ok {
			if id := strings.TrimSpace(mdReq.GetTaskId()); id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// NewSSEHandler serves `GET /api/events` over HTTP/1.1 and HTTP/2 using Go's
// `http.ResponseController` to bypass short unary write timeouts while enforcing
// strict multi-tenant workspace ownership on every stream subscription.
func NewSSEHandler(
	broker *EventBroker,
	authResolver StreamAuthResolver,
	heartbeatInterval time.Duration,
) http.Handler {
	if heartbeatInterval <= 0 {
		heartbeatInterval = DefaultStreamHeartbeat
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		if broker == nil || authResolver == nil {
			http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
			return
		}

		ident, targetWorkspaceID, err := authResolver(r.Context(), r)
		if err != nil {
			writeStreamAuthError(w, err)
			return
		}
		authCtx := authctx.WithIdentity(r.Context(), ident)
		if _, err := authctx.RequireWorkspaceOwnership(authCtx, targetWorkspaceID); err != nil {
			writeStreamAuthError(w, err)
			return
		}

		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{})

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)

		subCh, unsubscribe := broker.Subscribe(targetWorkspaceID)
		defer unsubscribe()

		readyJSON, _ := json.Marshal(map[string]string{
			"workspaceId": targetWorkspaceID.String(),
			"status":      "live",
		})
		if err := writeSSEFrame(w, rc, "", "ready", readyJSON); err != nil {
			return
		}

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_ = rc.SetWriteDeadline(time.Now().Add(streamWriteFrameTimeout))
				if _, wErr := fmt.Fprint(w, ": ping\n\n"); wErr != nil {
					return
				}
				if fErr := rc.Flush(); fErr != nil {
					return
				}
			case evt, ok := <-subCh:
				if !ok {
					return
				}
				if evt.WorkspaceID != targetWorkspaceID.String() {
					continue
				}
				raw, mErr := json.Marshal(evt)
				if mErr != nil {
					continue
				}
				if err := writeSSEFrame(w, rc, evt.ID, "mutation", raw); err != nil {
					return
				}
			}
		}
	})
}

func writeSSEFrame(w http.ResponseWriter, rc *http.ResponseController, id, eventName string, jsonPayload []byte) error {
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteFrameTimeout))
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, jsonPayload); err != nil {
		return err
	}
	return rc.Flush()
}

func writeStreamAuthError(w http.ResponseWriter, err error) {
	status := http.StatusUnauthorized
	switch {
	case errors.Is(err, errs.ErrPermissionDenied):
		status = http.StatusForbidden
	case errors.Is(err, errs.ErrValidation):
		status = http.StatusBadRequest
	}
	http.Error(w, http.StatusText(status), status)
}
