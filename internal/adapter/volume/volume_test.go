// Package volume_test verifies directory layout provisioning, POSIX flock advisory
// locking, and online SQLite VACUUM INTO snapshot management for SharedVolumeAdapter.
package volume_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mindmain/flowsk/internal/adapter/volume"
	"github.com/mindmain/flowsk/internal/domain/errs"
	_ "modernc.org/sqlite"
)

// TestNewSharedVolumeAdapterValidation verifies input validation on volume root paths.
func TestNewSharedVolumeAdapterValidation(t *testing.T) {
	t.Parallel()

	if _, err := volume.NewSharedVolumeAdapter(""); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("NewSharedVolumeAdapter(\"\") error = %v, want errs.ErrValidation", err)
	}
}

// TestEnsureLayoutCreatesDirectoriesWithSecurePermissions verifies that EnsureLayout
// creates RootDir, db/, locks/, snapshots/, and tmp/ with restrictive POSIX permissions.
func TestEnsureLayoutCreatesDirectoriesWithSecurePermissions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "nested", "flowsk-volume")

	adapter, err := volume.NewSharedVolumeAdapter(rootDir)
	if err != nil {
		t.Fatalf("NewSharedVolumeAdapter(%q): %v", rootDir, err)
	}

	layout, err := adapter.EnsureLayout(ctx)
	if err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	dirs := []string{
		layout.RootDir,
		layout.DatabaseDir,
		layout.LocksDir,
		layout.SnapshotsDir,
		layout.TempDir,
	}
	for _, dir := range dirs {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("expected directory %q to exist: %v", dir, statErr)
		}
		if !info.IsDir() {
			t.Fatalf("expected %q to be a directory", dir)
		}
		// Ensure group-write and world-rwx bits are never set (0750 or 0700).
		if perm := info.Mode().Perm(); perm&0o027 != 0 {
			t.Fatalf("directory %q has overly permissive mode %04o", dir, perm)
		}
	}
}

// TestAdvisoryLocksContentionAndCancellation verifies POSIX flock mutual exclusion,
// non-blocking errs.ErrConflict detection, release handoff, and context cancellation.
func TestAdvisoryLocksContentionAndCancellation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "flowsk-locks")

	adapterA, err := volume.NewSharedVolumeAdapter(rootDir)
	if err != nil {
		t.Fatalf("NewSharedVolumeAdapter A: %v", err)
	}
	adapterB, err := volume.NewSharedVolumeAdapter(rootDir)
	if err != nil {
		t.Fatalf("NewSharedVolumeAdapter B: %v", err)
	}

	if _, err := adapterA.EnsureLayout(ctx); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	releaseA, err := adapterA.Locker().AcquireExclusive(ctx, "schema-migration")
	if err != nil {
		t.Fatalf("AcquireExclusive A: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = releaseA()
		}
	}()

	// Non-blocking TryAcquireExclusive on a second descriptor must fail with ErrConflict.
	if _, err := adapterB.Locker().TryAcquireExclusive(ctx, "schema-migration"); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("TryAcquireExclusive while held = %v, want errs.ErrConflict", err)
	}

	// Blocking AcquireExclusive with a short timeout must respect context deadline.
	shortCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if _, err := adapterB.Locker().AcquireExclusive(shortCtx, "schema-migration"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireExclusive with expiring context = %v, want context.DeadlineExceeded", err)
	}

	// Releasing lock A allows adapter B to acquire and release cleanly.
	released = true
	if err := releaseA(); err != nil {
		t.Fatalf("releaseA: %v", err)
	}

	releaseB, err := adapterB.Locker().TryAcquireExclusive(ctx, "schema-migration")
	if err != nil {
		t.Fatalf("TryAcquireExclusive after release: %v", err)
	}
	if err := releaseB(); err != nil {
		t.Fatalf("releaseB: %v", err)
	}
}

