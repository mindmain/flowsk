// File service.go implements the pure domain application service for managing
// logical Projects inside a Workspace, strictly rejecting host filesystem paths
// and recording atomic audit trails.
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/tx"
)

const (
	defaultListProjectsLimit int32 = 50
	maxListProjectsLimit     int32 = 200
	maxProjectNameRunes            = 120
	maxProjectDescRunes            = 4000
	maxDefaultBranchRunes          = 128
	maxProjectMarkdownBytes        = 262144
)

var (
	projectSlugPattern      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	projectKeyPrefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	windowsDrivePathPattern = regexp.MustCompile(`(?i)(^|[\s"'])[a-z]:[\\/]`)
	scpGitURLPattern        = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// Service defines pure domain operations for managing logical Projects inside a Workspace.
type Service interface {
	// CreateProject validates inputs, rejects any host-local path metadata, and persists
	// a new Project inside an atomic audited transaction.
	CreateProject(ctx context.Context, in CreateProjectInput) (Project, error)

	// GetProject loads a single Project by ID within workspaceID after verifying
	// caller tenant ownership.
	GetProject(ctx context.Context, workspaceID, projectID uuid.UUID) (Project, error)

	// GetProjectBySlug resolves a Project by its workspace-unique slug after verifying
	// caller tenant ownership.
	GetProjectBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (Project, error)

	// ListProjects returns defensive copies of Projects matching filter within the
	// caller's authorized Workspace.
	ListProjects(ctx context.Context, filter ListProjectsFilter) ([]Project, error)

	// UpdateProject updates mutable Project metadata inside an atomic audited transaction.
	UpdateProject(ctx context.Context, in UpdateProjectInput) (Project, error)

	// DeleteProject soft-deletes a Project inside an atomic audited transaction,
	// detaching any project_bindings edges without deleting child sub-projects.
	DeleteProject(ctx context.Context, workspaceID, projectID uuid.UUID) error

	// BindSubproject creates or updates a directed parent->child edge in the
	// multi-binding project DAG after verifying acyclicity and relative subpath safety.
	BindSubproject(ctx context.Context, in BindSubprojectInput) (ProjectBinding, error)

	// UnbindSubproject removes only the parent->child binding edge without deleting
	// either project or its tasks.
	UnbindSubproject(ctx context.Context, in UnbindSubprojectInput) (bool, error)

	// ListBindings returns active parent->child project bindings matching filter.
	ListBindings(ctx context.Context, filter ListBindingsFilter) ([]ProjectBinding, error)

	// GetProjectMarkdown retrieves the optional extended Markdown document (`<project_id>.md`)
	// stored for (workspaceID, projectID).
	GetProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (markdown string, hasDoc bool, err error)

	// PutProjectMarkdown writes the extended Markdown document (`<project_id>.md`) and
	// marks HasMarkdownDoc = true inside an atomic audited transaction.
	PutProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID, markdown string) (Project, error)

	// DeleteProjectMarkdown deletes the extended Markdown document (`<project_id>.md`) and
	// marks HasMarkdownDoc = false inside an atomic audited transaction.
	DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (Project, error)
}

// MarkdownStore abstracts tenant-isolated persistence of optional extended Project
// Markdown documents (`<project_id>.md`) without importing filesystem packages.
type MarkdownStore interface {
	// ReadProjectMarkdown reads the UTF-8 Markdown content for (workspaceID, projectID).
	ReadProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (string, error)

	// WriteProjectMarkdown writes UTF-8 Markdown content for (workspaceID, projectID).
	WriteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID, markdown string) error

	// DeleteProjectMarkdown deletes the Markdown document for (workspaceID, projectID) if present.
	DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) error

	// HasProjectMarkdown reports whether a Markdown document exists for (workspaceID, projectID).
	HasProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (bool, error)
}

