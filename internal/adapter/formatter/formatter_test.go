// Package formatter_test verifies all 10 OutputFormatter methods across all
// supported terminal and agent encodings (human, compact, tsv, jsonl, json,
// markdown, md), including visual goal progress bars, priority/status/blocker/lease
// badges, and strict line-safe MaxTokens budget truncation with "+N more omitted"
// continuation trailers.
package formatter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mindmain/flowsk/internal/adapter/formatter"
	"github.com/mindmain/flowsk/internal/domain/audit"
	"github.com/mindmain/flowsk/internal/domain/authctx"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"github.com/mindmain/flowsk/internal/domain/objective"
	"github.com/mindmain/flowsk/internal/domain/project"
	"github.com/mindmain/flowsk/internal/domain/task"
	"github.com/mindmain/flowsk/internal/domain/tx"
	"github.com/mindmain/flowsk/internal/domain/workspace"
)

type sampleFixtures struct {
	workspaces []workspace.Workspace
	proj       project.Project
	projects   []project.Project
	objProg    formatter.ObjectiveProgress
	objectives []formatter.ObjectiveProgress
	fullTask   task.Task
	queue      []task.CompactTaskSummary
	digest     task.ProjectDigest
	board      formatter.BoardView
	audits     []audit.Record
}

func buildFixtures() sampleFixtures {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	target := now.Add(14 * 24 * time.Hour)
	leaseExp := now.Add(30 * time.Minute)

	wsID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	objID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	taskID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	actorID := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	ws := workspace.Workspace{
		ID:                     wsID,
		Slug:                   "acme-eng",
		Name:                   "Acme Engineering",
		Description:            "Core platform workspace",
		DefaultLeaseTTLSeconds: 900,
		Labels:                 map[string]string{"tier": "prod"},
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	proj := project.Project{
		ID:            projID,
		WorkspaceID:   wsID,
		Slug:          "flowsk-core",
		KeyPrefix:     "FLW",
		Name:          "Flowsk Core Engine",
		Description:   "Local-first and ConnectRPC multi-agent coordination engine",
		RemoteRepoURL: "https://github.com/mindmain/flowsk",
		DefaultBranch: "main",
		Status:        project.StatusActive,
		Tags:          []string{"golang", "sqlite"},
		TaskSeq:       110,
		CreatedBy:     actorID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	obj := objective.Objective{
		ID:              objID,
		WorkspaceID:     wsID,
		ProjectID:       projID,
		Slug:            "v1-terminal-ux",
		Title:           "Deliver first-class terminal UX and compact agent protocol",
		Summary:         "Ensure human boards and agent queues coexist cleanly.",
		SuccessCriteria: []string{"Progress bars render in TTY", "Compact format stays ultra-low-token"},
		Status:          objective.StatusActive,
		PriorityRank:    1,
		TargetDate:      &target,
		CreatedBy:       actorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	objProg := formatter.ObjectiveProgress{
		Objective:      obj,
		TotalTasks:     8,
		CompletedTasks: 3,
		BlockedTasks:   1,
	}

	fullTask := task.Task{
		ID:                 taskID,
		WorkspaceID:        wsID,
		ProjectID:          projID,
		ObjectiveID:        &objID,
		Key:                "FLW-104",
		Kind:               task.KindFeature,
		Title:              "Add ClaimLease fencing and terminal board renderer",
		Description:        "Implement optimistic lease checks and terminal Kanban formatting with zero repo pollution.",
		AcceptanceCriteria: []string{"Enforce lease token on renewal", "Render [URGENT] and @agent-1 badges"},
		Status:             task.StatusBlocked,
		Priority:           task.PriorityCritical,
		Tags:               []string{"golang", "tx"},
		Labels:             map[string]string{"area": "adapter"},
		ActiveLease: &task.ClaimLease{
			TaskID:        taskID,
			WorkspaceID:   wsID,
			LeaseToken:    "lease-tok-01",
			HolderActorID: actorID,
			HolderKind:    authctx.ActorKindAgent,
			HolderHandle:  "agent-1",
			AcquiredAt:    now,
			HeartbeatAt:   now,
			ExpiresAt:     leaseExp,
		},
		UnresolvedBlockerCount: 2,
		Version:                3,
		CreatedBy:              actorID,
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	readyItem := task.CompactTaskSummary{
		ID:            uuid.New(),
		Key:           "FLW-105",
		Kind:          task.KindFeature,
		Title:         "Wire ConnectRPC client headers",
		Status:        task.StatusReady,
		Priority:      task.PriorityCritical,
		ObjectiveSlug: "v1-terminal-ux",
		Tags:          []string{"rpc", "client"},
		UpdatedAt:     now,
	}
	leasedItem := task.CompactTaskSummary{
		ID:             taskID,
		Key:            "FLW-104",
		Kind:           task.KindFeature,
		Title:          "Add ClaimLease fencing",
		Status:         task.StatusInProgress,
		Priority:       task.PriorityHigh,
		ObjectiveSlug:  "v1-terminal-ux",
		HolderHandle:   "agent-1",
		LeaseExpiresAt: &leaseExp,
		Tags:           []string{"golang", "tx"},
		UpdatedAt:      now,
	}
	blockedItem := task.CompactTaskSummary{
		ID:                     uuid.New(),
		Key:                    "FLW-106",
		Kind:                   task.KindBug,
		Title:                  "Unblock downstream migration gate",
		Status:                 task.StatusBlocked,
		Priority:               task.PriorityCritical,
		UnresolvedBlockerCount: 1,
		Tags:                   []string{"db"},
		UpdatedAt:              now,
	}

	digest := task.ProjectDigest{
		WorkspaceID:      wsID,
		ProjectID:        projID,
		ProjectSlug:      proj.Slug,
		ProjectKeyPrefix: proj.KeyPrefix,
		GeneratedAt:      now,
		CountsByStatus: map[task.Status]int32{
			task.StatusReady:      4,
			task.StatusInProgress: 2,
			task.StatusBlocked:    1,
			task.StatusDone:       3,
		},
		ActiveObjectives: []task.ObjectiveDigestItem{
			{
				ID:            objID,
				Slug:          obj.Slug,
				Title:         obj.Title,
				PriorityRank:  1,
				OpenTaskCount: 5,
				DoneTaskCount: 3,
			},
		},
		ReadyQueue:   []task.CompactTaskSummary{readyItem},
		ActiveLeases: []task.CompactTaskSummary{leasedItem},
		BlockedTasks: []task.CompactTaskSummary{blockedItem},
	}

	board := formatter.BoardView{
		Project:    proj,
		Objectives: []formatter.ObjectiveProgress{objProg},
		Digest:     digest,
	}

	audits := []audit.Record{
		{
			ID:           uuid.New(),
			WorkspaceID:  wsID,
			ActorID:      actorID,
			ActorKind:    authctx.ActorKindAgent,
			ActorHandle:  "agent-1",
			Action:       tx.AuditActionClaim,
			ResourceType: "task",
			ResourceID:   "FLW-104",
			NewValues:    []byte(`{"status":"in_progress","holder":"agent-1"}`),
			OccurredAt:   now,
		},
	}

	return sampleFixtures{
		workspaces: []workspace.Workspace{ws},
		proj:       proj,
		projects:   []project.Project{proj},
		objProg:    objProg,
		objectives: []formatter.ObjectiveProgress{objProg},
		fullTask:   fullTask,
		queue:      []task.CompactTaskSummary{readyItem, leasedItem, blockedItem},
		digest:     digest,
		board:      board,
		audits:     audits,
	}
}

// TestOutputFormatter_All10MethodsAcrossAllFormats verifies every OutputFormatter
// method across human, compact, tsv, jsonl, json, markdown, and md encodings.
func TestOutputFormatter_All10MethodsAcrossAllFormats(t *testing.T) {
	t.Parallel()

	fx := buildFixtures()
	outFmt := formatter.NewOutputFormatter()
	ctx := context.Background()

	formats := []formatter.Format{
		formatter.FormatHuman,
		formatter.FormatCompact,
		formatter.FormatTSV,
		formatter.FormatJSONL,
		formatter.FormatJSON,
		formatter.FormatMarkdown,
		formatter.FormatMarkdownShort,
	}

	type methodCase struct {
		name string
		call func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error
	}

	methods := []methodCase{
		{
			name: "FormatWorkspaces",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatWorkspaces(ctx, w, fx.workspaces, opts)
			},
		},
		{
			name: "FormatProject",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatProject(ctx, w, fx.proj, opts)
			},
		},
		{
			name: "FormatProjects",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatProjects(ctx, w, fx.projects, opts)
			},
		},
		{
			name: "FormatObjective",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatObjective(ctx, w, fx.objProg, opts)
			},
		},
		{
			name: "FormatObjectives",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatObjectives(ctx, w, fx.objectives, opts)
			},
		},
		{
			name: "FormatTask",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatTask(ctx, w, fx.fullTask, opts)
			},
		},
		{
			name: "FormatTaskQueue",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatTaskQueue(ctx, w, fx.queue, opts)
			},
		},
		{
			name: "FormatProjectDigest",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatProjectDigest(ctx, w, fx.digest, opts)
			},
		},
		{
			name: "FormatBoard",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatBoard(ctx, w, fx.board, opts)
			},
		},
		{
			name: "FormatAuditRecords",
			call: func(w *bytes.Buffer, opts formatter.TokenBudgetOptions) error {
				return outFmt.FormatAuditRecords(ctx, w, fx.audits, opts)
			},
		},
	}

	for _, mc := range methods {
		for _, f := range formats {
			t.Run(mc.name+"_"+string(f), func(t *testing.T) {
				t.Parallel()
				var buf bytes.Buffer
				err := mc.call(&buf, formatter.TokenBudgetOptions{
					Format:    f,
					OmitUUIDs: true,
				})
				if err != nil {
					t.Fatalf("%s(%s) returned unexpected error: %v", mc.name, f, err)
				}
				rendered := buf.String()
				if strings.TrimSpace(rendered) == "" {
					t.Fatalf("%s(%s) produced empty output", mc.name, f)
				}

				switch f {
				case formatter.FormatJSON:
					var parsed any
					if jsonErr := json.Unmarshal(buf.Bytes(), &parsed); jsonErr != nil {
						t.Fatalf("%s(json) emitted invalid JSON: %v\n%s", mc.name, jsonErr, rendered)
					}
				case formatter.FormatJSONL:
					lines := strings.Split(strings.TrimSpace(rendered), "\n")
					for i, line := range lines {
						var parsed map[string]any
						if jsonErr := json.Unmarshal([]byte(line), &parsed); jsonErr != nil {
							t.Fatalf("%s(jsonl) line %d invalid JSON: %v\n%s", mc.name, i, jsonErr, line)
						}
					}
				case formatter.FormatTSV:
					if !strings.Contains(rendered, "\t") {
						t.Fatalf("%s(tsv) expected tab-separated columns, got:\n%s", mc.name, rendered)
					}
				case formatter.FormatMarkdown, formatter.FormatMarkdownShort:
					if !strings.Contains(rendered, "#") && !strings.Contains(rendered, "|") {
						t.Fatalf("%s(%s) expected GFM headings or tables, got:\n%s", mc.name, f, rendered)
					}
				}
			})
		}
	}
}

