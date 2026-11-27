// File repositories.go implements all 5 concrete SQLite domain repositories
// (`WorkspaceRepository`, `ProjectRepository`, `ObjectiveRepository`,
// `TaskRepository`, and `AuditRepository`) strictly over `sqlite/sqlcgen`.
package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/adapter/db/sqlite/sqlcgen"
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

// resolveReadQuerier selects either the active transaction Querier or the concurrent read pool.
func resolveReadQuerier(adapter *Adapter, scoped bool, scope tx.Scope) (sqlcgen.Querier, error) {
	if scoped {
		return adapter.bridge.QueriesForScope(scope)
	}
	return adapter.bridge.ReadQueries(), nil
}

// resolveWriteQuerier selects either the active transaction Querier or acquires the serialized writer pool.
func resolveWriteQuerier(ctx context.Context, op string, adapter *Adapter, scoped bool, scope tx.Scope) (sqlcgen.Querier, func(), error) {
	if scoped {
		q, err := adapter.bridge.QueriesForScope(scope)
		return q, func() {}, err
	}
	release, err := adapter.acquireWriteSlot(ctx, op)
	if err != nil {
		return nil, nil, err
	}
	return adapter.bridge.WriteQueries(), release, nil
}

// ensureSQLiteActorRow guarantees a valid `actors` row exists inside workspaceID for principalActorID,
// handling cross-workspace primary-key collisions deterministically without polluting ListMembers.
func ensureSQLiteActorRow(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID, principalActorID uuid.UUID,
	hintKind authctx.ActorKind,
	hintHandle string,
) (*string, error) {
	if principalActorID == uuid.Nil {
		return nil, nil
	}
	wsStr := workspaceID.String()
	directStr := principalActorID.String()

	if _, err := q.GetActorByID(ctx, sqlcgen.GetActorByIDParams{WorkspaceID: wsStr, ID: directStr}); err == nil {
		return &directStr, nil
	}
	scopedID := db.ScopedActorRowID(workspaceID, principalActorID)
	scopedStr := scopedID.String()
	if _, err := q.GetActorByID(ctx, sqlcgen.GetActorByIDParams{WorkspaceID: wsStr, ID: scopedStr}); err == nil {
		return &scopedStr, nil
	}

	// Scan workspace actors in case a fallback UUID row already maps to principalActorID.
	if existingActors, listErr := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: wsStr}); listErr == nil {
		for _, a := range existingActors {
			meta := db.DecodeActorCapabilities([]byte(a.CapabilitiesJson))
			if meta.PrincipalActorID == directStr {
				rowID := a.ID
				return &rowID, nil
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
	caps := string(db.EncodeActorCapabilities(db.ActorCapabilitiesMeta{
		PrincipalActorID: directStr,
		MemberKind:       authctx.ActorKind(kindStr),
		IsStub:           true,
	}))

	candidates := []string{directStr, scopedStr, uuid.NewString()}
	for _, candidateID := range candidates {
		stubHandle := db.StubActorHandlePrefix + candidateID
		created, createErr := q.CreateActor(ctx, sqlcgen.CreateActorParams{
			ID:               candidateID,
			WorkspaceID:      wsStr,
			Handle:           stubHandle,
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
	return nil, errs.New(errs.ErrConflict, "sqlite.ensureActorRow", "unable to allocate unique actor row ID")
}

// buildSQLiteActorLookup indexes all active actors in a workspace by their SQL row ID.
func buildSQLiteActorLookup(ctx context.Context, q sqlcgen.Querier, workspaceID uuid.UUID) map[string]sqlcgen.Actor {
	rows, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{
		WorkspaceID: workspaceID.String(),
	})
	if err != nil {
		return nil
	}
	out := make(map[string]sqlcgen.Actor, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func resolveSQLitePrincipalFromActorMap(actorMap map[string]sqlcgen.Actor, rowIDPtr *string) (uuid.UUID, authctx.ActorKind, string) {
	if rowIDPtr == nil || *rowIDPtr == "" {
		return uuid.Nil, "", ""
	}
	if row, ok := actorMap[*rowIDPtr]; ok {
		rowUUID, _ := uuid.Parse(row.ID)
		meta := db.DecodeActorCapabilities([]byte(row.CapabilitiesJson))
		principalID := db.ResolvePrincipalActorID(rowUUID, meta)
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
	parsed, _ := uuid.Parse(*rowIDPtr)
	return parsed, "", ""
}

// =============================================================================
// 1. WORKSPACE REPOSITORY (SQLite)
// =============================================================================

// WorkspaceRepository implements `workspace.Repository` over SQLite sqlc queries.
type WorkspaceRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of WorkspaceRepository bound to scope.
func (r *WorkspaceRepository) WithScope(scope tx.Scope) workspace.Repository {
	return &WorkspaceRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapSQLiteWorkspace(row sqlcgen.Workspace) (workspace.Workspace, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return workspace.Workspace{}, err
	}
	desc, ttl, labels := db.DecodeWorkspaceDescription(row.Description)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		parsed := db.ParseSQLiteTime(*row.DeletedAt)
		deletedAt = &parsed
	}
	return workspace.Workspace{
		ID:                     id,
		Slug:                   row.Slug,
		Name:                   row.Name,
		Description:            desc,
		DefaultLeaseTTLSeconds: ttl,
		Labels:                 labels,
		CreatedAt:              db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:              db.ParseSQLiteTime(row.UpdatedAt),
		DeletedAt:              deletedAt,
	}, nil
}

// Create inserts a newly provisioned Workspace row.
func (r *WorkspaceRepository) Create(ctx context.Context, ws workspace.Workspace) error {
	const op = "sqlite.WorkspaceRepository.Create"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsID := ws.ID
	if wsID == uuid.Nil {
		wsID = uuid.New()
	}
	_, err = q.CreateWorkspace(ctx, sqlcgen.CreateWorkspaceParams{
		ID:          wsID.String(),
		Slug:        ws.Slug,
		Name:        ws.Name,
		Description: db.EncodeWorkspaceDescription(ws),
	})
	return db.MapSQLDriverError(op, err)
}

// GetByID fetches an active Workspace by its primary UUID.
func (r *WorkspaceRepository) GetByID(ctx context.Context, workspaceID uuid.UUID) (workspace.Workspace, error) {
	const op = "sqlite.WorkspaceRepository.GetByID"
	if err := ctx.Err(); err != nil {
		return workspace.Workspace{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return workspace.Workspace{}, err
	}
	row, err := q.GetWorkspaceByID(ctx, workspaceID.String())
	if err != nil {
		return workspace.Workspace{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteWorkspace(row)
}

// GetBySlug fetches an active Workspace by its unique slug.
func (r *WorkspaceRepository) GetBySlug(ctx context.Context, slug string) (workspace.Workspace, error) {
	const op = "sqlite.WorkspaceRepository.GetBySlug"
	if err := ctx.Err(); err != nil {
		return workspace.Workspace{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return workspace.Workspace{}, err
	}
	row, err := q.GetWorkspaceBySlug(ctx, slug)
	if err != nil {
		return workspace.Workspace{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteWorkspace(row)
}

// ListForActor returns all active Workspaces where actorID holds a registered membership, sorted by Slug ASC.
func (r *WorkspaceRepository) ListForActor(ctx context.Context, actorID uuid.UUID) ([]workspace.Workspace, error) {
	const op = "sqlite.WorkspaceRepository.ListForActor"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	allWS, err := q.ListWorkspaces(ctx)
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}

	var matched []workspace.Workspace
	for _, wsRow := range allWS {
		wsDomain, mapErr := mapSQLiteWorkspace(wsRow)
		if mapErr != nil {
			continue
		}
		actors, listErr := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{
			WorkspaceID: wsRow.ID,
		})
		if listErr != nil {
			return nil, db.MapSQLDriverError(op, listErr)
		}
		for _, a := range actors {
			meta := db.DecodeActorCapabilities([]byte(a.CapabilitiesJson))
			if meta.IsStub {
				continue
			}
			rowUUID, _ := uuid.Parse(a.ID)
			if db.ResolvePrincipalActorID(rowUUID, meta) == actorID {
				matched = append(matched, wsDomain)
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
	const op = "sqlite.WorkspaceRepository.Update"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	encodedDesc := db.EncodeWorkspaceDescription(ws)
	_, err = q.UpdateWorkspace(ctx, sqlcgen.UpdateWorkspaceParams{
		Slug:        &ws.Slug,
		Name:        &ws.Name,
		Description: &encodedDesc,
		ID:          ws.ID.String(),
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks the target Workspace as soft-deleted.
func (r *WorkspaceRepository) SoftDelete(ctx context.Context, workspaceID uuid.UUID) error {
	const op = "sqlite.WorkspaceRepository.SoftDelete"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	rows, err := q.SoftDeleteWorkspace(ctx, workspaceID.String())
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "workspace not found or already soft-deleted")
	}
	return nil
}

// UpsertMember inserts or updates a WorkspaceMember inside m.WorkspaceID.
func (r *WorkspaceRepository) UpsertMember(ctx context.Context, m workspace.WorkspaceMember) error {
	const op = "sqlite.WorkspaceRepository.UpsertMember"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if m.WorkspaceID == uuid.Nil || !m.Kind.Valid() || strings.TrimSpace(m.Handle) == "" {
		return errs.Validation(op, "invalid workspace member fields", errs.FieldViolation{
			Field:       "member",
			Description: "workspace_id, valid kind, and non-empty handle are required",
		})
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	actorID := m.ActorID
	if actorID == uuid.Nil {
		actorID = uuid.New()
	}
	wsStr := m.WorkspaceID.String()

	// Check handle collision with a different principal in the same workspace.
	if byHandle, hErr := q.GetActorByHandle(ctx, sqlcgen.GetActorByHandleParams{
		WorkspaceID: wsStr,
		Handle:      m.Handle,
	}); hErr == nil {
		hUUID, _ := uuid.Parse(byHandle.ID)
		hMeta := db.DecodeActorCapabilities([]byte(byHandle.CapabilitiesJson))
		if db.ResolvePrincipalActorID(hUUID, hMeta) != actorID {
			return errs.New(errs.ErrConflict, op, "actor handle already belongs to another workspace member")
		}
	} else if !errors.Is(hErr, sql.ErrNoRows) {
		return db.MapSQLDriverError(op, hErr)
	}

	display := m.DisplayName
	hasRawDisplay := false
	if strings.TrimSpace(display) == "" {
		display = m.Handle
		hasRawDisplay = true
	}
	capsStr := string(db.EncodeActorCapabilities(db.ActorCapabilitiesMeta{
		PrincipalActorID:  actorID.String(),
		Role:              m.Role,
		MemberKind:        m.Kind,
		RawDisplayName:    m.DisplayName,
		HasRawDisplayName: hasRawDisplay,
		IsStub:            false,
	}))

	// Check if a row for this principal already exists in the workspace.
	existingActors, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{WorkspaceID: wsStr})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	for _, existing := range existingActors {
		exUUID, _ := uuid.Parse(existing.ID)
		exMeta := db.DecodeActorCapabilities([]byte(existing.CapabilitiesJson))
		if db.ResolvePrincipalActorID(exUUID, exMeta) == actorID {
			_, updErr := q.UpdateActor(ctx, sqlcgen.UpdateActorParams{
				Handle:           &m.Handle,
				DisplayName:      &display,
				CapabilitiesJson: &capsStr,
				WorkspaceID:      wsStr,
				ID:               existing.ID,
			})
			return db.MapSQLDriverError(op, updErr)
		}
	}

	scopedStr := db.ScopedActorRowID(m.WorkspaceID, actorID).String()
	for _, candidateID := range []string{actorID.String(), scopedStr, uuid.NewString()} {
		_, createErr := q.CreateActor(ctx, sqlcgen.CreateActorParams{
			ID:               candidateID,
			WorkspaceID:      wsStr,
			Handle:           m.Handle,
			DisplayName:      display,
			Kind:             string(m.Kind),
			HarnessHint:      "",
			CapabilitiesJson: capsStr,
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
	const op = "sqlite.WorkspaceRepository.ListMembers"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListActorsByWorkspace(ctx, sqlcgen.ListActorsByWorkspaceParams{
		WorkspaceID: workspaceID.String(),
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}

	out := make([]workspace.WorkspaceMember, 0, len(rows))
	for _, row := range rows {
		meta := db.DecodeActorCapabilities([]byte(row.CapabilitiesJson))
		if meta.IsStub {
			continue
		}
		rowUUID, _ := uuid.Parse(row.ID)
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
			ActorID:     db.ResolvePrincipalActorID(rowUUID, meta),
			Kind:        kind,
			Handle:      row.Handle,
			DisplayName: display,
			Role:        role,
			CreatedAt:   db.ParseSQLiteTime(row.CreatedAt),
		})
	}
	slices.SortFunc(out, func(a, b workspace.WorkspaceMember) int {
		return cmp.Compare(a.Handle, b.Handle)
	})
	return out, nil
}

// =============================================================================
// 2. PROJECT REPOSITORY (SQLite)
// =============================================================================

// ProjectRepository implements `project.Repository` over SQLite sqlc queries.
type ProjectRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of ProjectRepository bound to scope.
func (r *ProjectRepository) WithScope(scope tx.Scope) project.Repository {
	return &ProjectRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func seedSQLiteProjectWorkflowStates(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID) error {
	existing, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	for _, seed := range db.DefaultWorkflowStateSeeds() {
		var isDef int64
		if seed.IsDefault {
			isDef = 1
		}
		if _, err := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
			ID:          uuid.NewString(),
			WorkspaceID: workspaceID.String(),
			ProjectID:   projectID.String(),
			Name:        seed.Name,
			Category:    seed.Category,
			Position:    int64(seed.Position),
			Color:       seed.Color,
			IsDefault:   isDef,
		}); err != nil {
			return err
		}
	}
	return nil
}

func mapSQLiteProject(row sqlcgen.Project, actorMap map[string]sqlcgen.Actor) (project.Project, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return project.Project{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return project.Project{}, err
	}
	desc, meta := db.DecodeProjectDescription(row.Description)
	createdBy, _, _ := resolveSQLitePrincipalFromActorMap(actorMap, row.CreatedByActorID)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		parsed := db.ParseSQLiteTime(*row.DeletedAt)
		deletedAt = &parsed
	}
	return project.Project{
		ID:             id,
		WorkspaceID:    wsID,
		Slug:           row.Slug,
		KeyPrefix:      row.KeyPrefix,
		Name:           row.Name,
		Description:    desc,
		RemoteRepoURL:  meta.RemoteRepoURL,
		DefaultBranch:  meta.DefaultBranch,
		Status:         project.Status(row.Status),
		Tags:           meta.Tags,
		TaskSeq:        row.IssueCounter,
		HasMarkdownDoc: row.HasMarkdownDoc != 0,
		CreatedBy:      createdBy,
		CreatedAt:      db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:      db.ParseSQLiteTime(row.UpdatedAt),
		DeletedAt:      deletedAt,
	}, nil
}

// Create inserts a new Project and seeds its 7 canonical workflow_states columns.
func (r *ProjectRepository) Create(ctx context.Context, p project.Project) error {
	const op = "sqlite.ProjectRepository.Create"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	projID := p.ID
	if projID == uuid.Nil {
		projID = uuid.New()
	}
	status := p.Status
	if status == "" {
		status = project.StatusActive
	}
	createdByPtr, err := ensureSQLiteActorRow(ctx, q, p.WorkspaceID, p.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	if _, err := q.CreateProject(ctx, sqlcgen.CreateProjectParams{
		ID:               projID.String(),
		WorkspaceID:      p.WorkspaceID.String(),
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

	return db.MapSQLDriverError(op, seedSQLiteProjectWorkflowStates(ctx, q, p.WorkspaceID, projID))
}

// GetByID retrieves an active Project by (workspaceID, projectID).
func (r *ProjectRepository) GetByID(ctx context.Context, workspaceID, projectID uuid.UUID) (project.Project, error) {
	const op = "sqlite.ProjectRepository.GetByID"
	if err := ctx.Err(); err != nil {
		return project.Project{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	row, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          projectID.String(),
	})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteProject(row, buildSQLiteActorLookup(ctx, q, workspaceID))
}

// GetBySlug retrieves an active Project by (workspaceID, slug).
func (r *ProjectRepository) GetBySlug(ctx context.Context, workspaceID uuid.UUID, slug string) (project.Project, error) {
	const op = "sqlite.ProjectRepository.GetBySlug"
	if err := ctx.Err(); err != nil {
		return project.Project{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	row, err := q.GetProjectBySlug(ctx, sqlcgen.GetProjectBySlugParams{
		WorkspaceID: workspaceID.String(),
		Slug:        slug,
	})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteProject(row, buildSQLiteActorLookup(ctx, q, workspaceID))
}

// List returns active Projects matching filter within filter.WorkspaceID ordered by Slug ASC.
func (r *ProjectRepository) List(ctx context.Context, filter project.ListProjectsFilter) ([]project.Project, error) {
	const op = "sqlite.ProjectRepository.List"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	var statusPtr *string
	if filter.Status != nil {
		s := string(*filter.Status)
		statusPtr = &s
	}
	rows, err := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{
		WorkspaceID: filter.WorkspaceID.String(),
		Status:      statusPtr,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, filter.WorkspaceID)
	mapped := make([]project.Project, 0, len(rows))
	for _, row := range rows {
		p, mapErr := mapSQLiteProject(row, actorMap)
		if mapErr == nil {
			mapped = append(mapped, p)
		}
	}
	return db.SortAndSliceProjects(mapped, filter), nil
}

// Update persists changes to an active Project matching (p.WorkspaceID, p.ID).
func (r *ProjectRepository) Update(ctx context.Context, p project.Project) error {
	const op = "sqlite.ProjectRepository.Update"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	encodedDesc := db.EncodeProjectDescription(p)
	statusStr := string(p.Status)
	_, err = q.UpdateProject(ctx, sqlcgen.UpdateProjectParams{
		Slug:        &p.Slug,
		Name:        &p.Name,
		Description: &encodedDesc,
		Status:      &statusStr,
		WorkspaceID: p.WorkspaceID.String(),
		ID:          p.ID.String(),
	})
	return db.MapSQLDriverError(op, err)
}

// NextTaskSequence atomically increments and returns the next task sequence number for (workspaceID, projectID).
func (r *ProjectRepository) NextTaskSequence(ctx context.Context, workspaceID, projectID uuid.UUID) (string, int64, error) {
	const op = "sqlite.ProjectRepository.NextTaskSequence"
	if err := ctx.Err(); err != nil {
		return "", 0, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return "", 0, err
	}
	defer release()

	updated, err := q.IncrementProjectIssueCounter(ctx, sqlcgen.IncrementProjectIssueCounterParams{
		WorkspaceID: workspaceID.String(),
		ID:          projectID.String(),
	})
	if err != nil {
		return "", 0, db.MapSQLDriverError(op, err)
	}
	return updated.KeyPrefix, updated.IssueCounter, nil
}

// SoftDelete marks (workspaceID, projectID) as soft-deleted after cascading soft-deletes
// to its tasks (releasing active leases) and objectives inside the enclosing transaction.
func (r *ProjectRepository) SoftDelete(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	const op = "sqlite.ProjectRepository.SoftDelete"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsStr := workspaceID.String()
	projStr := projectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if err := q.SoftDeleteTasksByProject(ctx, sqlcgen.SoftDeleteTasksByProjectParams{
		WorkspaceID: wsStr,
		ProjectID:   projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if err := q.SoftDeleteObjectivesByProject(ctx, sqlcgen.SoftDeleteObjectivesByProjectParams{
		WorkspaceID: wsStr,
		ProjectID:   projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	rows, err := q.SoftDeleteProject(ctx, sqlcgen.SoftDeleteProjectParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "project not found or already soft-deleted")
	}
	if err := q.DeleteProjectBindingsForProject(ctx, sqlcgen.DeleteProjectBindingsForProjectParams{
		WorkspaceID: wsStr,
		ProjectID:   projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	return nil
}

func mapSQLiteProjectBinding(row sqlcgen.ProjectBinding, actorMap map[string]sqlcgen.Actor) (project.ProjectBinding, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	parentID, err := uuid.Parse(row.ParentProjectID)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	childID, err := uuid.Parse(row.ChildProjectID)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	createdBy, _, _ := resolveSQLitePrincipalFromActorMap(actorMap, row.CreatedByActorID)
	return project.ProjectBinding{
		ID:              id,
		WorkspaceID:     wsID,
		ParentProjectID: parentID,
		ChildProjectID:  childID,
		ScopeSubpath:    row.ScopeSubpath,
		CreatedBy:       createdBy,
		CreatedAt:       db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:       db.ParseSQLiteTime(row.UpdatedAt),
	}, nil
}

// UpsertBinding creates or updates a parent->child sub-project binding edge in SQLite.
func (r *ProjectRepository) UpsertBinding(ctx context.Context, b project.ProjectBinding) (project.ProjectBinding, error) {
	const op = "sqlite.ProjectRepository.UpsertBinding"
	if err := ctx.Err(); err != nil {
		return project.ProjectBinding{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.ProjectBinding{}, err
	}
	defer release()

	edgeID := b.ID
	if edgeID == uuid.Nil {
		edgeID = uuid.New()
	}
	createdByPtr, err := ensureSQLiteActorRow(ctx, q, b.WorkspaceID, b.CreatedBy, "", "")
	if err != nil {
		return project.ProjectBinding{}, db.MapSQLDriverError(op, err)
	}
	row, err := q.UpsertProjectBinding(ctx, sqlcgen.UpsertProjectBindingParams{
		ID:               edgeID.String(),
		WorkspaceID:      b.WorkspaceID.String(),
		ParentProjectID:  b.ParentProjectID.String(),
		ChildProjectID:   b.ChildProjectID.String(),
		ScopeSubpath:     b.ScopeSubpath,
		CreatedByActorID: createdByPtr,
	})
	if err != nil {
		return project.ProjectBinding{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteProjectBinding(row, buildSQLiteActorLookup(ctx, q, b.WorkspaceID))
}

// DeleteBinding removes a single parent->child binding edge without deleting either project.
func (r *ProjectRepository) DeleteBinding(ctx context.Context, workspaceID, parentProjectID, childProjectID uuid.UUID) (bool, error) {
	const op = "sqlite.ProjectRepository.DeleteBinding"
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return false, err
	}
	defer release()

	rows, err := q.DeleteProjectBinding(ctx, sqlcgen.DeleteProjectBindingParams{
		WorkspaceID:     workspaceID.String(),
		ParentProjectID: parentProjectID.String(),
		ChildProjectID:  childProjectID.String(),
	})
	if err != nil {
		return false, db.MapSQLDriverError(op, err)
	}
	return rows > 0, nil
}

// DeleteBindingsForProject removes all binding edges referencing projectID.
func (r *ProjectRepository) DeleteBindingsForProject(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	const op = "sqlite.ProjectRepository.DeleteBindingsForProject"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	return db.MapSQLDriverError(op, q.DeleteProjectBindingsForProject(ctx, sqlcgen.DeleteProjectBindingsForProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	}))
}

// ListBindings returns active ProjectBindings where both parent and child projects are non-deleted.
func (r *ProjectRepository) ListBindings(ctx context.Context, filter project.ListBindingsFilter) ([]project.ProjectBinding, error) {
	const op = "sqlite.ProjectRepository.ListBindings"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	var parentPtr, childPtr *string
	if filter.ParentProjectID != nil && *filter.ParentProjectID != uuid.Nil {
		s := filter.ParentProjectID.String()
		parentPtr = &s
	}
	if filter.ChildProjectID != nil && *filter.ChildProjectID != uuid.Nil {
		s := filter.ChildProjectID.String()
		childPtr = &s
	}
	rows, err := q.ListProjectBindingsByWorkspace(ctx, sqlcgen.ListProjectBindingsByWorkspaceParams{
		WorkspaceID:     filter.WorkspaceID.String(),
		ParentProjectID: parentPtr,
		ChildProjectID:  childPtr,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, filter.WorkspaceID)
	out := make([]project.ProjectBinding, 0, len(rows))
	for _, r := range rows {
		if mapped, mErr := mapSQLiteProjectBinding(r, actorMap); mErr == nil {
			out = append(out, mapped)
		}
	}
	return out, nil
}

// SetHasMarkdownDoc updates the has_markdown_doc flag on an active project row.
func (r *ProjectRepository) SetHasMarkdownDoc(ctx context.Context, workspaceID, projectID uuid.UUID, hasDoc bool) (project.Project, error) {
	const op = "sqlite.ProjectRepository.SetHasMarkdownDoc"
	if err := ctx.Err(); err != nil {
		return project.Project{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.Project{}, err
	}
	defer release()

	flag := int64(0)
	if hasDoc {
		flag = 1
	}
	row, err := q.SetProjectMarkdownDoc(ctx, sqlcgen.SetProjectMarkdownDocParams{
		HasMarkdownDoc: flag,
		WorkspaceID:    workspaceID.String(),
		ID:             projectID.String(),
	})
	if err != nil {
		return project.Project{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteProject(row, buildSQLiteActorLookup(ctx, q, workspaceID))
}

func mapSQLiteWorkflowState(row sqlcgen.WorkflowState) (project.WorkflowState, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return project.WorkflowState{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return project.WorkflowState{}, err
	}
	projID, err := uuid.Parse(row.ProjectID)
	if err != nil {
		return project.WorkflowState{}, err
	}
	return project.WorkflowState{
		ID:          id,
		WorkspaceID: wsID,
		ProjectID:   projID,
		Name:        row.Name,
		Category:    db.DomainWorkflowStateCategory(row.Name, row.Category),
		ColorHex:    row.Color,
		Position:    int32(row.Position),
		IsDefault:   row.IsDefault != 0,
		CreatedAt:   db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:   db.ParseSQLiteTime(row.UpdatedAt),
	}, nil
}

func mapSQLiteProjectLabel(row sqlcgen.Label) (project.ProjectLabel, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return project.ProjectLabel{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return project.ProjectLabel{}, err
	}
	var projID uuid.UUID
	if row.ProjectID != nil && *row.ProjectID != "" {
		projID, _ = uuid.Parse(*row.ProjectID)
	}
	return project.ProjectLabel{
		ID:          id,
		WorkspaceID: wsID,
		ProjectID:   projID,
		Name:        row.Name,
		ColorHex:    row.Color,
		Description: row.Description,
		CreatedAt:   db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:   db.ParseSQLiteTime(row.UpdatedAt),
	}, nil
}

// ListWorkflowStates returns all active WorkflowStates for (workspaceID, projectID) ordered by Position.
func (r *ProjectRepository) ListWorkflowStates(ctx context.Context, workspaceID, projectID uuid.UUID) ([]project.WorkflowState, error) {
	const op = "sqlite.ProjectRepository.ListWorkflowStates"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          projectID.String(),
	}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	rows, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	out := make([]project.WorkflowState, 0, len(rows))
	for _, row := range rows {
		if st, mErr := mapSQLiteWorkflowState(row); mErr == nil {
			out = append(out, st)
		}
	}
	return out, nil
}

// CreateWorkflowState inserts a custom WorkflowState scoped to (st.WorkspaceID, st.ProjectID).
func (r *ProjectRepository) CreateWorkflowState(ctx context.Context, st project.WorkflowState) (project.WorkflowState, error) {
	const op = "sqlite.ProjectRepository.CreateWorkflowState"
	if err := ctx.Err(); err != nil {
		return project.WorkflowState{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.WorkflowState{}, err
	}
	defer release()

	wsStr := st.WorkspaceID.String()
	projStr := st.ProjectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	if st.IsDefault {
		existing, lErr := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
			WorkspaceID: wsStr,
			ProjectID:   projStr,
		})
		if lErr != nil {
			return project.WorkflowState{}, db.MapSQLDriverError(op, lErr)
		}
		zero := int64(0)
		for _, ex := range existing {
			if ex.IsDefault != 0 {
				if _, uErr := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
					IsDefault:   &zero,
					WorkspaceID: wsStr,
					ProjectID:   projStr,
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
	var isDef int64
	if st.IsDefault {
		isDef = 1
	}
	color := st.ColorHex
	if strings.TrimSpace(color) == "" {
		color = "#6B7280"
	}
	row, err := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
		ID:          stID.String(),
		WorkspaceID: wsStr,
		ProjectID:   projStr,
		Name:        st.Name,
		Category:    db.SQLWorkflowStateCategory(st.Category),
		Position:    int64(st.Position),
		Color:       color,
		IsDefault:   isDef,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	mapped, err := mapSQLiteWorkflowState(row)
	if err != nil {
		return project.WorkflowState{}, err
	}
	mapped.Description = st.Description
	if strings.EqualFold(strings.TrimSpace(st.Category), "archived") {
		mapped.Category = "archived"
	}
	return mapped, nil
}

// UpdateWorkflowState mutates an existing WorkflowState inside (in.WorkspaceID, in.ProjectID).
func (r *ProjectRepository) UpdateWorkflowState(ctx context.Context, in project.UpdateWorkflowStateInput) (project.WorkflowState, error) {
	const op = "sqlite.ProjectRepository.UpdateWorkflowState"
	if err := ctx.Err(); err != nil {
		return project.WorkflowState{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.WorkflowState{}, err
	}
	defer release()

	wsStr := in.WorkspaceID.String()
	projStr := in.ProjectID.String()
	stStr := in.StateID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	cur, err := q.GetWorkflowStateByID(ctx, sqlcgen.GetWorkflowStateByIDParams{
		WorkspaceID: wsStr,
		ID:          stStr,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	if cur.ProjectID != projStr {
		return project.WorkflowState{}, errs.New(errs.ErrNotFound, op, "workflow state not found in project")
	}

	if in.IsDefault != nil && *in.IsDefault {
		existing, lErr := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
			WorkspaceID: wsStr,
			ProjectID:   projStr,
		})
		if lErr != nil {
			return project.WorkflowState{}, db.MapSQLDriverError(op, lErr)
		}
		zero := int64(0)
		for _, ex := range existing {
			if ex.ID != stStr && ex.IsDefault != 0 {
				if _, uErr := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
					IsDefault:   &zero,
					WorkspaceID: wsStr,
					ProjectID:   projStr,
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
	var posPtr *int64
	if in.Position != nil {
		p := int64(*in.Position)
		posPtr = &p
	}
	var defPtr *int64
	if in.IsDefault != nil {
		var d int64
		if *in.IsDefault {
			d = 1
		}
		defPtr = &d
	}

	row, err := q.UpdateWorkflowState(ctx, sqlcgen.UpdateWorkflowStateParams{
		Name:        in.Name,
		Category:    catPtr,
		Position:    posPtr,
		Color:       in.ColorHex,
		IsDefault:   defPtr,
		WorkspaceID: wsStr,
		ProjectID:   projStr,
		ID:          stStr,
	})
	if err != nil {
		return project.WorkflowState{}, db.MapSQLDriverError(op, err)
	}
	mapped, err := mapSQLiteWorkflowState(row)
	if err != nil {
		return project.WorkflowState{}, err
	}
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
	const op = "sqlite.ProjectRepository.ListLabels"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	wsStr := workspaceID.String()
	projStr := projectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	rows, err := q.ListLabelsByProject(ctx, sqlcgen.ListLabelsByProjectParams{
		WorkspaceID: wsStr,
		ProjectID:   &projStr,
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	out := make([]project.ProjectLabel, 0, len(rows))
	for _, row := range rows {
		if lbl, mErr := mapSQLiteProjectLabel(row); mErr == nil {
			out = append(out, lbl)
		}
	}
	return out, nil
}

// CreateLabel inserts a new ProjectLabel scoped to (lbl.WorkspaceID, lbl.ProjectID).
func (r *ProjectRepository) CreateLabel(ctx context.Context, lbl project.ProjectLabel) (project.ProjectLabel, error) {
	const op = "sqlite.ProjectRepository.CreateLabel"
	if err := ctx.Err(); err != nil {
		return project.ProjectLabel{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return project.ProjectLabel{}, err
	}
	defer release()

	wsStr := lbl.WorkspaceID.String()
	projStr := lbl.ProjectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return project.ProjectLabel{}, db.MapSQLDriverError(op, err)
	}
	lblID := lbl.ID
	if lblID == uuid.Nil {
		lblID = uuid.New()
	}
	color := lbl.ColorHex
	if strings.TrimSpace(color) == "" {
		color = "#6B7280"
	}
	row, err := q.CreateLabel(ctx, sqlcgen.CreateLabelParams{
		ID:          lblID.String(),
		WorkspaceID: wsStr,
		ProjectID:   &projStr,
		Name:        lbl.Name,
		Color:       color,
		Description: lbl.Description,
	})
	if err != nil {
		return project.ProjectLabel{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteProjectLabel(row)
}

// DeleteLabel soft-deletes an active ProjectLabel matching (workspaceID, projectID, labelID).
func (r *ProjectRepository) DeleteLabel(ctx context.Context, workspaceID, projectID, labelID uuid.UUID) error {
	const op = "sqlite.ProjectRepository.DeleteLabel"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsStr := workspaceID.String()
	projStr := projectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	rows, err := q.SoftDeleteLabel(ctx, sqlcgen.SoftDeleteLabelParams{
		WorkspaceID: wsStr,
		ProjectID:   &projStr,
		ID:          labelID.String(),
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
// 3. OBJECTIVE REPOSITORY (SQLite)
// =============================================================================

// ObjectiveRepository implements `objective.Repository` over SQLite sqlc queries.
type ObjectiveRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of ObjectiveRepository bound to scope.
func (r *ObjectiveRepository) WithScope(scope tx.Scope) objective.Repository {
	return &ObjectiveRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapSQLiteObjective(row sqlcgen.Objective, actorMap map[string]sqlcgen.Actor) (objective.Objective, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return objective.Objective{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return objective.Objective{}, err
	}
	projID, err := uuid.Parse(row.ProjectID)
	if err != nil {
		return objective.Objective{}, err
	}
	summary, meta := db.DecodeObjectiveDescription(row.Description, row.TargetDate != nil)
	var targetDate *time.Time
	if meta.HasTargetDate && row.TargetDate != nil {
		parsed := db.ParseSQLiteTime(*row.TargetDate)
		targetDate = &parsed
	}
	createdBy, _, _ := resolveSQLitePrincipalFromActorMap(actorMap, row.CreatedByActorID)
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		parsed := db.ParseSQLiteTime(*row.DeletedAt)
		deletedAt = &parsed
	}
	kind := objective.Kind(row.Kind)
	if !kind.Valid() {
		kind = objective.KindGoal
	}
	return objective.Objective{
		ID:              id,
		WorkspaceID:     wsID,
		ProjectID:       projID,
		Kind:            kind,
		Slug:            meta.Slug,
		Title:           row.Title,
		Summary:         summary,
		SuccessCriteria: meta.SuccessCriteria,
		Status:          objective.Status(row.Status),
		PriorityRank:    meta.PriorityRank,
		TargetDate:      targetDate,
		CreatedBy:       createdBy,
		CreatedAt:       db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:       db.ParseSQLiteTime(row.UpdatedAt),
		DeletedAt:       deletedAt,
	}, nil
}

// Create inserts a new Objective scoped to (o.WorkspaceID, o.ProjectID) after checking project-scoped Slug uniqueness.
func (r *ObjectiveRepository) Create(ctx context.Context, o objective.Objective) error {
	const op = "sqlite.ObjectiveRepository.Create"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: o.WorkspaceID.String(),
		ID:          o.ProjectID.String(),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	if o.Slug != "" {
		existingObjs, listErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: o.WorkspaceID.String(),
			ProjectID:   o.ProjectID.String(),
		})
		if listErr != nil {
			return db.MapSQLDriverError(op, listErr)
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
	var targetPtr *string
	if o.TargetDate != nil {
		s := db.FormatSQLiteTime(*o.TargetDate)
		targetPtr = &s
	}
	createdByPtr, err := ensureSQLiteActorRow(ctx, q, o.WorkspaceID, o.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	kindStr := string(o.Kind)
	if !o.Kind.Valid() {
		kindStr = string(objective.KindGoal)
	}
	_, err = q.CreateObjective(ctx, sqlcgen.CreateObjectiveParams{
		ID:               objID.String(),
		WorkspaceID:      o.WorkspaceID.String(),
		ProjectID:        o.ProjectID.String(),
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
	const op = "sqlite.ObjectiveRepository.GetByID"
	if err := ctx.Err(); err != nil {
		return objective.Objective{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return objective.Objective{}, err
	}
	row, err := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          objectiveID.String(),
	})
	if err != nil {
		return objective.Objective{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteObjective(row, buildSQLiteActorLookup(ctx, q, workspaceID))
}

// GetBySlug retrieves an active Objective by (workspaceID, projectID, slug).
func (r *ObjectiveRepository) GetBySlug(ctx context.Context, workspaceID, projectID uuid.UUID, slug string) (objective.Objective, error) {
	const op = "sqlite.ObjectiveRepository.GetBySlug"
	if err := ctx.Err(); err != nil {
		return objective.Objective{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return objective.Objective{}, err
	}
	rows, err := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return objective.Objective{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	for _, row := range rows {
		mapped, mapErr := mapSQLiteObjective(row, actorMap)
		if mapErr == nil && mapped.Slug == slug {
			return mapped, nil
		}
	}
	return objective.Objective{}, errs.New(errs.ErrNotFound, op, "objective slug not found in project")
}

// List returns active Objectives matching filter ordered by PriorityRank ASC, CreatedAt ASC.
func (r *ObjectiveRepository) List(ctx context.Context, filter objective.ListObjectivesFilter) ([]objective.Objective, error) {
	const op = "sqlite.ObjectiveRepository.List"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
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

	var projectIDs []string
	if filter.ProjectID != uuid.Nil {
		projectIDs = []string{filter.ProjectID.String()}
	} else {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{
			WorkspaceID: filter.WorkspaceID.String(),
		})
		if pErr != nil {
			return nil, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			projectIDs = append(projectIDs, p.ID)
		}
	}

	actorMap := buildSQLiteActorLookup(ctx, q, filter.WorkspaceID)
	var collected []objective.Objective
	for _, pid := range projectIDs {
		rows, lErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: filter.WorkspaceID.String(),
			ProjectID:   pid,
			Kind:        kindPtr,
			Status:      statusPtr,
		})
		if lErr != nil {
			return nil, db.MapSQLDriverError(op, lErr)
		}
		for _, row := range rows {
			if mapped, mErr := mapSQLiteObjective(row, actorMap); mErr == nil {
				collected = append(collected, mapped)
			}
		}
	}
	return db.SortAndSliceObjectives(collected, filter.Offset, filter.Limit), nil
}

// Update persists modifications to an active Objective matching (o.WorkspaceID, o.ID).
func (r *ObjectiveRepository) Update(ctx context.Context, o objective.Objective) error {
	const op = "sqlite.ObjectiveRepository.Update"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	existing, err := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
		WorkspaceID: o.WorkspaceID.String(),
		ID:          o.ID.String(),
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	if o.Slug != "" {
		siblings, sErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: o.WorkspaceID.String(),
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
	var targetPtr *string
	if o.TargetDate != nil {
		s := db.FormatSQLiteTime(*o.TargetDate)
		targetPtr = &s
	}

	_, err = q.UpdateObjective(ctx, sqlcgen.UpdateObjectiveParams{
		ParentObjectiveID: existing.ParentObjectiveID,
		Title:             &o.Title,
		Description:       &encodedDesc,
		Kind:              kindPtr,
		Status:            &statusStr,
		StartsAt:          existing.StartsAt,
		TargetDate:        targetPtr,
		WorkspaceID:       o.WorkspaceID.String(),
		ID:                o.ID.String(),
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks (workspaceID, objectiveID) as soft-deleted.
func (r *ObjectiveRepository) SoftDelete(ctx context.Context, workspaceID, objectiveID uuid.UUID) error {
	const op = "sqlite.ObjectiveRepository.SoftDelete"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	rows, err := q.SoftDeleteObjective(ctx, sqlcgen.SoftDeleteObjectiveParams{
		WorkspaceID: workspaceID.String(),
		ID:          objectiveID.String(),
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
// 4. TASK REPOSITORY (SQLite)
// =============================================================================

// TaskRepository implements `task.Repository` over SQLite sqlc queries.
type TaskRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of TaskRepository bound to scope.
func (r *TaskRepository) WithScope(scope tx.Scope) task.Repository {
	return &TaskRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func resolveSQLiteWorkflowStateID(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID, st task.Status) (string, error) {
	if err := seedSQLiteProjectWorkflowStates(ctx, q, workspaceID, projectID); err != nil {
		return "", err
	}
	states, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return "", err
	}
	wantName := strings.ToLower(db.WorkflowStateNameForStatus(st))
	for _, ws := range states {
		if strings.ToLower(ws.Name) == wantName {
			return ws.ID, nil
		}
	}
	for _, seed := range db.DefaultWorkflowStateSeeds() {
		if strings.ToLower(seed.Name) == wantName {
			var isDef int64
			if seed.IsDefault {
				isDef = 1
			}
			created, cErr := q.CreateWorkflowState(ctx, sqlcgen.CreateWorkflowStateParams{
				ID:          uuid.NewString(),
				WorkspaceID: workspaceID.String(),
				ProjectID:   projectID.String(),
				Name:        seed.Name,
				Category:    seed.Category,
				Position:    int64(seed.Position),
				Color:       seed.Color,
				IsDefault:   isDef,
			})
			if cErr == nil {
				return created.ID, nil
			}
			break
		}
	}
	for _, ws := range states {
		if ws.IsDefault == 1 {
			return ws.ID, nil
		}
	}
	if len(states) > 0 {
		return states[0].ID, nil
	}
	return "", errs.New(errs.ErrPreconditionFailed, "sqlite.resolveWorkflowStateID", "project has no workflow states")
}

func buildSQLiteStateMap(ctx context.Context, q sqlcgen.Querier, workspaceID, projectID uuid.UUID) map[string]task.Status {
	states, err := q.ListWorkflowStatesByProject(ctx, sqlcgen.ListWorkflowStatesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return nil
	}
	out := make(map[string]task.Status, len(states))
	for _, st := range states {
		out[st.ID] = db.TaskStatusFromWorkflowState(st.Name, st.Category)
	}
	return out
}

func decodeSQLiteStoredLease(row sqlcgen.Task, wsID, taskID uuid.UUID, actorMap map[string]sqlcgen.Actor) *task.ClaimLease {
	if row.ClaimedByActorID == nil || row.ClaimToken == nil || row.ClaimExpiresAt == nil {
		return nil
	}
	holderID, holderKind, holderHandle := resolveSQLitePrincipalFromActorMap(actorMap, row.ClaimedByActorID)
	expAt := db.ParseSQLiteTime(*row.ClaimExpiresAt)
	updAt := db.ParseSQLiteTime(row.UpdatedAt)
	lease := db.DecodeClaimToken(wsID, taskID, holderID, holderKind, holderHandle, *row.ClaimToken, expAt, updAt)
	return &lease
}

func countSQLiteUnresolvedBlockers(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID, taskID uuid.UUID,
	stateCache map[string]map[string]task.Status,
) (int32, error) {
	rels, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: workspaceID.String(),
		TaskID:      taskID.String(),
	})
	if err != nil {
		return 0, err
	}
	targetStr := taskID.String()
	var count int32
	for _, rel := range rels {
		if rel.TargetTaskID != targetStr || rel.RelationType != "blocks" {
			continue
		}
		blockerRow, bErr := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
			WorkspaceID: workspaceID.String(),
			ID:          rel.SourceTaskID,
		})
		if bErr != nil {
			continue
		}
		projStates, ok := stateCache[blockerRow.ProjectID]
		if !ok {
			bProjID, _ := uuid.Parse(blockerRow.ProjectID)
			projStates = buildSQLiteStateMap(ctx, q, workspaceID, bProjID)
			stateCache[blockerRow.ProjectID] = projStates
		}
		st := projStates[blockerRow.StateID]
		if !st.IsTerminal() {
			count++
		}
	}
	return count, nil
}

func mapSQLiteTaskRow(
	ctx context.Context,
	q sqlcgen.Querier,
	row sqlcgen.Task,
	actorMap map[string]sqlcgen.Actor,
	stateCache map[string]map[string]task.Status,
	now time.Time,
) (task.Task, *task.ClaimLease, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return task.Task{}, nil, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return task.Task{}, nil, err
	}
	projID, err := uuid.Parse(row.ProjectID)
	if err != nil {
		return task.Task{}, nil, err
	}

	projStates, ok := stateCache[row.ProjectID]
	if !ok {
		projStates = buildSQLiteStateMap(ctx, q, wsID, projID)
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
		int32(row.Priority),
		row.ObjectiveID != nil,
		row.ParentTaskID != nil,
		row.AssigneeActorID != nil,
	)

	var objID *uuid.UUID
	if meta.HasObjectiveID && row.ObjectiveID != nil {
		if parsed, pErr := uuid.Parse(*row.ObjectiveID); pErr == nil {
			objID = &parsed
		}
	}
	var parentID *uuid.UUID
	if meta.HasParentTaskID && row.ParentTaskID != nil {
		if parsed, pErr := uuid.Parse(*row.ParentTaskID); pErr == nil {
			parentID = &parsed
		}
	}
	var assigneeID *uuid.UUID
	if meta.HasAssigneeActorID && row.AssigneeActorID != nil {
		principal, _, _ := resolveSQLitePrincipalFromActorMap(actorMap, row.AssigneeActorID)
		if principal != uuid.Nil {
			assigneeID = &principal
		}
	}

	var createdBy uuid.UUID
	if meta.CreatedBy != "" {
		createdBy, _ = uuid.Parse(meta.CreatedBy)
	} else {
		createdBy, _, _ = resolveSQLitePrincipalFromActorMap(actorMap, row.ReporterActorID)
	}

	storedLease := decodeSQLiteStoredLease(row, wsID, id, actorMap)
	var activeLease *task.ClaimLease
	if storedLease != nil && !storedLease.IsExpiredAt(now) {
		clonedLease := storedLease.Clone()
		activeLease = &clonedLease
	}

	blockerCount, err := countSQLiteUnresolvedBlockers(ctx, q, wsID, id, stateCache)
	if err != nil {
		return task.Task{}, nil, err
	}

	var completedAt *time.Time
	if row.CompletedAt != nil {
		parsed := db.ParseSQLiteTime(*row.CompletedAt)
		completedAt = &parsed
	}
	var lastSignalAt *time.Time
	if row.LastSignalAt != nil {
		parsed := db.ParseSQLiteTime(*row.LastSignalAt)
		if !parsed.IsZero() {
			lastSignalAt = &parsed
		}
	}
	var deletedAt *time.Time
	if row.DeletedAt != nil {
		parsed := db.ParseSQLiteTime(*row.DeletedAt)
		deletedAt = &parsed
	}

	var extID string
	if row.ExternalID != nil {
		extID = *row.ExternalID
	}

	return task.Task{
		ID:                     id,
		WorkspaceID:            wsID,
		ProjectID:              projID,
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
		HasMarkdownDoc:         row.HasMarkdownDoc != 0,
		Version:                meta.Version,
		CreatedBy:              createdBy,
		CreatedAt:              db.ParseSQLiteTime(row.CreatedAt),
		UpdatedAt:              db.ParseSQLiteTime(row.UpdatedAt),
		CompletedAt:            completedAt,
		LastSignalAt:           lastSignalAt,
		DeletedAt:              deletedAt,
	}, storedLease, nil
}

// Create inserts a new Task scoped to t.WorkspaceID, auto-allocating Key & sequence when t.Key is empty.
func (r *TaskRepository) Create(ctx context.Context, t task.Task) error {
	const op = "sqlite.TaskRepository.Create"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsStr := t.WorkspaceID.String()
	projStr := t.ProjectID.String()
	if _, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: wsStr,
		ID:          projStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	var objPtr, parentPtr *string
	if t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil {
		objRow, objErr := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
			WorkspaceID: wsStr,
			ID:          t.ObjectiveID.String(),
		})
		if objErr != nil {
			if errors.Is(objErr, sql.ErrNoRows) {
				return errs.New(errs.ErrNotFound, op, "objective not found in workspace")
			}
			return db.MapSQLDriverError(op, objErr)
		}
		if objRow.ProjectID != projStr {
			return errs.Validation(op, "objective belongs to a different project", errs.FieldViolation{
				Field:       "objective_id",
				Description: "objective_id must belong to the same project_id",
			})
		}
		s := t.ObjectiveID.String()
		objPtr = &s
	}
	if t.ParentTaskID != nil && *t.ParentTaskID != uuid.Nil {
		s := t.ParentTaskID.String()
		parentPtr = &s
	}

	key := strings.TrimSpace(t.Key)
	var seqNum int64
	reallocate := key == ""
	keepCustomKey := false

	if !reallocate {
		if parsedSeq, ok := db.ParseTrailingSequence(key); ok {
			conflict, cErr := q.CheckTaskSequenceOrIdentifierConflict(ctx, sqlcgen.CheckTaskSequenceOrIdentifierConflictParams{
				WorkspaceID:    wsStr,
				ProjectID:      projStr,
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
				WorkspaceID:    wsStr,
				ProjectID:      projStr,
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
				WorkspaceID: wsStr,
				ID:          projStr,
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
				WorkspaceID:    wsStr,
				ProjectID:      projStr,
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
	stateID, err := resolveSQLiteWorkflowStateID(ctx, q, t.WorkspaceID, t.ProjectID, status)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	reporterPtr, err := ensureSQLiteActorRow(ctx, q, t.WorkspaceID, t.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	var assigneePtr *string
	if t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil {
		assigneePtr, err = ensureSQLiteActorRow(ctx, q, t.WorkspaceID, *t.AssigneeActorID, "", "")
		if err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	var completedPtr *string
	if t.CompletedAt != nil {
		s := db.FormatSQLiteTime(*t.CompletedAt)
		completedPtr = &s
	} else if status.IsTerminal() {
		s := db.FormatSQLiteTime(time.Now().UTC())
		completedPtr = &s
	}

	version := t.Version
	if version <= 0 {
		version = 1
	}

	var extPtr *string
	if trimmedExt := strings.TrimSpace(t.ExternalID); trimmedExt != "" {
		extPtr = &trimmedExt
	}
	var lastSignalPtr *string
	if t.LastSignalAt != nil && !t.LastSignalAt.IsZero() {
		s := db.FormatSQLiteTime(t.LastSignalAt.UTC())
		lastSignalPtr = &s
	}

	_, err = q.CreateTask(ctx, sqlcgen.CreateTaskParams{
		ID:                 taskID.String(),
		WorkspaceID:        t.WorkspaceID.String(),
		ProjectID:          t.ProjectID.String(),
		ObjectiveID:        objPtr,
		ParentTaskID:       parentPtr,
		StateID:            stateID,
		SequenceNumber:     seqNum,
		Identifier:         key,
		ExternalID:         extPtr,
		Kind:               db.SQLTaskKind(t.Kind),
		Priority:           int64(db.SQLTaskPriority(t.Priority)),
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
	const op = "sqlite.TaskRepository.GetByID"
	if err := ctx.Err(); err != nil {
		return task.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          taskID.String(),
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapSQLiteTaskRow(ctx, q, row, actorMap, make(map[string]map[string]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// GetByKey fetches a full Task aggregate by (workspaceID, key).
func (r *TaskRepository) GetByKey(ctx context.Context, workspaceID uuid.UUID, key string) (task.Task, error) {
	const op = "sqlite.TaskRepository.GetByKey"
	if err := ctx.Err(); err != nil {
		return task.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByIdentifier(ctx, sqlcgen.GetTaskByIdentifierParams{
		WorkspaceID: workspaceID.String(),
		Identifier:  key,
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapSQLiteTaskRow(ctx, q, row, actorMap, make(map[string]map[string]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// GetByExternalID fetches a full Task aggregate by (workspaceID, projectID, externalID).
func (r *TaskRepository) GetByExternalID(ctx context.Context, workspaceID, projectID uuid.UUID, externalID string) (task.Task, error) {
	const op = "sqlite.TaskRepository.GetByExternalID"
	if err := ctx.Err(); err != nil {
		return task.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	ext := strings.TrimSpace(externalID)
	if ext == "" {
		return task.Task{}, errs.New(errs.ErrNotFound, op, "task not found")
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	row, err := q.GetTaskByExternalID(ctx, sqlcgen.GetTaskByExternalIDParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
		ExternalID:  &ext,
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapSQLiteTaskRow(ctx, q, row, actorMap, make(map[string]map[string]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// Update persists field and status mutations on an active Task, enforcing expectedVersion optimistic locking.
func (r *TaskRepository) Update(ctx context.Context, t task.Task, expectedVersion *int64) error {
	const op = "sqlite.TaskRepository.Update"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	existing, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: t.WorkspaceID.String(),
		ID:          t.ID.String(),
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	_, existingMeta := db.DecodeTaskDescription(
		existing.Description,
		existing.Kind,
		int32(existing.Priority),
		existing.ObjectiveID != nil,
		existing.ParentTaskID != nil,
		existing.AssigneeActorID != nil,
	)
	if expectedVersion != nil && existingMeta.Version != *expectedVersion {
		return errs.New(errs.ErrConflict, op, "task optimistic concurrency version mismatch")
	}
	nextVersion := existingMeta.Version + 1

	projID, _ := uuid.Parse(existing.ProjectID)
	status := t.Status
	if status == "" {
		status = task.StatusReady
	}
	stateID, err := resolveSQLiteWorkflowStateID(ctx, q, t.WorkspaceID, projID, status)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	var objPtr, parentPtr, assigneePtr *string
	if t.ObjectiveID != nil && *t.ObjectiveID != uuid.Nil {
		objRow, objErr := q.GetObjectiveByID(ctx, sqlcgen.GetObjectiveByIDParams{
			WorkspaceID: t.WorkspaceID.String(),
			ID:          t.ObjectiveID.String(),
		})
		if objErr != nil {
			if errors.Is(objErr, sql.ErrNoRows) {
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
		s := t.ObjectiveID.String()
		objPtr = &s
	}
	if t.ParentTaskID != nil && *t.ParentTaskID != uuid.Nil {
		s := t.ParentTaskID.String()
		parentPtr = &s
	}
	if t.AssigneeActorID != nil && *t.AssigneeActorID != uuid.Nil {
		assigneePtr, err = ensureSQLiteActorRow(ctx, q, t.WorkspaceID, *t.AssigneeActorID, "", "")
		if err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	sqlKind := db.SQLTaskKind(t.Kind)
	sqlPrio := int64(db.SQLTaskPriority(t.Priority))
	encodedDesc := db.EncodeTaskDescription(t, nextVersion)
	encodedAC := db.EncodeAcceptanceCriteria(t.AcceptanceCriteria)

	var lastSignalPtr *string
	if t.LastSignalAt != nil && !t.LastSignalAt.IsZero() {
		s := db.FormatSQLiteTime(t.LastSignalAt.UTC())
		lastSignalPtr = &s
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
		WorkspaceID:        t.WorkspaceID.String(),
		ID:                 t.ID.String(),
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
			WorkspaceID: t.WorkspaceID.String(),
			ID:          t.ID.String(),
		}); err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}

	var completedPtr *string
	if t.CompletedAt != nil {
		s := db.FormatSQLiteTime(*t.CompletedAt)
		completedPtr = &s
	} else if status.IsTerminal() {
		s := db.FormatSQLiteTime(time.Now().UTC())
		completedPtr = &s
	}

	_, err = q.TransitionTaskState(ctx, sqlcgen.TransitionTaskStateParams{
		StateID:     stateID,
		CompletedAt: completedPtr,
		WorkspaceID: t.WorkspaceID.String(),
		ID:          t.ID.String(),
	})
	return db.MapSQLDriverError(op, err)
}

// SoftDelete marks (workspaceID, taskID) as soft-deleted and releases any held lease.
func (r *TaskRepository) SoftDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "sqlite.TaskRepository.SoftDelete"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	rows, err := q.SoftDeleteTask(ctx, sqlcgen.SoftDeleteTaskParams{
		WorkspaceID: workspaceID.String(),
		ID:          taskID.String(),
	})
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
	const op = "sqlite.TaskRepository.ListByWorkspace"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListTasksByWorkspace(ctx, workspaceID.String())
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	stateCache := make(map[string]map[string]task.Status)
	now := time.Now().UTC()
	out := make([]task.Task, 0, len(rows))
	for _, row := range rows {
		mapped, _, mErr := mapSQLiteTaskRow(ctx, q, row, actorMap, stateCache, now)
		if mErr != nil {
			return nil, db.MapSQLDriverError(op, mErr)
		}
		out = append(out, mapped)
	}
	return out, nil
}

// SetHasMarkdownDoc toggles has_markdown_doc on (workspaceID, taskID) and returns the updated Task.
func (r *TaskRepository) SetHasMarkdownDoc(ctx context.Context, workspaceID, taskID uuid.UUID, hasDoc bool) (task.Task, error) {
	const op = "sqlite.TaskRepository.SetHasMarkdownDoc"
	if err := ctx.Err(); err != nil {
		return task.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	defer release()

	var flag int64
	if hasDoc {
		flag = 1
	}
	row, err := q.SetTaskMarkdownDoc(ctx, sqlcgen.SetTaskMarkdownDocParams{
		HasMarkdownDoc: flag,
		WorkspaceID:    workspaceID.String(),
		ID:             taskID.String(),
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapSQLiteTaskRow(ctx, q, row, actorMap, make(map[string]map[string]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// TouchSignal updates last_signal_at on (workspaceID, taskID) without changing updated_at and returns the updated Task.
func (r *TaskRepository) TouchSignal(ctx context.Context, workspaceID, taskID uuid.UUID, signalAt time.Time) (task.Task, error) {
	const op = "sqlite.TaskRepository.TouchSignal"
	if err := ctx.Err(); err != nil {
		return task.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.Task{}, err
	}
	defer release()

	var sigPtr *string
	if !signalAt.IsZero() {
		formatted := db.FormatSQLiteTime(signalAt.UTC())
		sigPtr = &formatted
	}
	row, err := q.TouchTaskLastSignal(ctx, sqlcgen.TouchTaskLastSignalParams{
		LastSignalAt: sigPtr,
		WorkspaceID:  workspaceID.String(),
		ID:           taskID.String(),
	})
	if err != nil {
		return task.Task{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	mapped, _, err := mapSQLiteTaskRow(ctx, q, row, actorMap, make(map[string]map[string]task.Status), time.Now().UTC())
	return mapped, db.MapSQLDriverError(op, err)
}

// HardDelete detaches child subtasks and permanently deletes (workspaceID, taskID).
func (r *TaskRepository) HardDelete(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "sqlite.TaskRepository.HardDelete"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsStr := workspaceID.String()
	taskIDStr := taskID.String()
	rels, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: wsStr,
		TaskID:      taskIDStr,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	for _, rel := range rels {
		if relID, pErr := uuid.Parse(rel.ID); pErr == nil {
			if dErr := q.DeleteDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.DeleteDSLExecutionBatchByIdempotencyKeyParams{
				WorkspaceID:    wsStr,
				IdempotencyKey: db.RelationMetaIdempotencyKey(relID),
			}); dErr != nil {
				return db.MapSQLDriverError(op, dErr)
			}
		}
	}
	if err := q.DetachSubtasksFromParent(ctx, sqlcgen.DetachSubtasksFromParentParams{
		WorkspaceID:  wsStr,
		ParentTaskID: &taskIDStr,
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	rows, err := q.HardDeleteTask(ctx, sqlcgen.HardDeleteTaskParams{
		WorkspaceID: wsStr,
		ID:          taskIDStr,
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task not found")
	}
	return nil
}

func loadSQLiteProjectHydratedTasks(
	ctx context.Context,
	q sqlcgen.Querier,
	workspaceID uuid.UUID,
	projectIDs []uuid.UUID,
	actorMap map[string]sqlcgen.Actor,
	now time.Time,
) ([]task.Task, map[uuid.UUID]string, map[uuid.UUID]string, map[uuid.UUID]task.ClaimLease, error) {
	stateCache := make(map[string]map[string]task.Status)
	var allTasks []task.Task
	objSlugs := make(map[uuid.UUID]string)
	parentKeys := make(map[uuid.UUID]string)
	leases := make(map[uuid.UUID]task.ClaimLease)

	for _, pid := range projectIDs {
		objs, oErr := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
			WorkspaceID: workspaceID.String(),
			ProjectID:   pid.String(),
		})
		if oErr != nil {
			return nil, nil, nil, nil, oErr
		}
		for _, oRow := range objs {
			if oUUID, pErr := uuid.Parse(oRow.ID); pErr == nil {
				_, oMeta := db.DecodeObjectiveDescription(oRow.Description, oRow.TargetDate != nil)
				objSlugs[oUUID] = oMeta.Slug
			}
		}

		rows, tErr := q.ListTasksByProject(ctx, sqlcgen.ListTasksByProjectParams{
			WorkspaceID: workspaceID.String(),
			ProjectID:   pid.String(),
		})
		if tErr != nil {
			return nil, nil, nil, nil, tErr
		}
		for _, row := range rows {
			mapped, storedLease, mErr := mapSQLiteTaskRow(ctx, q, row, actorMap, stateCache, now)
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

// QueryCompactQueue returns token-optimized CompactTaskSummary rows matching filter.
func (r *TaskRepository) QueryCompactQueue(ctx context.Context, filter task.AgentWorkQueueFilter, now time.Time) ([]task.CompactTaskSummary, error) {
	const op = "sqlite.TaskRepository.QueryCompactQueue"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}

	var projectIDs []uuid.UUID
	if filter.ProjectID != uuid.Nil {
		projectIDs = []uuid.UUID{filter.ProjectID}
	} else {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{
			WorkspaceID: filter.WorkspaceID.String(),
		})
		if pErr != nil {
			return nil, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			if pid, uErr := uuid.Parse(p.ID); uErr == nil {
				projectIDs = append(projectIDs, pid)
			}
		}
	}

	actorMap := buildSQLiteActorLookup(ctx, q, filter.WorkspaceID)
	tasks, objSlugs, parentKeys, leases, err := loadSQLiteProjectHydratedTasks(ctx, q, filter.WorkspaceID, projectIDs, actorMap, now)
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}
	return db.FilterAndSortCompactQueue(tasks, objSlugs, parentKeys, leases, filter, now), nil
}

// BuildProjectDigest assembles a situational awareness ProjectDigest snapshot for (workspaceID, projectID).
func (r *TaskRepository) BuildProjectDigest(ctx context.Context, workspaceID, projectID uuid.UUID, opts task.DigestOptions, now time.Time) (task.ProjectDigest, error) {
	const op = "sqlite.TaskRepository.BuildProjectDigest"
	if err := ctx.Err(); err != nil {
		return task.ProjectDigest{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.ProjectDigest{}, err
	}

	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	projRow, err := q.GetProjectByID(ctx, sqlcgen.GetProjectByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          projectID.String(),
	})
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	projDomain, err := mapSQLiteProject(projRow, actorMap)
	if err != nil {
		return task.ProjectDigest{}, err
	}

	objRows, err := q.ListObjectivesByProject(ctx, sqlcgen.ListObjectivesByProjectParams{
		WorkspaceID: workspaceID.String(),
		ProjectID:   projectID.String(),
	})
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	objs := make([]objective.Objective, 0, len(objRows))
	for _, oRow := range objRows {
		if o, mErr := mapSQLiteObjective(oRow, actorMap); mErr == nil {
			objs = append(objs, o)
		}
	}

	tasks, _, _, leases, err := loadSQLiteProjectHydratedTasks(ctx, q, workspaceID, []uuid.UUID{projectID}, actorMap, now)
	if err != nil {
		return task.ProjectDigest{}, db.MapSQLDriverError(op, err)
	}
	return db.AssembleProjectDigest(projDomain, objs, tasks, leases, opts, now), nil
}

// UpsertLease atomically acquires or renews the exclusive ClaimLease on lease.TaskID.
func (r *TaskRepository) UpsertLease(ctx context.Context, lease task.ClaimLease) error {
	const op = "sqlite.TaskRepository.UpsertLease"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	actorRowPtr, err := ensureSQLiteActorRow(ctx, q, lease.WorkspaceID, lease.HolderActorID, lease.HolderKind, lease.HolderHandle)
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	encodedToken := db.EncodeClaimToken(lease)
	expStr := db.FormatSQLiteTime(lease.ExpiresAt)

	_, err = q.ClaimTaskLease(ctx, sqlcgen.ClaimTaskLeaseParams{
		ActorID:        actorRowPtr,
		ClaimToken:     &encodedToken,
		ClaimExpiresAt: &expStr,
		StateID:        nil,
		WorkspaceID:    lease.WorkspaceID.String(),
		ID:             lease.TaskID.String(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if _, getErr := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
				WorkspaceID: lease.WorkspaceID.String(),
				ID:          lease.TaskID.String(),
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
	const op = "sqlite.TaskRepository.GetLease"
	if err := ctx.Err(); err != nil {
		return task.ClaimLease{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return task.ClaimLease{}, err
	}
	row, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          taskID.String(),
	})
	if err != nil {
		return task.ClaimLease{}, db.MapSQLDriverError(op, err)
	}
	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	stored := decodeSQLiteStoredLease(row, workspaceID, taskID, actorMap)
	if stored == nil {
		return task.ClaimLease{}, errs.New(errs.ErrNotFound, op, "no active or expired claim lease on task")
	}
	return *stored, nil
}

// DeleteLease clears the ClaimLease columns on (workspaceID, taskID).
func (r *TaskRepository) DeleteLease(ctx context.Context, workspaceID, taskID uuid.UUID) error {
	const op = "sqlite.TaskRepository.DeleteLease"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	rows, err := q.ReleaseTaskLease(ctx, sqlcgen.ReleaseTaskLeaseParams{
		WorkspaceID: workspaceID.String(),
		ID:          taskID.String(),
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
	const op = "sqlite.TaskRepository.DeleteExpiredLeases"
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return 0, err
	}
	defer release()

	var workspaceIDs []string
	if workspaceID != uuid.Nil {
		workspaceIDs = []string{workspaceID.String()}
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
	for _, wsStr := range workspaceIDs {
		projs, pErr := q.ListProjectsByWorkspace(ctx, sqlcgen.ListProjectsByWorkspaceParams{WorkspaceID: wsStr})
		if pErr != nil {
			return reaped, db.MapSQLDriverError(op, pErr)
		}
		for _, p := range projs {
			tasks, tErr := q.ListTasksByProject(ctx, sqlcgen.ListTasksByProjectParams{
				WorkspaceID: wsStr,
				ProjectID:   p.ID,
			})
			if tErr != nil {
				return reaped, db.MapSQLDriverError(op, tErr)
			}
			for _, tRow := range tasks {
				if tRow.ClaimedByActorID == nil {
					continue
				}
				expired := tRow.ClaimExpiresAt == nil || !cutoff.Before(db.ParseSQLiteTime(*tRow.ClaimExpiresAt))
				if expired {
					n, rErr := q.ReleaseTaskLease(ctx, sqlcgen.ReleaseTaskLeaseParams{
						WorkspaceID: wsStr,
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
	const op = "sqlite.TaskRepository.CreateRelation"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
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

	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	// Verify both endpoints belong to rel.WorkspaceID and are active.
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: rel.WorkspaceID.String(),
		ID:          rel.SourceTaskID.String(),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: rel.WorkspaceID.String(),
		ID:          rel.TargetTaskID.String(),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	relID := rel.ID
	if relID == uuid.Nil {
		relID = uuid.New()
	}
	createdByPtr, err := ensureSQLiteActorRow(ctx, q, rel.WorkspaceID, rel.CreatedBy, "", "")
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}

	if _, err := q.CreateTaskRelation(ctx, sqlcgen.CreateTaskRelationParams{
		ID:               relID.String(),
		WorkspaceID:      rel.WorkspaceID.String(),
		SourceTaskID:     rel.SourceTaskID.String(),
		TargetTaskID:     rel.TargetTaskID.String(),
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
			ID:             uuid.NewString(),
			WorkspaceID:    rel.WorkspaceID.String(),
			ActorID:        createdByPtr,
			IdempotencyKey: db.RelationMetaIdempotencyKey(relID),
			Mode:           "relation_meta",
			ScriptHash:     relID.String(),
			StatementCount: 0,
			Status:         "succeeded",
			SummaryJson:    string(sidecarJSON),
		}); err != nil {
			return db.MapSQLDriverError(op, err)
		}
	}
	return nil
}

// DeleteRelation removes a directed TaskRelation edge matching (workspaceID, relationID).
func (r *TaskRepository) DeleteRelation(ctx context.Context, workspaceID, relationID uuid.UUID) error {
	const op = "sqlite.TaskRepository.DeleteRelation"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	wsStr := workspaceID.String()
	rows, err := q.DeleteTaskRelation(ctx, sqlcgen.DeleteTaskRelationParams{
		WorkspaceID: wsStr,
		ID:          relationID.String(),
	})
	if err != nil {
		return db.MapSQLDriverError(op, err)
	}
	if rows == 0 {
		return errs.New(errs.ErrNotFound, op, "task relation not found")
	}
	if err := q.DeleteDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.DeleteDSLExecutionBatchByIdempotencyKeyParams{
		WorkspaceID:    wsStr,
		IdempotencyKey: db.RelationMetaIdempotencyKey(relationID),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}
	return nil
}

// ListRelationsForTask returns all inbound and outbound TaskRelation edges for (workspaceID, taskID).
func (r *TaskRepository) ListRelationsForTask(ctx context.Context, workspaceID, taskID uuid.UUID) ([]task.TaskRelation, error) {
	const op = "sqlite.TaskRepository.ListRelationsForTask"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListTaskRelations(ctx, sqlcgen.ListTaskRelationsParams{
		WorkspaceID: workspaceID.String(),
		TaskID:      taskID.String(),
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}

	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	out := make([]task.TaskRelation, 0, len(rows))
	for _, row := range rows {
		relID, _ := uuid.Parse(row.ID)
		srcID, _ := uuid.Parse(row.SourceTaskID)
		tgtID, _ := uuid.Parse(row.TargetTaskID)
		createdBy, _, _ := resolveSQLitePrincipalFromActorMap(actorMap, row.CreatedByActorID)

		domType := task.RelationType(row.RelationType)
		note := ""
		if batch, bErr := q.GetDSLExecutionBatchByIdempotencyKey(ctx, sqlcgen.GetDSLExecutionBatchByIdempotencyKeyParams{
			WorkspaceID:    workspaceID.String(),
			IdempotencyKey: db.RelationMetaIdempotencyKey(relID),
		}); bErr == nil {
			var sc db.RelationSidecarMeta
			if json.Unmarshal([]byte(batch.SummaryJson), &sc) == nil {
				if sc.DomainType != "" {
					domType = sc.DomainType
				}
				note = sc.Note
			}
		}

		out = append(out, task.TaskRelation{
			ID:           relID,
			WorkspaceID:  workspaceID,
			SourceTaskID: srcID,
			TargetTaskID: tgtID,
			Type:         domType,
			Note:         note,
			CreatedBy:    createdBy,
			CreatedAt:    db.ParseSQLiteTime(row.CreatedAt),
		})
	}
	return out, nil
}

// CreateComment appends an immutable TaskComment to (c.WorkspaceID, c.TaskID).
func (r *TaskRepository) CreateComment(ctx context.Context, c task.TaskComment) error {
	const op = "sqlite.TaskRepository.CreateComment"
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	q, release, err := resolveWriteQuerier(ctx, op, r.adapter, r.scoped, r.scope)
	if err != nil {
		return err
	}
	defer release()

	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: c.WorkspaceID.String(),
		ID:          c.TaskID.String(),
	}); err != nil {
		return db.MapSQLDriverError(op, err)
	}

	commentID := c.ID
	if commentID == uuid.Nil {
		commentID = uuid.New()
	}
	authorPtr, err := ensureSQLiteActorRow(ctx, q, c.WorkspaceID, c.AuthorActorID, c.AuthorKind, c.AuthorHandle)
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
		ID:            commentID.String(),
		WorkspaceID:   c.WorkspaceID.String(),
		TaskID:        c.TaskID.String(),
		AuthorActorID: *authorPtr,
		Kind:          db.SQLCommentKind(c.Kind),
		Body:          c.Body,
		MetadataJson:  string(db.EncodeCommentMeta(c)),
	})
	return db.MapSQLDriverError(op, err)
}

// ListComments returns TaskComments for (workspaceID, taskID) ordered chronologically.
func (r *TaskRepository) ListComments(ctx context.Context, workspaceID, taskID uuid.UUID, limit int32) ([]task.TaskComment, error) {
	const op = "sqlite.TaskRepository.ListComments"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}

	if _, err := q.GetTaskByID(ctx, sqlcgen.GetTaskByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          taskID.String(),
	}); err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}

	rows, err := q.ListTaskComments(ctx, sqlcgen.ListTaskCommentsParams{
		WorkspaceID: workspaceID.String(),
		TaskID:      taskID.String(),
	})
	if err != nil {
		return nil, db.MapSQLDriverError(op, err)
	}

	actorMap := buildSQLiteActorLookup(ctx, q, workspaceID)
	out := make([]task.TaskComment, 0, len(rows))
	for _, row := range rows {
		cID, _ := uuid.Parse(row.ID)
		meta := db.DecodeCommentMeta([]byte(row.MetadataJson), row.Kind)
		authorRowID := row.AuthorActorID
		principalID, actorKind, actorHandle := resolveSQLitePrincipalFromActorMap(actorMap, &authorRowID)
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
			ID:            cID,
			WorkspaceID:   workspaceID,
			TaskID:        taskID,
			AuthorActorID: principalID,
			AuthorKind:    actorKind,
			AuthorHandle:  actorHandle,
			Kind:          meta.DomainKind,
			Body:          row.Body,
			CreatedAt:     db.ParseSQLiteTime(row.CreatedAt),
		})
	}
	if limit > 0 && len(out) > int(limit) {
		out = out[:int(limit)]
	}
	return out, nil
}

// =============================================================================
// 5. AUDIT REPOSITORY (SQLite)
// =============================================================================

// AuditRepository implements `audit.Repository` over SQLite sqlc queries.
type AuditRepository struct {
	adapter *Adapter
	scoped  bool
	scope   tx.Scope
}

// WithScope returns a shallow copy of AuditRepository bound to scope.
func (r *AuditRepository) WithScope(scope tx.Scope) audit.Repository {
	return &AuditRepository{adapter: r.adapter, scoped: true, scope: scope}
}

func mapSQLiteAuditRecord(row sqlcgen.AuditLog, actorMap map[string]sqlcgen.Actor) (audit.Record, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return audit.Record{}, err
	}
	wsID, err := uuid.Parse(row.WorkspaceID)
	if err != nil {
		return audit.Record{}, err
	}
	actorID, actorKind, actorHandle := resolveSQLitePrincipalFromActorMap(actorMap, row.ActorID)

	var oldBytes, newBytes []byte
	if row.OldValuesJson != nil && *row.OldValuesJson != "" {
		oldBytes = []byte(*row.OldValuesJson)
	}
	if row.NewValuesJson != nil && *row.NewValuesJson != "" {
		newBytes = []byte(*row.NewValuesJson)
	}
	return audit.Record{
		ID:           id,
		WorkspaceID:  wsID,
		ActorID:      actorID,
		ActorKind:    actorKind,
		ActorHandle:  actorHandle,
		Action:       tx.AuditAction(row.Action),
		ResourceType: row.ResourceType,
		ResourceID:   row.ResourceID,
		OldValues:    oldBytes,
		NewValues:    newBytes,
		OccurredAt:   db.ParseSQLiteTime(row.CreatedAt),
	}, nil
}

// GetByID retrieves a single immutable audit Record by (workspaceID, auditID).
func (r *AuditRepository) GetByID(ctx context.Context, workspaceID, auditID uuid.UUID) (audit.Record, error) {
	const op = "sqlite.AuditRepository.GetByID"
	if err := ctx.Err(); err != nil {
		return audit.Record{}, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return audit.Record{}, err
	}
	row, err := q.GetAuditLogByID(ctx, sqlcgen.GetAuditLogByIDParams{
		WorkspaceID: workspaceID.String(),
		ID:          auditID.String(),
	})
	if err != nil {
		return audit.Record{}, db.MapSQLDriverError(op, err)
	}
	return mapSQLiteAuditRecord(row, buildSQLiteActorLookup(ctx, q, workspaceID))
}

// Query returns immutable audit Records matching filter within filter.WorkspaceID ordered newest-first.
func (r *AuditRepository) Query(ctx context.Context, filter audit.QueryFilter) ([]audit.Record, error) {
	const op = "sqlite.AuditRepository.Query"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	q, err := resolveReadQuerier(r.adapter, r.scoped, r.scope)
	if err != nil {
		return nil, err
	}

	wsStr := filter.WorkspaceID.String()
	actorMap := buildSQLiteActorLookup(ctx, q, filter.WorkspaceID)
	hasBothResource := filter.ResourceType != "" && filter.ResourceID != ""
	effectiveLimit := filter.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 100
	}

	if !db.AuditQueryNeedsPostFilter(filter) {
		sqlOffset := int64(max(filter.Offset, 0))
		sqlLimit := int64(effectiveLimit)
		var rows []sqlcgen.AuditLog
		if hasBothResource {
			rows, err = q.ListAuditLogsByResource(ctx, sqlcgen.ListAuditLogsByResourceParams{
				WorkspaceID:  wsStr,
				ResourceType: filter.ResourceType,
				ResourceID:   filter.ResourceID,
				Offset:       sqlOffset,
				Limit:        sqlLimit,
			})
		} else {
			rows, err = q.ListAuditLogsByWorkspace(ctx, sqlcgen.ListAuditLogsByWorkspaceParams{
				WorkspaceID: wsStr,
				Offset:      sqlOffset,
				Limit:       sqlLimit,
			})
		}
		if err != nil {
			return nil, db.MapSQLDriverError(op, err)
		}
		mapped := make([]audit.Record, 0, len(rows))
		for _, row := range rows {
			if rec, mErr := mapSQLiteAuditRecord(row, actorMap); mErr == nil {
				mapped = append(mapped, rec)
			}
		}
		sliceFilter := filter
		sliceFilter.Offset = 0
		sliceFilter.Limit = effectiveLimit
		return db.FilterSortAndSliceAuditRecords(mapped, sliceFilter), nil
	}

	want := int64(max(filter.Offset, 0)) + int64(effectiveLimit)
	batchSize := max(int64(10000), want*4)
	var mapped []audit.Record
	probeFilter := filter
	probeFilter.Offset = 0
	probeFilter.Limit = int32(min(want+64, 1<<30))

	for sqlOffset := int64(0); ; sqlOffset += batchSize {
		var rows []sqlcgen.AuditLog
		if hasBothResource {
			rows, err = q.ListAuditLogsByResource(ctx, sqlcgen.ListAuditLogsByResourceParams{
				WorkspaceID:  wsStr,
				ResourceType: filter.ResourceType,
				ResourceID:   filter.ResourceID,
				Offset:       sqlOffset,
				Limit:        batchSize,
			})
		} else {
			rows, err = q.ListAuditLogsByWorkspace(ctx, sqlcgen.ListAuditLogsByWorkspaceParams{
				WorkspaceID: wsStr,
				Offset:      sqlOffset,
				Limit:       batchSize,
			})
		}
		if err != nil {
			return nil, db.MapSQLDriverError(op, err)
		}
		for _, row := range rows {
			if rec, mErr := mapSQLiteAuditRecord(row, actorMap); mErr == nil {
				mapped = append(mapped, rec)
			}
		}
		if int64(len(rows)) < batchSize {
			break
		}
		if int64(len(db.FilterSortAndSliceAuditRecords(mapped, probeFilter))) >= want+64 {
			break
		}
	}
	return db.FilterSortAndSliceAuditRecords(mapped, filter), nil
}
