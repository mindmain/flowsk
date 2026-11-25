// Package errs defines canonical, transport-agnostic domain sentinel errors and
// structured error wrappers for the Flowsk domain, language, and adapter layers.
//
// Architectural Intent:
//   - Domain services, repositories, DSL executors, and infrastructure adapters
//     MUST return errors rooted in these sentinels rather than transport-specific
//     status codes (such as ConnectRPC codes, HTTP status codes, or MCP JSON-RPC codes)
//     or raw driver errors (such as sql.ErrNoRows or pgconn.PgError).
//   - Transport handlers (ConnectRPC, MCP, CLI) inspect returned errors via
//     errors.Is and errors.As to translate them deterministically into wire-level
//     error codes without leaking database internals or stack traces to callers.
package errs

import (
	"errors"
	"fmt"
)

var (
	// ErrNotImplemented indicates that an interface method or skeleton constructor
	// stub has not yet been wired with production business or persistence logic.
	// Transport layers map this sentinel to CodeUnimplemented / HTTP 501.
	ErrNotImplemented = errors.New("not implemented")

	// ErrUnauthenticated indicates that the caller did not supply valid actor or
	// workspace authentication credentials in the request context.
	// Transport layers map this sentinel to CodeUnauthenticated / HTTP 401.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrPermissionDenied indicates that the authenticated caller lacks authority
	// to access or mutate the target workspace or resource, or violated mandatory
	// cross-tenant isolation invariants.
	// Transport layers map this sentinel to CodePermissionDenied / HTTP 403.
	ErrPermissionDenied = errors.New("permission denied")

	// ErrNotFound indicates that the requested domain entity does not exist within
	// the caller's authorized workspace scope (or has been soft-deleted).
	// Transport layers map this sentinel to CodeNotFound / HTTP 404.
	ErrNotFound = errors.New("resource not found")

	// ErrConflict indicates a uniqueness violation, duplicate slug/identifier,
	// optimistic concurrency version mismatch, or conflicting relation edge.
	// Transport layers map this sentinel to CodeAlreadyExists or CodeAborted / HTTP 409.
	ErrConflict = errors.New("resource conflict")

	// ErrValidation indicates that caller-supplied command inputs, DSL syntax/semantics,
	// or domain invariants failed validation prior to persistence.
	// Transport layers map this sentinel to CodeInvalidArgument / HTTP 400.
	ErrValidation = errors.New("validation failed")

	// ErrPreconditionFailed indicates that a requested state transition or operation
	// cannot proceed because current entity state violates a domain precondition
	// (for example, completing a task blocked by unresolved dependencies or releasing
	// an unheld lease).
	// Transport layers map this sentinel to CodeFailedPrecondition / HTTP 412.
	ErrPreconditionFailed = errors.New("precondition failed")

	// ErrLeaseHeld indicates that an agent or human actor attempted to claim, mutate,
	// or exclusively lock a task whose active ClaimLease is currently owned by a
	// different actor and has not yet expired.
	// Transport layers map this sentinel to CodeResourceExhausted or CodeFailedPrecondition.
	ErrLeaseHeld = errors.New("claim lease already held by another actor")
)

// FieldViolation describes a single field-level validation or invariant failure
// attached to a structured DomainError.
type FieldViolation struct {
	// Field is the canonical domain attribute path or DSL token/argument name
	// that violated a constraint (for example, "title", "slug", "$epic.priority").
	Field string

	// Description is a human- and LLM-readable explanation of why the value was
	// rejected and how the caller can remediate it.
	Description string
}

// DomainError enriches a canonical sentinel error with structured operation context,
// human-safe diagnostic messages, resource metadata, and optional field violations.
//
// Ownership & Security Invariant:
//   - Message and Violations are safe to surface to authenticated clients and LLM harnesses.
//   - Cause holds the underlying wrapped error chain (inspected via Unwrap) and MUST NOT
//     be serialized verbatim over external network transports if it wraps raw driver errors.
type DomainError struct {
	// Sentinel is the canonical domain error category (for example, ErrValidation or ErrNotFound).
	// Unwrap includes Sentinel so errors.Is(err, errs.ErrNotFound) evaluates to true.
	Sentinel error

	// Op identifies the bounded-context operation where the error originated
	// (for example, "task.Service.ClaimTask" or "language.Executor.Execute").
	Op string

	// ResourceType names the domain aggregate kind involved (for example, "task", "project").
	ResourceType string

	// ResourceID is the optional stringified identifier or slug of the target entity.
	ResourceID string

	// Message is a concise, client-safe explanation of the failure.
	Message string

	// Violations holds optional per-field validation details when Sentinel is ErrValidation.
	Violations []FieldViolation

	// Cause is the optional underlying root error wrapped into the error chain.
	Cause error
}

// Error formats the structured domain error into a deterministic diagnostic string.
func (e *DomainError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := e.Message
	if base == "" && e.Sentinel != nil {
		base = e.Sentinel.Error()
	}
	if e.Op != "" {
		base = fmt.Sprintf("%s: %s", e.Op, base)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", base, e.Cause)
	}
	return base
}

// Unwrap returns the underlying error chain so standard library errors.Is and
// errors.As traverse both Sentinel and Cause seamlessly.
func (e *DomainError) Unwrap() []error {
	if e == nil {
		return nil
	}
	switch {
	case e.Sentinel != nil && e.Cause != nil:
		return []error{e.Sentinel, e.Cause}
	case e.Sentinel != nil:
		return []error{e.Sentinel}
	case e.Cause != nil:
		return []error{e.Cause}
	default:
		return nil
	}
}

// ViolationsCopy returns a defensive copy of the FieldViolation slice so callers
// cannot mutate internal error state across package boundaries.
func (e *DomainError) ViolationsCopy() []FieldViolation {
	if e == nil || len(e.Violations) == 0 {
		return nil
	}
	out := make([]FieldViolation, len(e.Violations))
	copy(out, e.Violations)
	return out
}

// Wrap constructs a DomainError binding a canonical sentinel, operation name,
// client-safe message, and wrapped underlying cause using %w semantics.
func Wrap(sentinel error, op, message string, cause error) error {
	if cause != nil {
		cause = fmt.Errorf("%w", cause)
	}
	return &DomainError{
		Sentinel: sentinel,
		Op:       op,
		Message:  message,
		Cause:    cause,
	}
}

// New constructs a DomainError for a canonical sentinel without an external cause.
func New(sentinel error, op, message string) error {
	return &DomainError{
		Sentinel: sentinel,
		Op:       op,
		Message:  message,
	}
}

// Validation constructs an ErrValidation DomainError carrying defensive copies
// of one or more field-level violations.
func Validation(op, message string, violations ...FieldViolation) error {
	copied := make([]FieldViolation, len(violations))
	copy(copied, violations)
	return &DomainError{
		Sentinel:   ErrValidation,
		Op:         op,
		Message:    message,
		Violations: copied,
	}
}

// NotImplemented constructs a standardized ErrNotImplemented DomainError for
// skeleton methods awaiting concrete implementation.
func NotImplemented(op string) error {
	return &DomainError{
		Sentinel: ErrNotImplemented,
		Op:       op,
		Message:  "operation stub is not yet implemented",
	}
}