// ServiceSkeleton implements Service with strict multi-tenant workspace isolation,
// zero host-path leakage validation, defensive slice copies, and atomic audits.
// The exported type name ServiceSkeleton is retained for cross-package wiring compatibility.
type ServiceSkeleton struct {
	repo    Repository
	txMgr   tx.Manager
	mdStore MarkdownStore
}

// Compile-time check ensuring *ServiceSkeleton implements Service.
var _ Service = (*ServiceSkeleton)(nil)

// NewService constructs a project ServiceSkeleton with resolved Repository and
// tx.Manager dependencies.
func NewService(repo Repository, txMgr tx.Manager) *ServiceSkeleton {
	return &ServiceSkeleton{
		repo:  repo,
		txMgr: txMgr,
	}
}

// WithMarkdownStore attaches an optional MarkdownStore for extended project `.md` resources.
func (s *ServiceSkeleton) WithMarkdownStore(store MarkdownStore) *ServiceSkeleton {
	if s != nil {
		s.mdStore = store
	}
	return s
}

// CreateProject validates inputs, rejects any host-local filesystem paths in
// Name or RemoteRepoURL, and persists a new Project inside an atomic audited transaction.
func (s *ServiceSkeleton) CreateProject(ctx context.Context, in CreateProjectInput) (Project, error) {
	const op = "project.Service.CreateProject"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Project{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Project{}, err
	}

	slug := strings.TrimSpace(in.Slug)
	keyPrefix := strings.TrimSpace(in.KeyPrefix)
	name := strings.TrimSpace(in.Name)
	desc := strings.TrimSpace(in.Description)
	branch := strings.TrimSpace(in.DefaultBranch)

	var violations []errs.FieldViolation
	if len(slug) < 2 || len(slug) > 64 || !projectSlugPattern.MatchString(slug) {
		violations = append(violations, errs.FieldViolation{
			Field:       "slug",
			Description: "slug must be 2-64 lowercase ASCII alphanumeric characters separated by single hyphens",
		})
	}
	if !projectKeyPrefixPattern.MatchString(keyPrefix) {
		violations = append(violations, errs.FieldViolation{
			Field:       "key_prefix",
			Description: "key_prefix must be 2-10 uppercase ASCII alphanumeric characters starting with a letter",
		})
	}
	if violation := validateProjectName(name); violation != nil {
		violations = append(violations, *violation)
	}
	if utf8.RuneCountInString(desc) > maxProjectDescRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 4000 characters",
		})
	}
	remoteURL, remoteViolation := normalizeAndValidateRemoteRepoURL(in.RemoteRepoURL)
	if remoteViolation != nil {
		violations = append(violations, *remoteViolation)
	}
	if branch != "" && (utf8.RuneCountInString(branch) > maxDefaultBranchRunes || containsHostFilesystemPath(branch)) {
		violations = append(violations, errs.FieldViolation{
			Field:       "default_branch",
			Description: "default_branch must be a valid branch name without filesystem paths",
		})
	}
	tags, tagViolation := sanitizeTags(in.Tags)
	if tagViolation != nil {
		violations = append(violations, *tagViolation)
	}
	if len(violations) > 0 {
		return Project{}, errs.Validation(op, "invalid project creation parameters", violations...)
	}

	now := time.Now().UTC()
	proj := Project{
		ID:            newUUID(),
		WorkspaceID:   in.WorkspaceID,
		Slug:          slug,
		KeyPrefix:     keyPrefix,
		Name:          name,
		Description:   desc,
		RemoteRepoURL: remoteURL,
		DefaultBranch: branch,
		Status:        StatusActive,
		Tags:          tags,
		TaskSeq:       0,
		CreatedBy:     caller.ActorID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if err := scopedRepo.Create(ctx, proj.Clone()); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, proj)
		if err != nil {
			return err
		}
		return scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "project",
			ResourceID:   proj.ID.String(),
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	}); err != nil {
		return Project{}, err
	}

	return proj.Clone(), nil
}

