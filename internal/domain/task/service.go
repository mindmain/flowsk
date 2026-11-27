// File service.go implements the pure domain workflow service for hierarchical
// Tasks, DAG cycle prevention, blocker preconditions, exclusive time-bounded
// agent ClaimLeases, and token-compact work queues and digests.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

const (
	// DefaultLeaseTTLSeconds is the fallback agent claim lease duration (15 minutes)
	// used when ClaimTaskInput.TTLSeconds is <= 0.
	DefaultLeaseTTLSeconds int64 = 900

	// MaxLeaseTTLSeconds bounds individual task claim leases to 24 hours.
	MaxLeaseTTLSeconds int64 = 86400

	defaultWorkQueueLimit   int32 = 25
	maxWorkQueueLimit       int32 = 1000
	defaultDigestReadyMax   int32 = 15
	defaultDigestBlockedMax int32 = 10
	maxDigestItemsLimit     int32 = 100
	defaultCommentsLimit    int32 = 50
	maxCommentsLimit        int32 = 200

	maxTaskTitleRunes       = 300
	maxTaskExternalIDRunes  = 255
	maxTaskDescriptionRunes = 32000
	maxCommentBodyRunes     = 16000
	maxTaskMarkdownBytes    = 262144
	fallbackTaskKeyPrefix   = "TSK"
)

// Service defines the pure domain workflow, dependency graph, exclusive agent
// lease claiming, and compact digest operations for Tasks.
type Service interface {
	// CreateTask allocates the next project task key, validates hierarchy/objective
	// references, and persists a new Task inside an atomic audited transaction.
	CreateTask(ctx context.Context, in CreateTaskInput) (Task, error)

	// GetTask fetches a complete Task aggregate by UUID within workspaceID.
	GetTask(ctx context.Context, workspaceID, taskID uuid.UUID) (Task, error)

	// GetTaskByKey fetches a complete Task aggregate by short key (e.g., "FLW-104")
	// within workspaceID.
	GetTaskByKey(ctx context.Context, workspaceID uuid.UUID, key string) (Task, error)

	// GetTaskByExternalID fetches a complete Task aggregate by its project-scoped
	// external tracker ID (e.g., "CU-869abc") within (workspaceID, projectID).
	GetTaskByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (Task, error)

	// UpdateTask applies partial updates to a Task inside an atomic audited transaction,
	// respecting active ClaimLease ownership and optional optimistic locking.
	UpdateTask(ctx context.Context, in UpdateTaskInput) (Task, error)

	// TransitionTask validates state-machine preconditions (such as unresolved blockers
	// and lease ownership), updates task Status, optionally appends a resolution comment,
	// and optionally releases the active lease in one atomic audited transaction.
	TransitionTask(ctx context.Context, in TransitionTaskInput) (Task, error)

	// DeleteTask soft-deletes a Task and cleans up its active lease in an atomic audited transaction.
	DeleteTask(ctx context.Context, workspaceID, taskID uuid.UUID) error

	// ClaimTask acquires or renews an exclusive time-bounded ClaimLease on a Task for
	// the authenticated actor inside an atomic audited transaction.
	// Returns errs.ErrLeaseHeld if an unexpired lease is currently owned by another actor.
	ClaimTask(ctx context.Context, in ClaimTaskInput) (ClaimLease, error)

	// ReleaseTask voluntarily releases (or with Force=true administratively revokes)
	// an active ClaimLease and optionally records a handoff comment atomically.
	ReleaseTask(ctx context.Context, in ReleaseTaskInput) error

	// PollWorkQueue returns token-compact task summaries matching filter for autonomous
	// agent harnesses or CLI dashboards.
	PollWorkQueue(ctx context.Context, filter AgentWorkQueueFilter) ([]CompactTaskSummary, error)

	// GetProjectDigest returns a token-budgeted situational awareness digest of a Project.
	GetProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, opts DigestOptions) (ProjectDigest, error)

	// AddRelation validates DAG cycle invariants and persists a directed TaskRelation
	// inside an atomic audited transaction.
	AddRelation(ctx context.Context, in AddRelationInput) (TaskRelation, error)

	// RemoveRelation deletes a directed TaskRelation inside an atomic audited transaction.
	RemoveRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error

	// ListRelations returns defensive copies of all inbound and outbound relations for taskID.
	ListRelations(ctx context.Context, workspaceID, taskID uuid.UUID) ([]TaskRelation, error)

	// AddComment appends a structured TaskComment inside an atomic audited transaction.
	AddComment(ctx context.Context, in AddCommentInput) (TaskComment, error)

	// ListComments returns chronologically ordered TaskComments for taskID.
	ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]TaskComment, error)

	// GetTaskMarkdown retrieves the optional extended Markdown document (`<task_id>.md`)
	// stored for (workspaceID, taskID).
	GetTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (markdown string, hasDoc bool, err error)

	// PutTaskMarkdown writes the extended Markdown document (`<task_id>.md`) and marks
	// HasMarkdownDoc = true inside an atomic audited transaction.
	PutTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID, markdown string) (Task, error)

	// DeleteTaskMarkdown deletes the extended Markdown document (`<task_id>.md`) and marks
	// HasMarkdownDoc = false inside an atomic audited transaction.
	DeleteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (Task, error)

	// PurgeArchivedTasks permanently deletes all tasks in StatusArchived across workspaceID
	// and deletes their associated `.md` files inside an atomic audited transaction.
	PurgeArchivedTasks(ctx context.Context, workspaceID uuid.UUID) (PurgeArchivedResult, error)

	// InspectOrphans scans workspaceID for `.md` files without an active DB entity and
	// active DB entities marked HasMarkdownDoc whose `.md` file is missing on disk.
	InspectOrphans(ctx context.Context, workspaceID uuid.UUID) (OrphanResourcesReport, error)

	// PruneOrphans reconciles orphan `.md` files and broken DB HasMarkdownDoc flags
	// across workspaceID (running in preview-only mode when dryRun is true).
	PruneOrphans(ctx context.Context, workspaceID uuid.UUID, dryRun bool) (PruneResourcesResult, error)
}

// MarkdownStore abstracts tenant-isolated storage for optional extended Task and
// Project Markdown documents without importing filesystem packages.
type MarkdownStore interface {
	// ReadTaskMarkdown reads the UTF-8 Markdown content for (workspaceID, taskID).
	ReadTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (string, error)

	// WriteTaskMarkdown writes UTF-8 Markdown content for (workspaceID, taskID).
	WriteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID, markdown string) error

	// DeleteTaskMarkdown deletes the Markdown document for (workspaceID, taskID) if present.
	DeleteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) error

	// HasTaskMarkdown reports whether a Markdown document exists for (workspaceID, taskID).
	HasTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (bool, error)

	// ScanTaskFiles lists valid `<uuid>.md` stems and malformed relative filenames in the workspace task store.
	ScanTaskFiles(ctx context.Context, workspaceID uuid.UUID) (validIDs []uuid.UUID, malformedRelKeys []string, err error)

	// RemoveMalformedTaskMarkdown removes a malformed relative `.md` file discovered by ScanTaskFiles.
	RemoveMalformedTaskMarkdown(ctx context.Context, relKey string) error

	// DeleteProjectMarkdown deletes an orphan project Markdown file for (workspaceID, projectID).
	DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) error

	// HasProjectMarkdown reports whether a project Markdown document exists on disk.
	HasProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (bool, error)

	// ScanProjectFiles lists valid `<uuid>.md` stems and malformed relative filenames in the workspace project store.
	ScanProjectFiles(ctx context.Context, workspaceID uuid.UUID) (validIDs []uuid.UUID, malformedRelKeys []string, err error)

	// RemoveMalformedProjectMarkdown removes a malformed relative `.md` file discovered by ScanProjectFiles.
	RemoveMalformedProjectMarkdown(ctx context.Context, relKey string) error
}

// ProjectDocEntry represents minimal active project metadata used to detect
// orphan `.md` files and missing DB Markdown flags across a workspace.
type ProjectDocEntry struct {
	// ID is the active project UUID.
	ID uuid.UUID

	// KeyPrefix is the short uppercase project prefix (e.g., "FLW").
	KeyPrefix string

	// HasMarkdownDoc reflects the persisted projects.has_markdown_doc indicator.
	HasMarkdownDoc bool
}

// ProjectDocInspector provides workspace-scoped read/clear access to project
// Markdown flags without coupling the task domain package to the project package.
type ProjectDocInspector interface {
	// ListProjectDocs returns all active projects in workspaceID with their HasMarkdownDoc flags.
	ListProjectDocs(ctx context.Context, workspaceID uuid.UUID) ([]ProjectDocEntry, error)

	// ClearProjectMarkdownFlag resets projects.has_markdown_doc = false inside scope.
	ClearProjectMarkdownFlag(ctx context.Context, scope tx.Scope, workspaceID, projectID uuid.UUID) error
}

// KeyAllocator mints sequential, project-scoped human/LLM task keys (such as
// "FLW-42") atomically within an active transaction Scope.
type KeyAllocator interface {
	// AllocateTaskKey returns the next unique task key for (workspaceID, projectID)
	// using the enclosing transaction scope.
	AllocateTaskKey(ctx context.Context, scope tx.Scope, workspaceID, projectID uuid.UUID) (string, error)
}