// TestOutputFormatter_HumanTerminalUXBadgesAndProgressBars verifies that FormatHuman
// renders visual goal completion progress bars ("3/8 [...] 37%"), priority badges
// ("[URGENT]"), blocker badges ("[BLOCKED]"), and lease holder handles ("@agent-1").
func TestOutputFormatter_HumanTerminalUXBadgesAndProgressBars(t *testing.T) {
	t.Parallel()

	fx := buildFixtures()
	outFmt := formatter.NewOutputFormatter()
	ctx := context.Background()

	var boardBuf bytes.Buffer
	if err := outFmt.FormatBoard(ctx, &boardBuf, fx.board, formatter.TokenBudgetOptions{
		Format:        formatter.FormatHuman,
		TerminalWidth: 110,
	}); err != nil {
		t.Fatalf("FormatBoard(human) failed: %v", err)
	}
	boardText := boardBuf.String()

	requiredBoardTokens := []string{
		"Flowsk Core Engine",
		"FLW",
		"3/8",
		"37%",
		"█",
		"░",
		"[URGENT]",
		"[BLOCKED]",
		"@agent-1",
		"FLW-104",
		"FLW-105",
		"FLW-106",
	}
	for _, tok := range requiredBoardTokens {
		if !strings.Contains(boardText, tok) {
			t.Fatalf("FormatBoard(human) missing expected terminal UX token %q in:\n%s", tok, boardText)
		}
	}

	var taskBuf bytes.Buffer
	if err := outFmt.FormatTask(ctx, &taskBuf, fx.fullTask, formatter.TokenBudgetOptions{
		Format:            formatter.FormatHuman,
		TruncateBodyChars: 40,
	}); err != nil {
		t.Fatalf("FormatTask(human) failed: %v", err)
	}
	taskText := taskBuf.String()
	for _, tok := range []string{"FLW-104", "[URGENT]", "[BLOCKED]", "@agent-1"} {
		if !strings.Contains(taskText, tok) {
			t.Fatalf("FormatTask(human) missing token %q in:\n%s", tok, taskText)
		}
	}
}

