// Package languagehandler implements the Buf ConnectRPC `LanguageServiceHandler`
// transport adapter for executing, validating, and documenting Cloudflare Code
// Mode ES6 JavaScript scripts (`flowsk.*`) inside a single ACID transaction.
package languagehandler

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	flowsk_language_pb "github.com/mindmain/flowsk/gen/go/flowsk/language/v1"
	"github.com/mindmain/flowsk/gen/go/flowsk/language/v1/flowsk_language_pbconnect"
	"github.com/mindmain/flowsk/internal/adapter/codemode"
	"github.com/mindmain/flowsk/internal/adapter/db"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/language"
	"github.com/mindmain/flowsk/internal/transport/connectrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	guideSpecVersion = "2.0.0"
	guideFilename    = "FLOWSK_CODEMODE.md"
)

// CodeExecutor evaluates a sandboxed ES6 JavaScript snippet inside one outer
// ACID database transaction (`*codemode.Engine` satisfies this port).
type CodeExecutor interface {
	// Execute evaluates in.Script against the `flowsk` SDK inside a single transaction.
	Execute(ctx context.Context, in codemode.ExecuteInput) (codemode.ExecuteOutput, error)
}

// DataDependencies defines the segregated persistence provider contracts consumed
// by `New` when constructing a default `codemode.Engine`.
type DataDependencies interface {
	db.TxManagerProvider
	db.ProjectRepoProvider
	db.ObjectiveRepoProvider
	db.TaskRepoProvider
}

// ServicesDependencies defines the segregated service provider contracts consumed
// by `New` to extract the Code Mode JavaScript executor and SDK guide provider.
type ServicesDependencies interface {
	// CodeExecutor returns the resolved Code Mode JavaScript execution engine.
	CodeExecutor() CodeExecutor

	// GuideProvider returns the resolved Code Mode TypeScript SDK specification and examples provider.
	GuideProvider() language.GuideProvider
}

// Handler implements `flowsk_language_pbconnect.LanguageServiceHandler` with
// pre-resolved Code Mode dependencies and zero stored provider wrappers.
type Handler struct {
	txMgr         tx.Manager
	projectRepo   project.Repository
	objectiveRepo objective.Repository
	taskRepo      task.Repository
	executor      CodeExecutor
	guide         language.GuideProvider
}

// Compile-time assertion ensuring *Handler satisfies LanguageServiceHandler.
var _ flowsk_language_pbconnect.LanguageServiceHandler = (*Handler)(nil)

// New resolves `DataDependencies` and `ServicesDependencies` immediately at startup
// and returns an immutable `LanguageService` ConnectRPC handler.
func New(dataDeps DataDependencies, svcDeps ServicesDependencies) *Handler {
	var (
		txMgr         tx.Manager
		projectRepo   project.Repository
		objectiveRepo objective.Repository
		taskRepo      task.Repository
		executor      CodeExecutor
		guide         language.GuideProvider
	)
	if dataDeps != nil {
		txMgr = dataDeps.TxManager()
		projectRepo = dataDeps.ProjectRepository()
		objectiveRepo = dataDeps.ObjectiveRepository()
		taskRepo = dataDeps.TaskRepository()
	}
	if svcDeps != nil {
		executor = svcDeps.CodeExecutor()
		guide = svcDeps.GuideProvider()
	}
	if executor == nil {
		executor = codemode.NewEngine(codemode.Dependencies{
			TxManager:     txMgr,
			ProjectRepo:   projectRepo,
			ObjectiveRepo: objectiveRepo,
			TaskRepo:      taskRepo,
		})
	}
	if guide == nil {
		guide = language.NewGuideProvider()
	}
	return &Handler{
		txMgr:         txMgr,
		projectRepo:   projectRepo,
		objectiveRepo: objectiveRepo,
		taskRepo:      taskRepo,
		executor:      executor,
		guide:         guide,
	}
}

// Executor returns the resolved `CodeExecutor` bound to this handler.
func (h *Handler) Executor() CodeExecutor {
	if h == nil {
		return nil
	}
	return h.executor
}

// Guide returns the resolved `language.GuideProvider` bound to this handler.
func (h *Handler) Guide() language.GuideProvider {
	if h == nil {
		return nil
	}
	return h.guide
}

// TxManager returns the resolved `tx.Manager` bound to this handler.
func (h *Handler) TxManager() tx.Manager {
	if h == nil {
		return nil
	}
	return h.txMgr
}