// GetProject loads a single Project by ID within workspaceID after verifying
// caller tenant ownership.
func (s *ServiceSkeleton) GetProject(ctx context.Context, workspaceID, projectID uuid.UUID) (Project, error) {
	const op = "project.Service.GetProject"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Project{}, err
	}
	if projectID == uuid.Nil {
		return Project{}, errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return Project{}, err
	}

	p, err := s.repo.GetByID(ctx, workspaceID, projectID)
	if err != nil {
		return Project{}, err
	}
	return p.Clone(), nil
}

// GetProjectBySlug resolves a Project by its workspace-unique slug after verifying
// caller tenant ownership.
func (s *ServiceSkeleton) GetProjectBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (Project, error) {
	const op = "project.Service.GetProjectBySlug"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return Project{}, err
	}
	normalized := strings.TrimSpace(slug)
	if normalized == "" {
		return Project{}, errs.Validation(op, "slug is required", errs.FieldViolation{
			Field:       "slug",
			Description: "slug must not be empty",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return Project{}, err
	}

	p, err := s.repo.GetBySlug(ctx, workspaceID, normalized)
	if err != nil {
		return Project{}, err
	}
	return p.Clone(), nil
}

// ListProjects returns defensive copies of Projects matching filter within the
// caller's authorized Workspace.
func (s *ServiceSkeleton) ListProjects(ctx context.Context, filter ListProjectsFilter) ([]Project, error) {
	const op = "project.Service.ListProjects"
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
	if filter.Status != nil && !filter.Status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "status filter must be active, paused, or archived",
		})
	}
	if filter.Offset < 0 {
		violations = append(violations, errs.FieldViolation{
			Field:       "offset",
			Description: "offset must be >= 0",
		})
	}
	if len(violations) > 0 {
		return nil, errs.Validation(op, "invalid project list filter", violations...)
	}

	normalized := filter
	normalized.Tag = strings.TrimSpace(filter.Tag)
	if normalized.Limit <= 0 {
		normalized.Limit = defaultListProjectsLimit
	} else if normalized.Limit > maxListProjectsLimit {
		normalized.Limit = maxListProjectsLimit
	}
	if filter.Status != nil {
		st := *filter.Status
		normalized.Status = &st
	}

	items, err := s.repo.List(ctx, normalized)
	if err != nil {
		return nil, err
	}
	out := make([]Project, len(items))
	for i := range items {
		out[i] = items[i].Clone()
	}
	return out, nil
}

// UpdateProject updates mutable Project metadata inside an atomic audited transaction.
func (s *ServiceSkeleton) UpdateProject(ctx context.Context, in UpdateProjectInput) (Project, error) {
	const op = "project.Service.UpdateProject"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return Project{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Project{}, err
	}

	var violations []errs.FieldViolation
	if in.ProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	var nextName string
	if in.Name != nil {
		nextName = strings.TrimSpace(*in.Name)
		if violation := validateProjectName(nextName); violation != nil {
			violations = append(violations, *violation)
		}
	}
	if in.Description != nil && utf8.RuneCountInString(strings.TrimSpace(*in.Description)) > maxProjectDescRunes {
		violations = append(violations, errs.FieldViolation{
			Field:       "description",
			Description: "description must not exceed 4000 characters",
		})
	}
	var nextRemoteURL string
	if in.RemoteRepoURL != nil {
		var remoteViolation *errs.FieldViolation
		nextRemoteURL, remoteViolation = normalizeAndValidateRemoteRepoURL(*in.RemoteRepoURL)
		if remoteViolation != nil {
			violations = append(violations, *remoteViolation)
		}
	}
	if in.DefaultBranch != nil {
		br := strings.TrimSpace(*in.DefaultBranch)
		if br != "" && (utf8.RuneCountInString(br) > maxDefaultBranchRunes || containsHostFilesystemPath(br)) {
			violations = append(violations, errs.FieldViolation{
				Field:       "default_branch",
				Description: "default_branch must not contain filesystem paths or exceed 128 characters",
			})
		}
	}
	if in.Status != nil && !in.Status.Valid() {
		violations = append(violations, errs.FieldViolation{
			Field:       "status",
			Description: "status must be active, paused, or archived",
		})
	}
	var nextTags []string
	if in.Tags != nil {
		var tagViolation *errs.FieldViolation
		nextTags, tagViolation = sanitizeTags(in.Tags)
		if tagViolation != nil {
			violations = append(violations, *tagViolation)
		}
	}
	if len(violations) > 0 {
		return Project{}, errs.Validation(op, "invalid project update parameters", violations...)
	}

	var updated Project
	if err := s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, in.WorkspaceID, in.ProjectID)
		if err != nil {
			return err
		}
		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		next := current.Clone()
		action := tx.AuditActionUpdate

		if in.Name != nil {
			next.Name = nextName
		}
		if in.Description != nil {
			next.Description = strings.TrimSpace(*in.Description)
		}
		if in.RemoteRepoURL != nil {
			next.RemoteRepoURL = nextRemoteURL
		}
		if in.DefaultBranch != nil {
			next.DefaultBranch = strings.TrimSpace(*in.DefaultBranch)
		}
		if in.Status != nil {
			if next.Status != *in.Status {
				action = tx.AuditActionTransition
			}
			next.Status = *in.Status
		}
		if in.Tags != nil {
			next.Tags = nextTags
		}
		next.UpdatedAt = now

		if err := scopedRepo.Update(ctx, next.Clone()); err != nil {
			return err
		}
		newSnapshot, err := marshalSnapshot(op, next)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(ctx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       action,
			ResourceType: "project",
			ResourceID:   in.ProjectID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Project{}, err
	}

	return updated.Clone(), nil
}

