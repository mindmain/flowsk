// File repositories.go implements all 5 concrete PostgreSQL domain repositories
// (`WorkspaceRepository`, `ProjectRepository`, `ObjectiveRepository`,
// `TaskRepository`, and `AuditRepository`) strictly over `postgres/sqlcgen`.
package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/postgres/sqlcgen"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

// Compile-time repository interface checks.
var (
	_ workspace.Repository            = (*WorkspaceRepository)(nil)
	_ project.Repository              = (*ProjectRepository)(nil)
	_ project.BoardMetadataRepository = (*ProjectRepository)(nil)
	_ objective.Repository            = (*ObjectiveRepository)(nil)
	_ task.Repository                 = (*TaskRepository)(nil)
	_ audit.Repository                = (*AuditRepository)(nil)
)

// resolvePostgresQuerier returns either the active `pgx.Tx` Querier or lazily opens the pool Querier.
func resolvePostgresQuerier(ctx context.Context, op string, adapter *Adapter, scoped bool, scope tx.Scope) (sqlcgen.Querier, error) {
	if scoped {
		return ResolveScopeQuerier(nil, scope)
	}
	_, bridge, err := adapter.ensurePool(ctx, op)
	if err != nil {
		return nil, err
	}
	return bridge.Queries(), nil
}

// ensurePostgresActorRow guarantees a valid `actors` row exists inside workspaceID for principalActorID.
func ensurePostgresActorRow(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID, principalActorID uuid.UUID,
	hintKind authctx.ActorKind,
	hintHandle string,
) (*uuid.UUID, error) {
	if principalActorID == uuid.Nil {
		return nil, nil
	}
	if _, err := q.GetActorByID(ctx, sqlcgen.GetActorByIDParams{WorkspaceID: workspaceID, ID: principalActorID}); err == nil {
		idCopy := principalActorID
		return &idCopy, nil
	}
	scopedID := db.ScopedActorRowID(workspaceID, principalActorID)
	if _, err := q.GetActorByID(ctx, sqlcgen.GetActorByIDParams{WorkspaceID: workspaceID, ID: scopedID}); err == nil {
		return &scopedID, nil
	}

	if existingActors, listErr := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: workspaceID}); listErr == nil {
		for _, a := range existingActors {
			meta := db.DecodeActorCapabilities(a.CapabilitiesJson)
			if meta.PrincipalActorID == principalActorID.String() {
				idCopy := a.ID
				return &idCopy, nil
			}
		}
	}

	kindStr := string(hintKind)
	if !hintKind.Valid() {
		kindStr = string(authctx.ActorKindHuman)
	}
	display := strings.TrimSpace(hintHandle)
	if display == "" {
		display = "system-actor"
	}
	caps := db.EncodeActorCapabilities(db.ActorCapabilitiesMeta{
		PrincipalActorID: principalActorID.String(),
		MemberKind:       authctx.ActorKind(kindStr),
		IsStub:           true,
	})

	for _, candidateID := range []uuid.UUID{principalActorID, scopedID, uuid.New()} {
		exists, existsErr := q.CheckActorIDExists(ctx, candidateID)
		if existsErr != nil {
			return nil, existsErr
		}
		if exists {
			continue
		}
		created, createErr := q.CreateActor(ctx, sqlcgen.CreateActorParams{
			ID:               candidateID,
			WorkspaceID:      workspaceID,
			Handle:           db.StubActorHandlePrefix + candidateID.String(),
			DisplayName:      display,
			Kind:             kindStr,
			HarnessHint:      "",
			CapabilitiesJson: caps,
		})
		if createErr == nil {
			idCopy := created.ID
			return &idCopy, nil
		}
		if !db.IsPrimaryKeyConflict(createErr) {
			return nil, createErr
		}
	}
	return nil, errs.New(errs.ErrConflict, "postgres.ensureActorRow", "unable to allocate unique actor row ID")
}

func buildPostgresActorLookup(ctx context.Context, q sqlcgen.Querier, workspaceID uuid.UUID) map[uuid.UUID]sqlcgen.Actor {
	rows, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil
	}
	out := make(map[uuid.UUID]sqlcgen.Actor, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func resolvePostgresPrincipalFromActorMap(actorMap map[uuid.UUID]sqlcgen.Actor, rowIDPtr *uuid.UUID) (uuid.UUID, authctx.ActorKind, string) {
	if rowIDPtr == nil || *rowIDPtr == uuid.Nil {
		return uuid.Nil, "", ""
	}
	if row, ok := actorMap[*rowIDPtr]; ok {
		meta := db.DecodeActorCapabilities(row.CapabilitiesJson)
		principalID := db.ResolvePrincipalActorID(row.ID, meta)
		kind := authctx.ActorKind(row.Kind)
		if meta.MemberKind.Valid() {
			kind = meta.MemberKind
		}
		handle := row.Handle
		if meta.IsStub && strings.HasPrefix(handle, db.StubActorHandlePrefix) {
			handle = ""
		}
		return principalID, kind, handle
	}
	return *rowIDPtr, "", ""
}

// =============================================================================
// 1. WORKSPACE REPOSITORY (PostgreSQL)
// =============================================================================

// WorkspaceRepository implements `workspace.Repository` over PostgreSQL sqlc queries.
type WorkspaceRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of WorkspaceRepository bound to scope.
func (r *WorkspaceRepository) WithScope(scope tx.Scope) workspace.Repository {
	return &WorkspaceRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapPostgresWorkspace(row sqlcgen.Workspace) workspace.Workspace {
	desc, ttl, labels := db.DecodeWorkspaceDescription(row.Description)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		t := row.DeletedAt.UTC()
		deletedAt = &t
	}
	return workspace.Workspace{
		ID:                     row.ID,
		Slug:                   row.Slug,
		Name:                   row.Name,
		Description:            desc,
		DefaultLeaseTTLSeconds: ttl,
		Labels:                 labels,
		CreatedAt:              row.CreatedAt.UTC(),
		UpdatedAt:              row.UpdatedAt.UTC(),
		DeletedAt:              deletedAt,
	}
}

// Create inserts a newly provisioned Workspace row.
func (r *WorkspaceRepository) Create(ctx context.Context, ws workspace.Workspace) error {
	const op = "postgres.WorkspaceRepository.Create"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	wsID := ws.ID
	if wsID == uuid.Nil {
		wsID = uuid.New()
	}
	_, err = q.CreateWorkspace(ctx, sqlcgen.CreateWorkspaceParams{
		ID:          wsID,
		Slug:        ws.Slug,
		Name:        ws.Name,
		Description: db.EncodeWorkspaceDescription(ws),
	})
	return db.MapSQLDriverError(op, err)
}

// GetByID fetches an active Workspace by its primary UUID.
func (r *WorkspaceRepository) GetByID(ctx context.Context, workspaceID uuid.UUID) (workspace.Workspace, error) {
	const op = "postgres.WorkspaceRepository.GetByID"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return workspace.Workspace{}, err
	}
	row, err := q.GetWorkspaceByID(ctx, workspaceID)
	if err != nil {
		return workspace.Workspace{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresWorkspace(row), nil
}

// GetBySlug fetches an active Workspace by its unique slug.
func (r *WorkspaceRepository) GetBySlug(ctx context.Context, slug string) (workspace.Workspace, error) {
	const op = "postgres.WorkspaceRepository.GetBySlug"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return workspace.Workspace{}, err
	}
	row, err := q.GetWorkspaceBySlug(ctx, slug)
	if err != nil {
		return workspace.Workspace{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresWorkspace(row), nil
}

// ListForActor returns all active Workspaces where actorID holds membership, ordered by Slug ASC.
func (r *WorkspaceRepository) ListForActor(ctx context.Context, actorID uuid.UUID) ([]workspace.Workspace, error) {
	const op = "postgres.WorkspaceRepository.ListForActor"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	allWS, err := q.ListWorkspaces(ctx)
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	var matched []workspace.Workspace
	for _, wsRow := range allWS {
		actors, lErr := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: wsRow.ID})
		if lErr != nil {
			return nil, db.MapSQLDriverError(op, lErr)
		}
		for _, a := range actors {
			meta := db.DecodeActorCapabilities(a.CapabilitiesJson)
			if meta.IsStub {
				continue
			}
			if db.ResolvePrincipalActorID(a.ID, meta) == actorID {
				matched = append(matched, mapPostgresWorkspace(wsRow))
				break
			}
		}
	}
	slices.SortFunc(matched, func(a, b workspace.Workspace) int {
		return cmp.Compare(a.Slug, b.Slug)
	})
	return matched, nil
}

// Update persists mutable metadata changes to an existing active Workspace.
func (r *WorkspaceRepository) Update(ctx context.Context, ws workspace.Workspace) error {
	const op = "postgres.WorkspaceRepository.Update"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	encodedDesc := db.EncodeWorkspaceDescription(ws)
	_, err = q.UpdateWorkspace(ctx, sqlcgen.UpdateWorkspaceParams{
		Slug:        &ws.Slug,
		Name:        &ws.Name,
		Description: &encodedDesc,
		ID:          ws.ID,
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks the target Workspace as soft-deleted.
func (r *WorkspaceRepository) SoftDelete(ctx context.Context, workspaceID uuid.UUID) error {
	const op = "postgres.WorkspaceRepository.SoftDelete"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rows, err := q.SoftDeleteWorkspace(ctx, workspaceID)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "workspace not found or already soft-deleted")
	}
	return nil
}

// UpsertMember inserts or updates a WorkspaceMember scoped strictly to m.WorkspaceID.
func (r *WorkspaceRepository) UpsertMember(ctx context.Context, m workspace.WorkspaceMember) error {
	const op = "postgres.WorkspaceRepository.UpsertMember"
	if m.WorkspaceID == uuid.Nil || !m.Kind.Valid() || strings.TrimSpace(m.Handle) == "" {
		return errs.Validation(op, "invalid workspace member fields", errs.FieldViolation{
			Field:       "member",
			Description: "workspace_id, valid kind, and non-empty handle are required",
		})
	}
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}

	actorID := m.ActorID
	if actorID == uuid.Nil {
		actorID = uuid.New()
	}

	if byHandle, hErr := q.GetActorByHandle(ctx, sqlcgen.GetActorByHandleParams{
		WorkspaceID: m.WorkspaceID,
		Handle:      m.Handle,
	}); hErr == nil {
		hMeta := db.DecodeActorCapabilities(byHandle.CapabilitiesJson)
		if db.ResolvePrincipalActorID(byHandle.ID, hMeta) != actorID {
			return errs.New(errs.ErrConflict, op, "actor handle already belongs to another workspace member")
		}
	} else if !errors.Is(hErr, pgx.ErrNoRows) {
		return db.MapSQLDriverError(op, hErr)
	}

	display := m.DisplayName
	hasRawDisplay := false
	if strings.TrimSpace(display) == "" {
		display = m.Handle
		hasRawDisplay = true
	}
	caps := db.EncodeActorCapabilities(db.ActorCapabilitiesMeta{
		PrincipalActorID:  actorID.String(),
		Role:              m.Role,
		MemberKind:        m.Kind,
		RawDisplayName:    m.DisplayName,
		HasRawDisplayName: hasRawDisplay,
		IsStub:            false,
	})

	existingActors, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: m.WorkspaceID})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	for _, ex := range existingActors {
		exMeta := db.DecodeActorCapabilities(ex.CapabilitiesJson)
		if db.ResolvePrincipalActorID(ex.ID, exMeta) == actorID {
			_, updErr := q.UpdateActor(ctx, sqlcgen.UpdateActorParams{
				Handle:           &m.Handle,
				DisplayName:      &display,
				CapabilitiesJson: caps,
				WorkspaceID:      m.WorkspaceID,
				ID:               ex.ID,
			})
			return db.MapSQLDriverError(op, updErr)
		}
	}

	scopedID := db.ScopedActorRowID(m.WorkspaceID, actorID)
	for _, candidateID := range []uuid.UUID{actorID, scopedID, uuid.New()} {
		exists, existsErr := q.CheckActorIDExists(ctx, candidateID)
		if existsErr != nil {
			return db.MapSQLDriverError(op, existsErr)
		}
		if exists {
			continue
		}
		_, createErr := q.CreateActor(ctx, sqlcgen.CreateActorParams{
			ID:               candidateID,
			WorkspaceID:      m.WorkspaceID,
			Handle:           m.Handle,
			DisplayName:      display,
			Kind:             string(m.Kind),
			HarnessHint:      "",
			CapabilitiesJson: caps,
		})
		if createErr == nil {
			return nil
		}
		if !db.IsPrimaryKeyConflict(createErr) {
			return db.MapSQLDriverError(op, createErr)
		}
	}
	return errs.New(errs.ErrConflict, op, "unable to allocate unique member row identifier")
}

