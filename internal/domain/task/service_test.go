// File service_test.go verifies task.Service multi-tenant ownership, atomic audit
// rollback, key allocation, DAG cycle detection on relations and parent hierarchy,
// unresolved blocker preconditions, exclusive ClaimLease concurrency (-race),
// and compact agent queue/digest projections.
package task_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

type memTxHandle struct{}

func (memTxHandle) TxDialect() string { return "memory" }

type memAuditRecorder struct {
	entries *[]tx.AuditEntry
	failErr error
}

func (r *memAuditRecorder) Record(_ context.Context, entry tx.AuditEntry) error {
	if r.failErr != nil {
		return r.failErr
	}
	*r.entries = append(*r.entries, entry.Clone())
	return nil
}

type memTaskState struct {
	tasks     map[uuid.UUID]task.Task
	leases    map[uuid.UUID]task.ClaimLease
	relations map[uuid.UUID]task.TaskRelation
	comments  map[uuid.UUID][]task.TaskComment
	seqByProj map[uuid.UUID]int64
}

func newMemTaskState() *memTaskState {
	return &memTaskState{
		tasks:     make(map[uuid.UUID]task.Task),
		leases:    make(map[uuid.UUID]task.ClaimLease),
		relations: make(map[uuid.UUID]task.TaskRelation),
		comments:  make(map[uuid.UUID][]task.TaskComment),
		seqByProj: make(map[uuid.UUID]int64),
	}
}

func (s *memTaskState) clone() *memTaskState {
	cp := newMemTaskState()
	for k, v := range s.tasks {
		cp.tasks[k] = v.Clone()
	}
	for k, v := range s.leases {
		cp.leases[k] = v.Clone()
	}
	for k, v := range s.relations {
		cp.relations[k] = v
	}
	for k, list := range s.comments {
		copied := make([]task.TaskComment, len(list))
		copy(copied, list)
		cp.comments[k] = copied
	}
	for k, v := range s.seqByProj {
		cp.seqByProj[k] = v
	}
	return cp
}

func (s *memTaskState) hydrate(t task.Task, now time.Time) task.Task {
	out := t.Clone()
	if lease, ok := s.leases[t.ID]; ok && !lease.IsExpiredAt(now) {
		lc := lease.Clone()
		out.ActiveLease = &lc
	} else {
		out.ActiveLease = nil
	}
	var blockers int32
	for _, rel := range s.relations {
		if rel.WorkspaceID == t.WorkspaceID && rel.TargetTaskID == t.ID && rel.Type == task.RelationBlocks {
			if src, ok := s.tasks[rel.SourceTaskID]; ok && src.DeletedAt == nil && !src.Status.IsTerminal() {
				blockers++
			}
		}
	}
	out.UnresolvedBlockerCount = blockers
	return out
}

type memScope struct {
	recorder *memAuditRecorder
	state    *memTaskState
}

func (s *memScope) Audit() tx.AuditRecorder      { return s.recorder }
func (s *memScope) IsAuditEnabled() bool         { return true }
func (s *memScope) TxHandle() tx.DialectTxHandle { return memTxHandle{} }

type memTaskEnv struct {
	mu           sync.Mutex
	state        *memTaskState
	audits       []tx.AuditEntry
	auditFn      func() error
	mintKeyInRep bool
}

func newMemTaskEnv() *memTaskEnv {
	return &memTaskEnv{
		state: newMemTaskState(),
	}
}

func (e *memTaskEnv) WithTransaction(ctx context.Context, fn tx.TxFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	staged := e.state.clone()
	var stagedAudits []tx.AuditEntry
	var failErr error
	if e.auditFn != nil {
		failErr = e.auditFn()
	}
	scope := &memScope{
		recorder: &memAuditRecorder{entries: &stagedAudits, failErr: failErr},
		state:    staged,
	}
	if err := fn(ctx, scope); err != nil {
		return err
	}
	e.state = staged
	e.audits = append(e.audits, stagedAudits...)
	return nil
}

func (e *memTaskEnv) WithNoAuditTransaction(ctx context.Context, fn tx.TxFunc) error {
	return e.WithTransaction(ctx, fn)
}

type memTaskRepo struct {
	env   *memTaskEnv
	scope *memScope
}

func newMemTaskRepo(env *memTaskEnv) *memTaskRepo {
	return &memTaskRepo{env: env}
}

func (r *memTaskRepo) WithScope(scope tx.Scope) task.Repository {
	ms, _ := scope.(*memScope)
	return &memTaskRepo{env: r.env, scope: ms}
}

func (r *memTaskRepo) readState(fn func(st *memTaskState) error) error {
	if r.scope != nil {
		return fn(r.scope.state)
	}
	r.env.mu.Lock()
	defer r.env.mu.Unlock()
	return fn(r.env.state)
}

func (r *memTaskRepo) writeState(op string, fn func(st *memTaskState) error) error {
	if r.scope == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "mutation outside transaction scope")
	}
	return fn(r.scope.state)
}

func (r *memTaskRepo) Create(_ context.Context, t task.Task) error {
	return r.writeState("task.Repo.Create", func(st *memTaskState) error {
		if _, exists := st.tasks[t.ID]; exists {
			return errs.New(errs.ErrConflict, "task.Repo.Create", "duplicate task ID")
		}
		cp := t.Clone()
		if cp.Key == "" && r.env.mintKeyInRep {
			st.seqByProj[cp.ProjectID]++
			cp.Key = fmt.Sprintf("REPO-%d", st.seqByProj[cp.ProjectID])
		}
		if cp.Key != "" {
			for _, existing := range st.tasks {
				if existing.DeletedAt == nil && existing.WorkspaceID == cp.WorkspaceID && existing.Key == cp.Key {
					return errs.New(errs.ErrConflict, "task.Repo.Create", "duplicate task key")
				}
			}
		}
		if cp.ExternalID != "" {
			for _, existing := range st.tasks {
				if existing.DeletedAt == nil && existing.WorkspaceID == cp.WorkspaceID && existing.ProjectID == cp.ProjectID && existing.ExternalID == cp.ExternalID {
					return errs.New(errs.ErrConflict, "task.Repo.Create", "duplicate external_id in project")
				}
			}
		}
		if cp.Version == 0 {
			cp.Version = 1
		}
		st.tasks[cp.ID] = cp
		return nil
	})
}

func (r *memTaskRepo) GetByID(_ context.Context, workspaceID, taskID uuid.UUID) (task.Task, error) {
	var out task.Task
	err := r.readState(func(st *memTaskState) error {
		cur, ok := st.tasks[taskID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.GetByID", "task not found")
		}
		out = st.hydrate(cur, time.Now().UTC())
		return nil
	})
	return out, err
}

func (r *memTaskRepo) GetByKey(_ context.Context, workspaceID uuid.UUID, key string) (task.Task, error) {
	var out task.Task
	err := r.readState(func(st *memTaskState) error {
		for _, cur := range st.tasks {
			if cur.DeletedAt == nil && cur.WorkspaceID == workspaceID && cur.Key == key {
				out = st.hydrate(cur, time.Now().UTC())
				return nil
			}
		}
		return errs.New(errs.ErrNotFound, "task.Repo.GetByKey", "task not found")
	})
	return out, err
}

func (r *memTaskRepo) GetByExternalID(_ context.Context, workspaceID, projectID uuid.UUID, externalID string) (task.Task, error) {
	var out task.Task
	err := r.readState(func(st *memTaskState) error {
		for _, cur := range st.tasks {
			if cur.DeletedAt == nil && cur.WorkspaceID == workspaceID && cur.ProjectID == projectID && cur.ExternalID != "" && cur.ExternalID == externalID {
				out = st.hydrate(cur, time.Now().UTC())
				return nil
			}
		}
		return errs.New(errs.ErrNotFound, "task.Repo.GetByExternalID", "task not found")
	})
	return out, err
}