// KeyAllocatorFunc adapts an ordinary function to satisfy KeyAllocator.
type KeyAllocatorFunc func(ctx context.Context, scope tx.Scope, workspaceID, projectID uuid.UUID) (string, error)

// AllocateTaskKey invokes f(ctx, scope, workspaceID, projectID).
func (f KeyAllocatorFunc) AllocateTaskKey(ctx context.Context, scope tx.Scope, workspaceID, projectID uuid.UUID) (string, error) {
	return f(ctx, scope, workspaceID, projectID)
}

// ServiceSkeleton is the pure domain implementation of task.Service.
// The exported type name ServiceSkeleton is preserved for dependency-injection
// signature compatibility across transport and CLI wiring.
type ServiceSkeleton struct {
	repo        Repository
	txMgr       tx.Manager
	keyAlloc    KeyAllocator
	mdStore     MarkdownStore
	projDocs    ProjectDocInspector
	fallbackMu  sync.Mutex
	fallbackSeq map[uuid.UUID]int64
}

// Compile-time check ensuring *ServiceSkeleton satisfies Service.
var _ Service = (*ServiceSkeleton)(nil)

// NewService constructs a task ServiceSkeleton with resolved Repository and
// tx.Manager dependencies.
func NewService(repo Repository, txMgr tx.Manager) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:        repo,
		txMgr:       txMgr,
		fallbackSeq: make(map[uuid.UUID]int64),
	}
}

// NewServiceWithKeyAllocator constructs a task ServiceSkeleton configured with an
// explicit transactional KeyAllocator for minting project-prefixed task keys.
func NewServiceWithKeyAllocator(repo Repository, txMgr tx.Manager, alloc KeyAllocator) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:        repo,
		txMgr:       txMgr,
		keyAlloc:    alloc,
		fallbackSeq: make(map[uuid.UUID]int64),
	}
}

// WithMarkdownResources wires the optional MarkdownStore and ProjectDocInspector
// used for task `.md` persistence, archive purging, and orphan pruning.
func (s *ServiceSkeleton) WithMarkdownResources(mdStore MarkdownStore, projDocs ProjectDocInspector) *ServiceSkeleton {
	if s != nil {
		s.mdStore = mdStore
		s.projDocs = projDocs
	}
	return s
}

// CreateTask allocates the next project task key, validates hierarchy references,
// and persists a new Task inside an atomic audited transaction.
func (s *ServiceSkeleton) CreateTask(ctx context.Context, in CreateTaskInput) (Task, error) {
	const op = "task.Service.CreateTask"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Task{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Task{}, err
	}

	title := strings.TrimSpace(in.Title)
	desc := strings.TrimSpace(in.Description)
	kind := in.Kind
	if kind == "" {
		kind = KindChore
	}
	status := in.Status
	if status == "" {
		status = StatusReady
	}

	var violations []errs.FieldViolation
	if in.ProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if title == "" || utf8.RuneCountInString(title) > maxTaskTitleRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "title",
			Description: "title must be between 1 and 300 characters",
		})
	}
	if utf8.RuneCountInString(desc) > maxTaskDescriptionRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 32000 characters",
		})
	}
	if !isValidKind(kind) {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be epic, feature, bug, chore, or research",
		})
	}
	if !status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "invalid initial task status",
		})
	}
	if !isValidPriority(in.Priority) {
		violations = append(violations, errs.FieldViolation{
			Field:       "priority",
			Description: "priority must be between 0 (None) and 4 (Low)",
		})
	}
	criteria, critViolation := sanitizeStringSlice(in.AcceptanceCriteria, "acceptance_criteria")
	if critViolation != nil {
		violations = append(violations, *critViolation)
	}
	tags, tagViolation := sanitizeStringSlice(in.Tags, "tags")
	if tagViolation != nil {
		violations = append(violations, *tagViolation)
	}
	labels, labelViolation := sanitizeLabels(in.Labels)
	if labelViolation != nil {
		violations = append(violations, *labelViolation)
	}
	extID, extViolation := normalizeExternalID(in.ExternalID)
	if extViolation != nil {
		violations = append(violations, *extViolation)
	}
	if len(violations) > 0 {
		return Task{}, errs.Validation(op, "invalid task creation parameters", violations...)
	}

	now := time.Now().UTC()
	var completedAt *time.Time
	if status.IsTerminal() {
		completedAt = &now
	}
	var lastSignalAt *time.Time
	if authctx.ShouldEmitTaskSignal(ctx) && status.AcceptsSignal() {
		sig := now
		lastSignalAt = &sig
	}

	created := Task{
		ID:                 newUUID(),
		WorkspaceID:        in.WorkspaceID,
		ProjectID:          in.ProjectID,
		ObjectiveID:        normalizeUUIDPtr(in.ObjectiveID),
		ParentTaskID:       normalizeUUIDPtr(in.ParentTaskID),
		ExternalID:         extID,
		Kind:               kind,
		Title:              title,
		Description:        desc,
		AcceptanceCriteria: criteria,
		Status:             status,
		Priority:           in.Priority,
		AssigneeActorID:    normalizeUUIDPtr(in.AssigneeActorID),
		Tags:               tags,
		Labels:             labels,
		Version:            1,
		CreatedBy:          caller.ActorID,
		CreatedAt:          now,
		UpdatedAt:          now,
		CompletedAt:        completedAt,
		LastSignalAt:       lastSignalAt,
	}

	var out Task
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)

		if created.ExternalID != "" {
			if existing, extErr := scopedRepo.GetByExternalID(ctx, in.WorkspaceID, in.ProjectID, created.ExternalID); extErr == nil && existing.ID != uuid.Nil {
				return errs.New(errs.ErrConflict, op, fmt.Sprintf("external_id %q already exists in this project", created.ExternalID))
			} else if extErr != nil && !errors.Is(extErr, errs.ErrNotFound) {
				return extErr
			}
		}

		if created.ParentTaskID != nil {
			parent, err := scopedRepo.GetByID(ctx, in.WorkspaceID, *created.ParentTaskID)
			if err != nil {
				return err
			}
			if parent.ProjectID != in.ProjectID {
				return errs.Validation(op, "parent task belongs to a different project", errs.FieldViolation{
					Field:       "parent_task_id",
					Description: "parent_task_id must belong to the same project_id",
				})
			}
		}

		if s.keyAlloc != nil {
			key, err := s.keyAlloc.AllocateTaskKey(ctx, scope, in.WorkspaceID, in.ProjectID)
			if err != nil {
				return err
			}
			created.Key = strings.TrimSpace(key)
		}

		if err := scopedRepo.Create(ctx, created.Clone()); err != nil {
			return err
		}

		persisted, err := scopedRepo.GetByID(ctx, in.WorkspaceID, created.ID)
		if err != nil {
			return err
		}
		if persisted.Key == "" {
			fallbackKey, err := s.allocateFallbackKey(ctx, scopedRepo, in.WorkspaceID, in.ProjectID)
			if err != nil {
				return err
			}
			persisted.Key = fallbackKey
			if err := scopedRepo.Update(ctx, persisted.Clone(), nil); err != nil {
				return err
			}
			reloaded, err := scopedRepo.GetByID(ctx, in.WorkspaceID, created.ID)
			if err == nil {
				persisted = reloaded
			}
		}

		newSnapshot, err := marshalSnapshot(op, persisted)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "task",
			ResourceID:   persisted.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		out = persisted
		return nil
	}); err != nil {
		return Task{}, err
	}

	return out.Clone(), nil
}

// GetTask fetches a complete Task aggregate by UUID within workspaceID.
func (s *ServiceSkeleton) GetTask(ctx context.Context, workspaceID, taskID uuid.UUID) (Task, error) {
	const op = "task.Service.GetTask"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Task{}, err
	}
	if taskID == uuid.Nil {
		return Task{}, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return Task{}, err
	}

	t, err := s.repo.GetByID(ctx, workspaceID, taskID)
	if err != nil {
		return Task{}, err
	}
	if authctx.ShouldEmitTaskSignal(ctx) && t.Status.AcceptsSignal() {
		t, err = s.recordReadSignal(ctx, caller, t)
		if err != nil {
			return Task{}, err
		}
	}
	return t.Clone(), nil
}