// ListMembers returns all non-stub members belonging to workspaceID ordered by Handle ASC.
func (r *WorkspaceRepository) ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]workspace.WorkspaceMember, error) {
	const op = "postgres.WorkspaceRepository.ListMembers"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: workspaceID})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	out := make([]workspace.WorkspaceMember, 0, len(rows))
	for _, row := range rows {
		meta := db.DecodeActorCapabilities(row.CapabilitiesJson)
		if meta.IsStub {
			continue
		}
		kind := authctx.ActorKind(row.Kind)
		if meta.MemberKind.Valid() {
			kind = meta.MemberKind
		}
		display := row.DisplayName
		if meta.HasRawDisplayName {
			display = meta.RawDisplayName
		}
		role := meta.Role
		if role == "" {
			role = workspace.MemberRoleContributor
		}
		out = append(out, workspace.WorkspaceMember{
			WorkspaceID: workspaceID,
			ActorID:     db.ResolvePrincipalActorID(row.ID, meta),
			Kind:        kind,
			Handle:      row.Handle,
			DisplayName: display,
			Role:        role,
			CreatedAt:   row.CreatedAt.UTC(),
		})
	}
	slices.SortFunc(out, func(a, b workspace.WorkspaceMember) int {
		return cmp.Compare(a.Handle, b.Handle)
	})
	return out, nil
}

// =============================================================================
// 2. PROJECT REPOSITORY (PostgreSQL)
// =============================================================================

// ProjectRepository implements `project.Repository` over PostgreSQL sqlc queries.
type ProjectRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of ProjectRepository bound to scope.
func (r *ProjectRepository) WithScope(scope tx.Scope) project.Repository {
	return &ProjectRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func seedPostgresProjectWorkflowStates(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID) error {
	existing, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	for _, seed := range db.DefaultWorkflowStateSeeds() {
		if _, err := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
			ID:          uuid.New(),
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Name:        seed.Name,
			Category:    seed.Category,
			Position:    seed.Position,
			Color:       seed.Color,
			IsDefault:   seed.IsDefault,
		}); err != nil {
			return err
		}
	}
	return nil
}

func mapPostgresProject(row sqlcgen.Project, actorMap map[uuid.UUID]sqlcgen.Actor) project.Project {
	desc, meta := db.DecodeProjectDescription(row.Description)
	createdBy, _, _ := resolvePostgresPrincipalFromActorMap(actorMap, row.CreatedByActorID)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		t := row.DeletedAt.UTC()
		deletedAt = &t
	}
	return project.Project{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		Slug:           row.Slug,
		KeyPrefix:      row.KeyPrefix,
		Name:           row.Name,
		Description:    desc,
		RemoteRepoURL:  meta.RemoteRepoURL,
		DefaultBranch:  meta.DefaultBranch,
		Status:         project.Status(row.Status),
		Tags:           meta.Tags,
		TaskSeq:        row.IssueCounter,
		HasMarkdownDoc: row.HasMarkdownDoc,
		CreatedBy:      createdBy,
		CreatedAt:      row.CreatedAt.UTC(),
		UpdatedAt:      row.UpdatedAt.UTC(),
		DeletedAt:      deletedAt,
	}
}

// Create inserts a new Project and seeds standard `workflow_states`.
func (r *ProjectRepository) Create(ctx context.Context, p project.Project) error {
	const op = "postgres.ProjectRepository.Create"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	projID := p.ID
	if projID == uuid.Nil {
		projID = uuid.New()
	}
	status := p.Status
	if status == "" {
		status = project.StatusActive
	}
	createdByPtr, err := ensurePostgresActorRow(ctx, q, p.WorkspaceID, p.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if _, err := q.CreateProject(ctx, sqlcgen.CreateProjectParams{
		ID:               projID,
		WorkspaceID:      p.WorkspaceID,
		KeyPrefix:        p.KeyPrefix,
		Slug:             p.Slug,
		Name:             p.Name,
		Description:      db.EncodeProjectDescription(p),
		Status:           string(status),
		IssueCounter:     max(p.TaskSeq, 0),
		CreatedByActorID: createdByPtr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	return db.MapSQLDriverError(op, seedPostgresProjectWorkflowStates(ctx, q, p.WorkspaceID, projID))
}

// GetByID retrieves an active Project by (workspaceID, projectID).
func (r *ProjectRepository) GetByID(ctx context.Context, workspaceID, projectID uuid.UUID) (project.Project, error) {
	const op = "postgres.ProjectRepository.GetByID"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	row, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{WorkspaceID: workspaceID, ID: projectID})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresProject(row, buildPostgresActorLookup(ctx, q, workspaceID)), nil
}

// GetBySlug retrieves an active Project by (workspaceID, slug).
func (r *ProjectRepository) GetBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (project.Project, error) {
	const op = "postgres.ProjectRepository.GetBySlug"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	row, err := q.GetProjectBySlug(ctx, sqlcgen.GetProjectBySlugParams{WorkspaceID: workspaceID, Slug: slug})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresProject(row, buildPostgresActorLookup(ctx, q, workspaceID)), nil
}

// List returns active Projects matching filter ordered deterministically by Slug ASC.
func (r *ProjectRepository) List(ctx context.Context, filter project.ListProjectsFilter) ([]project.Project, error) {
	const op = "postgres.ProjectRepository.List"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	var statusPtr *string
	if filter.Status != nil {
		s := string(*filter.Status)
		statusPtr = &s
	}
	rows, err := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{
		WorkspaceID: filter.WorkspaceID,
		Status:      statusPtr,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, filter.WorkspaceID)
	mapped := make([]project.Project, 0, len(rows))
	for _, row := range rows {
		mapped = append(mapped, mapPostgresProject(row, actorMap))
	}
	return db.SortAndSliceProjects(mapped, filter), nil
}

// Update persists changes to an active Project matching (p.WorkspaceID, p.ID).
func (r *ProjectRepository) Update(ctx context.Context, p project.Project) error {
	const op = "postgres.ProjectRepository.Update"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	encodedDesc := db.EncodeProjectDescription(p)
	statusStr := string(p.Status)
	_, err = q.UpdateProject(ctx, sqlcgen.UpdateProjectParams{
		Slug:        &p.Slug,
		Name:        &p.Name,
		Description: &encodedDesc,
		Status:      &statusStr,
		WorkspaceID: p.WorkspaceID,
		ID:          p.ID,
	})
	return db.MapSQLDriverError(op, err)
}

// NextTaskSequence atomically increments and returns the next task sequence number for (workspaceID, projectID).
func (r *ProjectRepository) NextTaskSequence(ctx context.Context, workspaceID, projectID uuid.UUID) (string, int64, error) {
	const op = "postgres.ProjectRepository.NextTaskSequence"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return "", 0, err
	}
	updated, err := q.IncrementProjectIssueCounter(ctx, sqlcgen.IncrementProjectIssueCounterParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	})
	if err != nil {
		return "", 0, db.MapSQLDriverError(op, err)
	}
	return updated.KeyPrefix, updated.IssueCounter, nil
}