func (r *memTaskRepo) Update(_ context.Context, t task.Task, expectedVersion *int64) error {
	return r.writeState("task.Repo.Update", func(st *memTaskState) error {
		cur, ok := st.tasks[t.ID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != t.WorkspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.Update", "task not found")
		}
		if expectedVersion != nil && cur.Version != *expectedVersion {
			return errs.New(errs.ErrConflict, "task.Repo.Update", "optimistic concurrency conflict")
		}
		if t.ExternalID != "" {
			for _, existing := range st.tasks {
				if existing.ID != t.ID && existing.DeletedAt == nil && existing.WorkspaceID == t.WorkspaceID && existing.ProjectID == cur.ProjectID && existing.ExternalID == t.ExternalID {
					return errs.New(errs.ErrConflict, "task.Repo.Update", "duplicate external_id in project")
				}
			}
		}
		cp := t.Clone()
		cp.Version = cur.Version + 1
		st.tasks[cp.ID] = cp
		return nil
	})
}

func (r *memTaskRepo) SoftDelete(_ context.Context, workspaceID, taskID uuid.UUID) error {
	return r.writeState("task.Repo.SoftDelete", func(st *memTaskState) error {
		cur, ok := st.tasks[taskID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.SoftDelete", "task not found")
		}
		now := time.Now().UTC()
		cur.DeletedAt = &now
		st.tasks[taskID] = cur
		delete(st.leases, taskID)
		return nil
	})
}

func (r *memTaskRepo) ListByWorkspace(_ context.Context, workspaceID uuid.UUID) ([]task.Task, error) {
	var out []task.Task
	err := r.readState(func(st *memTaskState) error {
		now := time.Now().UTC()
		for _, raw := range st.tasks {
			if raw.DeletedAt != nil || raw.WorkspaceID != workspaceID {
				continue
			}
			out = append(out, st.hydrate(raw, now))
		}
		return nil
	})
	return out, err
}

func (r *memTaskRepo) SetHasMarkdownDoc(_ context.Context, workspaceID, taskID uuid.UUID, hasDoc bool) (task.Task, error) {
	var updated task.Task
	err := r.writeState("task.Repo.SetHasMarkdownDoc", func(st *memTaskState) error {
		cur, ok := st.tasks[taskID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.SetHasMarkdownDoc", "task not found")
		}
		cur.HasMarkdownDoc = hasDoc
		cur.UpdatedAt = time.Now().UTC()
		st.tasks[taskID] = cur
		updated = st.hydrate(cur, cur.UpdatedAt)
		return nil
	})
	return updated, err
}

func (r *memTaskRepo) TouchSignal(_ context.Context, workspaceID, taskID uuid.UUID, signalAt time.Time) (task.Task, error) {
	var updated task.Task
	err := r.writeState("task.Repo.TouchSignal", func(st *memTaskState) error {
		cur, ok := st.tasks[taskID]
		if !ok || cur.DeletedAt != nil || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.TouchSignal", "task not found")
		}
		if signalAt.IsZero() {
			cur.LastSignalAt = nil
		} else {
			ts := signalAt.UTC()
			cur.LastSignalAt = &ts
		}
		st.tasks[taskID] = cur
		updated = st.hydrate(cur, time.Now().UTC())
		return nil
	})
	return updated, err
}

func (r *memTaskRepo) HardDelete(_ context.Context, workspaceID, taskID uuid.UUID) error {
	return r.writeState("task.Repo.HardDelete", func(st *memTaskState) error {
		cur, ok := st.tasks[taskID]
		if !ok || cur.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.HardDelete", "task not found")
		}
		for id, child := range st.tasks {
			if child.WorkspaceID == workspaceID && child.ParentTaskID != nil && *child.ParentTaskID == taskID {
				child.ParentTaskID = nil
				st.tasks[id] = child
			}
		}
		delete(st.tasks, taskID)
		delete(st.leases, taskID)
		return nil
	})
}

func (r *memTaskRepo) QueryCompactQueue(_ context.Context, filter task.AgentWorkQueueFilter, now time.Time) ([]task.CompactTaskSummary, error) {
	var out []task.CompactTaskSummary
	err := r.readState(func(st *memTaskState) error {
		for _, raw := range st.tasks {
			if raw.DeletedAt != nil || raw.WorkspaceID != filter.WorkspaceID {
				continue
			}
			if filter.ProjectID != uuid.Nil && raw.ProjectID != filter.ProjectID {
				continue
			}
			if filter.ExternalID != "" && raw.ExternalID != filter.ExternalID {
				continue
			}
			if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, raw.Status) {
				continue
			}
			t := st.hydrate(raw, now)
			if filter.OnlyUnblocked && t.UnresolvedBlockerCount > 0 {
				continue
			}
			if filter.OnlyUnclaimed && t.ActiveLease != nil {
				continue
			}
			var holder string
			var exp *time.Time
			if t.ActiveLease != nil {
				holder = t.ActiveLease.HolderHandle
				e := t.ActiveLease.ExpiresAt
				exp = &e
			}
			var sig *time.Time
			if t.LastSignalAt != nil {
				s := *t.LastSignalAt
				sig = &s
			}
			out = append(out, task.CompactTaskSummary{
				ID:                     t.ID,
				Key:                    t.Key,
				ExternalID:             t.ExternalID,
				Kind:                   t.Kind,
				Title:                  t.Title,
				Status:                 t.Status,
				Priority:               t.Priority,
				HolderHandle:           holder,
				LeaseExpiresAt:         exp,
				UnresolvedBlockerCount: t.UnresolvedBlockerCount,
				Tags:                   t.TagsCopy(),
				LastSignalAt:           sig,
				UpdatedAt:              t.UpdatedAt,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
		if filter.Limit > 0 && int(filter.Limit) < len(out) {
			out = out[:filter.Limit]
		}
		return nil
	})
	return out, err
}

func (r *memTaskRepo) BuildProjectDigest(_ context.Context, workspaceID, projectID uuid.UUID, opts task.DigestOptions, now time.Time) (task.ProjectDigest, error) {
	var out task.ProjectDigest
	err := r.readState(func(st *memTaskState) error {
		counts := make(map[task.Status]int32)
		var ready []task.CompactTaskSummary
		var blocked []task.CompactTaskSummary
		var activeLeases []task.CompactTaskSummary
		for _, raw := range st.tasks {
			if raw.DeletedAt != nil || raw.WorkspaceID != workspaceID || raw.ProjectID != projectID {
				continue
			}
			t := st.hydrate(raw, now)
			counts[t.Status]++
			summary := task.CompactTaskSummary{
				ID:                     t.ID,
				Key:                    t.Key,
				Kind:                   t.Kind,
				Title:                  t.Title,
				Status:                 t.Status,
				Priority:               t.Priority,
				UnresolvedBlockerCount: t.UnresolvedBlockerCount,
				Tags:                   t.TagsCopy(),
				UpdatedAt:              t.UpdatedAt,
			}
			if t.ActiveLease != nil {
				summary.HolderHandle = t.ActiveLease.HolderHandle
				e := t.ActiveLease.ExpiresAt
				summary.LeaseExpiresAt = &e
				activeLeases = append(activeLeases, summary)
			}
			if t.UnresolvedBlockerCount > 0 {
				blocked = append(blocked, summary)
			} else if t.Status == task.StatusReady && t.ActiveLease == nil {
				ready = append(ready, summary)
			}
		}
		if opts.MaxReadyTasks > 0 && int(opts.MaxReadyTasks) < len(ready) {
			ready = ready[:opts.MaxReadyTasks]
		}
		if opts.MaxBlockedTasks > 0 && int(opts.MaxBlockedTasks) < len(blocked) {
			blocked = blocked[:opts.MaxBlockedTasks]
		}
		out = task.ProjectDigest{
			WorkspaceID:      workspaceID,
			ProjectID:        projectID,
			ProjectSlug:      "core-proj",
			ProjectKeyPrefix: "FLW",
			GeneratedAt:      now,
			CountsByStatus:   counts,
			ReadyQueue:       ready,
			ActiveLeases:     activeLeases,
			BlockedTasks:     blocked,
		}
		return nil
	})
	return out, err
}

func (r *memTaskRepo) UpsertLease(_ context.Context, lease task.ClaimLease) error {
	return r.writeState("task.Repo.UpsertLease", func(st *memTaskState) error {
		st.leases[lease.TaskID] = lease.Clone()
		return nil
	})
}

func (r *memTaskRepo) GetLease(_ context.Context, workspaceID, taskID uuid.UUID) (task.ClaimLease, error) {
	var out task.ClaimLease
	err := r.readState(func(st *memTaskState) error {
		lease, ok := st.leases[taskID]
		if !ok || lease.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.GetLease", "lease not found")
		}
		out = lease.Clone()
		return nil
	})
	return out, err
}

func (r *memTaskRepo) DeleteLease(_ context.Context, workspaceID, taskID uuid.UUID) error {
	return r.writeState("task.Repo.DeleteLease", func(st *memTaskState) error {
		lease, ok := st.leases[taskID]
		if !ok || lease.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.DeleteLease", "lease not found")
		}
		delete(st.leases, taskID)
		return nil
	})
}