// GetTaskByKey fetches a complete Task aggregate by short key (e.g., "FLW-104")
// within workspaceID.
func (s *ServiceSkeleton) GetTaskByKey(ctx context.Context, workspaceID uuid.UUID, key string) (Task, error) {
	const op = "task.Service.GetTaskByKey"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Task{}, err
	}
	normalized := strings.TrimSpace(key)
	if normalized == "" {
		return Task{}, errs.Validation(op, "task key is required", errs.FieldViolation{
			Field:       "key",
			Description: "key must not be empty",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return Task{}, err
	}

	t, err := s.repo.GetByKey(ctx, workspaceID, normalized)
	if err != nil {
		return Task{}, err
	}
	if authctx.ShouldEmitTaskSignal(ctx) && t.Status.AcceptsSignal() {
		t, err = s.recordReadSignal(ctx, caller, t)
		if err != nil {
			return Task{}, err
		}
	}
	return t.Clone(), nil
}

// GetTaskByExternalID fetches a complete Task aggregate by its project-scoped
// external tracker ID (e.g., "CU-869abc") within (workspaceID, projectID).
func (s *ServiceSkeleton) GetTaskByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (Task, error) {
	const op = "task.Service.GetTaskByExternalID"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Task{}, err
	}
	var violations []errs.FieldViolation
	if projectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	extID, extViolation := normalizeExternalID(externalID)
	if extViolation != nil {
		violations = append(violations, *extViolation)
	} else if extID == "" {
		violations = append(violations, errs.FieldViolation{
			Field:       "external_id",
			Description: "external_id must not be empty",
		})
	}
	if len(violations) > 0 {
		return Task{}, errs.Validation(op, "invalid external task lookup parameters", violations...)
	}
	if err := s.ensureReadReady(op); err != nil {
		return Task{}, err
	}

	t, err := s.repo.GetByExternalID(ctx, workspaceID, projectID, extID)
	if err != nil {
		return Task{}, err
	}
	if authctx.ShouldEmitTaskSignal(ctx) && t.Status.AcceptsSignal() {
		t, err = s.recordReadSignal(ctx, caller, t)
		if err != nil {
			return Task{}, err
		}
	}
	return t.Clone(), nil
}

func (s *ServiceSkeleton) recordReadSignal(ctx context.Context, caller authctx.Identity, current Task) (Task, error) {
	const op = "task.Service.recordReadSignal"
	if s.txMgr == nil || !current.Status.AcceptsSignal() {
		return current, nil
	}
	var touched Task
	err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()
		oldSnap, snapErr := marshalSnapshot(op, current)
		if snapErr != nil {
			return snapErr
		}
		next, touchErr := scopedRepo.TouchSignal(txCtx, current.WorkspaceID, current.ID, now)
		if touchErr != nil {
			return touchErr
		}
		newSnap, newErr := marshalSnapshot(op, next)
		if newErr != nil {
			return newErr
		}
		if recErr := scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  current.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionUpdate,
			ResourceType: "task",
			ResourceID:   current.ID.String(),
			OldValues:    oldSnap,
			NewValues:    newSnap,
			OccurredAt:   now,
		}); recErr != nil {
			return recErr
		}
		touched = next
		return nil
	})
	if err != nil {
		if errors.Is(err, errs.ErrNotImplemented) {
			return current, nil
		}
		return Task{}, err
	}
	return touched, nil
}

func normalizeExternalID(raw string) (string, *errs.FieldViolation) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if utf8.RuneCountInString(trimmed) > maxTaskExternalIDRunes {
		return "", &errs.FieldViolation{
			Field:       "external_id",
			Description: "external_id must not exceed 255 characters",
		}
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", &errs.FieldViolation{
				Field:       "external_id",
				Description: "external_id must not contain control characters or newlines",
			}
		}
	}
	return trimmed, nil
}

// UpdateTask applies partial updates to a Task inside an atomic audited transaction,
// enforcing active ClaimLease exclusivity, parent DAG acyclicity, and optional optimistic locking.
func (s *ServiceSkeleton) UpdateTask(ctx context.Context, in UpdateTaskInput) (Task, error) {
	const op = "task.Service.UpdateTask"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Task{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Task{}, err
	}

	var violations []errs.FieldViolation
	if in.TaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if in.ParentTaskID != nil && *in.ParentTaskID == in.TaskID && in.TaskID != uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "parent_task_id",
			Description: "task cannot be set as its own parent",
		})
	}
	if in.Kind != nil && !isValidKind(*in.Kind) {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "kind must be epic, feature, bug, chore, or research",
		})
	}
	if in.Title != nil {
		trimmed := strings.TrimSpace(*in.Title)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > maxTaskTitleRunes {
			violations = append(violations, errs.FieldViolation{
				Field:       "title",
				Description: "title must be between 1 and 300 characters",
			})
		}
	}
	if in.Description != nil && utf8.RuneCountInString(strings.TrimSpace(*in.Description)) > maxTaskDescriptionRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 32000 characters",
		})
	}
	if in.Priority != nil && !isValidPriority(*in.Priority) {
		violations = append(violations, errs.FieldViolation{
			Field:       "priority",
			Description: "priority must be between 0 (None) and 4 (Low)",
		})
	}
	var nextCriteria []string
	if in.AcceptanceCriteria != nil {
		var v *errs.FieldViolation
		nextCriteria, v = sanitizeStringSlice(in.AcceptanceCriteria, "acceptance_criteria")
		if v != nil {
			violations = append(violations, *v)
		}
	}
	var nextTags []string
	if in.Tags != nil {
		var v *errs.FieldViolation
		nextTags, v = sanitizeStringSlice(in.Tags, "tags")
		if v != nil {
			violations = append(violations, *v)
		}
	}
	var nextLabels map[string]string
	if in.Labels != nil {
		var v *errs.FieldViolation
		nextLabels, v = sanitizeLabels(in.Labels)
		if v != nil {
			violations = append(violations, *v)
		}
	}
	var nextExtID string
	if in.ExternalID != nil {
		var v *errs.FieldViolation
		nextExtID, v = normalizeExternalID(*in.ExternalID)
		if v != nil {
			violations = append(violations, *v)
		}
	}
	if len(violations) > 0 {
		return Task{}, errs.Validation(op, "invalid task update parameters", violations...)
	}

	var updated Task
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()

		current, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err != nil {
			return err
		}
		if err := ensureLeaseAllowsActor(ctx, scopedRepo, op, current, caller.ActorID, now); err != nil {
			return err
		}
		if in.ExpectedVersion != nil && current.Version != *in.ExpectedVersion {
			return errs.New(errs.ErrConflict, op, "task optimistic concurrency version mismatch")
		}

		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}

		next := current.Clone()
		if in.ObjectiveID != nil {
			next.ObjectiveID = normalizeUUIDPtr(in.ObjectiveID)
		}
		if in.ParentTaskID != nil {
			resolvedParent := normalizeUUIDPtr(in.ParentTaskID)
			if resolvedParent != nil {
				parentTask, err := scopedRepo.GetByID(ctx, in.WorkspaceID, *resolvedParent)
				if err != nil {
					return err
				}
				if parentTask.ProjectID != current.ProjectID {
					return errs.Validation(op, "parent task belongs to a different project", errs.FieldViolation{
						Field:       "parent_task_id",
						Description: "parent_task_id must belong to the same project_id",
					})
				}
				if err := detectHierarchyCycle(ctx, scopedRepo, op, in.WorkspaceID, in.TaskID, *resolvedParent); err != nil {
					return err
				}
			}
			next.ParentTaskID = resolvedParent
		}
		if in.Kind != nil {
			next.Kind = *in.Kind
		}
		if in.Title != nil {
			next.Title = strings.TrimSpace(*in.Title)
		}
		if in.Description != nil {
			next.Description = strings.TrimSpace(*in.Description)
		}
		if in.AcceptanceCriteria != nil {
			next.AcceptanceCriteria = nextCriteria
		}
		if in.Priority != nil {
			next.Priority = *in.Priority
		}
		if in.AssigneeActorID != nil {
			next.AssigneeActorID = normalizeUUIDPtr(in.AssigneeActorID)
		}
		if in.Tags != nil {
			next.Tags = nextTags
		}
		if in.Labels != nil {
			next.Labels = nextLabels
		}
		if in.ExternalID != nil {
			if nextExtID != "" && nextExtID != current.ExternalID {
				if existing, extErr := scopedRepo.GetByExternalID(ctx, in.WorkspaceID, current.ProjectID, nextExtID); extErr == nil && existing.ID != current.ID {
					return errs.New(errs.ErrConflict, op, fmt.Sprintf("external_id %q already exists in this project", nextExtID))
				} else if extErr != nil && !errors.Is(extErr, errs.ErrNotFound) {
					return extErr
				}
			}
			next.ExternalID = nextExtID
		}
		next.UpdatedAt = now
		if authctx.ShouldEmitTaskSignal(ctx) && next.Status.AcceptsSignal() {
			sig := now
			next.LastSignalAt = &sig
		}

		if err := scopedRepo.Update(ctx, next.Clone(), in.ExpectedVersion); err != nil {
			return err
		}
		reloaded, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err == nil {
			next = reloaded
		}

		newSnapshot, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionUpdate,
			ResourceType: "task",
			ResourceID:   in.TaskID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Task{}, err
	}

	return updated.Clone(), nil
}