// ProjectRepository returns the resolved `project.Repository` bound to this handler.
func (h *Handler) ProjectRepository() project.Repository {
	if h == nil {
		return nil
	}
	return h.projectRepo
}

// ObjectiveRepository returns the resolved `objective.Repository` bound to this handler.
func (h *Handler) ObjectiveRepository() objective.Repository {
	if h == nil {
		return nil
	}
	return h.objectiveRepo
}

// TaskRepository returns the resolved `task.Repository` bound to this handler.
func (h *Handler) TaskRepository() task.Repository {
	if h == nil {
		return nil
	}
	return h.taskRepo
}

// ExecuteScript evaluates a sandboxed ES6 JavaScript script atomically or in dry-run mode.
func (h *Handler) ExecuteScript(
	ctx context.Context,
	req *connect.Request[flowsk_language_pb.ExecuteScriptRequest],
) (*connect.Response[flowsk_language_pb.ExecuteScriptResponse], error) {
	const op = "languagehandler.ExecuteScript"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	defaultProjectID, err := connectrpc.ParseOptionalUUID(op, "default_project_id", req.Msg.DefaultProjectId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if strings.TrimSpace(req.Msg.GetScript()) == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "script is required", errs.FieldViolation{
			Field:       "script",
			Description: "script must not be empty",
		}))
	}

	dryRun, err := isDryRunMode(op, req.Msg.GetMode())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if err := validateOutputDensity(op, req.Msg.GetOutputDensity()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	jsOut, jsErr := h.executor.Execute(ctx, codemode.ExecuteInput{
		WorkspaceID:      workspaceID,
		DefaultProjectID: defaultProjectID,
		Script:           req.Msg.GetScript(),
		DryRun:           dryRun,
	})
	if jsErr != nil {
		return nil, connectrpc.MapDomainError(jsErr)
	}
	effMode := flowsk_language_pb.ExecutionMode_EXECUTION_MODE_ATOMIC_COMMIT
	if dryRun {
		effMode = flowsk_language_pb.ExecutionMode_EXECUTION_MODE_DRY_RUN
	}
	return connect.NewResponse(&flowsk_language_pb.ExecuteScriptResponse{
		Committed:          jsOut.Committed,
		EffectiveMode:      effMode,
		StatementsExecuted: int32(jsOut.Mutations),
		CompactOutput:      jsOut.Summary,
		ExecutedAt:         timestamppb.New(time.Now().UTC()),
	}), nil
}

// ValidateScript statically compiles and validates an ES6 JavaScript snippet without touching persistence.
func (h *Handler) ValidateScript(
	ctx context.Context,
	req *connect.Request[flowsk_language_pb.ValidateScriptRequest],
) (*connect.Response[flowsk_language_pb.ValidateScriptResponse], error) {
	const op = "languagehandler.ValidateScript"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseRequiredUUID(op, "workspace_id", req.Msg.GetWorkspaceId())
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseRequiredUUID(op, "actor_id", req.Msg.GetActorId()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if strings.TrimSpace(req.Msg.GetScript()) == "" {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "script is required", errs.FieldViolation{
			Field:       "script",
			Description: "script must not be empty",
		}))
	}
	if _, err := authctx.RequireWorkspaceOwnership(ctx, workspaceID); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	if err := codemode.ValidateSyntax(req.Msg.GetScript()); err != nil {
		if errors.Is(err, errs.ErrValidation) {
			return connect.NewResponse(&flowsk_language_pb.ValidateScriptResponse{
				Valid:       false,
				Diagnostics: validationDiagnosticsFromError(err),
			}), nil
		}
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_language_pb.ValidateScriptResponse{
		Valid:          true,
		StatementCount: 1,
	}), nil
}