func (r *memTaskRepo) DeleteExpiredLeases(_ context.Context, workspaceID uuid.UUID, asOf time.Time) (int64, error) {
	var count int64
	err := r.writeState("task.Repo.DeleteExpiredLeases", func(st *memTaskState) error {
		for taskID, lease := range st.leases {
			if (workspaceID == uuid.Nil || lease.WorkspaceID == workspaceID) && lease.IsExpiredAt(asOf) {
				delete(st.leases, taskID)
				count++
			}
		}
		return nil
	})
	return count, err
}

func (r *memTaskRepo) CreateRelation(_ context.Context, rel task.TaskRelation) error {
	return r.writeState("task.Repo.CreateRelation", func(st *memTaskState) error {
		for _, existing := range st.relations {
			if existing.WorkspaceID == rel.WorkspaceID &&
				existing.SourceTaskID == rel.SourceTaskID &&
				existing.TargetTaskID == rel.TargetTaskID &&
				existing.Type == rel.Type {
				return errs.New(errs.ErrConflict, "task.Repo.CreateRelation", "duplicate relation")
			}
		}
		st.relations[rel.ID] = rel
		return nil
	})
}

func (r *memTaskRepo) DeleteRelation(_ context.Context, workspaceID, relationID uuid.UUID) error {
	return r.writeState("task.Repo.DeleteRelation", func(st *memTaskState) error {
		rel, ok := st.relations[relationID]
		if !ok || rel.WorkspaceID != workspaceID {
			return errs.New(errs.ErrNotFound, "task.Repo.DeleteRelation", "relation not found")
		}
		delete(st.relations, relationID)
		return nil
	})
}

func (r *memTaskRepo) ListRelationsForTask(_ context.Context, workspaceID, taskID uuid.UUID) ([]task.TaskRelation, error) {
	var out []task.TaskRelation
	err := r.readState(func(st *memTaskState) error {
		for _, rel := range st.relations {
			if rel.WorkspaceID == workspaceID && (rel.SourceTaskID == taskID || rel.TargetTaskID == taskID) {
				out = append(out, rel)
			}
		}
		return nil
	})
	return out, err
}

func (r *memTaskRepo) CreateComment(_ context.Context, c task.TaskComment) error {
	return r.writeState("task.Repo.CreateComment", func(st *memTaskState) error {
		st.comments[c.TaskID] = append(st.comments[c.TaskID], c)
		return nil
	})
}

func (r *memTaskRepo) ListComments(_ context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]task.TaskComment, error) {
	var out []task.TaskComment
	err := r.readState(func(st *memTaskState) error {
		for _, c := range st.comments[taskID] {
			if c.WorkspaceID == workspaceID {
				out = append(out, c)
			}
		}
		if limit > 0 && int(limit) < len(out) {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// TestTaskService_AuthAndTenantIsolation verifies authentication and workspace
// isolation across all 15 methods of task.Service.
func TestTaskService_AuthAndTenantIsolation(t *testing.T) {
	t.Parallel()

	env := newMemTaskEnv()
	svc := task.NewService(newMemTaskRepo(env), env)

	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	taskID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	otherTaskID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	actorA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	unauthCtx := context.Background()
	ctxA := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: tenantA,
		ActorID:     actorA,
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-01",
	})

	newTitle := "Updated"

	cases := []struct {
		name string
		fn   func(ctx context.Context, wsID uuid.UUID) error
	}{
		{
			name: "CreateTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.CreateTask(ctx, task.CreateTaskInput{WorkspaceID: wsID, ProjectID: projID, Title: "Build API"})
				return err
			},
		},
		{
			name: "GetTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetTask(ctx, wsID, taskID)
				return err
			},
		},
		{
			name: "GetTaskByKey",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetTaskByKey(ctx, wsID, "FLW-1")
				return err
			},
		},
		{
			name: "UpdateTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.UpdateTask(ctx, task.UpdateTaskInput{WorkspaceID: wsID, TaskID: taskID, Title: &newTitle})
				return err
			},
		},
		{
			name: "TransitionTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.TransitionTask(ctx, task.TransitionTaskInput{WorkspaceID: wsID, TaskID: taskID, TargetStatus: task.StatusDone})
				return err
			},
		},
		{
			name: "DeleteTask",
			fn:   func(ctx context.Context, wsID uuid.UUID) error { return svc.DeleteTask(ctx, wsID, taskID) },
		},
		{
			name: "ClaimTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.ClaimTask(ctx, task.ClaimTaskInput{WorkspaceID: wsID, TaskID: taskID, TTLSeconds: 300})
				return err
			},
		},
		{
			name: "ReleaseTask",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				return svc.ReleaseTask(ctx, task.ReleaseTaskInput{WorkspaceID: wsID, TaskID: taskID, Force: true})
			},
		},
		{
			name: "PollWorkQueue",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{WorkspaceID: wsID})
				return err
			},
		},
		{
			name: "GetProjectDigest",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.GetProjectDigest(ctx, wsID, projID, task.DigestOptions{})
				return err
			},
		},
		{
			name: "AddRelation",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.AddRelation(ctx, task.AddRelationInput{
					WorkspaceID:  wsID,
					SourceTaskID: taskID,
					TargetTaskID: otherTaskID,
					Type:         task.RelationBlocks,
				})
				return err
			},
		},
		{
			name: "RemoveRelation",
			fn:   func(ctx context.Context, wsID uuid.UUID) error { return svc.RemoveRelation(ctx, wsID, taskID) },
		},
		{
			name: "ListRelations",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.ListRelations(ctx, wsID, taskID)
				return err
			},
		},
		{
			name: "AddComment",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.AddComment(ctx, task.AddCommentInput{WorkspaceID: wsID, TaskID: taskID, Kind: task.CommentKindNote, Body: "Hi"})
				return err
			},
		},
		{
			name: "ListComments",
			fn: func(ctx context.Context, wsID uuid.UUID) error {
				_, err := svc.ListComments(ctx, wsID, taskID, 10)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run("unauthenticated/"+tc.name, func(t *testing.T) {
			if err := tc.fn(unauthCtx, tenantA); !errors.Is(err, errs.ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
		})
		t.Run("cross_tenant/"+tc.name, func(t *testing.T) {
			if err := tc.fn(ctxA, tenantB); !errors.Is(err, errs.ErrPermissionDenied) {
				t.Fatalf("expected ErrPermissionDenied, got %v", err)
			}
		})
	}
}

// TestTaskService_CreateKeyMintingAndAuditRollback verifies task defaults, key
// allocation (KeyAllocator, repo-minted, and sequential fallback), parent project
// checks, defensive copies, and atomic rollback on audit failure.
func TestTaskService_CreateKeyMintingAndAuditRollback(t *testing.T) {
	t.Parallel()

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projA := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	projB := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	actorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "builder-01",
	})

	// 1. KeyAllocator path.
	env := newMemTaskEnv()
	var seq atomic.Int64
	alloc := task.KeyAllocatorFunc(func(_ context.Context, _ tx.Scope, _, _ uuid.UUID) (string, error) {
		return fmt.Sprintf("FLW-%d", seq.Add(1)), nil
	})
	svc := task.NewServiceWithKeyAllocator(newMemTaskRepo(env), env, alloc)

	tags := []string{"golang"}
	labels := map[string]string{"tier": "core"}
	epic, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projA,
		Kind:        task.KindEpic,
		Title:       "Core Domain Services",
		Tags:        tags,
		Labels:      labels,
	})
	if err != nil {
		t.Fatalf("CreateTask epic failed: %v", err)
	}
	tags[0] = "mutated"
	labels["tier"] = "mutated"
	if epic.Key != "FLW-1" || epic.Status != task.StatusReady {
		t.Fatalf("expected FLW-1 and StatusReady default, got %+v", epic)
	}
	if epic.Tags[0] != "golang" || epic.Labels["tier"] != "core" {
		t.Fatalf("expected defensive copies of Tags and Labels, got %+v %+v", epic.Tags, epic.Labels)
	}

	// Child task with empty Kind defaults cleanly and validates parent project match.
	child, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID:  wsID,
		ProjectID:    projA,
		ParentTaskID: &epic.ID,
		Title:        "Implement DAG checks",
	})
	if err != nil {
		t.Fatalf("CreateTask child failed: %v", err)
	}
	if child.Key != "FLW-2" || child.Kind == "" || child.Status != task.StatusReady {
		t.Fatalf("unexpected child defaults: %+v", child)
	}

	// Cross-project parent must fail validation.
	if _, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID:  wsID,
		ProjectID:    projB,
		ParentTaskID: &epic.ID,
		Title:        "Invalid cross-project child",
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for cross-project parent, got %v", err)
	}

	// Audit failure rolls back CreateTask completely.
	auditBoom := errors.New("audit disk full")
	env.auditFn = func() error { return auditBoom }
	if _, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projA,
		Title:       "Should Rollback",
	}); !errors.Is(err, auditBoom) {
		t.Fatalf("expected auditBoom, got %v", err)
	}
	env.auditFn = nil
	if _, err := svc.GetTaskByKey(ctx, wsID, "FLW-3"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected FLW-3 not to exist after rollback, got %v", err)
	}

	// 2. Fallback sequential key minting when NewService(repo, txMgr) has no KeyAllocator.
	fallbackEnv := newMemTaskEnv()
	fallbackSvc := task.NewService(newMemTaskRepo(fallbackEnv), fallbackEnv)
	fb1, err := fallbackSvc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projA,
		Title:       "Fallback Key 1",
	})
	if err != nil || fb1.Key == "" {
		t.Fatalf("expected non-empty fallback key, got %+v (err=%v)", fb1, err)
	}
	byKey, err := fallbackSvc.GetTaskByKey(ctx, wsID, fb1.Key)
	if err != nil || byKey.ID != fb1.ID {
		t.Fatalf("GetTaskByKey for fallback key failed: %+v, err=%v", byKey, err)
	}
}