// TestSnapshotsCreateListAndPrune verifies consistent online SQLite VACUUM INTO
// backups, SHA-256 digest calculation, newest-first ordering, and retention pruning.
func TestSnapshotsCreateListAndPrune(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "flowsk-snapshots")

	adapter, err := volume.NewSharedVolumeAdapter(rootDir)
	if err != nil {
		t.Fatalf("NewSharedVolumeAdapter: %v", err)
	}
	layout, err := adapter.EnsureLayout(ctx)
	if err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	// Seed a live WAL-mode SQLite database inside Layout.DatabaseDir/flowsk.db.
	dbPath := filepath.Join(layout.DatabaseDir, "flowsk.db")
	rawDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open seed db: %v", err)
	}
	defer func() { _ = rawDB.Close() }()

	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE sample_items (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`); err != nil {
		t.Fatalf("create sample_items: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `INSERT INTO sample_items (name) VALUES ('alpha'), ('beta');`); err != nil {
		t.Fatalf("insert sample_items: %v", err)
	}

	var created []volume.SnapshotMetadata
	for i, label := range []string{"pre-migrate", "hourly", "release"} {
		if i > 0 {
			time.Sleep(5 * time.Millisecond)
		}
		meta, snapErr := adapter.Snapshots().CreateSnapshot(ctx, label)
		if snapErr != nil {
			t.Fatalf("CreateSnapshot(%q): %v", label, snapErr)
		}
		if meta.ID == "" || meta.FilePath == "" || meta.SizeBytes <= 0 || meta.ChecksumSHA256 == "" || meta.CreatedAt.IsZero() {
			t.Fatalf("incomplete SnapshotMetadata: %+v", meta)
		}

		// Verify the SHA-256 checksum matches the finalized file on disk.
		rawBytes, readErr := os.ReadFile(meta.FilePath)
		if readErr != nil {
			t.Fatalf("ReadFile(%q): %v", meta.FilePath, readErr)
		}
		sum := sha256.Sum256(rawBytes)
		if gotHex := hex.EncodeToString(sum[:]); gotHex != meta.ChecksumSHA256 {
			t.Fatalf("checksum mismatch: got %s, meta has %s", gotHex, meta.ChecksumSHA256)
		}
		created = append(created, meta)
	}

	// Verify the latest snapshot is a valid standalone SQLite database containing our rows.
	snapDB, err := sql.Open("sqlite", created[len(created)-1].FilePath)
	if err != nil {
		t.Fatalf("open snapshot sqlite db: %v", err)
	}
	defer func() { _ = snapDB.Close() }()

	var rowCount int
	if err := snapDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sample_items`).Scan(&rowCount); err != nil {
		t.Fatalf("query snapshot sample_items: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("snapshot rowCount = %d, want 2", rowCount)
	}

	// ListSnapshots must return all 3 finalized snapshots ordered newest-first.
	listed, err := adapter.Snapshots().ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("len(ListSnapshots) = %d, want 3", len(listed))
	}
	if listed[0].ID != created[2].ID || listed[2].ID != created[0].ID {
		t.Fatalf("ListSnapshots order = [%s, %s, %s], want newest-first [%s, ..., %s]",
			listed[0].ID, listed[1].ID, listed[2].ID, created[2].ID, created[0].ID)
	}

	// PruneSnapshots(keepLatest=1) must remove the 2 older snapshots and preserve the newest.
	removed, err := adapter.Snapshots().PruneSnapshots(ctx, 1)
	if err != nil {
		t.Fatalf("PruneSnapshots(1): %v", err)
	}
	if removed != 2 {
		t.Fatalf("PruneSnapshots(1) removed = %d, want 2", removed)
	}

	remaining, err := adapter.Snapshots().ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots after prune: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != created[2].ID {
		t.Fatalf("remaining snapshots = %+v, want only %s", remaining, created[2].ID)
	}

	// Verify NewSharedVolumeAdapterWithDB supports an explicit external SQLite file path.
	customAdapter, err := volume.NewSharedVolumeAdapterWithDB(filepath.Join(t.TempDir(), "custom-vol"), dbPath)
	if err != nil {
		t.Fatalf("NewSharedVolumeAdapterWithDB: %v", err)
	}
	if _, err := customAdapter.EnsureLayout(ctx); err != nil {
		t.Fatalf("customAdapter.EnsureLayout: %v", err)
	}
	if customMeta, err := customAdapter.Snapshots().CreateSnapshot(ctx, "custom-db"); err != nil || customMeta.SizeBytes <= 0 {
		t.Fatalf("customAdapter.CreateSnapshot = %+v, err=%v", customMeta, err)
	}
}