// TransitionTask validates unresolved blocker and active lease preconditions,
// transitions task Status, sets CompletedAt on terminal transitions, optionally
// appends a resolution comment, and optionally releases the active lease inside
// one atomic audited transaction.
func (s *ServiceSkeleton) TransitionTask(ctx context.Context, in TransitionTaskInput) (Task, error) {
	const op = "task.Service.TransitionTask"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Task{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Task{}, err
	}

	var violations []errs.FieldViolation
	if in.TaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if !in.TargetStatus.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "target_status",
			Description: "target_status must be a valid workflow status",
		})
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.ResolutionComment)) > maxCommentBodyRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "resolution_comment",
			Description: "resolution_comment must not exceed 16000 characters",
		})
	}
	if len(violations) > 0 {
		return Task{}, errs.Validation(op, "invalid task transition parameters", violations...)
	}

	var transitioned Task
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()

		current, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err != nil {
			return err
		}
		if err := ensureLeaseAllowsActor(ctx, scopedRepo, op, current, caller.ActorID, now); err != nil {
			return err
		}
		if in.ExpectedVersion != nil && current.Version != *in.ExpectedVersion {
			return errs.New(errs.ErrConflict, op, "task optimistic concurrency version mismatch")
		}

		if in.TargetStatus == StatusInProgress || in.TargetStatus == StatusDone {
			if err := ensureNoUnresolvedBlockers(ctx, scopedRepo, op, current); err != nil {
				return err
			}
		}

		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}

		next := current.Clone()
		next.Status = in.TargetStatus
		next.UpdatedAt = now
		if authctx.ShouldEmitTaskSignal(ctx) && in.TargetStatus.AcceptsSignal() {
			sig := now
			next.LastSignalAt = &sig
		}
		if in.TargetStatus.IsTerminal() {
			if next.CompletedAt == nil {
				completed := now
				next.CompletedAt = &completed
			}
		} else {
			next.CompletedAt = nil
		}

		if in.ReleaseActiveLease {
			if err := scopedRepo.DeleteLease(ctx, in.WorkspaceID, in.TaskID); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
			next.ActiveLease = nil
		}

		if err := scopedRepo.Update(ctx, next.Clone(), in.ExpectedVersion); err != nil {
			return err
		}

		note := strings.TrimSpace(in.ResolutionComment)
		if note != "" {
			commentKind := CommentKindProgress
			switch {
			case in.TargetStatus.IsTerminal():
				commentKind = CommentKindDecision
			case in.TargetStatus == StatusBlocked:
				commentKind = CommentKindBlocker
			case in.TargetStatus == StatusInReview:
				commentKind = CommentKindHandoff
			}
			if err := scopedRepo.CreateComment(ctx, TaskComment{
				ID:            newUUID(),
				WorkspaceID:   in.WorkspaceID,
				TaskID:        in.TaskID,
				AuthorActorID: caller.ActorID,
				AuthorKind:    caller.Kind,
				AuthorHandle:  caller.Handle,
				Kind:          commentKind,
				Body:          note,
				CreatedAt:     now,
			}); err != nil {
				return err
			}
		}

		reloaded, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err == nil {
			next = reloaded
		}

		newSnapshot, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionTransition,
			ResourceType: "task",
			ResourceID:   in.TaskID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		transitioned = next
		return nil
	}); err != nil {
		return Task{}, err
	}

	return transitioned.Clone(), nil
}

// DeleteTask soft-deletes a Task and cleans up its active lease in an atomic audited transaction.
func (s *ServiceSkeleton) DeleteTask(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "task.Service.DeleteTask"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return err
	}
	if taskID == uuid.Nil {
		return errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()

		current, err := scopedRepo.GetByID(ctx, workspaceID, taskID)
		if err != nil {
			return err
		}
		if err := ensureLeaseAllowsActor(ctx, scopedRepo, op, current, caller.ActorID, now); err != nil {
			return err
		}

		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		if err := scopedRepo.DeleteLease(ctx, workspaceID, taskID); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		if err := scopedRepo.SoftDelete(ctx, workspaceID, taskID); err != nil {
			return err
		}

		deleted := current.Clone()
		deleted.ActiveLease = nil
		deleted.UpdatedAt = now
		deleted.DeletedAt = &now
		newSnapshot, err := marshalSnapshot(op, deleted)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionDelete,
			ResourceType: "task",
			ResourceID:   taskID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return err
	}

	if s.mdStore != nil {
		_ = s.mdStore.DeleteTaskMarkdown(context.WithoutCancel(ctx), workspaceID, taskID)
	}
	return nil
}

// ClaimTask acquires, renews, or preempts an expired exclusive ClaimLease on a Task
// inside an atomic audited transaction.
func (s *ServiceSkeleton) ClaimTask(ctx context.Context, in ClaimTaskInput) (ClaimLease, error) {
	const op = "task.Service.ClaimTask"
	if err := checkContext(ctx, op); err != nil {
		return ClaimLease{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return ClaimLease{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return ClaimLease{}, err
	}

	var violations []errs.FieldViolation
	if in.TaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	ttlSecs := in.TTLSeconds
	if ttlSecs <= 0 {
		ttlSecs = DefaultLeaseTTLSeconds
	} else if ttlSecs > MaxLeaseTTLSeconds {
		violations = append(violations, errs.FieldViolation{
			Field:       "ttl_seconds",
			Description: "ttl_seconds must not exceed 86400",
		})
	}
	if len(violations) > 0 {
		return ClaimLease{}, errs.Validation(op, "invalid task claim parameters", violations...)
	}

	ttl := time.Duration(ttlSecs) * time.Second
	var acquired ClaimLease

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()

		currentTask, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err != nil {
			return err
		}
		if currentTask.Status.IsTerminal() {
			return errs.New(errs.ErrPreconditionFailed, op, "cannot claim a task in a terminal status")
		}

		existingLease, hasExisting, err := resolveStoredLease(ctx, scopedRepo, in.WorkspaceID, currentTask)
		if err != nil {
			return err
		}

		var oldSnapshot []byte
		if hasExisting {
			oldSnapshot, err = marshalSnapshot(op, existingLease)
			if err != nil {
				return err
			}
		}

		var nextLease ClaimLease
		if hasExisting && !existingLease.IsExpiredAt(now) {
			if existingLease.HolderActorID != caller.ActorID {
				return errs.New(
					errs.ErrLeaseHeld,
					op,
					fmt.Sprintf("task is actively leased by actor %q until %s", existingLease.HolderHandle, existingLease.ExpiresAt.Format(time.RFC3339)),
				)
			}
			suppliedToken := strings.TrimSpace(in.LeaseToken)
			if suppliedToken != "" && suppliedToken != existingLease.LeaseToken {
				return errs.New(errs.ErrPreconditionFailed, op, "supplied lease_token does not match active lease")
			}
			sessionID := strings.TrimSpace(in.AgentSessionID)
			if sessionID == "" {
				sessionID = existingLease.AgentSessionID
			}
			token := existingLease.LeaseToken
			if token == "" {
				token = newUUID().String()
			}
			expiresAt := now.Add(ttl)
			if !expiresAt.After(existingLease.ExpiresAt) {
				expiresAt = existingLease.ExpiresAt.Add(time.Millisecond)
			}
			nextLease = ClaimLease{
				TaskID:         in.TaskID,
				WorkspaceID:    in.WorkspaceID,
				LeaseToken:     token,
				HolderActorID:  caller.ActorID,
				HolderKind:     caller.Kind,
				HolderHandle:   caller.Handle,
				AgentSessionID: sessionID,
				AcquiredAt:     existingLease.AcquiredAt,
				HeartbeatAt:    now,
				ExpiresAt:      expiresAt,
			}
		} else {
			nextLease = ClaimLease{
				TaskID:         in.TaskID,
				WorkspaceID:    in.WorkspaceID,
				LeaseToken:     newUUID().String(),
				HolderActorID:  caller.ActorID,
				HolderKind:     caller.Kind,
				HolderHandle:   caller.Handle,
				AgentSessionID: strings.TrimSpace(in.AgentSessionID),
				AcquiredAt:     now,
				HeartbeatAt:    now,
				ExpiresAt:      now.Add(ttl),
			}
		}

		effectiveStatus := currentTask.Status
		if in.AutoTransitionInProgress && currentTask.Status == StatusReady {
			if err := ensureNoUnresolvedBlockers(ctx, scopedRepo, op, currentTask); err != nil {
				return err
			}
			updatedTask := currentTask.Clone()
			updatedTask.Status = StatusInProgress
			effectiveStatus = StatusInProgress
			updatedTask.UpdatedAt = now
			if authctx.ShouldEmitTaskSignal(ctx) {
				sig := now
				updatedTask.LastSignalAt = &sig
			}
			if updatedTask.AssigneeActorID == nil {
				actorID := caller.ActorID
				updatedTask.AssigneeActorID = &actorID
			}
			if err := scopedRepo.Update(ctx, updatedTask, nil); err != nil {
				return err
			}
		}

		if err := scopedRepo.UpsertLease(ctx, nextLease.Clone()); err != nil {
			return err
		}
		if authctx.ShouldEmitTaskSignal(ctx) && effectiveStatus.AcceptsSignal() {
			if _, err := scopedRepo.TouchSignal(ctx, in.WorkspaceID, in.TaskID, now); err != nil && !errors.Is(err, errs.ErrNotImplemented) {
				return err
			}
		}

		newSnapshot, err := marshalSnapshot(op, nextLease)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionClaim,
			ResourceType: "task",
			ResourceID:   in.TaskID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		acquired = nextLease
		return nil
	}); err != nil {
		return ClaimLease{}, err
	}

	return acquired.Clone(), nil
}