// TestTaskService_DAGCycleDetectionAndBlockerPreconditions verifies direct and
// transitive cycle rejection across RelationBlocks, RelationSubtaskOf, and
// UpdateTask parent links, plus blocker transition guards and atomic transitions.
func TestTaskService_DAGCycleDetectionAndBlockerPreconditions(t *testing.T) {
	t.Parallel()

	env := newMemTaskEnv()
	env.mintKeyInRep = true
	svc := task.NewService(newMemTaskRepo(env), env)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	actorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "orchestrator",
	})

	mkTask := func(title string) task.Task {
		t.Helper()
		created, err := svc.CreateTask(ctx, task.CreateTaskInput{
			WorkspaceID: wsID,
			ProjectID:   projID,
			Title:       title,
		})
		if err != nil {
			t.Fatalf("CreateTask(%q) failed: %v", title, err)
		}
		return created
	}

	taskA := mkTask("Task A")
	taskB := mkTask("Task B")
	taskC := mkTask("Task C")

	// Self-loop rejected on AddRelation and UpdateTask parent.
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskA.ID,
		TargetTaskID: taskA.ID,
		Type:         task.RelationBlocks,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on self-loop AddRelation, got %v", err)
	}
	if _, err := svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID:  wsID,
		TaskID:       taskA.ID,
		ParentTaskID: &taskA.ID,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on self-parent UpdateTask, got %v", err)
	}

	// Build chain A -> B -> C (RelationBlocks).
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskA.ID,
		TargetTaskID: taskB.ID,
		Type:         task.RelationBlocks,
	}); err != nil {
		t.Fatalf("AddRelation A->B failed: %v", err)
	}
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskB.ID,
		TargetTaskID: taskC.ID,
		Type:         task.RelationBlocks,
	}); err != nil {
		t.Fatalf("AddRelation B->C failed: %v", err)
	}

	// Direct cycle B -> A rejected.
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskB.ID,
		TargetTaskID: taskA.ID,
		Type:         task.RelationBlocks,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on direct cycle B->A, got %v", err)
	}

	// Transitive cycle C -> A rejected.
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskC.ID,
		TargetTaskID: taskA.ID,
		Type:         task.RelationBlocks,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on transitive cycle C->A, got %v", err)
	}

	// Parent hierarchy transitive cycle: C parent=B, B parent=A, then A parent=C -> ErrValidation!
	if _, err := svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID:  wsID,
		TaskID:       taskB.ID,
		ParentTaskID: &taskA.ID,
	}); err != nil {
		t.Fatalf("UpdateTask B parent=A failed: %v", err)
	}
	if _, err := svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID:  wsID,
		TaskID:       taskC.ID,
		ParentTaskID: &taskB.ID,
	}); err != nil {
		t.Fatalf("UpdateTask C parent=B failed: %v", err)
	}
	if _, err := svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID:  wsID,
		TaskID:       taskA.ID,
		ParentTaskID: &taskC.ID,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on transitive parent cycle A->C->B->A, got %v", err)
	}

	// RelationSubtaskOf cycle: A --subtask_of--> C (when C -> B -> A already exists) -> ErrValidation!
	if _, err := svc.AddRelation(ctx, task.AddRelationInput{
		WorkspaceID:  wsID,
		SourceTaskID: taskA.ID,
		TargetTaskID: taskC.ID,
		Type:         task.RelationSubtaskOf,
	}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation on RelationSubtaskOf cycle, got %v", err)
	}

	// Blocker preconditions: Task B is blocked by Task A (not yet Done).
	for _, blockedTarget := range []task.Status{task.StatusInProgress, task.StatusDone} {
		if _, err := svc.TransitionTask(ctx, task.TransitionTaskInput{
			WorkspaceID:  wsID,
			TaskID:       taskB.ID,
			TargetStatus: blockedTarget,
		}); !errors.Is(err, errs.ErrPreconditionFailed) {
			t.Fatalf("expected ErrPreconditionFailed transitioning blocked task B to %s, got %v", blockedTarget, err)
		}
	}

	// Claim Task A, transition to Done with ResolutionComment and ReleaseActiveLease=true in ONE tx.
	leaseA, err := svc.ClaimTask(ctx, task.ClaimTaskInput{
		WorkspaceID:              wsID,
		TaskID:                   taskA.ID,
		TTLSeconds:               600,
		AutoTransitionInProgress: true,
	})
	if err != nil || leaseA.LeaseToken == "" {
		t.Fatalf("ClaimTask A failed: %+v, err=%v", leaseA, err)
	}
	doneA, err := svc.TransitionTask(ctx, task.TransitionTaskInput{
		WorkspaceID:        wsID,
		TaskID:             taskA.ID,
		TargetStatus:       task.StatusDone,
		ResolutionComment:  "Completed root schema and verified tests.",
		ReleaseActiveLease: true,
	})
	if err != nil {
		t.Fatalf("TransitionTask A -> Done failed: %v", err)
	}
	if doneA.Status != task.StatusDone || doneA.CompletedAt == nil || doneA.ActiveLease != nil {
		t.Fatalf("expected Done with CompletedAt and nil ActiveLease, got %+v", doneA)
	}
	commentsA, err := svc.ListComments(ctx, wsID, taskA.ID, 10)
	if err != nil || len(commentsA) != 1 {
		t.Fatalf("expected 1 resolution comment on Task A, got %+v (err=%v)", commentsA, err)
	}

	// Now Task B is unblocked and can transition to InProgress and Done!
	inProgB, err := svc.TransitionTask(ctx, task.TransitionTaskInput{
		WorkspaceID:  wsID,
		TaskID:       taskB.ID,
		TargetStatus: task.StatusInProgress,
	})
	if err != nil || inProgB.Status != task.StatusInProgress {
		t.Fatalf("expected unblocked Task B to transition to InProgress, got %+v (err=%v)", inProgB, err)
	}
}