// DeleteProject soft-deletes a Project inside an atomic audited transaction.
func (s *ServiceSkeleton) DeleteProject(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	const op = "project.Service.DeleteProject"
	if err := checkContext(ctx, op); err != nil {
		return err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return err
	}
	if projectID == uuid.Nil {
		return errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return err
	}

	return s.txMgr.WithTransaction(ctx, func(ctx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(ctx, workspaceID, projectID)
		if err != nil {
			return err
		}
		oldSnapshot, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := scopedRepo.DeleteBindingsForProject(ctx, workspaceID, projectID); err != nil {
			return err
		}
		if err := scopedRepo.SoftDelete(ctx, workspaceID, projectID); err != nil {
			return err
		}
		if s.mdStore != nil {
			_ = s.mdStore.DeleteProjectMarkdown(ctx, workspaceID, projectID)
		}
		deleted := current.Clone()
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
			ResourceType: "project",
			ResourceID:   projectID.String(),
			OldValues:    oldSnapshot,
			NewValues:    newSnapshot,
			OccurredAt:   now,
		})
	})
}

// BindSubproject links a child sub-project under a macro/parent project inside
// an atomic audited transaction, enforcing multi-binding DAG acyclicity and
// ensuring ScopeSubpath never contains absolute or traversal host paths.
func (s *ServiceSkeleton) BindSubproject(ctx context.Context, in BindSubprojectInput) (ProjectBinding, error) {
	const op = "project.Service.BindSubproject"
	if err := checkContext(ctx, op); err != nil {
		return ProjectBinding{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return ProjectBinding{}, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return ProjectBinding{}, err
	}

	var violations []errs.FieldViolation
	if in.ParentProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{Field: "parent_project_id", Description: "parent_project_id must be a non-nil UUID"})
	}
	if in.ChildProjectID == uuid.Nil {
		violations = append(violations, errs.FieldViolation{Field: "child_project_id", Description: "child_project_id must be a non-nil UUID"})
	}
	if in.ParentProjectID != uuid.Nil && in.ParentProjectID == in.ChildProjectID {
		violations = append(violations, errs.FieldViolation{Field: "child_project_id", Description: "a project cannot be bound as a sub-project of itself"})
	}
	subpath, subViolation := normalizeRelativeSubpath(in.ScopeSubpath)
	if subViolation != nil {
		violations = append(violations, *subViolation)
	}
	if len(violations) > 0 {
		return ProjectBinding{}, errs.Validation(op, "invalid sub-project binding parameters", violations...)
	}

	var persisted ProjectBinding
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		if _, err := scopedRepo.GetByID(txCtx, in.WorkspaceID, in.ParentProjectID); err != nil {
			return err
		}
		if _, err := scopedRepo.GetByID(txCtx, in.WorkspaceID, in.ChildProjectID); err != nil {
			return err
		}

		// Verify DAG acyclicity: ensure ParentProjectID is not reachable from ChildProjectID.
		allEdges, err := scopedRepo.ListBindings(txCtx, ListBindingsFilter{WorkspaceID: in.WorkspaceID})
		if err != nil {
			return err
		}
		adj := make(map[uuid.UUID][]uuid.UUID)
		for _, e := range allEdges {
			adj[e.ParentProjectID] = append(adj[e.ParentProjectID], e.ChildProjectID)
		}
		visited := map[uuid.UUID]bool{in.ChildProjectID: true}
		queue := []uuid.UUID{in.ChildProjectID}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur == in.ParentProjectID {
				return errs.New(errs.ErrConflict, op, "binding would introduce a cycle in the project hierarchy")
			}
			for _, nxt := range adj[cur] {
				if !visited[nxt] {
					visited[nxt] = true
					queue = append(queue, nxt)
				}
			}
		}

		now := time.Now().UTC()
		candidate := ProjectBinding{
			ID:              newUUID(),
			WorkspaceID:     in.WorkspaceID,
			ParentProjectID: in.ParentProjectID,
			ChildProjectID:  in.ChildProjectID,
			ScopeSubpath:    subpath,
			CreatedBy:       caller.ActorID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		saved, err := scopedRepo.UpsertBinding(txCtx, candidate)
		if err != nil {
			return err
		}
		snap, err := marshalSnapshot(op, saved)
		if err != nil {
			return err
		}
		if err := scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionCreate,
			ResourceType: "project_binding",
			ResourceID:   saved.ID.String(),
			NewValues:    snap,
			OccurredAt:   now,
		}); err != nil {
			return err
		}
		persisted = saved
		return nil
	}); err != nil {
		return ProjectBinding{}, err
	}
	return persisted, nil
}

