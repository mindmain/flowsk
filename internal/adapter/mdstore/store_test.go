// File store_test.go verifies sibling projects/tasks directory creation,
// cross-tenant workspace isolation, atomic CRUD, and orphan Markdown scanning/pruning.
package mdstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/adapter/mdstore"
	"github.com/mindmain/flowsk/internal/domain/errs"
)

func TestFileStore_SiblingLayoutAndTenantIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()

	store, err := mdstore.NewFileStore(root)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Verify sibling projects/ and tasks/ directories exist at the same level under root.
	for _, sub := range []string{mdstore.ProjectsSubdir, mdstore.TasksSubdir} {
		info, statErr := os.Stat(filepath.Join(root, sub))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("expected sibling directory %q to exist under root, err=%v", sub, statErr)
		}
	}

	wsA := uuid.New()
	wsB := uuid.New()
	taskID := uuid.New()
	projID := uuid.New()

	if err := store.WriteTaskMarkdown(ctx, wsA, taskID, "# Task Spec A\nDetails"); err != nil {
		t.Fatalf("WriteTaskMarkdown: %v", err)
	}
	if err := store.WriteProjectMarkdown(ctx, wsA, projID, "# Project Charter A"); err != nil {
		t.Fatalf("WriteProjectMarkdown: %v", err)
	}

	// Workspace B MUST NOT be able to read or delete Workspace A's task/project Markdown.
	if _, err := store.ReadTaskMarkdown(ctx, wsB, taskID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected cross-workspace ReadTaskMarkdown to fail with ErrNotFound, got %v", err)
	}
	if err := store.DeleteTaskMarkdown(ctx, wsB, taskID); err != nil {
		t.Fatalf("DeleteTaskMarkdown in foreign workspace: %v", err)
	}
	gotTaskMD, err := store.ReadTaskMarkdown(ctx, wsA, taskID)
	if err != nil || gotTaskMD != "# Task Spec A\nDetails" {
		t.Fatalf("Workspace A task markdown corrupted or missing: %q, err=%v", gotTaskMD, err)
	}

	gotProjMD, err := store.ReadProjectMarkdown(ctx, wsA, projID)
	if err != nil || gotProjMD != "# Project Charter A" {
		t.Fatalf("Workspace A project markdown mismatch: %q, err=%v", gotProjMD, err)
	}

	// Drop an unindexed stray file and an orphan UUID file inside tasks/.
	orphanUUID := uuid.New()
	if err := store.WriteTaskMarkdown(ctx, wsA, orphanUUID, "orphan"); err != nil {
		t.Fatalf("WriteTaskMarkdown orphan: %v", err)
	}
	strayPath := filepath.Join(root, mdstore.TasksSubdir, wsA.String(), "not-a-uuid.md")
	if err := os.WriteFile(strayPath, []byte("stray"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	scanA, err := store.ScanTaskMarkdown(ctx, wsA)
	if err != nil {
		t.Fatalf("ScanTaskMarkdown wsA: %v", err)
	}
	if len(scanA.ValidIDs) != 2 {
		t.Fatalf("expected 2 valid UUIDs in wsA scan, got %d", len(scanA.ValidIDs))
	}
	if len(scanA.MalformedRelKeys) != 1 {
		t.Fatalf("expected 1 malformed file in wsA scan, got %v", scanA.MalformedRelKeys)
	}

	// Workspace B scan must not include Workspace A's UUID files.
	scanB, err := store.ScanTaskMarkdown(ctx, wsB)
	if err != nil {
		t.Fatalf("ScanTaskMarkdown wsB: %v", err)
	}
	if len(scanB.ValidIDs) != 0 {
		t.Fatalf("expected 0 valid UUIDs in wsB scan, got %d", len(scanB.ValidIDs))
	}

	// Reject path traversal attempts on RemoveMalformedTaskMarkdown.
	if err := store.RemoveMalformedTaskMarkdown(ctx, "../outside.md"); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected path traversal rejection, got %v", err)
	}
	if err := store.RemoveMalformedTaskMarkdown(ctx, scanA.MalformedRelKeys[0]); err != nil {
		t.Fatalf("RemoveMalformedTaskMarkdown: %v", err)
	}
	if _, statErr := os.Stat(strayPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected malformed file to be removed, statErr=%v", statErr)
	}

	// Shared parent <baseDir>/ files must NEVER be scanned or deleted by any workspace.
	sharedParentTaskFile := filepath.Join(root, mdstore.TasksSubdir, "shared-parent.md")
	if err := os.WriteFile(sharedParentTaskFile, []byte("shared"), 0o600); err != nil {
		t.Fatalf("write shared parent file: %v", err)
	}
	scanBAfterParent, err := store.ScanTaskMarkdown(ctx, wsB)
	if err != nil {
		t.Fatalf("ScanTaskMarkdown wsB with shared parent file: %v", err)
	}
	if len(scanBAfterParent.MalformedRelKeys) != 0 {
		t.Fatalf("expected shared parent file excluded from workspace scan, got %v", scanBAfterParent.MalformedRelKeys)
	}
	if err := store.RemoveMalformedTaskMarkdown(ctx, "shared-parent.md"); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("expected shared parent deletion attempt to fail with ErrValidation, got %v", err)
	}
	if _, statErr := os.Stat(sharedParentTaskFile); statErr != nil {
		t.Fatalf("shared parent file must remain untouched on disk, statErr=%v", statErr)
	}
}