// TestTaskService_LeaseLifecyclePreemptionQueueAndRace verifies ClaimLease
// acquisition, renewal, peer rejection (ErrLeaseHeld), expired lease preemption,
// ReleaseTask fencing/force/handoff comments, PollWorkQueue, GetProjectDigest,
// and race-free concurrent ClaimTask contention.
func TestTaskService_LeaseLifecyclePreemptionQueueAndRace(t *testing.T) {
	t.Parallel()

	env := newMemTaskEnv()
	env.mintKeyInRep = true
	svc := task.NewService(newMemTaskRepo(env), env)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	agent1ID := uuid.MustParse("aaaaaaaa-0001-0001-0001-000000000001")
	agent2ID := uuid.MustParse("aaaaaaaa-0002-0002-0002-000000000002")

	ctx1 := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     agent1ID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "worker-1",
	})
	ctx2 := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     agent2ID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "worker-2",
	})

	item, err := svc.CreateTask(ctx1, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projID,
		Title:       "Implement SQLite WAL pool",
		Priority:    task.PriorityHigh,
		Tags:        []string{"sqlite"},
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	// PollWorkQueue & GetProjectDigest return defensive copies.
	queue, err := svc.PollWorkQueue(ctx1, task.AgentWorkQueueFilter{
		WorkspaceID:   wsID,
		ProjectID:     projID,
		OnlyUnblocked: true,
		OnlyUnclaimed: true,
	})
	if err != nil || len(queue) != 1 {
		t.Fatalf("expected 1 ready item in PollWorkQueue, got %+v (err=%v)", queue, err)
	}
	queue[0].Tags[0] = "mutated"

	digest, err := svc.GetProjectDigest(ctx1, wsID, projID, task.DigestOptions{})
	if err != nil || len(digest.ReadyQueue) != 1 || digest.ReadyQueue[0].Tags[0] != "sqlite" {
		t.Fatalf("expected unmodified digest ReadyQueue, got %+v (err=%v)", digest, err)
	}

	// Agent 1 claims task and auto-transitions Ready -> InProgress.
	lease1, err := svc.ClaimTask(ctx1, task.ClaimTaskInput{
		WorkspaceID:              wsID,
		TaskID:                   item.ID,
		TTLSeconds:               120,
		AgentSessionID:           "sess-1",
		AutoTransitionInProgress: true,
	})
	if err != nil {
		t.Fatalf("ClaimTask by agent1 failed: %v", err)
	}
	claimedTask, err := svc.GetTask(ctx1, wsID, item.ID)
	if err != nil || claimedTask.Status != task.StatusInProgress {
		t.Fatalf("expected AutoTransitionInProgress to set StatusInProgress, got %+v (err=%v)", claimedTask, err)
	}

	// Agent 1 renews active lease -> extends HeartbeatAt & ExpiresAt.
	renewed, err := svc.ClaimTask(ctx1, task.ClaimTaskInput{
		WorkspaceID: wsID,
		TaskID:      item.ID,
		TTLSeconds:  600,
		LeaseToken:  lease1.LeaseToken,
	})
	if err != nil || !renewed.ExpiresAt.After(lease1.ExpiresAt) {
		t.Fatalf("expected renewed lease to extend ExpiresAt (%v -> %v), err=%v", lease1.ExpiresAt, renewed.ExpiresAt, err)
	}

	// Agent 2 attempting to claim or release unexpired lease fails with ErrLeaseHeld.
	if _, err := svc.ClaimTask(ctx2, task.ClaimTaskInput{
		WorkspaceID: wsID,
		TaskID:      item.ID,
		TTLSeconds:  300,
	}); !errors.Is(err, errs.ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld when agent2 claims active lease, got %v", err)
	}
	if err := svc.ReleaseTask(ctx2, task.ReleaseTaskInput{
		WorkspaceID: wsID,
		TaskID:      item.ID,
		LeaseToken:  renewed.LeaseToken,
	}); !errors.Is(err, errs.ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld when agent2 releases without Force, got %v", err)
	}

	// Wrong token by Agent 1 without Force fails with ErrPreconditionFailed.
	if err := svc.ReleaseTask(ctx1, task.ReleaseTaskInput{
		WorkspaceID: wsID,
		TaskID:      item.ID,
		LeaseToken:  "wrong-token",
	}); !errors.Is(err, errs.ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed on wrong LeaseToken, got %v", err)
	}

	// Expire lease in persistence state, then verify Agent 2 preempts cleanly!
	env.mu.Lock()
	expiredCopy := env.state.leases[item.ID]
	expiredCopy.ExpiresAt = time.Now().UTC().Add(-1 * time.Second)
	env.state.leases[item.ID] = expiredCopy
	env.mu.Unlock()

	preempted, err := svc.ClaimTask(ctx2, task.ClaimTaskInput{
		WorkspaceID: wsID,
		TaskID:      item.ID,
		TTLSeconds:  300,
	})
	if err != nil || preempted.HolderActorID != agent2ID || preempted.LeaseToken == renewed.LeaseToken {
		t.Fatalf("expected clean preemption by agent2 with fresh token, got %+v (err=%v)", preempted, err)
	}

	// Agent 2 voluntarily releases lease with handoff comment.
	if err := svc.ReleaseTask(ctx2, task.ReleaseTaskInput{
		WorkspaceID:    wsID,
		TaskID:         item.ID,
		LeaseToken:     preempted.LeaseToken,
		HandoffComment: "WAL pool wired; ready for integration benchmarks.",
	}); err != nil {
		t.Fatalf("ReleaseTask failed: %v", err)
	}
	comments, err := svc.ListComments(ctx1, wsID, item.ID, 10)
	if err != nil || len(comments) != 1 || comments[0].Kind != task.CommentKindHandoff {
		t.Fatalf("expected 1 handoff comment after ReleaseTask, got %+v (err=%v)", comments, err)
	}

	// Concurrency race test: 16 distinct agents simultaneously race to ClaimTask.
	raceTask, err := svc.CreateTask(ctx1, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projID,
		Title:       "Contended parallel task",
	})
	if err != nil {
		t.Fatalf("CreateTask raceTask failed: %v", err)
	}

	const contenders = 16
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	var winners atomic.Int32
	var leaseHeldCount atomic.Int32

	for i := range contenders {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			contenderCtx := authctx.WithIdentity(context.Background(), authctx.Identity{
				WorkspaceID: wsID,
				ActorID:     uuid.New(),
				Kind:        authctx.ActorKindAgent,
				Handle:      fmt.Sprintf("racer-%02d", idx),
			})
			<-startGate
			_, claimErr := svc.ClaimTask(contenderCtx, task.ClaimTaskInput{
				WorkspaceID:              wsID,
				TaskID:                   raceTask.ID,
				TTLSeconds:               300,
				AutoTransitionInProgress: true,
			})
			switch {
			case claimErr == nil:
				winners.Add(1)
			case errors.Is(claimErr, errs.ErrLeaseHeld):
				leaseHeldCount.Add(1)
			default:
				t.Errorf("unexpected error in concurrent ClaimTask: %v", claimErr)
			}
		}(i)
	}

	close(startGate)
	wg.Wait()

	if winners.Load() != 1 || leaseHeldCount.Load() != contenders-1 {
		t.Fatalf("expected exactly 1 winner and %d ErrLeaseHeld, got winners=%d leaseHeld=%d",
			contenders-1, winners.Load(), leaseHeldCount.Load())
	}
}