// SoftDelete marks (workspaceID, projectID) as soft-deleted after cascading soft-deletes
// to its tasks (releasing active leases) and objectives inside the enclosing transaction.
func (r *ProjectRepository) SoftDelete(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	const op = "postgres.ProjectRepository.SoftDelete"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if err := q.SoftDeleteTasksByProject(ctx, sqlcgen.SoftDeleteTasksByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if err := q.SoftDeleteObjectivesByProject(ctx, sqlcgen.SoftDeleteObjectivesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	rows, err := q.SoftDeleteProject(ctx, sqlcgen.SoftDeleteProjectParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "project not found or already soft-deleted")
	}
	if err := q.DeleteProjectBindingsForProject(ctx, sqlcgen.DeleteProjectBindingsForProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	return nil
}

func mapPostgresProjectBinding(row sqlcgen.ProjectBinding, actorMap map[uuid.UUID]sqlcgen.Actor) project.ProjectBinding {
	createdBy, _, _ := resolvePostgresPrincipalFromActorMap(actorMap, row.CreatedByActorID)
	return project.ProjectBinding{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		ParentProjectID: row.ParentProjectID,
		ChildProjectID:  row.ChildProjectID,
		ScopeSubpath:    row.ScopeSubpath,
		CreatedBy:       createdBy,
		CreatedAt:       row.CreatedAt.UTC(),
		UpdatedAt:       row.UpdatedAt.UTC(),
	}
}

// UpsertBinding creates or updates a parent->child sub-project binding edge in PostgreSQL.
func (r *ProjectRepository) UpsertBinding(ctx context.Context, b project.ProjectBinding) (project.ProjectBinding, error) {
	const op = "postgres.ProjectRepository.UpsertBinding"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	edgeID := b.ID
	if edgeID == uuid.Nil {
		edgeID = uuid.New()
	}
	createdByPtr, err := ensurePostgresActorRow(ctx, q, b.WorkspaceID, b.CreatedBy, "", "")
	if err != nil {
		return project.ProjectBinding{}, db.MapSQLDriverError(op, err)
	}
	row, err := q.UpsertProjectBinding(ctx, sqlcgen.UpsertProjectBindingParams{
		ID:               edgeID,
		WorkspaceID:      b.WorkspaceID,
		ParentProjectID:  b.ParentProjectID,
		ChildProjectID:   b.ChildProjectID,
		ScopeSubpath:     b.ScopeSubpath,
		CreatedByActorID: createdByPtr,
	})
	if err != nil {
		return project.ProjectBinding{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresProjectBinding(row, buildPostgresActorLookup(ctx, q, b.WorkspaceID)), nil
}

// DeleteBinding removes a single parent->child binding edge without deleting either project.
func (r *ProjectRepository) DeleteBinding(ctx context.Context, workspaceID, parentProjectID, childProjectID uuid.UUID) (bool, error) {
	const op = "postgres.ProjectRepository.DeleteBinding"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return false, err
	}
	rows, err := q.DeleteProjectBinding(ctx, sqlcgen.DeleteProjectBindingParams{
		WorkspaceID:     workspaceID,
		ParentProjectID: parentProjectID,
		ChildProjectID:  childProjectID,
	})
	if err != nil {
		return false, db.MapSQLDriverError(op, err)
	}
	return rows > 0, nil
}

// DeleteBindingsForProject removes all binding edges referencing projectID.
func (r *ProjectRepository) DeleteBindingsForProject(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	const op = "postgres.ProjectRepository.DeleteBindingsForProject"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	return db.MapSQLDriverError(op, q.DeleteProjectBindingsForProject(ctx, sqlcgen.DeleteProjectBindingsForProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	}))
}

// ListBindings returns active ProjectBindings where both parent and child projects are non-deleted.
func (r *ProjectRepository) ListBindings(ctx context.Context, filter project.ListBindingsFilter) ([]project.ProjectBinding, error) {
	const op = "postgres.ProjectRepository.ListBindings"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListProjectBindingsByWorkspace(ctx, sqlcgen.ListProjectBindingsByWorkspaceParams{
		WorkspaceID:     filter.WorkspaceID,
		ParentProjectID: filter.ParentProjectID,
		ChildProjectID:  filter.ChildProjectID,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, filter.WorkspaceID)
	out := make([]project.ProjectBinding, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapPostgresProjectBinding(r, actorMap))
	}
	return out, nil
}

// SetHasMarkdownDoc toggles has_markdown_doc on (workspaceID, projectID) and returns the updated Project.
func (r *ProjectRepository) SetHasMarkdownDoc(ctx context.Context, workspaceID, projectID uuid.UUID, hasDoc bool) (project.Project, error) {
	const op = "postgres.ProjectRepository.SetHasMarkdownDoc"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	row, err := q.SetProjectMarkdownDoc(ctx, sqlcgen.SetProjectMarkdownDocParams{
		HasMarkdownDoc: hasDoc,
		WorkspaceID:    workspaceID,
		ID:             projectID,
	})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresProject(row, buildPostgresActorLookup(ctx, q, workspaceID)), nil
}

func mapPostgresWorkflowState(row sqlcgen.WorkflowState) project.WorkflowState {
	return project.WorkflowState{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		ProjectID:   row.ProjectID,
		Name:        row.Name,
		Category:    db.DomainWorkflowStateCategory(row.Name, row.Category),
		ColorHex:    row.Color,
		Position:    row.Position,
		IsDefault:   row.IsDefault,
		CreatedAt:   row.CreatedAt.UTC(),
		UpdatedAt:   row.UpdatedAt.UTC(),
	}
}

func mapPostgresProjectLabel(row sqlcgen.Label) project.ProjectLabel {
	var projID uuid.UUID
	if row.ProjectID != nil {
		projID = *row.ProjectID
	}
	return project.ProjectLabel{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		ProjectID:   projID,
		Name:        row.Name,
		ColorHex:    row.Color,
		Description: row.Description,
		CreatedAt:   row.CreatedAt.UTC(),
		UpdatedAt:   row.UpdatedAt.UTC(),
	}
}

// ListWorkflowStates returns all active WorkflowStates for (workspaceID, projectID) ordered by Position.
func (r *ProjectRepository) ListWorkflowStates(ctx context.Context, workspaceID, projectID uuid.UUID) ([]project.WorkflowState, error) {
	const op = "postgres.ProjectRepository.ListWorkflowStates"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	rows, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	out := make([]project.WorkflowState, len(rows))
	for i, row := range rows {
		out[i] = mapPostgresWorkflowState(row)
	}
	return out, nil
}

// CreateWorkflowState inserts a custom WorkflowState scoped to (st.WorkspaceID, st.ProjectID).
func (r *ProjectRepository) CreateWorkflowState(ctx context.Context, st project.WorkflowState) (project.WorkflowState, error) {
	const op = "postgres.ProjectRepository.CreateWorkflowState"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.WorkflowState{}, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: st.WorkspaceID,
		ID:          st.ProjectID,
	}); err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	if st.IsDefault {
		existing, lErr := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
			WorkspaceID: st.WorkspaceID,
			ProjectID:   st.ProjectID,
		})
		if lErr != nil {
			return project.WorkflowState{}, db.MapSQLDriverError(op, lErr)
		}
		f := false
		for _, ex := range existing {
			if ex.IsDefault {
				if _, uErr := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
					IsDefault:   &f,
					WorkspaceID: st.WorkspaceID,
					ProjectID:   st.ProjectID,
					ID:          ex.ID,
				}); uErr != nil {
					return project.WorkflowState{}, db.MapSQLDriverError(op, uErr)
				}
			}
		}
	}

	stID := st.ID
	if stID == uuid.Nil {
		stID = uuid.New()
	}
	color := st.ColorHex
	if strings.TrimSpace(color) == "" {
		color = "#6B7280"
	}
	row, err := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
		ID:          stID,
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		Name:        st.Name,
		Category:    db.SQLWorkflowStateCategory(st.Category),
		Position:    st.Position,
		Color:       color,
		IsDefault:   st.IsDefault,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	mapped := mapPostgresWorkflowState(row)
	mapped.Description = st.Description
	if strings.EqualFold(strings.TrimSpace(st.Category), "archived") {
		mapped.Category = "archived"
	}
	return mapped, nil
}

// UpdateWorkflowState mutates an existing WorkflowState inside (in.WorkspaceID, in.ProjectID).
func (r *ProjectRepository) UpdateWorkflowState(ctx context.Context, in project.UpdateWorkflowStateInput) (project.WorkflowState, error) {
	const op = "postgres.ProjectRepository.UpdateWorkflowState"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.WorkflowState{}, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: in.WorkspaceID,
		ID:          in.ProjectID,
	}); err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	cur, err := q.GetWorkflowStateByID(ctx, sqlcgen.GetWorkflowStateByIDParams{
		WorkspaceID: in.WorkspaceID,
		ID:          in.StateID,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	if cur.ProjectID != in.ProjectID {
		return project.WorkflowState{}, errs.New(errs.ErrNotFound, op, "workflow state not found in project")
	}

	if in.IsDefault != nil && *in.IsDefault {
		existing, lErr := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
			WorkspaceID: in.WorkspaceID,
			ProjectID:   in.ProjectID,
		})
		if lErr != nil {
			return project.WorkflowState{}, db.MapSQLDriverError(op, lErr)
		}
		f := false
		for _, ex := range existing {
			if ex.ID != in.StateID && ex.IsDefault {
				if _, uErr := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
					IsDefault:   &f,
					WorkspaceID: in.WorkspaceID,
					ProjectID:   in.ProjectID,
					ID:          ex.ID,
				}); uErr != nil {
					return project.WorkflowState{}, db.MapSQLDriverError(op, uErr)
				}
			}
		}
	}

	var catPtr *string
	if in.Category != nil {
		sqlCat := db.SQLWorkflowStateCategory(*in.Category)
		catPtr = &sqlCat
	}
	row, err := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
		Name:        in.Name,
		Category:    catPtr,
		Position:    in.Position,
		Color:       in.ColorHex,
		IsDefault:   in.IsDefault,
		WorkspaceID: in.WorkspaceID,
		ProjectID:   in.ProjectID,
		ID:          in.StateID,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	mapped := mapPostgresWorkflowState(row)
	if in.Description != nil {
		mapped.Description = *in.Description
	}
	if in.Category != nil && strings.EqualFold(strings.TrimSpace(*in.Category), "archived") {
		mapped.Category = "archived"
	}
	return mapped, nil
}