// ReleaseTask voluntarily releases (or with Force=true administratively revokes)
// an active ClaimLease and optionally records a CommentKindHandoff comment atomically.
func (s *ServiceSkeleton) ReleaseTask(ctx context.Context, in ReleaseTaskInput) error {
	const op = "task.Service.ReleaseTask"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return err
	}
	if in.TaskID == uuid.Nil {
		return errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.HandoffComment)) > maxCommentBodyRunes {
		return errs.Validation(op, "handoff_comment is too long", errs.FieldViolation{
			Field:       "handoff_comment",
			Description: "handoff_comment must not exceed 16000 characters",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	return s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()

		currentTask, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err != nil {
			return err
		}

		existingLease, hasExisting, err := resolveStoredLease(ctx, scopedRepo, in.WorkspaceID, currentTask)
		if err != nil {
			return err
		}
		if !hasExisting {
			return errs.New(errs.ErrPreconditionFailed, op, "task does not hold an active claim lease")
		}

		if !in.Force {
			if existingLease.HolderActorID != caller.ActorID {
				return errs.New(errs.ErrLeaseHeld, op, "cannot release a claim lease owned by another actor without force=true")
			}
			suppliedToken := strings.TrimSpace(in.LeaseToken)
			if suppliedToken != "" && suppliedToken != existingLease.LeaseToken {
				return errs.New(errs.ErrPreconditionFailed, op, "lease fencing token mismatch")
			}
		}

		oldSnapshot, err := marshalSnapshot(op, existingLease)
		if err != nil {
			return err
		}

		if err := scopedRepo.DeleteLease(ctx, in.WorkspaceID, in.TaskID); err != nil {
			return err
		}
		if authctx.ShouldEmitTaskSignal(ctx) && currentTask.Status.AcceptsSignal() {
			if _, err := scopedRepo.TouchSignal(ctx, in.WorkspaceID, in.TaskID, now); err != nil && !errors.Is(err, errs.ErrNotImplemented) {
				return err
			}
		}

		handoff := strings.TrimSpace(in.HandoffComment)
		if handoff != "" {
			if err := scopedRepo.CreateComment(ctx, TaskComment{
				ID:            newUUID(),
				WorkspaceID:   in.WorkspaceID,
				TaskID:        in.TaskID,
				AuthorActorID: caller.ActorID,
				AuthorKind:    caller.Kind,
				AuthorHandle:  caller.Handle,
				Kind:          CommentKindHandoff,
				Body:          handoff,
				CreatedAt:     now,
			}); err != nil {
				return err
			}
		}

		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionRelease,
			ResourceType: "task",
			ResourceID:   in.TaskID.String(),
			OldValues:    oldSnapshot,
			OccurredAt:   now,
		})
	})
}

// PollWorkQueue returns token-compact task summaries matching filter for autonomous
// agent harnesses or CLI dashboards.
func (s *ServiceSkeleton) PollWorkQueue(ctx context.Context, filter AgentWorkQueueFilter) ([]CompactTaskSummary, error) {
	const op = "task.Service.PollWorkQueue"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	var violations []errs.FieldViolation
	for _, st := range filter.Statuses {
		if !st.Valid() {
			violations = append(violations, errs.FieldViolation{
				Field:       "statuses",
				Description: fmt.Sprintf("invalid status filter %q", st),
			})
		}
	}
	if filter.MaxPriority != nil && !isValidPriority(*filter.MaxPriority) {
		violations = append(violations, errs.FieldViolation{
			Field:       "max_priority",
			Description: "max_priority must be between 0 and 3",
		})
	}
	if len(violations) > 0 {
		return nil, errs.Validation(op, "invalid agent work queue filter", violations...)
	}

	normalized := AgentWorkQueueFilter{
		WorkspaceID:   filter.WorkspaceID,
		ProjectID:     filter.ProjectID,
		ObjectiveID:   normalizeUUIDPtr(filter.ObjectiveID),
		ParentTaskID:  normalizeUUIDPtr(filter.ParentTaskID),
		ExternalID:    strings.TrimSpace(filter.ExternalID),
		OnlyUnblocked: filter.OnlyUnblocked,
		OnlyUnclaimed: filter.OnlyUnclaimed,
		Limit:         filter.Limit,
	}
	if len(filter.Statuses) == 0 {
		normalized.Statuses = []Status{StatusReady}
	} else {
		normalized.Statuses = make([]Status, len(filter.Statuses))
		copy(normalized.Statuses, filter.Statuses)
	}
	if len(filter.RequiredTags) > 0 {
		normalized.RequiredTags = make([]string, len(filter.RequiredTags))
		copy(normalized.RequiredTags, filter.RequiredTags)
	}
	if filter.MaxPriority != nil {
		mp := *filter.MaxPriority
		normalized.MaxPriority = &mp
	}
	if normalized.Limit <= 0 {
		normalized.Limit = defaultWorkQueueLimit
	} else if normalized.Limit > maxWorkQueueLimit {
		normalized.Limit = maxWorkQueueLimit
	}

	items, err := s.repo.QueryCompactQueue(ctx, normalized, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := make([]CompactTaskSummary, len(items))
	for i := range items {
		out[i] = items[i].Clone()
	}
	return out, nil
}

// GetProjectDigest returns a token-budgeted situational awareness digest of a Project.
func (s *ServiceSkeleton) GetProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, opts DigestOptions) (ProjectDigest, error) {
	const op = "task.Service.GetProjectDigest"
	if err := checkContext(ctx, op); err != nil {
		return ProjectDigest{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return ProjectDigest{}, err
	}
	if projectID == uuid.Nil {
		return ProjectDigest{}, errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return ProjectDigest{}, err
	}

	normalized := opts
	if normalized.MaxReadyTasks <= 0 {
		normalized.MaxReadyTasks = defaultDigestReadyMax
	} else if normalized.MaxReadyTasks > maxDigestItemsLimit {
		normalized.MaxReadyTasks = maxDigestItemsLimit
	}
	if normalized.MaxBlockedTasks <= 0 {
		normalized.MaxBlockedTasks = defaultDigestBlockedMax
	} else if normalized.MaxBlockedTasks > maxDigestItemsLimit {
		normalized.MaxBlockedTasks = maxDigestItemsLimit
	}

	digest, err := s.repo.BuildProjectDigest(ctx, workspaceID, projectID, normalized, time.Now().UTC())
	if err != nil {
		return ProjectDigest{}, err
	}
	return digest.Clone(), nil
}

// AddRelation validates DAG cycle invariants and persists a directed TaskRelation
// inside an atomic audited transaction.
func (s *ServiceSkeleton) AddRelation(ctx context.Context, in AddRelationInput) (TaskRelation, error) {
	const op = "task.Service.AddRelation"
	if err := checkContext(ctx, op); err != nil {
		return TaskRelation{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return TaskRelation{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return TaskRelation{}, err
	}

	var violations []errs.FieldViolation
	if in.SourceTaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "source_task_id",
			Description: "source_task_id must be a non-nil UUID",
		})
	}
	if in.TargetTaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "target_task_id",
			Description: "target_task_id must be a non-nil UUID",
		})
	}
	if in.SourceTaskID != uuid.Nil && in.SourceTaskID == in.TargetTaskID {
		violations = append(violations, errs.FieldViolation{
			Field:       "target_task_id",
			Description: "reflexive self-relations (source_task_id == target_task_id) are forbidden",
		})
	}
	if !in.Type.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "type",
			Description: "relation type must be blocks, subtask_of, relates_to, or duplicates",
		})
	}
	if len(violations) > 0 {
		return TaskRelation{}, errs.Validation(op, "invalid task relation parameters", violations...)
	}

	now := time.Now().UTC()
	rel := TaskRelation{
		ID:           newUUID(),
		WorkspaceID:  in.WorkspaceID,
		SourceTaskID: in.SourceTaskID,
		TargetTaskID: in.TargetTaskID,
		Type:         in.Type,
		Note:         strings.TrimSpace(in.Note),
		CreatedBy:    caller.ActorID,
		CreatedAt:    now,
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)

		if _, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.SourceTaskID); err != nil {
			return err
		}
		if _, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TargetTaskID); err != nil {
			return err
		}

		switch in.Type {
		case RelationBlocks:
			if err := detectBlocksCycle(ctx, scopedRepo, op, in.WorkspaceID, in.SourceTaskID, in.TargetTaskID); err != nil {
				return err
			}
		case RelationSubtaskOf:
			if err := detectHierarchyCycle(ctx, scopedRepo, op, in.WorkspaceID, in.SourceTaskID, in.TargetTaskID); err != nil {
				return err
			}
		}

		if err := scopedRepo.CreateRelation(ctx, rel); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, rel)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "task_relation",
			ResourceID:   rel.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return TaskRelation{}, err
	}

	return rel, nil
}

// RemoveRelation deletes a directed TaskRelation inside an atomic audited transaction.
func (s *ServiceSkeleton) RemoveRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error {
	const op = "task.Service.RemoveRelation"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return err
	}
	if relationID == uuid.Nil {
		return errs.Validation(op, "relation_id is required", errs.FieldViolation{
			Field:       "relation_id",
			Description: "relation_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	return s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if err := scopedRepo.DeleteRelation(ctx, workspaceID, relationID); err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionDelete,
			ResourceType: "task_relation",
			ResourceID:   relationID.String(),
			OccurredAt:   time.Now().UTC(),
		})
	})
}