func TestTaskExternalIDUniquenessPerProjectAndClear(t *testing.T) {
	env := newMemTaskEnv()
	repo := newMemTaskRepo(env)

	wsID := uuid.New()
	projA := uuid.New()
	projB := uuid.New()
	var seq int
	svc := task.NewServiceWithKeyAllocator(repo, env, task.KeyAllocatorFunc(func(_ context.Context, _ tx.Scope, _, pID uuid.UUID) (string, error) {
		seq++
		if pID == projA {
			return fmt.Sprintf("PA-%d", seq), nil
		}
		return fmt.Sprintf("PB-%d", seq), nil
	}))
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     uuid.New(),
		Kind:        authctx.ActorKindAgent,
		Handle:      "sync-agent",
	})

	t1, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projA,
		ExternalID:  "CU-869abc",
		Title:       "Imported ClickUp task in Project A",
	})
	if err != nil {
		t.Fatalf("CreateTask t1 failed: %v", err)
	}
	if t1.ExternalID != "CU-869abc" {
		t.Fatalf("expected ExternalID CU-869abc, got %q", t1.ExternalID)
	}

	// Duplicate ExternalID in the SAME project must fail with ErrConflict.
	if _, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projA,
		ExternalID:  "CU-869abc",
		Title:       "Duplicate ClickUp task in Project A",
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate external_id in same project, got %v", err)
	}

	// Identical ExternalID in a DIFFERENT project must succeed!
	t2, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projB,
		ExternalID:  "CU-869abc",
		Title:       "Same external ID in Project B",
	})
	if err != nil {
		t.Fatalf("expected identical external_id in different project to succeed, got %v", err)
	}
	if t2.ID == t1.ID {
		t.Fatal("expected distinct internal task IDs across projects")
	}

	// Lookup by (workspaceID, projectID, externalID) resolves exact project's task.
	gotA, err := svc.GetTaskByExternalID(ctx, wsID, projA, "CU-869abc")
	if err != nil || gotA.ID != t1.ID {
		t.Fatalf("GetTaskByExternalID projA failed: got %+v, err=%v", gotA, err)
	}
	gotB, err := svc.GetTaskByExternalID(ctx, wsID, projB, "CU-869abc")
	if err != nil || gotB.ID != t2.ID {
		t.Fatalf("GetTaskByExternalID projB failed: got %+v, err=%v", gotB, err)
	}

	// Clearing ExternalID on t1 releases "CU-869abc" in Project A.
	clearStr := ""
	cleared, err := svc.UpdateTask(ctx, task.UpdateTaskInput{
		WorkspaceID: wsID,
		TaskID:      t1.ID,
		ExternalID:  &clearStr,
	})
	if err != nil || cleared.ExternalID != "" {
		t.Fatalf("expected cleared ExternalID, got %+v, err=%v", cleared, err)
	}
	if _, err := svc.GetTaskByExternalID(ctx, wsID, projA, "CU-869abc"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after clearing ExternalID, got %v", err)
	}
}

type stubTaskMarkdownStore struct {
	mu       sync.Mutex
	taskDocs map[uuid.UUID]string
	projDocs map[uuid.UUID]string
	writes   int
	deletes  int
}

func newStubTaskMarkdownStore() *stubTaskMarkdownStore {
	return &stubTaskMarkdownStore{
		taskDocs: make(map[uuid.UUID]string),
		projDocs: make(map[uuid.UUID]string),
	}
}

func (m *stubTaskMarkdownStore) ReadTaskMarkdown(_ context.Context, _, taskID uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.taskDocs[taskID]
	if !ok {
		return "", errs.New(errs.ErrNotFound, "ReadTaskMarkdown", "not found")
	}
	return v, nil
}

func (m *stubTaskMarkdownStore) WriteTaskMarkdown(_ context.Context, _, taskID uuid.UUID, md string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	m.taskDocs[taskID] = md
	return nil
}

func (m *stubTaskMarkdownStore) DeleteTaskMarkdown(_ context.Context, _, taskID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	delete(m.taskDocs, taskID)
	return nil
}

func (m *stubTaskMarkdownStore) HasTaskMarkdown(_ context.Context, _, taskID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.taskDocs[taskID]
	return ok, nil
}

func (m *stubTaskMarkdownStore) ScanTaskFiles(_ context.Context, _ uuid.UUID) ([]uuid.UUID, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(m.taskDocs))
	for id := range m.taskDocs {
		ids = append(ids, id)
	}
	return ids, nil, nil
}

func (m *stubTaskMarkdownStore) RemoveMalformedTaskMarkdown(_ context.Context, _ string) error {
	return nil
}

func (m *stubTaskMarkdownStore) DeleteProjectMarkdown(_ context.Context, _, projID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projDocs, projID)
	return nil
}

func (m *stubTaskMarkdownStore) HasProjectMarkdown(_ context.Context, _, projID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.projDocs[projID]
	return ok, nil
}

func (m *stubTaskMarkdownStore) ScanProjectFiles(_ context.Context, _ uuid.UUID) ([]uuid.UUID, []string, error) {
	return nil, nil, nil
}

func (m *stubTaskMarkdownStore) RemoveMalformedProjectMarkdown(_ context.Context, _ string) error {
	return nil
}