// UnbindSubproject removes only the parent->child binding edge inside an atomic
// audited transaction without deleting either project or its tasks.
func (s *ServiceSkeleton) UnbindSubproject(ctx context.Context, in UnbindSubprojectInput) (bool, error) {
	const op = "project.Service.UnbindSubproject"
	if err := checkContext(ctx, op); err != nil {
		return false, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, in.WorkspaceID)
	if err != nil {
		return false, err
	}
	if err := s.ensureWriteReady(op); err != nil {
		return false, err
	}
	if in.ParentProjectID == uuid.Nil || in.ChildProjectID == uuid.Nil {
		return false, errs.Validation(op, "parent_project_id and child_project_id are required")
	}

	var removed bool
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		ok, err := scopedRepo.DeleteBinding(txCtx, in.WorkspaceID, in.ParentProjectID, in.ChildProjectID)
		if err != nil {
			return err
		}
		removed = ok
		if !ok {
			return nil
		}
		snap, _ := marshalSnapshot(op, in)
		return scope.Audit().Record(txCtx, tx.AuditEntry{
			ID:           newUUID(),
			WorkspaceID:  in.WorkspaceID,
			ActorID:      caller.ActorID,
			Action:       tx.AuditActionDelete,
			ResourceType: "project_binding",
			ResourceID:   in.ParentProjectID.String() + ":" + in.ChildProjectID.String(),
			OldValues:    snap,
			OccurredAt:   time.Now().UTC(),
		})
	}); err != nil {
		return false, err
	}
	return removed, nil
}