// ListRelations returns defensive copies of all inbound and outbound relations for taskID.
func (s *ServiceSkeleton) ListRelations(ctx context.Context, workspaceID, taskID uuid.UUID) ([]TaskRelation, error) {
	const op = "task.Service.ListRelations"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	if taskID == uuid.Nil {
		return nil, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	rels, err := s.repo.ListRelationsForTask(ctx, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRelation, len(rels))
	copy(out, rels)
	return out, nil
}

// AddComment appends a structured TaskComment inside an atomic audited transaction.
func (s *ServiceSkeleton) AddComment(ctx context.Context, in AddCommentInput) (TaskComment, error) {
	const op = "task.Service.AddComment"
	if err := checkContext(ctx, op); err != nil {
		return TaskComment{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return TaskComment{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return TaskComment{}, err
	}

	kind := in.Kind
	if kind == "" {
		kind = CommentKindNote
	}
	body := strings.TrimSpace(in.Body)

	var violations []errs.FieldViolation
	if in.TaskID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if !isValidCommentKind(kind) {
		violations = append(violations, errs.FieldViolation{
			Field:       "kind",
			Description: "comment kind must be progress, blocker, decision, handoff, or note",
		})
	}
	if body == "" || utf8.RuneCountInString(body) > maxCommentBodyRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "body",
			Description: "comment body must be between 1 and 16000 characters",
		})
	}
	if len(violations) > 0 {
		return TaskComment{}, errs.Validation(op, "invalid task comment parameters", violations...)
	}

	now := time.Now().UTC()
	comment := TaskComment{
		ID:            newUUID(),
		WorkspaceID:   in.WorkspaceID,
		TaskID:        in.TaskID,
		AuthorActorID: caller.ActorID,
		AuthorKind:    caller.Kind,
		AuthorHandle:  caller.Handle,
		Kind:          kind,
		Body:          body,
		CreatedAt:     now,
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		currentTask, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.TaskID)
		if err != nil {
			return err
		}
		if err := scopedRepo.CreateComment(ctx, comment); err != nil {
			return err
		}
		if authctx.ShouldEmitTaskSignal(ctx) && currentTask.Status.AcceptsSignal() {
			if _, err := scopedRepo.TouchSignal(ctx, in.WorkspaceID, in.TaskID, now); err != nil && !errors.Is(err, errs.ErrNotImplemented) {
				return err
			}
		}
		newSnapshot, err := marshalSnapshot(op, comment)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "task_comment",
			ResourceID:   comment.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return TaskComment{}, err
	}

	return comment, nil
}

// ListComments returns chronologically ordered TaskComments for taskID.
func (s *ServiceSkeleton) ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]TaskComment, error) {
	const op = "task.Service.ListComments"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, err
	}
	if taskID == uuid.Nil {
		return nil, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}

	normalizedLimit := limit
	if normalizedLimit <= 0 {
		normalizedLimit = defaultCommentsLimit
	} else if normalizedLimit > maxCommentsLimit {
		normalizedLimit = maxCommentsLimit
	}

	comments, err := s.repo.ListComments(ctx, workspaceID, taskID, normalizedLimit)
	if err != nil {
		return nil, err
	}
	out := make([]TaskComment, len(comments))
	copy(out, comments)
	return out, nil
}

// detectBlocksCycle performs a breadth-first traversal starting from targetID along
// outgoing RelationBlocks edges to reject direct or transitive dependency cycles
// before inserting (sourceID --blocks--> targetID).
func detectBlocksCycle(ctx context.Context, scopedRepo Repository, op string, workspaceID, sourceID, targetID uuid.UUID) error {
	if sourceID == targetID {
		return errs.Validation(op, "dependency cycle detected", errs.FieldViolation{
			Field:       "target_task_id",
			Description: "task cannot block itself",
		})
	}
	visited := map[uuid.UUID]struct{}{targetID: {}}
	queue := []uuid.UUID{targetID}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		rels, err := scopedRepo.ListRelationsForTask(ctx, workspaceID, curr)
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel.Type != RelationBlocks || rel.SourceTaskID != curr {
				continue
			}
			next := rel.TargetTaskID
			if next == sourceID {
				return errs.Validation(op, "dependency cycle detected in blocks graph", errs.FieldViolation{
					Field:       "target_task_id",
					Description: "adding this blocks relation creates a circular dependency",
				})
			}
			if _, seen := visited[next]; !seen {
				visited[next] = struct{}{}
				queue = append(queue, next)
			}
		}
	}
	return nil
}

// detectHierarchyCycle performs a breadth-first traversal from parentID upward
// across both Task.ParentTaskID pointers and outgoing RelationSubtaskOf edges
// (child -> parent) to reject direct or transitive hierarchy cycles before linking
// (childID -> parentID).
func detectHierarchyCycle(ctx context.Context, scopedRepo Repository, op string, workspaceID, childID, parentID uuid.UUID) error {
	if childID == parentID {
		return errs.Validation(op, "hierarchy cycle detected", errs.FieldViolation{
			Field:       "parent_task_id",
			Description: "task cannot be a subtask of itself",
		})
	}
	visited := map[uuid.UUID]struct{}{parentID: {}}
	queue := []uuid.UUID{parentID}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		currTask, err := scopedRepo.GetByID(ctx, workspaceID, curr)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				continue
			}
			return err
		}
		if currTask.ParentTaskID != nil && *currTask.ParentTaskID != uuid.Nil {
			anc := *currTask.ParentTaskID
			if anc == childID {
				return errs.Validation(op, "hierarchy cycle detected in parent chain", errs.FieldViolation{
					Field:       "parent_task_id",
					Description: "assigning this parent creates a circular task hierarchy",
				})
			}
			if _, seen := visited[anc]; !seen {
				visited[anc] = struct{}{}
				queue = append(queue, anc)
			}
		}

		rels, err := scopedRepo.ListRelationsForTask(ctx, workspaceID, curr)
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel.Type != RelationSubtaskOf || rel.SourceTaskID != curr {
				continue
			}
			anc := rel.TargetTaskID
			if anc == childID {
				return errs.Validation(op, "hierarchy cycle detected in subtask_of graph", errs.FieldViolation{
					Field:       "target_task_id",
					Description: "adding this subtask_of relation creates a circular task hierarchy",
				})
			}
			if _, seen := visited[anc]; !seen {
				visited[anc] = struct{}{}
				queue = append(queue, anc)
			}
		}
	}
	return nil
}

func ensureNoUnresolvedBlockers(ctx context.Context, scopedRepo Repository, op string, t Task) error {
	if t.UnresolvedBlockerCount > 0 {
		return errs.New(
			errs.ErrPreconditionFailed,
			op,
			fmt.Sprintf("task %s has %d unresolved upstream blocker(s)", t.Key, t.UnresolvedBlockerCount),
		)
	}
	rels, err := scopedRepo.ListRelationsForTask(ctx, t.WorkspaceID, t.ID)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if rel.Type != RelationBlocks || rel.TargetTaskID != t.ID {
			continue
		}
		blocker, err := scopedRepo.GetByID(ctx, t.WorkspaceID, rel.SourceTaskID)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				continue
			}
			return err
		}
		if !blocker.Status.IsTerminal() {
			return errs.New(
				errs.ErrPreconditionFailed,
				op,
				fmt.Sprintf("task %s is blocked by incomplete task %s", t.Key, blocker.Key),
			)
		}
	}
	return nil
}

func resolveStoredLease(ctx context.Context, scopedRepo Repository, workspaceID uuid.UUID, t Task) (ClaimLease, bool, error) {
	lease, err := scopedRepo.GetLease(ctx, workspaceID, t.ID)
	if err == nil {
		return lease, true, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return ClaimLease{}, false, err
	}
	if t.ActiveLease != nil {
		return t.ActiveLease.Clone(), true, nil
	}
	return ClaimLease{}, false, nil
}

func ensureLeaseAllowsActor(ctx context.Context, scopedRepo Repository, op string, current Task, callerActorID uuid.UUID, now time.Time) error {
	lease, hasLease, err := resolveStoredLease(ctx, scopedRepo, current.WorkspaceID, current)
	if err != nil {
		return err
	}
	if hasLease && !lease.IsExpiredAt(now) && lease.HolderActorID != callerActorID {
		return errs.New(
			errs.ErrLeaseHeld,
			op,
			fmt.Sprintf("task is actively leased by actor %q until %s", lease.HolderHandle, lease.ExpiresAt.Format(time.RFC3339)),
		)
	}
	return nil
}

func (s *ServiceSkeleton) allocateFallbackKey(ctx context.Context, scopedRepo Repository, workspaceID, projectID uuid.UUID) (string, error) {
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	if s.fallbackSeq == nil {
		s.fallbackSeq = make(map[uuid.UUID]int64)
	}
	for {
		s.fallbackSeq[projectID]++
		candidate := fmt.Sprintf("%s-%d", fallbackTaskKeyPrefix, s.fallbackSeq[projectID])
		_, err := scopedRepo.GetByKey(ctx, workspaceID, candidate)
		if errors.Is(err, errs.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
}

func (s *ServiceSkeleton) ensureReadReady(op string) error {
	if s == nil || s.repo == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "task repository is not initialized")
	}
	return nil
}