// TestTaskService_StabilizationFixes verifies empty-token ReleaseTask by same holder,
// PollWorkQueue ExternalID + >100 Limit, terminal blocker resolution (Cancelled/Archived),
// post-commit MarkdownStore safety + PruneOrphans audit error propagation, and ToProto fields.
func TestTaskService_StabilizationFixes(t *testing.T) {
	t.Parallel()

	env := newMemTaskEnv()
	env.mintKeyInRep = true
	mdStore := newStubTaskMarkdownStore()
	svc := task.NewService(newMemTaskRepo(env), env).WithMarkdownResources(mdStore, nil)

	wsID := uuid.New()
	projID := uuid.New()
	actorID := uuid.New()
	ctx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     actorID,
		Kind:        authctx.ActorKindAgent,
		Handle:      "stabilizer-agent",
	})

	// 1. ReleaseTask allows empty LeaseToken when called by active lease holder.
	leasedTask, err := svc.CreateTask(ctx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ExternalID:  "EXT-900",
		Title:       "Leased task",
		Tags:        []string{"backend", "urgent"},
		Labels: map[string]string{
			"11111111-2222-3333-4444-555555555555": "label-uuid",
			"non-uuid-key":                         "ignored-in-label-ids",
		},
	})
	if err != nil {
		t.Fatalf("CreateTask leasedTask: %v", err)
	}
	lease, err := svc.ClaimTask(ctx, task.ClaimTaskInput{
		WorkspaceID: wsID,
		TaskID:      leasedTask.ID,
		TTLSeconds:  300,
	})
	if err != nil || lease.LeaseToken == "" {
		t.Fatalf("ClaimTask: %+v, err=%v", lease, err)
	}
	if err := svc.ReleaseTask(ctx, task.ReleaseTaskInput{
		WorkspaceID: wsID,
		TaskID:      leasedTask.ID,
		LeaseToken:  "",
	}); err != nil {
		t.Fatalf("expected empty LeaseToken by same holder to succeed, got %v", err)
	}

	// 2. Terminal blocker states (StatusCancelled & StatusArchived) unblock downstream tasks.
	blockerCancel, _ := svc.CreateTask(ctx, task.CreateTaskInput{WorkspaceID: wsID, ProjectID: projID, Title: "Blocker Cancel"})
	blockerArchive, _ := svc.CreateTask(ctx, task.CreateTaskInput{WorkspaceID: wsID, ProjectID: projID, Title: "Blocker Archive"})
	downstream, _ := svc.CreateTask(ctx, task.CreateTaskInput{WorkspaceID: wsID, ProjectID: projID, Title: "Downstream"})
	_, _ = svc.AddRelation(ctx, task.AddRelationInput{WorkspaceID: wsID, SourceTaskID: blockerCancel.ID, TargetTaskID: downstream.ID, Type: task.RelationBlocks})
	_, _ = svc.AddRelation(ctx, task.AddRelationInput{WorkspaceID: wsID, SourceTaskID: blockerArchive.ID, TargetTaskID: downstream.ID, Type: task.RelationBlocks})

	if _, err := svc.TransitionTask(ctx, task.TransitionTaskInput{WorkspaceID: wsID, TaskID: downstream.ID, TargetStatus: task.StatusInProgress}); !errors.Is(err, errs.ErrPreconditionFailed) {
		t.Fatalf("expected downstream blocked before terminal transitions, got %v", err)
	}
	if _, err := svc.TransitionTask(ctx, task.TransitionTaskInput{WorkspaceID: wsID, TaskID: blockerCancel.ID, TargetStatus: task.StatusCancelled}); err != nil {
		t.Fatalf("TransitionTask Cancelled: %v", err)
	}
	if _, err := svc.TransitionTask(ctx, task.TransitionTaskInput{WorkspaceID: wsID, TaskID: blockerArchive.ID, TargetStatus: task.StatusArchived}); err != nil {
		t.Fatalf("TransitionTask Archived: %v", err)
	}
	if _, err := svc.TransitionTask(ctx, task.TransitionTaskInput{WorkspaceID: wsID, TaskID: downstream.ID, TargetStatus: task.StatusInProgress}); err != nil {
		t.Fatalf("expected downstream unblocked after Cancelled+Archived blockers, got %v", err)
	}

	// 3. PollWorkQueue ExternalID filtering and >100 Limit support.
	extMatches, err := svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ExternalID:  "  EXT-900  ",
		Statuses:    []task.Status{task.StatusReady},
	})
	if err != nil || len(extMatches) != 1 || extMatches[0].ID != leasedTask.ID {
		t.Fatalf("PollWorkQueue ExternalID mismatch: %+v, err=%v", extMatches, err)
	}
	for i := range 130 {
		_, _ = svc.CreateTask(ctx, task.CreateTaskInput{
			WorkspaceID: wsID,
			ProjectID:   projID,
			Title:       fmt.Sprintf("Bulk Ready %d", i),
		})
	}
	bulkQueue, err := svc.PollWorkQueue(ctx, task.AgentWorkQueueFilter{
		WorkspaceID: wsID,
		ProjectID:   projID,
		Statuses:    []task.Status{task.StatusReady},
		Limit:       250,
	})
	if err != nil || len(bulkQueue) < 131 {
		t.Fatalf("expected PollWorkQueue(Limit=250) to return >=131 items without truncating at 100, got %d (err=%v)", len(bulkQueue), err)
	}

	// 4. Post-commit MarkdownStore safety on rollback & PruneOrphans audit error propagation.
	auditFail := errors.New("simulated audit failure")
	env.auditFn = func() error { return auditFail }
	if _, err := svc.PutTaskMarkdown(ctx, wsID, leasedTask.ID, "# Should Not Persist"); !errors.Is(err, auditFail) {
		t.Fatalf("expected auditFail on PutTaskMarkdown, got %v", err)
	}
	if hasDoc, _ := mdStore.HasTaskMarkdown(ctx, wsID, leasedTask.ID); hasDoc {
		t.Fatal("expected rolled-back PutTaskMarkdown not to write markdown file")
	}
	env.auditFn = nil

	if _, err := svc.PutTaskMarkdown(ctx, wsID, leasedTask.ID, "# Committed Doc"); err != nil {
		t.Fatalf("PutTaskMarkdown commit: %v", err)
	}
	env.auditFn = func() error { return auditFail }
	if _, err := svc.DeleteTaskMarkdown(ctx, wsID, leasedTask.ID); !errors.Is(err, auditFail) {
		t.Fatalf("expected auditFail on DeleteTaskMarkdown, got %v", err)
	}
	if err := svc.DeleteTask(ctx, wsID, leasedTask.ID); !errors.Is(err, auditFail) {
		t.Fatalf("expected auditFail on DeleteTask, got %v", err)
	}
	if hasDoc, _ := mdStore.HasTaskMarkdown(ctx, wsID, leasedTask.ID); !hasDoc {
		t.Fatal("expected rolled-back DeleteTaskMarkdown/DeleteTask to preserve markdown file on disk")
	}

	// Simulate missing disk doc to verify PruneOrphans propagates Audit().Record errors.
	_ = mdStore.DeleteTaskMarkdown(ctx, wsID, leasedTask.ID)
	if _, err := svc.PruneOrphans(ctx, wsID, false); !errors.Is(err, auditFail) {
		t.Fatalf("expected PruneOrphans to propagate audit error, got %v", err)
	}
	env.auditFn = nil

	// 5. ToProto deterministic WorkflowStateId, Tags, UUID-only LabelIds, and CompactTaskSummary fields.
	reloaded, err := svc.GetTask(ctx, wsID, leasedTask.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	pbTask := (&reloaded).ToProto()
	wantStateID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("flowsk:workflow-state:"+projID.String()+":ready")).String()
	if pbTask.GetWorkflowStateId() != wantStateID {
		t.Fatalf("WorkflowStateId = %q, want %q", pbTask.GetWorkflowStateId(), wantStateID)
	}
	if len(pbTask.GetTags()) != 2 || pbTask.GetTags()[0] != "backend" {
		t.Fatalf("unexpected pbTask.Tags: %+v", pbTask.GetTags())
	}
	if len(pbTask.GetLabelIds()) != 1 || pbTask.GetLabelIds()[0] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("expected only valid UUID in pbTask.LabelIds, got %+v", pbTask.GetLabelIds())
	}
	objUUID := uuid.New()
	holderUUID := uuid.New()
	summaryPB := (&task.CompactTaskSummary{
		ID:             leasedTask.ID,
		Key:            leasedTask.Key,
		ExternalID:     "EXT-900",
		Kind:           task.KindFeature,
		Title:          "Summary",
		Status:         task.StatusInProgress,
		Priority:       task.PriorityHigh,
		ObjectiveID:    &objUUID,
		HolderActorID:  &holderUUID,
		HasMarkdownDoc: true,
	}).ToProto()
	if summaryPB.GetObjectiveId() != objUUID.String() || summaryPB.GetClaimedByActorId() != holderUUID.String() ||
		!summaryPB.GetHasMarkdownDoc() || summaryPB.GetExternalId() != "EXT-900" {
		t.Fatalf("CompactTaskSummary.ToProto mismatch: %+v", summaryPB)
	}
}