// ListBindings returns active parent->child project bindings within filter.WorkspaceID.
func (s *ServiceSkeleton) ListBindings(ctx context.Context, filter ListBindingsFilter) ([]ProjectBinding, error) {
	const op = "project.Service.ListBindings"
	if err := checkContext(ctx, op); err != nil {
		return nil, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, filter.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.ensureReadReady(op); err != nil {
		return nil, err
	}
	return s.repo.ListBindings(ctx, filter)
}

// GetProjectMarkdown retrieves the optional extended Markdown document (`<project_id>.md`)
// for an authorized project.
func (s *ServiceSkeleton) GetProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (string, bool, error) {
	const op = "project.Service.GetProjectMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return "", false, err
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return "", false, err
	}
	if projectID == uuid.Nil {
		return "", false, errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if err := s.ensureReadReady(op); err != nil {
		return "", false, err
	}
	proj, err := s.repo.GetByID(ctx, workspaceID, projectID)
	if err != nil {
		return "", false, err
	}
	if s.mdStore == nil {
		return "", proj.HasMarkdownDoc, nil
	}
	content, err := s.mdStore.ReadProjectMarkdown(ctx, workspaceID, projectID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return content, true, nil
}

// PutProjectMarkdown writes the extended Markdown document (`<project_id>.md`) and
// marks HasMarkdownDoc = true inside an atomic audited transaction.
func (s *ServiceSkeleton) PutProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID, markdown string) (Project, error) {
	const op = "project.Service.PutProjectMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Project{}, err
	}
	if projectID == uuid.Nil {
		return Project{}, errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if len(markdown) > maxProjectMarkdownBytes {
		return Project{}, errs.Validation(op, "markdown document exceeds maximum size", errs.FieldViolation{
			Field:       "markdown",
			Description: "markdown must not exceed 262144 bytes",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Project{}, err
	}
	if s.mdStore == nil {
		return Project{}, errs.New(errs.ErrPreconditionFailed, op, "project markdown store is not configured")
	}

	var updated Project
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(txCtx, workspaceID, projectID)
		if err != nil {
			return err
		}
		oldSnap, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		next, err := scopedRepo.SetHasMarkdownDoc(txCtx, workspaceID, projectID, true)
		if err != nil {
			return err
		}
		if err := s.mdStore.WriteProjectMarkdown(txCtx, workspaceID, projectID, markdown); err != nil {
			return err
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
			ResourceType: "project",
			ResourceID:   projectID.String(),
			OldValues:    oldSnap,
			NewValues:    newSnap,
			OccurredAt:   time.Now().UTC(),
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Project{}, err
	}
	return updated.Clone(), nil
}

// DeleteProjectMarkdown deletes the extended Markdown document (`<project_id>.md`) and
// clears HasMarkdownDoc inside an atomic audited transaction.
func (s *ServiceSkeleton) DeleteProjectMarkdown(ctx context.Context, workspaceID, projectID uuid.UUID) (Project, error) {
	const op = "project.Service.DeleteProjectMarkdown"
	if err := checkContext(ctx, op); err != nil {
		return Project{}, err
	}
	caller, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID)
	if err != nil {
		return Project{}, err
	}
	if projectID == uuid.Nil {
		return Project{}, errs.Validation(op, "project_id is required", errs.FieldViolation{
			Field:       "project_id",
			Description: "project_id must be a non-nil UUID",
		})
	}
	if err := s.ensureWriteReady(op); err != nil {
		return Project{}, err
	}

	var updated Project
	if err := s.txMgr.WithTransaction(ctx, func(txCtx context.Context, scope tx.Scope) error {
		scopedRepo := s.repo.WithScope(scope)
		current, err := scopedRepo.GetByID(txCtx, workspaceID, projectID)
		if err != nil {
			return err
		}
		oldSnap, err := marshalSnapshot(op, current)
		if err != nil {
			return err
		}
		next, err := scopedRepo.SetHasMarkdownDoc(txCtx, workspaceID, projectID, false)
		if err != nil {
			return err
		}
		if s.mdStore != nil {
			if err := s.mdStore.DeleteProjectMarkdown(txCtx, workspaceID, projectID); err != nil {
				return err
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
			ResourceType: "project",
			ResourceID:   projectID.String(),
			OldValues:    oldSnap,
			NewValues:    newSnap,
			OccurredAt:   time.Now().UTC(),
		}); err != nil {
			return err
		}
		updated = next
		return nil
	}); err != nil {
		return Project{}, err
	}
	return updated.Clone(), nil
}

func normalizeRelativeSubpath(raw string) (string, *errs.FieldViolation) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	if containsHostFilesystemPath(trimmed) || strings.Contains(trimmed, `\`) {
		return "", &errs.FieldViolation{
			Field:       "scope_subpath",
			Description: "scope_subpath must be a clean relative subpath (e.g. 'apps/web'), never a host path",
		}
	}
	trimmed = strings.Trim(trimmed, "/")
	if utf8.RuneCountInString(trimmed) > 256 {
		return "", &errs.FieldViolation{
			Field:       "scope_subpath",
			Description: "scope_subpath must not exceed 256 characters",
		}
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", &errs.FieldViolation{
				Field:       "scope_subpath",
				Description: "scope_subpath must not contain empty or '..' traversal segments",
			}
		}
	}
	return trimmed, nil
}

func (s *ServiceSkeleton) ensureReadReady(op string) error {
	if s == nil || s.repo == nil {
		return errs.New(errs.ErrPreconditionFailed, op, "project repository is not initialized")
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

func validateProjectName(name string) *errs.FieldViolation {
	if name == "" || utf8.RuneCountInString(name) > maxProjectNameRunes {
		return &errs.FieldViolation{
			Field:       "name",
			Description: "name must be between 1 and 120 characters",
		}
	}
	if containsHostFilesystemPath(name) {
		return &errs.FieldViolation{
			Field:       "name",
			Description: "project name must never contain local host filesystem paths",
		}
	}
	return nil
}

func normalizeAndValidateRemoteRepoURL(raw string) (string, *errs.FieldViolation) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if containsHostFilesystemPath(trimmed) {
		return "", &errs.FieldViolation{
			Field:       "remote_repo_url",
			Description: "remote_repo_url must not be a local filesystem or file:// path",
		}
	}
	if scpGitURLPattern.MatchString(trimmed) {
		return trimmed, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", &errs.FieldViolation{
			Field:       "remote_repo_url",
			Description: "remote_repo_url must be a canonical remote VCS URL (https, ssh, or git)",
		}
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "ssh", "git":
	default:
		return "", &errs.FieldViolation{
			Field:       "remote_repo_url",
			Description: fmt.Sprintf("unsupported remote_repo_url scheme %q; local paths are forbidden", parsed.Scheme),
		}
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return "", &errs.FieldViolation{
			Field:       "remote_repo_url",
			Description: "remote_repo_url must include a repository path",
		}
	}
	return trimmed, nil
}

func containsHostFilesystemPath(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(trimmed, "/") ||
		strings.HasPrefix(trimmed, `\`) ||
		trimmed == "~" ||
		strings.HasPrefix(trimmed, "~/") ||
		strings.HasPrefix(trimmed, `~\`) ||
		strings.HasPrefix(trimmed, "./") ||
		strings.HasPrefix(trimmed, "../") ||
		strings.HasPrefix(trimmed, `.\`) ||
		strings.HasPrefix(trimmed, `..\`) {
		return true
	}
	if strings.HasPrefix(lower, "file:") || strings.Contains(lower, "file://") {
		return true
	}
	if windowsDrivePathPattern.MatchString(trimmed) {
		return true
	}
	if strings.Contains(lower, "/users/") ||
		strings.Contains(lower, `\users\`) ||
		strings.Contains(lower, "/home/") {
		return true
	}
	return false
}

func sanitizeTags(tags []string) ([]string, *errs.FieldViolation) {
	if len(tags) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, &errs.FieldViolation{
				Field:       "tags",
				Description: "project tags must not contain empty strings",
			}
		}
		out = append(out, trimmed)
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