// ListLabels returns active ProjectLabels visible to (workspaceID, projectID).
func (r *ProjectRepository) ListLabels(ctx context.Context, workspaceID, projectID uuid.UUID) ([]project.ProjectLabel, error) {
	const op = "postgres.ProjectRepository.ListLabels"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	projCopy := projectID
	rows, err := q.ListLabelsByProject(ctx, sqlcgen.ListLabelsByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   &projCopy,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	out := make([]project.ProjectLabel, len(rows))
	for i, row := range rows {
		out[i] = mapPostgresProjectLabel(row)
	}
	return out, nil
}

// CreateLabel inserts a new ProjectLabel scoped to (lbl.WorkspaceID, lbl.ProjectID).
func (r *ProjectRepository) CreateLabel(ctx context.Context, lbl project.ProjectLabel) (project.ProjectLabel, error) {
	const op = "postgres.ProjectRepository.CreateLabel"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.ProjectLabel{}, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: lbl.WorkspaceID,
		ID:          lbl.ProjectID,
	}); err != nil {
		return project.ProjectLabel{}, db.MapSQLDriverError(op, err)
	}
	lblID := lbl.ID
	if lblID == uuid.Nil {
		lblID = uuid.New()
	}
	projCopy := lbl.ProjectID
	color := lbl.ColorHex
	if strings.TrimSpace(color) == "" {
		color = "#6B7280"
	}
	row, err := q.CreateLabel(ctx, sqlcgen.CreateLabelParams{
		ID:          lblID,
		WorkspaceID: lbl.WorkspaceID,
		ProjectID:   &projCopy,
		Name:        lbl.Name,
		Color:       color,
		Description: lbl.Description,
	})
	if err != nil {
		return project.ProjectLabel{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresProjectLabel(row), nil
}

// DeleteLabel soft-deletes an active ProjectLabel matching (workspaceID, projectID, labelID).
func (r *ProjectRepository) DeleteLabel(ctx context.Context, workspaceID, projectID, labelID uuid.UUID) error {
	const op = "postgres.ProjectRepository.DeleteLabel"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID,
		ID:          projectID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	projCopy := projectID
	rows, err := q.SoftDeleteLabel(ctx, sqlcgen.SoftDeleteLabelParams{
		WorkspaceID: workspaceID,
		ProjectID:   &projCopy,
		ID:          labelID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "label not found in project")
	}
	return nil
}

// =============================================================================
// 3. OBJECTIVE REPOSITORY (PostgreSQL)
// =============================================================================

// ObjectiveRepository implements `objective.Repository` over PostgreSQL sqlc queries.
type ObjectiveRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of ObjectiveRepository bound to scope.
func (r *ObjectiveRepository) WithScope(scope tx.Scope) objective.Repository {
	return &ObjectiveRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapPostgresObjective(row sqlcgen.Objective, actorMap map[uuid.UUID]sqlcgen.Actor) objective.Objective {
	summary, meta := db.DecodeObjectiveDescription(row.Description, row.TargetDate != nil)
	var targetDate *time.Time
	if meta.HasTargetDate && row.TargetDate != nil {
		t := row.TargetDate.UTC()
		targetDate = &t
	}
	createdBy, _, _ := resolvePostgresPrincipalFromActorMap(actorMap, row.CreatedByActorID)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		t := row.DeletedAt.UTC()
		deletedAt = &t
	}
	kind := objective.Kind(row.Kind)
	if !kind.Valid() {
		kind = objective.KindGoal
	}
	return objective.Objective{
		ID:              row.ID,
		WorkspaceID:     row.WorkspaceID,
		ProjectID:       row.ProjectID,
		Kind:            kind,
		Slug:            meta.Slug,
		Title:           row.Title,
		Summary:         summary,
		SuccessCriteria: meta.SuccessCriteria,
		Status:          objective.Status(row.Status),
		PriorityRank:    meta.PriorityRank,
		TargetDate:      targetDate,
		CreatedBy:       createdBy,
		CreatedAt:       row.CreatedAt.UTC(),
		UpdatedAt:       row.UpdatedAt.UTC(),
		DeletedAt:       deletedAt,
	}
}

// Create inserts a new Objective scoped to (o.WorkspaceID, o.ProjectID).
func (r *ObjectiveRepository) Create(ctx context.Context, o objective.Objective) error {
	const op = "postgres.ObjectiveRepository.Create"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{WorkspaceID: o.WorkspaceID, ID: o.ProjectID}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if o.Slug != "" {
		existingObjs, lErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: o.WorkspaceID,
			ProjectID:   o.ProjectID,
		})
		if lErr != nil {
			return db.MapSQLDriverError(op, lErr)
		}
		for _, ex := range existingObjs {
			_, meta := db.DecodeObjectiveDescription(ex.Description, ex.TargetDate != nil)
			if meta.Slug == o.Slug {
				return errs.New(errs.ErrConflict, op, "objective slug already exists in project")
			}
		}
	}

	objID := o.ID
	if objID == uuid.Nil {
		objID = uuid.New()
	}
	status := o.Status
	if status == "" {
		status = objective.StatusActive
	}
	var targetPtr *time.Time
	if o.TargetDate != nil {
		utc := o.TargetDate.UTC()
		targetPtr = &utc
	}
	createdByPtr, err := ensurePostgresActorRow(ctx, q, o.WorkspaceID, o.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	kindStr := string(o.Kind)
	if !o.Kind.Valid() {
		kindStr = string(objective.KindGoal)
	}
	_, err = q.CreateObjective(ctx, sqlcgen.CreateObjectiveParams{
		ID:               objID,
		WorkspaceID:      o.WorkspaceID,
		ProjectID:        o.ProjectID,
		Title:            o.Title,
		Description:      db.EncodeObjectiveDescription(o),
		Kind:             kindStr,
		Status:           string(status),
		TargetDate:       targetPtr,
		CreatedByActorID: createdByPtr,
	})
	return db.MapSQLDriverError(op, err)
}

// GetByID retrieves an active Objective by (workspaceID, objectiveID).
func (r *ObjectiveRepository) GetByID(ctx context.Context, workspaceID, objectiveID uuid.UUID) (objective.Objective, error) {
	const op = "postgres.ObjectiveRepository.GetByID"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return objective.Objective{}, err
	}
	row, err := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{WorkspaceID: workspaceID, ID: objectiveID})
	if err != nil {
		return objective.Objective{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresObjective(row, buildPostgresActorLookup(ctx, q, workspaceID)), nil
}

// GetBySlug retrieves an active Objective by (workspaceID, projectID, slug).
func (r *ObjectiveRepository) GetBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (objective.Objective, error) {
	const op = "postgres.ObjectiveRepository.GetBySlug"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return objective.Objective{}, err
	}
	rows, err := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return objective.Objective{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	for _, row := range rows {
		mapped := mapPostgresObjective(row, actorMap)
		if mapped.Slug == slug {
			return mapped, nil
		}
	}
	return objective.Objective{}, errs.New(errs.ErrNotFound, op, "objective slug not found in project")
}

// List returns active Objectives matching filter ordered by PriorityRank ASC, CreatedAt ASC.
func (r *ObjectiveRepository) List(ctx context.Context, filter objective.ListObjectivesFilter) ([]objective.Objective, error) {
	const op = "postgres.ObjectiveRepository.List"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	var kindPtr, statusPtr *string
	if filter.Kind != nil {
		k := string(*filter.Kind)
		kindPtr = &k
	}
	if filter.Status != nil {
		s := string(*filter.Status)
		statusPtr = &s
	}

	var projectIDs []uuid.UUID
	if filter.ProjectID != uuid.Nil {
		projectIDs = []uuid.UUID{filter.ProjectID}
	} else {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{WorkspaceID: filter.WorkspaceID})
		if pErr != nil {
			return nil, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			projectIDs = append(projectIDs, p.ID)
		}
	}

	actorMap := buildPostgresActorLookup(ctx, q, filter.WorkspaceID)
	var collected []objective.Objective
	for _, pid := range projectIDs {
		rows, lErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: filter.WorkspaceID,
			ProjectID:   pid,
			Kind:        kindPtr,
			Status:      statusPtr,
		})
		if lErr != nil {
			return nil, db.MapSQLDriverError(op, lErr)
		}
		for _, row := range rows {
			collected = append(collected, mapPostgresObjective(row, actorMap))
		}
	}
	return db.SortAndSliceObjectives(collected, filter.Offset, filter.Limit), nil
}

// Update persists modifications to an active Objective matching (o.WorkspaceID, o.ID).
func (r *ObjectiveRepository) Update(ctx context.Context, o objective.Objective) error {
	const op = "postgres.ObjectiveRepository.Update"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	existing, err := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{WorkspaceID: o.WorkspaceID, ID: o.ID})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if o.Slug != "" {
		siblings, sErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: o.WorkspaceID,
			ProjectID:   existing.ProjectID,
		})
		if sErr != nil {
			return db.MapSQLDriverError(op, sErr)
		}
		for _, sib := range siblings {
			if sib.ID == existing.ID {
				continue
			}
			_, sibMeta := db.DecodeObjectiveDescription(sib.Description, sib.TargetDate != nil)
			if sibMeta.Slug == o.Slug {
				return errs.New(errs.ErrConflict, op, "objective slug already exists in project")
			}
		}
	}

	encodedDesc := db.EncodeObjectiveDescription(o)
	statusStr := string(o.Status)
	var kindPtr *string
	if o.Kind.Valid() {
		k := string(o.Kind)
		kindPtr = &k
	}
	var targetPtr *time.Time
	if o.TargetDate != nil {
		utc := o.TargetDate.UTC()
		targetPtr = &utc
	}
	_, err = q.UpdateObjective(ctx, sqlcgen.UpdateObjectiveParams{
		ParentObjectiveID: existing.ParentObjectiveID,
		Title:             &o.Title,
		Description:       &encodedDesc,
		Kind:              kindPtr,
		Status:            &statusStr,
		StartsAt:          existing.StartsAt,
		TargetDate:        targetPtr,
		WorkspaceID:       o.WorkspaceID,
		ID:                o.ID,
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks (workspaceID, objectiveID) as soft-deleted.
func (r *ObjectiveRepository) SoftDelete(ctx context.Context, workspaceID, objectiveID uuid.UUID) error {
	const op = "postgres.ObjectiveRepository.SoftDelete"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rows, err := q.SoftDeleteObjective(ctx, sqlcgen.SoftDeleteObjectiveParams{
		WorkspaceID: workspaceID,
		ID:          objectiveID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "objective not found or already soft-deleted")
	}
	return nil
}

// =============================================================================
// 4. TASK REPOSITORY (PostgreSQL)
// =============================================================================

// TaskRepository implements `task.Repository` over PostgreSQL sqlc queries.
type TaskRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of TaskRepository bound to scope.
func (r *TaskRepository) WithScope(scope tx.Scope) task.Repository {
	return &TaskRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func resolvePostgresWorkflowStateID(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID, st task.Status) (uuid.UUID, error) {
	if err := seedPostgresProjectWorkflowStates(ctx, q, workspaceID, projectID); err != nil {
		return uuid.Nil, err
	}
	states, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return uuid.Nil, err
	}
	wantName := strings.ToLower(db.WorkflowStateNameForStatus(st))
	for _, ws := range states {
		if strings.ToLower(ws.Name) == wantName {
			return ws.ID, nil
		}
	}
	for _, seed := range db.DefaultWorkflowStateSeeds() {
		if strings.ToLower(seed.Name) == wantName {
			created, cErr := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
				ID:          uuid.New(),
				WorkspaceID: workspaceID,
				ProjectID:   projectID,
				Name:        seed.Name,
				Category:    seed.Category,
				Position:    seed.Position,
				Color:       seed.Color,
				IsDefault:   seed.IsDefault,
			})
			if cErr == nil {
				return created.ID, nil
			}
			break
		}
	}
	for _, ws := range states {
		if ws.IsDefault {
			return ws.ID, nil
		}
	}
	if len(states) > 0 {
		return states[0].ID, nil
	}
	return uuid.Nil, errs.New(errs.ErrPreconditionFailed, "postgres.resolveWorkflowStateID", "project has no workflow states")
}