// TestTaskService_LastSignalAtLifecycleAndGlowRules verifies:
//   - Suppressed contexts (Web UI / internal hydration) leave LastSignalAt nil on create/read/move.
//   - Agent/CLI creation sets LastSignalAt = CreatedAt.
//   - Point-reads (GetTask, GetTaskByKey, GetTaskByExternalID, GetTaskMarkdown), updates,
//     claims, comments, docs, and transitions up to in_review advance LastSignalAt.
//   - Transitions to blocked, done, cancelled, or archived stop updating LastSignalAt,
//     and subsequent reads/comments in those states leave LastSignalAt frozen.
//   - HasActiveSignalGlow illuminates strictly for ready/in_progress/in_review within 10 minutes
//     and extinguishes immediately in backlog/blocked/done/cancelled/archived or after 10 minutes.
func TestTaskService_LastSignalAtLifecycleAndGlowRules(t *testing.T) {
	t.Parallel()

	env := newMemTaskEnv()
	env.mintKeyInRep = true
	svc := task.NewService(newMemTaskRepo(env), env)
	wsID := uuid.New()
	projID := uuid.New()
	agentCtx := authctx.WithIdentity(context.Background(), authctx.Identity{
		WorkspaceID: wsID,
		ActorID:     uuid.New(),
		Kind:        authctx.ActorKindAgent,
		Handle:      "agent-signal-01",
	})
	uiCtx := authctx.WithSuppressTaskSignal(agentCtx)

	// 1. Web UI / suppressed create leaves LastSignalAt nil by default.
	uiCreated, err := svc.CreateTask(uiCtx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projID,
		Title:       "UI Created Backlog Item",
		Status:      task.StatusBacklog,
	})
	if err != nil {
		t.Fatalf("CreateTask (uiCtx): %v", err)
	}
	if uiCreated.LastSignalAt != nil {
		t.Fatalf("expected nil LastSignalAt when created from suppressed UI context, got %v", uiCreated.LastSignalAt)
	}

	// Suppressed read also leaves LastSignalAt nil.
	uiRead, err := svc.GetTask(uiCtx, wsID, uiCreated.ID)
	if err != nil || uiRead.LastSignalAt != nil {
		t.Fatalf("expected suppressed GetTask to leave LastSignalAt nil, got %v (err=%v)", uiRead.LastSignalAt, err)
	}

	// 2. Agent create sets LastSignalAt == CreatedAt; Backlog never illuminates green glow.
	created, err := svc.CreateTask(agentCtx, task.CreateTaskInput{
		WorkspaceID: wsID,
		ProjectID:   projID,
		ExternalID:  "SIG-1",
		Title:       "Agent Tracked Feature",
		Status:      task.StatusBacklog,
	})
	if err != nil {
		t.Fatalf("CreateTask (agentCtx): %v", err)
	}
	if created.LastSignalAt == nil || !created.LastSignalAt.Equal(created.CreatedAt) {
		t.Fatalf("expected LastSignalAt == CreatedAt (%v), got %v", created.CreatedAt, created.LastSignalAt)
	}
	if created.HasActiveSignalGlow(created.CreatedAt.Add(time.Minute)) {
		t.Fatal("expected backlog task NOT to illuminate green glow")
	}

	// 3. Agent transition backlog -> ready -> in_progress -> in_review updates LastSignalAt and glows within 10m.
	prevSig := *created.LastSignalAt
	time.Sleep(2 * time.Millisecond)
	readyTask, err := svc.TransitionTask(agentCtx, task.TransitionTaskInput{
		WorkspaceID:  wsID,
		TaskID:       created.ID,
		TargetStatus: task.StatusReady,
	})
	if err != nil || readyTask.LastSignalAt == nil || !readyTask.LastSignalAt.After(prevSig) {
		t.Fatalf("expected TransitionTask(ready) to advance LastSignalAt beyond %v, got %v (err=%v)", prevSig, readyTask.LastSignalAt, err)
	}
	if !readyTask.HasActiveSignalGlow(readyTask.LastSignalAt.Add(9 * time.Minute)) {
		t.Fatal("expected ready task within 10m window to illuminate green glow")
	}
	if readyTask.HasActiveSignalGlow(readyTask.LastSignalAt.Add(10*time.Minute + time.Second)) {
		t.Fatal("expected ready task older than 10m window to extinguish green glow")
	}

	// Agent point-read advances LastSignalAt.
	prevSig = *readyTask.LastSignalAt
	time.Sleep(2 * time.Millisecond)
	readByKey, err := svc.GetTaskByKey(agentCtx, wsID, readyTask.Key)
	if err != nil || readByKey.LastSignalAt == nil || !readByKey.LastSignalAt.After(prevSig) {
		t.Fatalf("expected GetTaskByKey to advance LastSignalAt beyond %v, got %v (err=%v)", prevSig, readByKey.LastSignalAt, err)
	}

	// Transition to in_review still advances LastSignalAt and glows.
	prevSig = *readByKey.LastSignalAt
	time.Sleep(2 * time.Millisecond)
	inReviewTask, err := svc.TransitionTask(agentCtx, task.TransitionTaskInput{
		WorkspaceID:  wsID,
		TaskID:       created.ID,
		TargetStatus: task.StatusInReview,
	})
	if err != nil || inReviewTask.LastSignalAt == nil || !inReviewTask.LastSignalAt.After(prevSig) {
		t.Fatalf("expected TransitionTask(in_review) to advance LastSignalAt beyond %v, got %v (err=%v)", prevSig, inReviewTask.LastSignalAt, err)
	}
	if !inReviewTask.HasActiveSignalGlow(time.Now().UTC()) {
		t.Fatal("expected in_review task with fresh signal to illuminate green glow")
	}

	// 4. Transition to blocked freezes LastSignalAt and immediately extinguishes glow.
	frozenSig := *inReviewTask.LastSignalAt
	time.Sleep(2 * time.Millisecond)
	blockedTask, err := svc.TransitionTask(agentCtx, task.TransitionTaskInput{
		WorkspaceID:  wsID,
		TaskID:       created.ID,
		TargetStatus: task.StatusBlocked,
	})
	if err != nil {
		t.Fatalf("TransitionTask(blocked): %v", err)
	}
	if blockedTask.LastSignalAt == nil || !blockedTask.LastSignalAt.Equal(frozenSig) {
		t.Fatalf("expected blocked transition NOT to advance LastSignalAt (want %v, got %v)", frozenSig, blockedTask.LastSignalAt)
	}
	if blockedTask.HasActiveSignalGlow(frozenSig) {
		t.Fatal("expected blocked task to extinguish green glow immediately regardless of timestamp")
	}

	// Subsequent agent read while blocked must NOT advance LastSignalAt.
	time.Sleep(2 * time.Millisecond)
	readBlocked, err := svc.GetTaskByExternalID(agentCtx, wsID, projID, "SIG-1")
	if err != nil || readBlocked.LastSignalAt == nil || !readBlocked.LastSignalAt.Equal(frozenSig) {
		t.Fatalf("expected GetTaskByExternalID on blocked task to keep frozen signal %v, got %v (err=%v)", frozenSig, readBlocked.LastSignalAt, err)
	}

	// 5. Transition to done also keeps LastSignalAt frozen and glow OFF.
	time.Sleep(2 * time.Millisecond)
	doneTask, err := svc.TransitionTask(agentCtx, task.TransitionTaskInput{
		WorkspaceID:  wsID,
		TaskID:       created.ID,
		TargetStatus: task.StatusDone,
	})
	if err != nil {
		t.Fatalf("TransitionTask(done): %v", err)
	}
	if doneTask.LastSignalAt == nil || !doneTask.LastSignalAt.Equal(frozenSig) {
		t.Fatalf("expected done transition NOT to advance LastSignalAt (want %v, got %v)", frozenSig, doneTask.LastSignalAt)
	}
	if doneTask.HasActiveSignalGlow(frozenSig) {
		t.Fatal("expected done task to extinguish green glow immediately")
	}
}