// GetLanguageGuide returns the self-describing Code Mode Markdown specification and TypeScript `.d.ts` SDK declarations.
func (h *Handler) GetLanguageGuide(
	ctx context.Context,
	req *connect.Request[flowsk_language_pb.GetLanguageGuideRequest],
) (*connect.Response[flowsk_language_pb.GetLanguageGuideResponse], error) {
	const op = "languagehandler.GetLanguageGuide"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	if err := validateOutputDensity(op, req.Msg.GetDensity()); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	workspaceID, err := connectrpc.ParseOptionalUUID(op, "workspace_id", req.Msg.WorkspaceId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if workspaceID != nil {
		if _, err := authctx.RequireWorkspaceOwnership(ctx, *workspaceID); err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
	}

	spec, err := h.guide.MarkdownSpec(ctx)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	return connect.NewResponse(&flowsk_language_pb.GetLanguageGuideResponse{
		Version:      guideSpecVersion,
		Filename:     guideFilename,
		MarkdownSpec: spec,
		EbnfGrammar:  language.CodeModeTypeScriptSpec(),
	}), nil
}

// ListLanguageExamples returns curated, runnable Code Mode ES6 JavaScript example scripts.
func (h *Handler) ListLanguageExamples(
	ctx context.Context,
	req *connect.Request[flowsk_language_pb.ListLanguageExamplesRequest],
) (*connect.Response[flowsk_language_pb.ListLanguageExamplesResponse], error) {
	const op = "languagehandler.ListLanguageExamples"
	if err := ctx.Err(); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if req == nil || req.Msg == nil {
		return nil, connectrpc.MapDomainError(errs.Validation(op, "request message is required"))
	}

	workspaceID, err := connectrpc.ParseOptionalUUID(op, "workspace_id", req.Msg.WorkspaceId)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if _, err := connectrpc.ParseOptionalUUID(op, "actor_id", req.Msg.ActorId); err != nil {
		return nil, connectrpc.MapDomainError(err)
	}
	if workspaceID != nil {
		if _, err := authctx.RequireWorkspaceOwnership(ctx, *workspaceID); err != nil {
			return nil, connectrpc.MapDomainError(err)
		}
	}

	examples, err := h.guide.Examples(ctx)
	if err != nil {
		return nil, connectrpc.MapDomainError(err)
	}

	categoryFilter := strings.ToLower(strings.TrimSpace(req.Msg.GetCategoryFilter()))
	out := make([]*flowsk_language_pb.LanguageExample, 0, len(examples))
	for i := range examples {
		protoEx := examples[i].ToProto()
		if categoryFilter != "" &&
			!strings.EqualFold(protoEx.GetCategory(), categoryFilter) &&
			!strings.Contains(strings.ToLower(protoEx.GetSlug()), categoryFilter) {
			continue
		}
		out = append(out, protoEx)
	}
	return connect.NewResponse(&flowsk_language_pb.ListLanguageExamplesResponse{
		Examples: out,
	}), nil
}

func isDryRunMode(op string, m flowsk_language_pb.ExecutionMode) (bool, error) {
	switch m {
	case flowsk_language_pb.ExecutionMode_EXECUTION_MODE_UNSPECIFIED,
		flowsk_language_pb.ExecutionMode_EXECUTION_MODE_ATOMIC_COMMIT:
		return false, nil
	case flowsk_language_pb.ExecutionMode_EXECUTION_MODE_DRY_RUN:
		return true, nil
	default:
		return false, errs.Validation(op, "invalid execution mode", errs.FieldViolation{
			Field:       "mode",
			Description: "unsupported ExecutionMode enum value",
		})
	}
}

func validateOutputDensity(op string, d flowsk_language_pb.OutputDensity) error {
	switch d {
	case flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_UNSPECIFIED,
		flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_MINIMAL_TOKENS,
		flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_STANDARD,
		flowsk_language_pb.OutputDensity_OUTPUT_DENSITY_VERBOSE:
		return nil
	default:
		return errs.Validation(op, "invalid output density", errs.FieldViolation{
			Field:       "output_density",
			Description: "unsupported OutputDensity enum value",
		})
	}
}

func validationDiagnosticsFromError(err error) []*flowsk_language_pb.ScriptDiagnostic {
	var domErr *errs.DomainError
	if errors.As(err, &domErr) && domErr != nil {
		violations := domErr.ViolationsCopy()
		if len(violations) > 0 {
			out := make([]*flowsk_language_pb.ScriptDiagnostic, 0, len(violations))
			for _, v := range violations {
				msg := v.Description
				if v.Field != "" {
					msg = v.Field + ": " + v.Description
				}
				out = append(out, &flowsk_language_pb.ScriptDiagnostic{
					Severity: flowsk_language_pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR,
					Line:     1,
					Column:   1,
					Code:     "FLW_SYNTAX",
					Message:  msg,
				})
			}
			return out
		}
		if domErr.Message != "" {
			return []*flowsk_language_pb.ScriptDiagnostic{{
				Severity: flowsk_language_pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR,
				Line:     1,
				Column:   1,
				Code:     "FLW_SYNTAX",
				Message:  domErr.Message,
			}}
		}
	}
	return []*flowsk_language_pb.ScriptDiagnostic{{
		Severity: flowsk_language_pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR,
		Line:     1,
		Column:   1,
		Code:     "FLW_SYNTAX",
		Message:  "JavaScript syntax validation failed",
	}}
}