func buildPostgresStateMap(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID) map[uuid.UUID]task.Status {
	states, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return nil
	}
	out := make(map[uuid.UUID]task.Status, len(states))
	for _, st := range states {
		out[st.ID] = db.TaskStatusFromWorkflowState(st.Name, st.Category)
	}
	return out
}

func decodePostgresStoredLease(row sqlcgen.Task, actorMap map[uuid.UUID]sqlcgen.Actor) *task.ClaimLease {
	if row.ClaimedByActorID == nil || row.ClaimToken == nil || row.ClaimExpiresAt == nil {
		return nil
	}
	holderID, holderKind, holderHandle := resolvePostgresPrincipalFromActorMap(actorMap, row.ClaimedByActorID)
	lease := db.DecodeClaimToken(
		row.WorkspaceID,
		row.ID,
		holderID,
		holderKind,
		holderHandle,
		*row.ClaimToken,
		row.ClaimExpiresAt.UTC(),
		row.UpdatedAt.UTC(),
	)
	return &lease
}

func countPostgresUnresolvedBlockers(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID, taskID uuid.UUID,
	stateCache map[uuid.UUID]map[uuid.UUID]task.Status,
) (int32, error) {
	rels, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
	})
	if err != nil {
		return 0, err
	}
	var count int32
	for _, rel := range rels {
		if rel.TargetTaskID != taskID || rel.RelationType != "blocks" {
			continue
		}
		blockerRow, bErr := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
			WorkspaceID: workspaceID,
			ID:          rel.SourceTaskID,
		})
		if bErr != nil {
			continue
		}
		projStates, ok := stateCache[blockerRow.ProjectID]
		if !ok {
			projStates = buildPostgresStateMap(ctx, q, workspaceID, blockerRow.ProjectID)
			stateCache[blockerRow.ProjectID] = projStates
		}
		if !projStates[blockerRow.StateID].IsTerminal() {
			count++
		}
	}
	return count, nil
}

func mapPostgresTaskRow(
	ctx context.Context,
	q sqlcgen.Querier,
	row sqlcgen.Task,
	actorMap map[uuid.UUID]sqlcgen.Actor,
	stateCache map[uuid.UUID]map[uuid.UUID]task.Status,
	now time.Time,
) (task.Task, *task.ClaimLease, error) {
	projStates, ok := stateCache[row.ProjectID]
	if !ok {
		projStates = buildPostgresStateMap(ctx, q, row.WorkspaceID, row.ProjectID)
		stateCache[row.ProjectID] = projStates
	}
	status := projStates[row.StateID]
	if status == "" {
		if stRow, stErr := q.GetWorkflowStateByID(ctx, sqlcgen.GetWorkflowStateByIDParams{
			WorkspaceID: row.WorkspaceID,
			ID:          row.StateID,
		}); stErr == nil {
			status = db.TaskStatusFromWorkflowState(stRow.Name, stRow.Category)
		} else {
			status = task.StatusReady
		}
	}

	desc, meta := db.DecodeTaskDescription(
		row.Description,
		row.Kind,
		row.Priority,
		row.ObjectiveID != nil,
		row.ParentTaskID != nil,
		row.AssigneeActorID != nil,
	)

	var objID, parentID, assigneeID *uuid.UUID
	if meta.HasObjectiveID && row.ObjectiveID != nil {
		idCopy := *row.ObjectiveID
		objID = &idCopy
	}
	if meta.HasParentTaskID && row.ParentTaskID != nil {
		idCopy := *row.ParentTaskID
		parentID = &idCopy
	}
	if meta.HasAssigneeActorID && row.AssigneeActorID != nil {
		principal, _, _ := resolvePostgresPrincipalFromActorMap(actorMap, row.AssigneeActorID)
		if principal != uuid.Nil {
			assigneeID = &principal
		}
	}

	var createdBy uuid.UUID
	if meta.CreatedBy != "" {
		createdBy, _ = uuid.Parse(meta.CreatedBy)
	} else {
		createdBy, _, _ = resolvePostgresPrincipalFromActorMap(actorMap, row.ReporterActorID)
	}

	storedLease := decodePostgresStoredLease(row, actorMap)
	var activeLease *task.ClaimLease
	if storedLease != nil && !storedLease.IsExpiredAt(now) {
		cloned := storedLease.Clone()
		activeLease = &cloned
	}

	blockerCount, err := countPostgresUnresolvedBlockers(ctx, q, row.WorkspaceID, row.ID, stateCache)
	if err != nil {
		return task.Task{}, nil, err
	}

	var completedAt, lastSignalAt, deletedAt *time.Time
	if row.CompletedAt != nil {
		t := row.CompletedAt.UTC()
		completedAt = &t
	}
	if row.LastSignalAt != nil && !row.LastSignalAt.IsZero() {
		t := row.LastSignalAt.UTC()
		lastSignalAt = &t
	}
	if row.DeletedAt != nil {
		t := row.DeletedAt.UTC()
		deletedAt = &t
	}

	var extID string
	if row.ExternalID != nil {
		extID = *row.ExternalID
	}

	return task.Task{
		ID:                     row.ID,
		WorkspaceID:            row.WorkspaceID,
		ProjectID:              row.ProjectID,
		ObjectiveID:            objID,
		ParentTaskID:           parentID,
		Key:                    row.Identifier,
		ExternalID:             extID,
		Kind:                   meta.DomainKind,
		Title:                  row.Title,
		Description:            desc,
		AcceptanceCriteria:     db.DecodeAcceptanceCriteria(row.AcceptanceCriteria),
		Status:                 status,
		Priority:               meta.DomainPriority,
		AssigneeActorID:        assigneeID,
		Tags:                   meta.Tags,
		Labels:                 meta.Labels,
		ActiveLease:            activeLease,
		UnresolvedBlockerCount: blockerCount,
		HasMarkdownDoc:         row.HasMarkdownDoc,
		Version:                meta.Version,
		CreatedBy:              createdBy,
		CreatedAt:              row.CreatedAt.UTC(),
		UpdatedAt:              row.UpdatedAt.UTC(),
		CompletedAt:            completedAt,
		LastSignalAt:           lastSignalAt,
		DeletedAt:              deletedAt,
	}, storedLease, nil
}

