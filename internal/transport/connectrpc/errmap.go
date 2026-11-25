// Package connectrpc implements the inbound Buf ConnectRPC (`connectrpc.com/connect`)
// transport layer for Flowsk, providing deterministic domain error translation,
// authentication and Protobuf validation unary interceptors, and HTTP/H2C mux assembly.
//
// Strict Architectural Boundaries:
//   - Zero Business Logic or SQL: Transport handlers validate wire payloads, propagate
//     verified caller identity via `internal/domain/authctx`, delegate to domain/language
//     services, and serialize domain entities using `entity.ToProto()`.
//   - Zero Information Leakage: `MapDomainError` translates `internal/domain/errs`
//     sentinels into canonical Connect error codes while stripping raw `DomainError.Cause`
//     chains and masking unknown errors behind a generic `connect.CodeInternal` message.
//   - Zero Server Filesystem Footprint: Neither configuration nor RPC payloads ever
//     accept, log, or persist host repository filesystem paths.
package connectrpc

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

// errGenericInternal is the constant, sanitized wire error message returned whenever
// an unclassified or raw infrastructure error reaches the transport boundary.
var errGenericInternal = errors.New("internal server error")

// MapDomainError translates a domain, language, or context error into a sanitized
// `*connect.Error` suitable for returning across the wire to CLI, web, or agent callers.
//
// Sentinel Mapping Contract:
//   - `nil` -> `nil`
//   - `*connect.Error` -> returned as-is
//   - `context.Canceled` -> `connect.CodeCanceled`
//   - `context.DeadlineExceeded` -> `connect.CodeDeadlineExceeded`
//   - `errs.ErrNotImplemented` -> `connect.CodeUnimplemented`
//   - `errs.ErrValidation` -> `connect.CodeInvalidArgument`
//   - `errs.ErrUnauthenticated` -> `connect.CodeUnauthenticated`
//   - `errs.ErrPermissionDenied` -> `connect.CodePermissionDenied`
//   - `errs.ErrNotFound` -> `connect.CodeNotFound`
//   - `errs.ErrConflict` -> `connect.CodeAlreadyExists`
//   - `errs.ErrLeaseHeld` -> `connect.CodeFailedPrecondition`
//   - `errs.ErrPreconditionFailed` -> `connect.CodeFailedPrecondition`
//   - Any other error -> `connect.CodeInternal` with generic `"internal server error"`
//     (never exposing `DomainError.Cause`, SQL constraint names, DSNs, or driver strings).
func MapDomainError(err error) *connect.Error {
	if err == nil {
		return nil
	}

	var existing *connect.Error
	if errors.As(err, &existing) {
		return existing
	}

	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	case errors.Is(err, errs.ErrNotImplemented):
		return connect.NewError(connect.CodeUnimplemented, safeClientError(err, errs.ErrNotImplemented.Error()))
	case errors.Is(err, errs.ErrValidation):
		return connect.NewError(connect.CodeInvalidArgument, safeClientError(err, errs.ErrValidation.Error()))
	case errors.Is(err, errs.ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, safeClientError(err, errs.ErrUnauthenticated.Error()))
	case errors.Is(err, errs.ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, safeClientError(err, errs.ErrPermissionDenied.Error()))
	case errors.Is(err, errs.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, safeClientError(err, errs.ErrNotFound.Error()))
	case errors.Is(err, errs.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, safeClientError(err, errs.ErrConflict.Error()))
	case errors.Is(err, errs.ErrLeaseHeld):
		return connect.NewError(connect.CodeFailedPrecondition, safeClientError(err, errs.ErrLeaseHeld.Error()))
	case errors.Is(err, errs.ErrPreconditionFailed):
		return connect.NewError(connect.CodeFailedPrecondition, safeClientError(err, errs.ErrPreconditionFailed.Error()))
	default:
		return connect.NewError(connect.CodeInternal, errGenericInternal)
	}
}

// safeClientError extracts only client-safe diagnostic text (`DomainError.Message` and
// `DomainError.Violations`) from err, deliberately omitting `DomainError.Cause` and any
// unstructured wrapped error strings so database/driver internals never leak over RPC.
func safeClientError(err error, fallback string) error {
	var domErr *errs.DomainError
	if !errors.As(err, &domErr) || domErr == nil {
		return errors.New(fallback)
	}

	msg := strings.TrimSpace(domErr.Message)
	if msg == "" {
		if domErr.Sentinel != nil {
			msg = domErr.Sentinel.Error()
		} else {
			msg = fallback
		}
	}

	violations := domErr.ViolationsCopy()
	if len(violations) == 0 {
		return errors.New(msg)
	}

	var b strings.Builder
	b.WriteString(msg)
	b.WriteString(": ")
	for i, v := range violations {
		if i > 0 {
			b.WriteString("; ")
		}
		if v.Field != "" {
			b.WriteString(v.Field)
			b.WriteString(": ")
		}
		b.WriteString(v.Description)
	}
	return errors.New(b.String())
}