// TestOutputFormatter_MaxTokensNeverCutsMidLineAndAppendsOmittedTrailer verifies
// that when MaxTokens > 0, excess items are elided cleanly at whole-line boundaries
// and an explicit "+N more omitted" trailer is appended.
func TestOutputFormatter_MaxTokensNeverCutsMidLineAndAppendsOmittedTrailer(t *testing.T) {
	t.Parallel()

	outFmt := formatter.NewOutputFormatter()
	ctx := context.Background()

	const totalTasks = 12
	items := make([]task.CompactTaskSummary, 0, totalTasks)
	for i := 1; i <= totalTasks; i++ {
		items = append(items, task.CompactTaskSummary{
			ID:       uuid.New(),
			Key:      fmt.Sprintf("FLW-%03d", i),
			Kind:     task.KindFeature,
			Title:    fmt.Sprintf("Implement deterministic subsystem task number %02d for verification", i),
			Status:   task.StatusReady,
			Priority: task.PriorityHigh,
			Tags:     []string{"golang", "tdd"},
		})
	}

	for _, f := range []formatter.Format{
		formatter.FormatCompact,
		formatter.FormatHuman,
		formatter.FormatTSV,
		formatter.FormatJSONL,
		formatter.FormatMarkdown,
	} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			err := outFmt.FormatTaskQueue(ctx, &buf, items, formatter.TokenBudgetOptions{
				Format:    f,
				MaxTokens: 80, // ~320 bytes: fits a few items + trailer, never all 12.
				OmitUUIDs: true,
			})
			if err != nil {
				t.Fatalf("FormatTaskQueue(%s) with MaxTokens failed: %v", f, err)
			}

			out := buf.String()
			if !strings.HasSuffix(out, "\n") {
				t.Fatalf("expected newline-terminated output (never cut mid-line), got:\n%q", out)
			}
			if !strings.Contains(out, "more omitted") || !strings.Contains(out, "+") {
				t.Fatalf("expected explicit '+N more omitted' trailer in %s output, got:\n%s", f, out)
			}
			if strings.Contains(out, "FLW-012") {
				t.Fatalf("expected tail item FLW-012 to be elided under tight MaxTokens budget, got:\n%s", out)
			}

			// Verify no included FLW-xxx line was chopped mid-title.
			for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
				if strings.Contains(line, "FLW-") && !strings.Contains(line, "verification") {
					t.Fatalf("line appears cut mid-line in %s output: %q", f, line)
				}
			}
		})
	}
}

// TestOutputFormatter_ValidationAndCanceledContext verifies invalid Format
// rejection and context cancellation handling.
func TestOutputFormatter_ValidationAndCanceledContext(t *testing.T) {
	t.Parallel()

	outFmt := formatter.NewOutputFormatter()
	var buf bytes.Buffer

	err := outFmt.FormatWorkspaces(context.Background(), &buf, nil, formatter.TokenBudgetOptions{
		Format: formatter.Format("xml-unsupported"),
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected ErrValidation for unsupported format, got %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := outFmt.FormatWorkspaces(canceledCtx, &buf, nil, formatter.TokenBudgetOptions{Format: formatter.FormatCompact}); err == nil {
		t.Fatal("expected error for canceled context")
	}
}