// Create inserts a new Task row scoped to t.WorkspaceID, auto-allocating Key & sequence when t.Key is empty.
func (r *TaskRepository) Create(ctx context.Context, t task.Task) error {
	const op = "postgres.TaskRepository.Create"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{WorkspaceID: t.WorkspaceID, ID: t.ProjectID}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	var objPtr, parentPtr *uuid.UUID
	if t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil {
		objRow, objErr := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
			WorkspaceID: t.WorkspaceID,
			ID:          *t.ObjectiveID,
		})
		if objErr != nil {
			if errors.Is(objErr, pgx.ErrNoRows) {
				return errs.New(errs.ErrNotFound, op, "objective not found in workspace")
			}
			return db.MapSQLDriverError(op, objErr)
		}
		if objRow.ProjectID != t.ProjectID {
			return errs.Validation(op, "objective belongs to a different project", errs.FieldViolation{
				Field:       "objective_id",
				Description: "objective_id must belong to the same project_id",
			})
		}
		idCopy := *t.ObjectiveID
		objPtr = &idCopy
	}
	if t.ParentTaskID != nil && *t.ParentTaskID != uuid.Nil {
		idCopy := *t.ParentTaskID
		parentPtr = &idCopy
	}

	key := strings.TrimSpace(t.Key)
	var seqNum int64
	reallocate := key == ""
	keepCustomKey := false

	if !reallocate {
		if parsedSeq, ok := db.ParseTrailingSequence(key); ok {
			conflict, cErr := q.CheckTaskSequenceOrIdentifierConflict(ctx, sqlcgen.CheckTaskSequenceOrIdentifierConflictParams{
				WorkspaceID:    t.WorkspaceID,
				ProjectID:      t.ProjectID,
				SequenceNumber: parsedSeq,
				Identifier:     key,
			})
			if cErr != nil {
				return db.MapSQLDriverError(op, cErr)
			}
			if !conflict {
				seqNum = parsedSeq
			} else {
				reallocate = true
			}
		} else {
			idConflict, cErr := q.CheckTaskSequenceOrIdentifierConflict(ctx, sqlcgen.CheckTaskSequenceOrIdentifierConflictParams{
				WorkspaceID:    t.WorkspaceID,
				ProjectID:      t.ProjectID,
				SequenceNumber: -1,
				Identifier:     key,
			})
			if cErr != nil {
				return db.MapSQLDriverError(op, cErr)
			}
			keepCustomKey = !idConflict
			reallocate = true
		}
	}

	if reallocate {
		for {
			incProj, incErr := q.IncrementProjectIssueCounter(ctx, sqlcgen.IncrementProjectIssueCounterParams{
				WorkspaceID: t.WorkspaceID,
				ID:          t.ProjectID,
			})
			if incErr != nil {
				return db.MapSQLDriverError(op, incErr)
			}
			candidateSeq := incProj.IssueCounter
			candidateKey := fmt.Sprintf("%s-%d", incProj.KeyPrefix, candidateSeq)
			if keepCustomKey {
				candidateKey = key
			}
			conflict, cErr := q.CheckTaskSequenceOrIdentifierConflict(ctx, sqlcgen.CheckTaskSequenceOrIdentifierConflictParams{
				WorkspaceID:    t.WorkspaceID,
				ProjectID:      t.ProjectID,
				SequenceNumber: candidateSeq,
				Identifier:     candidateKey,
			})
			if cErr != nil {
				return db.MapSQLDriverError(op, cErr)
			}
			if !conflict {
				seqNum = candidateSeq
				key = candidateKey
				break
			}
		}
	}

	taskID := t.ID
	if taskID == uuid.Nil {
		taskID = uuid.New()
	}
	status := t.Status
	if status == "" {
		status = task.StatusReady
	}
	stateID, err := resolvePostgresWorkflowStateID(ctx, q, t.WorkspaceID, t.ProjectID, status)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	reporterPtr, err := ensurePostgresActorRow(ctx, q, t.WorkspaceID, t.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	var assigneePtr *uuid.UUID
	if t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil {
		assigneePtr, err = ensurePostgresActorRow(ctx, q, t.WorkspaceID, *t.AssigneeActorID, "", "")
		if err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	var completedPtr *time.Time
	if t.CompletedAt != nil {
		utc := t.CompletedAt.UTC()
		completedPtr = &utc
	} else if status.IsTerminal() {
		utc := time.Now().UTC()
		completedPtr = &utc
	}
	version := t.Version
	if version <= 0 {
		version = 1
	}

	var extPtr *string
	if trimmedExt := strings.TrimSpace(t.ExternalID); trimmedExt != "" {
		extPtr = &trimmedExt
	}
	var lastSignalPtr *time.Time
	if t.LastSignalAt != nil && !t.LastSignalAt.IsZero() {
		utc := t.LastSignalAt.UTC()
		lastSignalPtr = &utc
	}

	_, err = q.CreateTask(ctx, sqlcgen.CreateTaskParams{
		ID:                 taskID,
		WorkspaceID:        t.WorkspaceID,
		ProjectID:          t.ProjectID,
		ObjectiveID:        objPtr,
		ParentTaskID:       parentPtr,
		StateID:            stateID,
		SequenceNumber:     seqNum,
		Identifier:         key,
		ExternalID:         extPtr,
		Kind:               db.SQLTaskKind(t.Kind),
		Priority:           db.SQLTaskPriority(t.Priority),
		Title:              t.Title,
		Description:        db.EncodeTaskDescription(t, version),
		AcceptanceCriteria: db.EncodeAcceptanceCriteria(t.AcceptanceCriteria),
		ReporterActorID:    reporterPtr,
		AssigneeActorID:    assigneePtr,
		CompletedAt:        completedPtr,
		LastSignalAt:       lastSignalPtr,
	})
	return db.MapSQLDriverError(op, err)
}

// GetByID fetches a full Task aggregate by (workspaceID, taskID).
func (r *TaskRepository) GetByID(ctx context.Context, workspaceID, taskID uuid.UUID) (task.Task, error) {
	const op = "postgres.TaskRepository.GetByID"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: workspaceID, ID: taskID})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapPostgresTaskRow(ctx, q, row, actorMap, make(map[uuid.UUID]map[uuid.UUID]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// GetByKey fetches a full Task aggregate by (workspaceID, key).
func (r *TaskRepository) GetByKey(ctx context.Context, workspaceID uuid.UUID, key string) (task.Task, error) {
	const op = "postgres.TaskRepository.GetByKey"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByIdentifier(ctx, sqlcgen.GetTaskByIdentifierParams{WorkspaceID: workspaceID, Identifier: key})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapPostgresTaskRow(ctx, q, row, actorMap, make(map[uuid.UUID]map[uuid.UUID]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// GetByExternalID fetches a full Task aggregate by (workspaceID, projectID, externalID).
func (r *TaskRepository) GetByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (task.Task, error) {
	const op = "postgres.TaskRepository.GetByExternalID"
	ext := strings.TrimSpace(externalID)
	if ext == "" {
		return task.Task{}, errs.New(errs.ErrNotFound, op, "task not found")
	}
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByExternalID(ctx, sqlcgen.GetTaskByExternalIDParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		ExternalID:  &ext,
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapPostgresTaskRow(ctx, q, row, actorMap, make(map[uuid.UUID]map[uuid.UUID]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// Update persists field and status mutations on an active Task, enforcing expectedVersion optimistic locking.
func (r *TaskRepository) Update(ctx context.Context, t task.Task, expectedVersion *int64) error {
	const op = "postgres.TaskRepository.Update"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	existing, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: t.WorkspaceID, ID: t.ID})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	_, existingMeta := db.DecodeTaskDescription(
		existing.Description,
		existing.Kind,
		existing.Priority,
		existing.ObjectiveID != nil,
		existing.ParentTaskID != nil,
		existing.AssigneeActorID != nil,
	)
	if expectedVersion != nil && existingMeta.Version != *expectedVersion {
		return errs.New(errs.ErrConflict, op, "task optimistic concurrency version mismatch")
	}
	nextVersion := existingMeta.Version + 1

	status := t.Status
	if status == "" {
		status = task.StatusReady
	}
	stateID, err := resolvePostgresWorkflowStateID(ctx, q, t.WorkspaceID, existing.ProjectID, status)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	var objPtr, parentPtr, assigneePtr *uuid.UUID
	if t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil {
		objRow, objErr := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
			WorkspaceID: t.WorkspaceID,
			ID:          *t.ObjectiveID,
		})
		if objErr != nil {
			if errors.Is(objErr, pgx.ErrNoRows) {
				return errs.New(errs.ErrNotFound, op, "objective not found in workspace")
			}
			return db.MapSQLDriverError(op, objErr)
		}
		if objRow.ProjectID != existing.ProjectID {
			return errs.Validation(op, "objective belongs to a different project", errs.FieldViolation{
				Field:       "objective_id",
				Description: "objective_id must belong to the same project_id",
			})
		}
		idCopy := *t.ObjectiveID
		objPtr = &idCopy
	}
	if t.ParentTaskID != nil && *t.ParentTaskID != uuid.Nil {
		idCopy := *t.ParentTaskID
		parentPtr = &idCopy
	}
	if t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil {
		assigneePtr, err = ensurePostgresActorRow(ctx, q, t.WorkspaceID, *t.AssigneeActorID, "", "")
		if err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	sqlKind := db.SQLTaskKind(t.Kind)
	sqlPrio := db.SQLTaskPriority(t.Priority)
	encodedDesc := db.EncodeTaskDescription(t, nextVersion)
	encodedAC := db.EncodeAcceptanceCriteria(t.AcceptanceCriteria)

	var lastSignalPtr *time.Time
	if t.LastSignalAt != nil && !t.LastSignalAt.IsZero() {
		utc := t.LastSignalAt.UTC()
		lastSignalPtr = &utc
	}

	if _, err := q.UpdateTask(ctx, sqlcgen.UpdateTaskParams{
		ObjectiveID:        objPtr,
		ParentTaskID:       parentPtr,
		Kind:               &sqlKind,
		Priority:           &sqlPrio,
		Title:              &t.Title,
		Description:        &encodedDesc,
		AcceptanceCriteria: &encodedAC,
		EstimatePoints:     existing.EstimatePoints,
		AssigneeActorID:    assigneePtr,
		LastSignalAt:       lastSignalPtr,
		WorkspaceID:        t.WorkspaceID,
		ID:                 t.ID,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	existingExt := ""
	if existing.ExternalID != nil {
		existingExt = *existing.ExternalID
	}
	nextExt := strings.TrimSpace(t.ExternalID)
	if nextExt != existingExt {
		var nextExtPtr *string
		if nextExt != "" {
			nextExtPtr = &nextExt
		}
		if _, err := q.SetTaskExternalID(ctx, sqlcgen.SetTaskExternalIDParams{
			ExternalID:  nextExtPtr,
			WorkspaceID: t.WorkspaceID,
			ID:          t.ID,
		}); err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	var completedPtr *time.Time
	if t.CompletedAt != nil {
		utc := t.CompletedAt.UTC()
		completedPtr = &utc
	} else if status.IsTerminal() {
		utc := time.Now().UTC()
		completedPtr = &utc
	}
	_, err = q.TransitionTaskState(ctx, sqlcgen.TransitionTaskStateParams{
		StateID:     stateID,
		CompletedAt: completedPtr,
		WorkspaceID: t.WorkspaceID,
		ID:          t.ID,
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks (workspaceID, taskID) as soft-deleted and clears any active lease.
func (r *TaskRepository) SoftDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "postgres.TaskRepository.SoftDelete"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rows, err := q.SoftDeleteTask(ctx, sqlcgen.SoftDeleteTaskParams{WorkspaceID: workspaceID, ID: taskID})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task not found or already soft-deleted")
	}
	return nil
}

// ListByWorkspace returns all active Task aggregates across every non-deleted project in workspaceID.
func (r *TaskRepository) ListByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]task.Task, error) {
	const op = "postgres.TaskRepository.ListByWorkspace"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	stateCache := make(map[uuid.UUID]map[uuid.UUID]task.Status)
	now := time.Now().UTC()
	out := make([]task.Task, 0, len(rows))
	for _, row := range rows {
		mapped, _, mErr := mapPostgresTaskRow(ctx, q, row, actorMap, stateCache, now)
		if mErr != nil {
			return nil, db.MapSQLDriverError(op, mErr)
		}
		out = append(out, mapped)
	}
	return out, nil
}

// SetHasMarkdownDoc toggles has_markdown_doc on (workspaceID, taskID) and returns the updated Task.
func (r *TaskRepository) SetHasMarkdownDoc(ctx context.Context, workspaceID, taskID uuid.UUID, hasDoc bool) (task.Task, error) {
	const op = "postgres.TaskRepository.SetHasMarkdownDoc"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.SetTaskMarkdownDoc(ctx, sqlcgen.SetTaskMarkdownDocParams{
		HasMarkdownDoc: hasDoc,
		WorkspaceID:    workspaceID,
		ID:             taskID,
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapPostgresTaskRow(ctx, q, row, actorMap, make(map[uuid.UUID]map[uuid.UUID]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// TouchSignal updates last_signal_at on (workspaceID, taskID) without changing updated_at and returns the updated Task.
func (r *TaskRepository) TouchSignal(ctx context.Context, workspaceID, taskID uuid.UUID, signalAt time.Time) (task.Task, error) {
	const op = "postgres.TaskRepository.TouchSignal"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	var sigPtr *time.Time
	if !signalAt.IsZero() {
		utc := signalAt.UTC()
		sigPtr = &utc
	}
	row, err := q.TouchTaskLastSignal(ctx, sqlcgen.TouchTaskLastSignalParams{
		LastSignalAt: sigPtr,
		WorkspaceID:  workspaceID,
		ID:           taskID,
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapPostgresTaskRow(ctx, q, row, actorMap, make(map[uuid.UUID]map[uuid.UUID]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// HardDelete detaches child subtasks and permanently deletes (workspaceID, taskID).
func (r *TaskRepository) HardDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "postgres.TaskRepository.HardDelete"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rels, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	for _, rel := range rels {
		if dErr := q.DeleteDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.DeleteDSLExecutionBatchByIdempotencyKeyParams{
			WorkspaceID:    workspaceID,
			IdempotencyKey: db.RelationMetaIdempotencyKey(rel.ID),
		}); dErr != nil {
			return db.MapSQLDriverError(op, dErr)
		}
	}
	parentCopy := taskID
	if err := q.DetachSubtasksFromParent(ctx, sqlcgen.DetachSubtasksFromParentParams{
		WorkspaceID:  workspaceID,
		ParentTaskID: &parentCopy,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	rows, err := q.HardDeleteTask(ctx, sqlcgen.HardDeleteTaskParams{
		WorkspaceID: workspaceID,
		ID:          taskID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task not found")
	}
	return nil
}

func loadPostgresProjectHydratedTasks(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID uuid.UUID,
	projectIDs []uuid.UUID,
	actorMap map[uuid.UUID]sqlcgen.Actor,
	now time.Time,
) ([]task.Task, map[uuid.UUID]string, map[uuid.UUID]string, map[uuid.UUID]task.ClaimLease, error) {
	stateCache := make(map[uuid.UUID]map[uuid.UUID]task.Status)
	var allTasks []task.Task
	objSlugs := make(map[uuid.UUID]string)
	parentKeys := make(map[uuid.UUID]string)
	leases := make(map[uuid.UUID]task.ClaimLease)

	for _, pid := range projectIDs {
		objs, oErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: workspaceID,
			ProjectID:   pid,
		})
		if oErr != nil {
			return nil, nil, nil, nil, oErr
		}
		for _, oRow := range objs {
			_, oMeta := db.DecodeObjectiveDescription(oRow.Description, oRow.TargetDate != nil)
			objSlugs[oRow.ID] = oMeta.Slug
		}

		rows, tErr := q.ListTasksByProject(ctx, sqlcgen.ListTasksByProjectParams{
			WorkspaceID: workspaceID,
			ProjectID:   pid,
		})
		if tErr != nil {
			return nil, nil, nil, nil, tErr
		}
		for _, row := range rows {
			mapped, storedLease, mErr := mapPostgresTaskRow(ctx, q, row, actorMap, stateCache, now)
			if mErr != nil {
				return nil, nil, nil, nil, mErr
			}
			allTasks = append(allTasks, mapped)
			parentKeys[mapped.ID] = mapped.Key
			if storedLease != nil {
				leases[mapped.ID] = *storedLease
			}
		}
	}
	return allTasks, objSlugs, parentKeys, leases, nil
}

// QueryCompactQueue returns CompactTaskSummary rows matching filter.
func (r *TaskRepository) QueryCompactQueue(ctx context.Context, filter task.AgentWorkQueueFilter, now time.Time) ([]task.CompactTaskSummary, error) {
	const op = "postgres.TaskRepository.QueryCompactQueue"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	var projectIDs []uuid.UUID
	if filter.ProjectID != uuid.Nil {
		projectIDs = []uuid.UUID{filter.ProjectID}
	} else {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{WorkspaceID: filter.WorkspaceID})
		if pErr != nil {
			return nil, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			projectIDs = append(projectIDs, p.ID)
		}
	}
	actorMap := buildPostgresActorLookup(ctx, q, filter.WorkspaceID)
	tasks, objSlugs, parentKeys, leases, err := loadPostgresProjectHydratedTasks(ctx, q, filter.WorkspaceID, projectIDs, actorMap, now)
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	return db.FilterAndSortCompactQueue(tasks, objSlugs, parentKeys, leases, filter, now), nil
}

// BuildProjectDigest computes a ProjectDigest snapshot for (workspaceID, projectID).
func (r *TaskRepository) BuildProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, opts task.DigestOptions, now time.Time) (task.ProjectDigest, error) {
	const op = "postgres.TaskRepository.BuildProjectDigest"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.ProjectDigest{}, err
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	projRow, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{WorkspaceID: workspaceID, ID: projectID})
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	projDomain := mapPostgresProject(projRow, actorMap)

	objRows, err := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	objs := make([]objective.Objective, 0, len(objRows))
	for _, oRow := range objRows {
		objs = append(objs, mapPostgresObjective(oRow, actorMap))
	}

	tasks, _, _, leases, err := loadPostgresProjectHydratedTasks(ctx, q, workspaceID, []uuid.UUID{projectID}, actorMap, now)
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	return db.AssembleProjectDigest(projDomain, objs, tasks, leases, opts, now), nil
}

// UpsertLease atomically acquires or renews the exclusive ClaimLease on lease.TaskID.
func (r *TaskRepository) UpsertLease(ctx context.Context, lease task.ClaimLease) error {
	const op = "postgres.TaskRepository.UpsertLease"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	actorRowPtr, err := ensurePostgresActorRow(ctx, q, lease.WorkspaceID, lease.HolderActorID, lease.HolderKind, lease.HolderHandle)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	encodedToken := db.EncodeClaimToken(lease)
	expUTC := lease.ExpiresAt.UTC()

	_, err = q.ClaimTaskLease(ctx, sqlcgen.ClaimTaskLeaseParams{
		ActorID:        actorRowPtr,
		ClaimToken:     &encodedToken,
		ClaimExpiresAt: &expUTC,
		StateID:        nil,
		WorkspaceID:    lease.WorkspaceID,
		ID:             lease.TaskID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, getErr := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
				WorkspaceID: lease.WorkspaceID,
				ID:          lease.TaskID,
			}); getErr != nil {
				return db.MapSQLDriverError(op, getErr)
			}
			return errs.New(errs.ErrLeaseHeld, op, "task claim lease is currently held by another actor")
		}
		return db.MapSQLDriverError(op, err)
	}
	return nil
}

// GetLease retrieves the stored ClaimLease for (workspaceID, taskID).
func (r *TaskRepository) GetLease(ctx context.Context, workspaceID, taskID uuid.UUID) (task.ClaimLease, error) {
	const op = "postgres.TaskRepository.GetLease"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.ClaimLease{}, err
	}
	row, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: workspaceID, ID: taskID})
	if err != nil {
		return task.ClaimLease{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	stored := decodePostgresStoredLease(row, actorMap)
	if stored == nil {
		return task.ClaimLease{}, errs.New(errs.ErrNotFound, op, "no active or expired claim lease on task")
	}
	return *stored, nil
}

// DeleteLease clears the ClaimLease columns on (workspaceID, taskID).
func (r *TaskRepository) DeleteLease(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "postgres.TaskRepository.DeleteLease"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rows, err := q.ReleaseTaskLease(ctx, sqlcgen.ReleaseTaskLeaseParams{
		WorkspaceID: workspaceID,
		ID:          taskID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task not found in workspace")
	}
	return nil
}

// DeleteExpiredLeases reaps expired task leases (`expires_at <= asOf`) and returns the count cleared.
func (r *TaskRepository) DeleteExpiredLeases(ctx context.Context, workspaceID uuid.UUID, asOf time.Time) (int64, error) {
	const op = "postgres.TaskRepository.DeleteExpiredLeases"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return 0, err
	}
	var workspaceIDs []uuid.UUID
	if workspaceID != uuid.Nil {
		workspaceIDs = []uuid.UUID{workspaceID}
	} else {
		wss, wErr := q.ListWorkspaces(ctx)
		if wErr != nil {
			return 0, db.MapSQLDriverError(op, wErr)
		}
		for _, w := range wss {
			workspaceIDs = append(workspaceIDs, w.ID)
		}
	}

	cutoff := asOf.UTC()
	var reaped int64
	for _, wsID := range workspaceIDs {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{WorkspaceID: wsID})
		if pErr != nil {
			return reaped, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			tasks, tErr := q.ListTasksByProject(ctx, sqlcgen.ListTasksByProjectParams{
				WorkspaceID: wsID,
				ProjectID:   p.ID,
			})
			if tErr != nil {
				return reaped, db.MapSQLDriverError(op, tErr)
			}
			for _, tRow := range tasks {
				if tRow.ClaimedByActorID == nil {
					continue
				}
				expired := tRow.ClaimExpiresAt == nil || !cutoff.Before(tRow.ClaimExpiresAt.UTC())
				if expired {
					n, rErr := q.ReleaseTaskLease(ctx, sqlcgen.ReleaseTaskLeaseParams{
						WorkspaceID: wsID,
						ID:          tRow.ID,
					})
					if rErr != nil {
						return reaped, db.MapSQLDriverError(op, rErr)
					}
					reaped += n
				}
			}
		}
	}
	return reaped, nil
}

// CreateRelation inserts a directed TaskRelation edge and persists lossless sidecar metadata.
func (r *TaskRepository) CreateRelation(ctx context.Context, rel task.TaskRelation) error {
	const op = "postgres.TaskRepository.CreateRelation"
	if rel.SourceTaskID == rel.TargetTaskID {
		return errs.Validation(op, "reflexive task self-relation is forbidden", errs.FieldViolation{
			Field:       "target_task_id",
			Description: "source_task_id and target_task_id must differ",
		})
	}
	sqlRelType, err := db.SQLRelationType(op, rel.Type)
	if err != nil {
		return err
	}
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: rel.WorkspaceID, ID: rel.SourceTaskID}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: rel.WorkspaceID, ID: rel.TargetTaskID}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	relID := rel.ID
	if relID == uuid.Nil {
		relID = uuid.New()
	}
	createdByPtr, err := ensurePostgresActorRow(ctx, q, rel.WorkspaceID, rel.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if _, err := q.CreateTaskRelation(ctx, sqlcgen.CreateTaskRelationParams{
		ID:               relID,
		WorkspaceID:      rel.WorkspaceID,
		SourceTaskID:     rel.SourceTaskID,
		TargetTaskID:     rel.TargetTaskID,
		RelationType:     sqlRelType,
		CreatedByActorID: createdByPtr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	if rel.Type == task.RelationSubtaskOf || rel.Note != "" {
		sidecarJSON, _ := json.Marshal(db.RelationSidecarMeta{
			DomainType: rel.Type,
			Note:       rel.Note,
		})
		if _, err := q.CreateDSLExecutionBatch(ctx, sqlcgen.CreateDSLExecutionBatchParams{
			ID:             uuid.New(),
			WorkspaceID:    rel.WorkspaceID,
			ActorID:        createdByPtr,
			IdempotencyKey: db.RelationMetaIdempotencyKey(relID),
			Mode:           "relation_meta",
			ScriptHash:     relID.String(),
			StatementCount: 0,
			Status:         "succeeded",
			SummaryJson:    sidecarJSON,
		}); err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}
	return nil
}

// DeleteRelation removes a directed TaskRelation edge matching (workspaceID, relationID).
func (r *TaskRepository) DeleteRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error {
	const op = "postgres.TaskRepository.DeleteRelation"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	rows, err := q.DeleteTaskRelation(ctx, sqlcgen.DeleteTaskRelationParams{
		WorkspaceID: workspaceID,
		ID:          relationID,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task relation not found")
	}
	if err := q.DeleteDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.DeleteDSLExecutionBatchByIdempotencyKeyParams{
		WorkspaceID:    workspaceID,
		IdempotencyKey: db.RelationMetaIdempotencyKey(relationID),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	return nil
}

// ListRelationsForTask returns all inbound and outbound TaskRelation edges for (workspaceID, taskID).
func (r *TaskRepository) ListRelationsForTask(ctx context.Context, workspaceID, taskID uuid.UUID) ([]task.TaskRelation, error) {
	const op = "postgres.TaskRepository.ListRelationsForTask"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	out := make([]task.TaskRelation, 0, len(rows))
	for _, row := range rows {
		createdBy, _, _ := resolvePostgresPrincipalFromActorMap(actorMap, row.CreatedByActorID)
		domType := task.RelationType(row.RelationType)
		note := ""
		if batch, bErr := q.GetDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.GetDSLExecutionBatchByIdempotencyKeyParams{
			WorkspaceID:    workspaceID,
			IdempotencyKey: db.RelationMetaIdempotencyKey(row.ID),
		}); bErr == nil {
			var sc db.RelationSidecarMeta
			if json.Unmarshal(batch.SummaryJson, &sc) == nil {
				if sc.DomainType != "" {
					domType = sc.DomainType
				}
				note = sc.Note
			}
		}
		out = append(out, task.TaskRelation{
			ID:           row.ID,
			WorkspaceID:  workspaceID,
			SourceTaskID: row.SourceTaskID,
			TargetTaskID: row.TargetTaskID,
			Type:         domType,
			Note:         note,
			CreatedBy:    createdBy,
			CreatedAt:    row.CreatedAt.UTC(),
		})
	}
	return out, nil
}

// CreateComment appends an immutable TaskComment to (c.WorkspaceID, c.TaskID).
func (r *TaskRepository) CreateComment(ctx context.Context, c task.TaskComment) error {
	const op = "postgres.TaskRepository.CreateComment"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: c.WorkspaceID, ID: c.TaskID}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	commentID := c.ID
	if commentID == uuid.Nil {
		commentID = uuid.New()
	}
	authorPtr, err := ensurePostgresActorRow(ctx, q, c.WorkspaceID, c.AuthorActorID, c.AuthorKind, c.AuthorHandle)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if authorPtr == nil {
		return errs.Validation(op, "comment author_actor_id is required", errs.FieldViolation{
			Field:       "author_actor_id",
			Description: "TaskComment AuthorActorID must not be uuid.Nil",
		})
	}
	_, err = q.CreateTaskComment(ctx, sqlcgen.CreateTaskCommentParams{
		ID:            commentID,
		WorkspaceID:   c.WorkspaceID,
		TaskID:        c.TaskID,
		AuthorActorID: *authorPtr,
		Kind:          db.SQLCommentKind(c.Kind),
		Body:          c.Body,
		MetadataJson:  db.EncodeCommentMeta(c),
	})
	return db.MapSQLDriverError(op, err)
}

// ListComments returns TaskComments for (workspaceID, taskID) ordered chronologically.
func (r *TaskRepository) ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]task.TaskComment, error) {
	const op = "postgres.TaskRepository.ListComments"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{WorkspaceID: workspaceID, ID: taskID}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	rows, err := q.ListTaskComments(ctx, sqlcgen.ListTaskCommentsParams{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildPostgresActorLookup(ctx, q, workspaceID)
	out := make([]task.TaskComment, 0, len(rows))
	for _, row := range rows {
		meta := db.DecodeCommentMeta(row.MetadataJson, row.Kind)
		authorRowID := row.AuthorActorID
		principalID, actorKind, actorHandle := resolvePostgresPrincipalFromActorMap(actorMap, &authorRowID)
		if meta.AuthorActorID != "" {
			if parsed, pErr := uuid.Parse(meta.AuthorActorID); pErr == nil && parsed != uuid.Nil {
				principalID = parsed
			}
		}
		if meta.AuthorKind.Valid() {
			actorKind = meta.AuthorKind
		}
		if meta.AuthorHandle != "" {
			actorHandle = meta.AuthorHandle
		}
		out = append(out, task.TaskComment{
			ID:            row.ID,
			WorkspaceID:   workspaceID,
			TaskID:        taskID,
			AuthorActorID: principalID,
			AuthorKind:    actorKind,
			AuthorHandle:  actorHandle,
			Kind:          meta.DomainKind,
			Body:          row.Body,
			CreatedAt:     row.CreatedAt.UTC(),
		})
	}
	if limit > 0 && len(out) > int(limit) {
		out = out[:int(limit)]
	}
	return out, nil
}

// =============================================================================
// 5. AUDIT REPOSITORY (PostgreSQL)
// =============================================================================

// AuditRepository implements `audit.Repository` over PostgreSQL sqlc queries.
type AuditRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of AuditRepository bound to scope.
func (r *AuditRepository) WithScope(scope tx.Scope) audit.Repository {
	return &AuditRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapPostgresAuditRecord(row sqlcgen.AuditLog, actorMap map[uuid.UUID]sqlcgen.Actor) audit.Record {
	actorID, actorKind, actorHandle := resolvePostgresPrincipalFromActorMap(actorMap, row.ActorID)
	var oldBytes, newBytes []byte
	if len(row.OldValuesJson) > 0 {
		oldBytes = make([]byte, len(row.OldValuesJson))
		copy(oldBytes, row.OldValuesJson)
	}
	if len(row.NewValuesJson) > 0 {
		newBytes = make([]byte, len(row.NewValuesJson))
		copy(newBytes, row.NewValuesJson)
	}
	return audit.Record{
		ID:           row.ID,
		WorkspaceID:  row.WorkspaceID,
		ActorID:      actorID,
		ActorKind:    actorKind,
		ActorHandle:  actorHandle,
		Action:       tx.AuditAction(row.Action),
		ResourceType: row.ResourceType,
		ResourceID:   row.ResourceID,
		OldValues:    oldBytes,
		NewValues:    newBytes,
		OccurredAt:   row.CreatedAt.UTC(),
	}
}

// GetByID retrieves a single immutable audit Record by (workspaceID, auditID).
func (r *AuditRepository) GetByID(ctx context.Context, workspaceID, auditID uuid.UUID) (audit.Record, error) {
	const op = "postgres.AuditRepository.GetByID"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return audit.Record{}, err
	}
	row, err := q.GetAuditLogByID(ctx, sqlcgen.GetAuditLogByIDParams{WorkspaceID: workspaceID, ID: auditID})
	if err != nil {
		return audit.Record{}, db.MapSQLDriverError(op, err)
	}
	return mapPostgresAuditRecord(row, buildPostgresActorLookup(ctx, q, workspaceID)), nil
}

// Query returns immutable audit Records matching filter within filter.WorkspaceID ordered newest-first.
func (r *AuditRepository) Query(ctx context.Context, filter audit.QueryFilter) ([]audit.Record, error) {
	const op = "postgres.AuditRepository.Query"
	q, err := resolvePostgresQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	actorMap := buildPostgresActorLookup(ctx, q, filter.WorkspaceID)
	hasBothResource := filter.ResourceType != "" && filter.ResourceID != ""
	effectiveLimit := filter.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 100
	}

	if !db.AuditQueryNeedsPostFilter(filter) {
		sqlOffset := max(filter.Offset, 0)
		sqlLimit := effectiveLimit
		var rows []sqlcgen.AuditLog
		if hasBothResource {
			rows, err = q.ListAuditLogsByResource(ctx, sqlcgen.ListAuditLogsByResourceParams{
				WorkspaceID:  filter.WorkspaceID,
				ResourceType: filter.ResourceType,
				ResourceID:   filter.ResourceID,
				Offset:       sqlOffset,
				Limit:        sqlLimit,
			})
		} else {
			rows, err = q.ListAuditLogsByWorkspace(ctx, sqlcgen.ListAuditLogsByWorkspaceParams{
				WorkspaceID: filter.WorkspaceID,
				Offset:      sqlOffset,
				Limit:       sqlLimit,
			})
		}
		if err != nil {
			return nil, db.MapSQLDriverError(op, err)
		}
		mapped := make([]audit.Record, len(rows))
		for i, row := range rows {
			mapped[i] = mapPostgresAuditRecord(row, actorMap)
		}
		sliceFilter := filter
		sliceFilter.Offset = 0
		sliceFilter.Limit = effectiveLimit
		return db.FilterSortAndSliceAuditRecords(mapped, sliceFilter), nil
	}

	want := int64(max(filter.Offset, 0)) + int64(effectiveLimit)
	batchSize := int32(min(max(int64(10000), want*4), 1<<30))
	var mapped []audit.Record
	probeFilter := filter
	probeFilter.Offset = 0
	probeFilter.Limit = int32(min(want+64, 1<<30))

	for sqlOffset := int32(0); ; sqlOffset += batchSize {
		var rows []sqlcgen.AuditLog
		if hasBothResource {
			rows, err = q.ListAuditLogsByResource(ctx, sqlcgen.ListAuditLogsByResourceParams{
				WorkspaceID:  filter.WorkspaceID,
				ResourceType: filter.ResourceType,
				ResourceID:   filter.ResourceID,
				Offset:       sqlOffset,
				Limit:        batchSize,
			})
		} else {
			rows, err = q.ListAuditLogsByWorkspace(ctx, sqlcgen.ListAuditLogsByWorkspaceParams{
				WorkspaceID: filter.WorkspaceID,
				Offset:      sqlOffset,
				Limit:       batchSize,
			})
		}
		if err != nil {
			return nil, db.MapSQLDriverError(op, err)
		}
		for _, row := range rows {
			mapped = append(mapped, mapPostgresAuditRecord(row, actorMap))
		}
		if int32(len(rows)) < batchSize {
			break
		}
		if int64(len(db.FilterSortAndSliceAuditRecords(mapped, probeFilter))) >= want+64 {
			break
		}
	}
	return db.FilterSortAndSliceAuditRecords(mapped, filter), nil
}