func (s *ServiceSkeleton) ensureWriteReady(op string) error {
	if err := s.ensureReadReady(op); err != nil {
		return err
	}
	if s.txMgr == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "transaction manager is not initialized")
	}
	return nil
}

func checkContext(ctx context.Context, op string) error {
	if ctx == nil {
		return errs.New(errs.ErrUnauthenticated, op, "missing request context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func isValidKind(k Kind) bool {
	switch k {
	case KindEpic, KindFeature, KindBug, KindChore, KindResearch:
		return true
	default:
		return false
	}
}

func isValidPriority(p Priority) bool {
	return p >= PriorityNone && p <= PriorityLow
}

func isValidCommentKind(k CommentKind) bool {
	switch k {
	case CommentKindProgress, CommentKindBlocker, CommentKindDecision, CommentKindHandoff, CommentKindNote:
		return true
	default:
		return false
	}
}

func normalizeUUIDPtr(id *uuid.UUID) *uuid.UUID {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	cp := *id
	return &cp
}

func sanitizeStringSlice(items []string, field string) ([]string, *errs.FieldViolation) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, &errs.FieldViolation{
				Field:       field,
				Description: fmt.Sprintf("%s must not contain empty strings", field),
			}
		}
		out = append(out, trimmed)
	}
	return out, nil
}

func sanitizeLabels(labels map[string]string) (map[string]string, *errs.FieldViolation) {
	if len(labels) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if strings.TrimSpace(k) == "" {
			return nil, &errs.FieldViolation{
				Field:       "labels",
				Description: "label keys must not be empty",
			}
		}
		out[k] = v
	}
	return out, nil
}

func marshalSnapshot(op string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(errs.ErrValidation, op, "failed to encode audit snapshot", err)
	}
	return b, nil
}

func newUUID() uuid.UUID {
	if id, err := uuid.NewV7(); err == nil {
		return id
	}
	return uuid.New()
}

// GetTaskMarkdown retrieves the optional extended Markdown document (`<task_id>.md`)
// for an authorized task.
func (s *ServiceSkeleton) GetTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (string, bool, error) {
	const op = "task.Service.GetTaskMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return "", false, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return "", false, err
	}
	if taskID == uuid.Nil {
		return "", false, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return "", false, err
	}
	t, err := s.repo.GetByID(ctx, workspaceID, taskID)
	if err != nil {
		return "", false, err
	}
	if authctx.ShouldEmitTaskSignal(ctx) && t.Status.AcceptsSignal() {
		if _, sigErr := s.recordReadSignal(ctx, caller, t); sigErr != nil {
			return "", false, sigErr
		}
	}
	if s.mdStore == nil {
		return "", t.HasMarkdownDoc, nil
	}
	content, err := s.mdStore.ReadTaskMarkdown(ctx, workspaceID, taskID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return content, true, nil
}

// PutTaskMarkdown writes the extended Markdown document (`<task_id>.md`) and
// sets HasMarkdownDoc = true inside an atomic audited transaction.
func (s *ServiceSkeleton) PutTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID, markdown string) (Task, error) {
	const op = "task.Service.PutTaskMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Task{}, err
	}
	if taskID == uuid.Nil {
		return Task{}, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if len(markdown) > maxTaskMarkdownBytes {
		return Task{}, errs.Validation(op, "markdown document exceeds maximum size", errs.FieldViolation{
			Field:       "markdown",
			Description: "markdown must not exceed 262144 bytes",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Task{}, err
	}
	if s.mdStore == nil {
		return Task{}, errs.New(errs.ErrPreconditionFailed, op, "task markdown store is not configured")
	}

	var updated Task
	var prevHasDoc bool
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()
		current, err := scopedRepo.GetByID(txCtx, workspaceID, taskID)
		if err != nil {
			return err
		}
		if err := ensureLeaseAllowsActor(txCtx, scopedRepo, op, current, caller.ActorID, now); err != nil {
			return err
		}
		prevHasDoc = current.HasMarkdownDoc
		oldSnap, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		next, err := scopedRepo.SetHasMarkdownDoc(txCtx, workspaceID, taskID, true)
		if err != nil {
			return err
		}
		if authctx.ShouldEmitTaskSignal(txCtx) && next.Status.AcceptsSignal() {
			touched, tErr := scopedRepo.TouchSignal(txCtx, workspaceID, taskID, now)
			if tErr != nil && !errors.Is(tErr, errs.ErrNotImplemented) {
				return tErr
			}
			if tErr == nil {
				next = touched
			}
		}
		newSnap, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionUpdate,
			ResourceType: "task",
			ResourceID:   taskID.String(),
			OldValues:    oldSnap,
			NewValues:    newSnap,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Task{}, err
	}

	if err := s.mdStore.WriteTaskMarkdown(context.WithoutCancel(ctx), workspaceID, taskID, markdown); err != nil {
		if !prevHasDoc {
			_ = s.txMgr.WithNoAuditTransaction(context.WithoutCancel(ctx), func(cCtx context.Context, scope tx.Scope) error {
				_, cErr := s.repo.WithScope(scope).SetHasMarkdownDoc(cCtx, workspaceID, taskID, false)
				return cErr
			})
		}
		return Task{}, err
	}
	return updated.Clone(), nil
}

// DeleteTaskMarkdown deletes the extended Markdown document (`<task_id>.md`) and
// clears HasMarkdownDoc inside an atomic audited transaction.
func (s *ServiceSkeleton) DeleteTaskMarkdown(ctx context.Context, workspaceID, taskID uuid.UUID) (Task, error) {
	const op = "task.Service.DeleteTaskMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return Task{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Task{}, err
	}
	if taskID == uuid.Nil {
		return Task{}, errs.Validation(op, "task_id is required", errs.FieldViolation{
			Field:       "task_id",
			Description: "task_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Task{}, err
	}

	var updated Task
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		now := time.Now().UTC()
		current, err := scopedRepo.GetByID(txCtx, workspaceID, taskID)
		if err != nil {
			return err
		}
		if err := ensureLeaseAllowsActor(txCtx, scopedRepo, op, current, caller.ActorID, now); err != nil {
			return err
		}
		oldSnap, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		next, err := scopedRepo.SetHasMarkdownDoc(txCtx, workspaceID, taskID, false)
		if err != nil {
			return err
		}
		if authctx.ShouldEmitTaskSignal(txCtx) && next.Status.AcceptsSignal() {
			touched, tErr := scopedRepo.TouchSignal(txCtx, workspaceID, taskID, now)
			if tErr != nil && !errors.Is(tErr, errs.ErrNotImplemented) {
				return tErr
			}
			if tErr == nil {
				next = touched
			}
		}
		newSnap, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  workspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionUpdate,
			ResourceType: "task",
			ResourceID:   taskID.String(),
			OldValues:    oldSnap,
			NewValues:    newSnap,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Task{}, err
	}

	if s.mdStore != nil {
		if err := s.mdStore.DeleteTaskMarkdown(context.WithoutCancel(ctx), workspaceID, taskID); err != nil {
			return Task{}, err
		}
	}
	return updated.Clone(), nil
}

// PurgeArchivedTasks permanently deletes all tasks in StatusArchived across workspaceID
// and removes their `.md` resource files inside an atomic audited transaction.
func (s *ServiceSkeleton) PurgeArchivedTasks(ctx context.Context, workspaceID uuid.UUID) (PurgeArchivedResult, error) {
	const op = "task.Service.PurgeArchivedTasks"
	if err := checkContext(ctx, op); err != nil {
		return PurgeArchivedResult{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return PurgeArchivedResult{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return PurgeArchivedResult{}, err
	}

	var result PurgeArchivedResult
	var toDeleteTaskIDs []uuid.UUID

	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		allTasks, err := scopedRepo.ListByWorkspace(txCtx, workspaceID)
		if err != nil {
			return err
		}

		var archived []Task
		for _, t := range allTasks {
			if t.Status == StatusArchived {
				archived = append(archived, t)
			}
		}
		slices.SortFunc(archived, func(a, b Task) int {
			return strings.Compare(a.Key, b.Key)
		})

		now := time.Now().UTC()
		for _, t := range archived {
			oldSnap, err := marshalSnapshot(op, t)
			if err != nil {
				return err
			}
			if err := scopedRepo.HardDelete(txCtx, workspaceID, t.ID); err != nil {
				return err
			}
			if err := scope.Audit().Record(txCtx, tx.AuditEntry{
				ID:           newUUID(),
				WorkspaceID:  workspaceID,
				ActorID:      caller.ActorID,
				Action:       tx.AuditActionDelete,
				ResourceType: "task",
				ResourceID:   t.ID.String(),
				OldValues:    oldSnap,
				OccurredAt:   now,
			}); err != nil {
				return err
			}
			result.DeletedTaskCount++
			result.DeletedTaskKeys = append(result.DeletedTaskKeys, t.Key)
			toDeleteTaskIDs = append(toDeleteTaskIDs, t.ID)
		}
		return nil
	}); err != nil {
		return PurgeArchivedResult{}, err
	}

	if s.mdStore != nil {
		for _, tid := range toDeleteTaskIDs {
			hadDoc, _ := s.mdStore.HasTaskMarkdown(ctx, workspaceID, tid)
			if err := s.mdStore.DeleteTaskMarkdown(ctx, workspaceID, tid); err == nil && hadDoc {
				result.DeletedMarkdownCount++
			}
		}
	}
	return result, nil
}

type detectedOrphans struct {
	orphanTaskIDs        []uuid.UUID
	malformedTaskKeys    []string
	missingTaskDocs      []Task
	orphanProjectIDs     []uuid.UUID
	malformedProjectKeys []string
	missingProjectDocs   []ProjectDocEntry
}

func (s *ServiceSkeleton) collectOrphans(ctx context.Context, workspaceID uuid.UUID) (detectedOrphans, error) {
	var out detectedOrphans
	if s.mdStore == nil {
		return out, nil
	}

	tasks, err := s.repo.ListByWorkspace(ctx, workspaceID)
	if err != nil {
		return out, err
	}
	activeTasks := make(map[uuid.UUID]Task, len(tasks))
	for _, t := range tasks {
		activeTasks[t.ID] = t
	}

	validTaskIDs, malformedTasks, err := s.mdStore.ScanTaskFiles(ctx, workspaceID)
	if err != nil {
		return out, err
	}
	out.malformedTaskKeys = malformedTasks
	diskTaskSet := make(map[uuid.UUID]bool, len(validTaskIDs))
	for _, id := range validTaskIDs {
		diskTaskSet[id] = true
		if _, ok := activeTasks[id]; !ok {
			out.orphanTaskIDs = append(out.orphanTaskIDs, id)
		}
	}
	for _, t := range tasks {
		if t.HasMarkdownDoc && !diskTaskSet[t.ID] {
			out.missingTaskDocs = append(out.missingTaskDocs, t)
		}
	}
	slices.SortFunc(out.missingTaskDocs, func(a, b Task) int {
		return strings.Compare(a.Key, b.Key)
	})

	if s.projDocs != nil {
		projs, pErr := s.projDocs.ListProjectDocs(ctx, workspaceID)
		if pErr != nil {
			return out, pErr
		}
		activeProjs := make(map[uuid.UUID]ProjectDocEntry, len(projs))
		for _, p := range projs {
			activeProjs[p.ID] = p
		}

		validProjIDs, malformedProjs, scanErr := s.mdStore.ScanProjectFiles(ctx, workspaceID)
		if scanErr != nil {
			return out, scanErr
		}
		out.malformedProjectKeys = malformedProjs
		diskProjSet := make(map[uuid.UUID]bool, len(validProjIDs))
		for _, id := range validProjIDs {
			diskProjSet[id] = true
			if _, ok := activeProjs[id]; !ok {
				out.orphanProjectIDs = append(out.orphanProjectIDs, id)
			}
		}
		for _, p := range projs {
			if p.HasMarkdownDoc && !diskProjSet[p.ID] {
				out.missingProjectDocs = append(out.missingProjectDocs, p)
			}
		}
		slices.SortFunc(out.missingProjectDocs, func(a, b ProjectDocEntry) int {
			return strings.Compare(a.KeyPrefix, b.KeyPrefix)
		})
	}

	return out, nil
}

func logicalFileBasename(relKey string) string {
	idx := strings.LastIndexByte(relKey, '/')
	if idx >= 0 && idx+1 < len(relKey) {
		return relKey[idx+1:]
	}
	return relKey
}

// InspectOrphans scans workspaceID for `.md` files without an active DB entity and
// active DB entities marked HasMarkdownDoc whose `.md` file is missing on disk.
func (s *ServiceSkeleton) InspectOrphans(ctx context.Context, workspaceID uuid.UUID) (OrphanResourcesReport, error) {
	const op = "task.Service.InspectOrphans"
	if err := checkContext(ctx, op); err != nil {
		return OrphanResourcesReport{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return OrphanResourcesReport{}, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return OrphanResourcesReport{}, err
	}

	found, err := s.collectOrphans(ctx, workspaceID)
	if err != nil {
		return OrphanResourcesReport{}, err
	}

	var rep OrphanResourcesReport
	for _, id := range found.orphanTaskIDs {
		rep.OrphanTaskFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("orphan task markdown file: %s.md (no active DB task)", id))
	}
	for _, rel := range found.malformedTaskKeys {
		rep.OrphanTaskFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("orphan task markdown file (unindexed name): %s", logicalFileBasename(rel)))
	}
	for _, t := range found.missingTaskDocs {
		rep.MissingTaskFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("missing task markdown file on disk for %s (%s)", t.Key, t.ID))
	}
	for _, id := range found.orphanProjectIDs {
		rep.OrphanProjectFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("orphan project markdown file: %s.md (no active DB project)", id))
	}
	for _, rel := range found.malformedProjectKeys {
		rep.OrphanProjectFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("orphan project markdown file (unindexed name): %s", logicalFileBasename(rel)))
	}
	for _, p := range found.missingProjectDocs {
		rep.MissingProjectFilesCount++
		rep.IssueSummaries = append(rep.IssueSummaries, fmt.Sprintf("missing project markdown file on disk for %s (%s)", p.KeyPrefix, p.ID))
	}
	rep.TotalIssues = rep.OrphanTaskFilesCount + rep.MissingTaskFilesCount + rep.OrphanProjectFilesCount + rep.MissingProjectFilesCount
	return rep, nil
}

