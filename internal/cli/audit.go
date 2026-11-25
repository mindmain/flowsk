// File audit.go implements the `flowsk audit` CLI subcommand group for querying
// immutable, workspace-scoped mutation audit trails in terminal or compact form.
package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type auditCommandDeps interface {
	AuditDependency
	FormatterDependency
}

type auditHandler struct {
	opts      *GlobalOptions
	client    AuditClient
	formatter OutputFormatter
}

// newAuditCommand constructs `flowsk audit` (`list`, `get`) for inspecting
// append-only, workspace-scoped mutation logs.
func newAuditCommand(opts *GlobalOptions, deps auditCommandDeps) *cobra.Command {
	var (
		client    AuditClient
		formatter OutputFormatter
	)
	if deps != nil {
		client = deps.AuditClient()
		formatter = deps.OutputFormatter()
	}
	h := &auditHandler{
		opts:      opts,
		client:    client,
		formatter: formatter,
	}

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect immutable workspace-scoped mutation audit events",
		Long: `Queries the append-only audit trail recorded atomically alongside every
workspace, project, objective, task, claim, and Code Mode JavaScript mutation.`,
	}

	auditCmd.AddCommand(
		h.newListSubcommand(),
		h.newGetSubcommand(),
	)

	return auditCmd
}

func (h *auditHandler) newListSubcommand() *cobra.Command {
	var (
		entityType string
		entityID   string
		action     string
		since      string
		limit      int
		cursor     string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List audit trail events filtered by project, actor, entity, or timestamp",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("audit list context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0, got %d", limit)
			}
			sinceTime, err := parseSinceFlag(since)
			if err != nil {
				return err
			}
			res, err := h.client.ListAuditEvents(ctx, ListAuditEventsInput{
				Workspace:  defaultWorkspace(h.opts.Workspace),
				Project:    strings.TrimSpace(h.opts.Project),
				Actor:      strings.TrimSpace(h.opts.Actor),
				EntityType: strings.ToLower(strings.TrimSpace(entityType)),
				EntityID:   strings.TrimSpace(entityID),
				Action:     strings.TrimSpace(action),
				Since:      sinceTime,
				Limit:      limit,
				Cursor:     strings.TrimSpace(cursor),
			})
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, res)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&entityType, "entity-type", "", "Filter by entity type: workspace|project|objective|task|comment|state|label")
	flags.StringVar(&entityID, "entity-id", "", "Filter by specific entity UUID or logical key")
	flags.StringVar(&action, "action", "", "Filter by mutation action (e.g. task.claimed, task.moved, lang.executed)")
	flags.StringVar(&since, "since", "", "Filter events at or after RFC3339 timestamp or relative duration (e.g. 24h)")
	flags.IntVar(&limit, "limit", 50, "Maximum audit events to return")
	flags.StringVar(&cursor, "cursor", "", "Opaque pagination cursor")
	return cmd
}

func (h *auditHandler) newGetSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <audit-event-id>",
		Short: "Inspect a single immutable audit event and its structured diff metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("audit get context canceled: %w", err)
			}
			if h.client == nil {
				return ErrNotImplemented
			}
			id := strings.TrimSpace(args[0])
			if id == "" {
				return errors.New("audit event ID must not be empty")
			}
			ev, err := h.client.GetAuditEvent(ctx, defaultWorkspace(h.opts.Workspace), id)
			if err != nil {
				return err
			}
			return renderPayload(cmd.OutOrStdout(), h.formatter, h.opts, ev)
		},
	}
}

func parseSinceFlag(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		utc := ts.UTC()
		return &utc, nil
	}
	if dur, err := time.ParseDuration(trimmed); err == nil {
		if dur < 0 {
			return nil, fmt.Errorf("--since duration must be >= 0, got %q", raw)
		}
		utc := time.Now().UTC().Add(-dur)
		return &utc, nil
	}
	return nil, fmt.Errorf("invalid --since %q: must be an RFC3339 timestamp or relative duration (e.g. 24h)", raw)
}