// PruneOrphans reconciles orphan `.md` files and broken DB HasMarkdownDoc flags
// across workspaceID (running in preview-only mode when dryRun is true).
func (s *ServiceSkeleton) PruneOrphans(ctx context.Context, workspaceID uuid.UUID, dryRun bool) (PruneResourcesResult, error) {
	const op = "task.Service.PruneOrphans"
	if err := checkContext(ctx, op); err != nil {
		return PruneResourcesResult{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return PruneResourcesResult{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return PruneResourcesResult{}, err
	}

	found, err := s.collectOrphans(ctx, workspaceID)
	if err != nil {
		return PruneResourcesResult{}, err
	}

	res := PruneResourcesResult{DryRun: dryRun}

	if !dryRun && (len(found.missingTaskDocs) > 0 || len(found.missingProjectDocs) > 0) {
		if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
			scopedRepo := s.repo.WithScope(scope)
			now := time.Now().UTC()
			for _, t := range found.missingTaskDocs {
				if _, uErr := scopedRepo.SetHasMarkdownDoc(txCtx, workspaceID, t.ID, false); uErr != nil && !errors.Is(uErr, errs.ErrNotFound) {
					return uErr
				}
				if aErr := scope.Audit().Record(txCtx, tx.AuditEntry{
					ID:           newUUID(),
					WorkspaceID:  workspaceID,
					ActorID:      caller.ActorID,
					Action:       tx.AuditActionUpdate,
					ResourceType: "task",
					ResourceID:   t.ID.String(),
					OccurredAt:   now,
				}); aErr != nil {
					return aErr
				}
			}
			if s.projDocs != nil {
				for _, p := range found.missingProjectDocs {
					if cErr := s.projDocs.ClearProjectMarkdownFlag(txCtx, scope, workspaceID, p.ID); cErr != nil && !errors.Is(cErr, errs.ErrNotFound) {
						return cErr
					}
				}
			}
			return nil
		}); err != nil {
			return PruneResourcesResult{}, err
		}
	}

	for _, id := range found.orphanTaskIDs {
		if !dryRun && s.mdStore != nil {
			if err := s.mdStore.DeleteTaskMarkdown(ctx, workspaceID, id); err != nil {
				return PruneResourcesResult{}, err
			}
		}
		res.RemovedTaskFilesCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("removed orphan task markdown: %s.md", id))
	}
	for _, rel := range found.malformedTaskKeys {
		if !dryRun && s.mdStore != nil {
			if err := s.mdStore.RemoveMalformedTaskMarkdown(ctx, rel); err != nil {
				return PruneResourcesResult{}, err
			}
		}
		res.RemovedTaskFilesCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("removed unindexed task markdown: %s", logicalFileBasename(rel)))
	}
	for _, t := range found.missingTaskDocs {
		res.ClearedTaskRefsCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("cleared missing markdown flag on task %s (%s)", t.Key, t.ID))
	}
	for _, id := range found.orphanProjectIDs {
		if !dryRun && s.mdStore != nil {
			if err := s.mdStore.DeleteProjectMarkdown(ctx, workspaceID, id); err != nil {
				return PruneResourcesResult{}, err
			}
		}
		res.RemovedProjectFilesCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("removed orphan project markdown: %s.md", id))
	}
	for _, rel := range found.malformedProjectKeys {
		if !dryRun && s.mdStore != nil {
			if err := s.mdStore.RemoveMalformedProjectMarkdown(ctx, rel); err != nil {
				return PruneResourcesResult{}, err
			}
		}
		res.RemovedProjectFilesCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("removed unindexed project markdown: %s", logicalFileBasename(rel)))
	}
	for _, p := range found.missingProjectDocs {
		res.ClearedProjectRefsCount++
		res.CleanedSummaries = append(res.CleanedSummaries, fmt.Sprintf("cleared missing markdown flag on project %s (%s)", p.KeyPrefix, p.ID))
	}

	res.TotalCleaned = res.RemovedTaskFilesCount + res.ClearedTaskRefsCount + res.RemovedProjectFilesCount + res.ClearedProjectRefsCount
	return res, nil
}
