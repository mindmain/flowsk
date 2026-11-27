// Package volume implements filesystem layout provisioning, POSIX advisory file
// locking (`flock`), and atomic online SQLite backup snapshots (`VACUUM INTO`)
// for coordinating access to a shared local directory or mounted Docker volume.
//
// Architectural & Data-Integrity Rationale:
//   - When Flowsk runs in zero-external-database Docker Compose or multi-container
//     agent sandbox topologies backed by a shared host/Docker bind or named volume
//     (`/data/flowsk`), careless direct file copies (`cp flowsk.db ...`) or uncoordinated
//     DDL schema migrations across containers can corrupt SQLite `-wal` and `-shm` files.
//   - SharedVolumeAdapter standardizes subdirectories (`db/`, `locks/`, `snapshots/`, `tmp/`),
//     enforces restrictive POSIX permissions (`0750`), coordinates exclusive advisory locks
//     during migrations/maintenance via AdvisoryLockAdapter, and exports consistent online
//     snapshots (`VACUUM INTO` + SHA-256 + fsync + atomic rename) via SnapshotExporter.
package volume

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mindmain/flowsk/internal/domain/errs"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// Layout describes the canonical directory tree managed inside a Flowsk data volume.
type Layout struct {
	// RootDir is the canonical path of the mounted volume (for example, `/data/flowsk`).
	RootDir string

	// DatabaseDir holds the SQLite primary file and its `-wal` / `-shm` sidecars (`RootDir/db`).
	DatabaseDir string

	// LocksDir holds OS advisory lockfiles (`RootDir/locks`).
	LocksDir string

	// SnapshotsDir holds point-in-time consistent backups and JSONL exports (`RootDir/snapshots`).
	SnapshotsDir string

	// TempDir holds staging files written prior to atomic fsync + rename (`RootDir/tmp`).
	TempDir string
}

// SnapshotMetadata describes a consistent point-in-time backup artifact stored in SnapshotsDir.
type SnapshotMetadata struct {
	// ID is the unique timestamped identifier of the snapshot.
	ID string

	// FilePath is the canonical path to the completed snapshot file inside SnapshotsDir.
	FilePath string

	// SizeBytes is the finalized byte size of the snapshot artifact.
	SizeBytes int64

	// ChecksumSHA256 is the hex-encoded SHA-256 digest computed before atomic rename.
	ChecksumSHA256 string

	// CreatedAt is the UTC timestamp when the snapshot completed.
	CreatedAt time.Time
}

// ReleaseFunc releases a held OS advisory file lock and closes its underlying descriptor.
type ReleaseFunc func() error

// AdvisoryLockAdapter coordinates cross-process mutual exclusion on a shared
// local or Docker volume using POSIX `flock` lockfiles inside `LocksDir`.
//
// Usage Invariant:
//   - Schema migrations (`MigrationRunner.Up/Down`), WAL checkpoint compaction, and
//     snapshot pruning MUST acquire a named exclusive lock (for example, `"schema-migration"`)
//     before mutating shared volume artifacts.
type AdvisoryLockAdapter interface {
	// AcquireExclusive blocks until the named exclusive lock is acquired or ctx
	// expires/cancels, returning a ReleaseFunc that the caller MUST invoke via defer.
	AcquireExclusive(ctx context.Context, lockName string) (ReleaseFunc, error)

	// TryAcquireExclusive attempts a non-blocking exclusive lock acquisition.
	// Returns errs.ErrConflict if another process currently holds lockName.
	TryAcquireExclusive(ctx context.Context, lockName string) (ReleaseFunc, error)
}

// SnapshotExporter creates and prunes consistent point-in-time snapshots of the
// Flowsk state on the shared volume without blocking active readers or corrupting
// live SQLite WAL journals.
//
// Consistency Invariant:
//   - Implementations MUST use SQLite's online backup API or `VACUUM INTO` into
//     `Layout.TempDir`, compute SHA-256 checksums, `fsync` both file and parent directory,
//     and atomically `os.Rename` into `Layout.SnapshotsDir` — NEVER raw file reads of
//     an active `.db` file without its `-wal` state.
type SnapshotExporter interface {
	// CreateSnapshot writes a new consistent snapshot artifact and returns its metadata.
	CreateSnapshot(ctx context.Context, label string) (SnapshotMetadata, error)

	// ListSnapshots returns all finalized snapshots in SnapshotsDir ordered newest-first.
	ListSnapshots(ctx context.Context) ([]SnapshotMetadata, error)

	// PruneSnapshots deletes old snapshots exceeding keepLatest, returning the count removed.
	PruneSnapshots(ctx context.Context, keepLatest int) (int, error)
}

// SharedVolumeAdapter provisions, validates, locks, and snapshots a shared Flowsk
// local or Docker data volume.
type SharedVolumeAdapter interface {
	// EnsureLayout creates and permission-checks (`0700` / `0750`) the volume directory
	// hierarchy and verifies write/fsync health.
	EnsureLayout(ctx context.Context) (Layout, error)

	// Layout returns the resolved canonical directory paths for this volume.
	Layout() Layout

	// Locker returns the volume's cross-process AdvisoryLockAdapter.
	Locker() AdvisoryLockAdapter

	// Snapshots returns the volume's atomic SnapshotExporter.
	Snapshots() SnapshotExporter
}

// Adapter implements SharedVolumeAdapter, AdvisoryLockAdapter, and SnapshotExporter
// backed by POSIX `flock` lockfiles and SQLite `VACUUM INTO` online snapshots.
type Adapter struct {
	layout     Layout
	sqlitePath string
}

// AdapterSkeleton preserves type-level backward compatibility with callers written
// against the initial volume adapter stub type name.
type AdapterSkeleton = Adapter

// Compile-time checks ensuring *Adapter satisfies all volume adapter contracts.
var (
	_ SharedVolumeAdapter = (*Adapter)(nil)
	_ AdvisoryLockAdapter = (*Adapter)(nil)
	_ SnapshotExporter    = (*Adapter)(nil)
)

// NewSharedVolumeAdapter constructs a SharedVolumeAdapter rooted at rootDir
// (defaulting snapshot source discovery to `<rootDir>/db/flowsk.db`).
func NewSharedVolumeAdapter(rootDir string) (*Adapter, error) {
	return NewSharedVolumeAdapterWithDB(rootDir, "")
}

// NewSharedVolumeAdapterWithDB constructs a SharedVolumeAdapter rooted at rootDir
// with an explicit SQLite database file path for online `VACUUM INTO` snapshots.
// When sqlitePath is empty, `<rootDir>/db/flowsk.db` (or `<rootDir>/flowsk.db`) is used.
func NewSharedVolumeAdapterWithDB(rootDir, sqlitePath string) (*Adapter, error) {
	const op = "volume.NewSharedVolumeAdapter"
	if strings.TrimSpace(rootDir) == "" {
		return nil, errs.Validation(op, "volume root directory is required", errs.FieldViolation{
			Field:       "root_dir",
			Description: "volume RootDir must not be empty",
		})
	}
	cleanedRoot := filepath.Clean(rootDir)
	cleanedDB := ""
	if strings.TrimSpace(sqlitePath) != "" {
		cleanedDB = filepath.Clean(sqlitePath)
	}
	return &Adapter{
		layout: Layout{
			RootDir:      cleanedRoot,
			DatabaseDir:  filepath.Join(cleanedRoot, "db"),
			LocksDir:     filepath.Join(cleanedRoot, "locks"),
			SnapshotsDir: filepath.Join(cleanedRoot, "snapshots"),
			TempDir:      filepath.Join(cleanedRoot, "tmp"),
		},
		sqlitePath: cleanedDB,
	}, nil
}

// EnsureLayout creates and permission-tightens (`0750`) the volume directory tree
// and verifies atomic write + fsync + rename health inside TempDir.
func (a *Adapter) EnsureLayout(ctx context.Context) (Layout, error) {
	const op = "volume.SharedVolumeAdapter.EnsureLayout"
	if err := ctx.Err(); err != nil {
		return Layout{}, fmt.Errorf("%s: %w", op, err)
	}

	dirs := []string{
		a.layout.RootDir,
		a.layout.DatabaseDir,
		a.layout.LocksDir,
		a.layout.SnapshotsDir,
		a.layout.TempDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return Layout{}, fmt.Errorf("%s: create directory %q: %w", op, dir, err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			return Layout{}, fmt.Errorf("%s: stat directory %q: %w", op, dir, err)
		}
		if !info.IsDir() {
			return Layout{}, errs.Validation(op, "volume path is not a directory", errs.FieldViolation{
				Field:       "root_dir",
				Description: fmt.Sprintf("%q exists as a non-directory file", dir),
			})
		}
		if info.Mode().Perm()&0o027 != 0 {
			if err := os.Chmod(dir, 0o750); err != nil {
				return Layout{}, fmt.Errorf("%s: tighten permissions on %q: %w", op, dir, err)
			}
		}
	}

	// Verify write + fsync + atomic rename capability inside TempDir.
	probeSrc := filepath.Join(a.layout.TempDir, fmt.Sprintf(".probe-%s.tmp", uuid.NewString()))
	probeDst := probeSrc + ".ok"
	probeFile, err := os.OpenFile(filepath.Clean(probeSrc), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Layout{}, fmt.Errorf("%s: open write probe: %w", op, err)
	}
	if _, err := probeFile.WriteString("ok"); err != nil {
		_ = probeFile.Close()
		_ = os.Remove(probeSrc)
		return Layout{}, fmt.Errorf("%s: write probe: %w", op, err)
	}
	if err := probeFile.Sync(); err != nil {
		_ = probeFile.Close()
		_ = os.Remove(probeSrc)
		return Layout{}, fmt.Errorf("%s: fsync probe: %w", op, err)
	}
	if err := probeFile.Close(); err != nil {
		_ = os.Remove(probeSrc)
		return Layout{}, fmt.Errorf("%s: close probe: %w", op, err)
	}
	if err := os.Rename(probeSrc, probeDst); err != nil {
		_ = os.Remove(probeSrc)
		return Layout{}, fmt.Errorf("%s: atomic rename probe: %w", op, err)
	}
	_ = os.Remove(probeDst)

	return a.layout, nil
}

// Layout returns the configured canonical Layout paths.
func (a *Adapter) Layout() Layout {
	return a.layout
}

// Locker returns the AdvisoryLockAdapter bound to this volume.
func (a *Adapter) Locker() AdvisoryLockAdapter {
	return a
}

// Snapshots returns the SnapshotExporter bound to this volume.
func (a *Adapter) Snapshots() SnapshotExporter {
	return a
}

// validateLockName enforces safe single-component lock identifiers inside LocksDir.
func validateLockName(op, lockName string) (string, error) {
	trimmed := strings.TrimSpace(lockName)
	if trimmed == "" || strings.ContainsAny(trimmed, `/\`) || trimmed == "." || trimmed == ".." || strings.Contains(trimmed, "..") {
		return "", errs.Validation(op, "invalid advisory lock name", errs.FieldViolation{
			Field:       "lock_name",
			Description: "lockName must be a non-empty basename without path separators or '..'",
		})
	}
	return trimmed, nil
}

// openAndTryFlock opens the lockfile in LocksDir and attempts a non-blocking POSIX flock.
func (a *Adapter) openAndTryFlock(op, cleanName string) (*os.File, bool, error) {
	if err := os.MkdirAll(a.layout.LocksDir, 0o750); err != nil {
		return nil, false, fmt.Errorf("%s: create locks dir: %w", op, err)
	}
	lockPath := filepath.Join(a.layout.LocksDir, cleanName+".lock")
	f, err := os.OpenFile(filepath.Clean(lockPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("%s: open lockfile: %w", op, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: flock: %w", op, err)
	}
	return f, true, nil
}

// buildReleaseFunc wraps an acquired lockfile descriptor in an idempotent ReleaseFunc.
func buildReleaseFunc(op string, f *os.File) ReleaseFunc {
	return sync.OnceValue(func() error {
		unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		closeErr := f.Close()
		if unlockErr != nil {
			return fmt.Errorf("%s: unlock: %w", op, unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("%s: close lockfile: %w", op, closeErr)
		}
		return nil
	})
}

// TryAcquireExclusive attempts a non-blocking POSIX `flock` acquisition on lockName.
func (a *Adapter) TryAcquireExclusive(ctx context.Context, lockName string) (ReleaseFunc, error) {
	const op = "volume.AdvisoryLockAdapter.TryAcquireExclusive"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	cleanName, err := validateLockName(op, lockName)
	if err != nil {
		return nil, err
	}
	f, acquired, err := a.openAndTryFlock(op, cleanName)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errs.New(errs.ErrConflict, op, fmt.Sprintf("advisory lock %q is already held", cleanName))
	}
	return buildReleaseFunc(op, f), nil
}

// AcquireExclusive blocks until the named POSIX `flock` lock is acquired or ctx cancels.
func (a *Adapter) AcquireExclusive(ctx context.Context, lockName string) (ReleaseFunc, error) {
	const op = "volume.AdvisoryLockAdapter.AcquireExclusive"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	cleanName, err := validateLockName(op, lockName)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		f, acquired, flockErr := a.openAndTryFlock(op, cleanName)
		if flockErr != nil {
			return nil, flockErr
		}
		if acquired {
			return buildReleaseFunc(op, f), nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: %w", op, ctx.Err())
		case <-ticker.C:
		}
	}
}

// snapshotSidecar persists immutable metadata next to finalized `.db` snapshot files.
type snapshotSidecar struct {
	ID             string    `json:"id"`
	SizeBytes      int64     `json:"size_bytes"`
	ChecksumSHA256 string    `json:"checksum_sha256"`
	CreatedAt      time.Time `json:"created_at"`
}

// resolveSourceSQLitePath locates the active SQLite database file on the volume.
func (a *Adapter) resolveSourceSQLitePath(op string) (string, error) {
	candidates := make([]string, 0, 2)
	if a.sqlitePath != "" {
		candidates = append(candidates, a.sqlitePath)
	} else {
		candidates = append(
			candidates,
			filepath.Join(a.layout.DatabaseDir, "flowsk.db"),
			filepath.Join(a.layout.RootDir, "flowsk.db"),
		)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errs.New(errs.ErrNotFound, op, "source SQLite database file does not exist on volume")
}

// sanitizeSnapshotLabel strips unsafe filename runes from a caller-provided snapshot label.
func sanitizeSnapshotLabel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "snapshot"
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-_")
	if cleaned == "" {
		return "snapshot"
	}
	return cleaned
}

const snapshotLockName = "snapshots"

// computeFileSHA256AndSync calculates the SHA-256 hex digest and byte length of path after fsync.
func computeFileSHA256AndSync(path string) (checksum string, size int64, err error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}

// computeFileSHA256ReadOnly calculates the SHA-256 hex digest of path using a
// strictly read-only descriptor (`O_RDONLY`) without mutating or fsyncing the file.
func computeFileSHA256ReadOnly(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// syncDirectory fsyncs a directory descriptor so atomic renames become crash-durable.
func syncDirectory(dirPath string) error {
	dirFile, err := os.Open(filepath.Clean(dirPath))
	if err != nil {
		return err
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// CreateSnapshot exports a consistent online backup of the volume's SQLite database
// using `VACUUM INTO` into TempDir, computes its SHA-256 digest, fsyncs, and atomically
// renames it into SnapshotsDir while holding the exclusive snapshots advisory lock.
func (a *Adapter) CreateSnapshot(ctx context.Context, label string) (SnapshotMetadata, error) {
	const op = "volume.SnapshotExporter.CreateSnapshot"
	if err := ctx.Err(); err != nil {
		return SnapshotMetadata{}, fmt.Errorf("%s: %w", op, err)
	}

	release, err := a.Locker().AcquireExclusive(ctx, snapshotLockName)
	if err != nil {
		return SnapshotMetadata{}, fmt.Errorf("%s: acquire snapshot lock: %w", op, err)
	}
	defer func() { _ = release() }()

	if err := os.MkdirAll(a.layout.SnapshotsDir, 0o750); err != nil {
		return SnapshotMetadata{}, fmt.Errorf("%s: create snapshots dir: %w", op, err)
	}
	if err := os.MkdirAll(a.layout.TempDir, 0o750); err != nil {
		return SnapshotMetadata{}, fmt.Errorf("%s: create temp dir: %w", op, err)
	}

	srcDBPath, err := a.resolveSourceSQLitePath(op)
	if err != nil {
		return SnapshotMetadata{}, err
	}

	createdAt := time.Now().UTC()
	safeLabel := sanitizeSnapshotLabel(label)
	shortSuffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	snapID := fmt.Sprintf("%s-%s-%s", createdAt.Format("20060102T150405.000000000Z"), safeLabel, shortSuffix)

	stagedDBPath := filepath.Join(a.layout.TempDir, snapID+".db.tmp")
	stagedMetaPath := filepath.Join(a.layout.TempDir, snapID+".meta.json.tmp")
	finalDBPath := filepath.Join(a.layout.SnapshotsDir, snapID+".db")
	finalMetaPath := filepath.Join(a.layout.SnapshotsDir, snapID+".meta.json")

	_ = os.Remove(stagedDBPath)
	_ = os.Remove(stagedMetaPath)

	srcSQLDB, err := sql.Open("sqlite", srcDBPath+"?_busy_timeout=10000&_foreign_keys=on")
	if err != nil {
		return SnapshotMetadata{}, fmt.Errorf("%s: open source sqlite: %w", op, err)
	}
	defer func() { _ = srcSQLDB.Close() }()

	escapedStagePath := strings.ReplaceAll(stagedDBPath, "'", "''")
	if _, err := srcSQLDB.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escapedStagePath)); err != nil {
		_ = os.Remove(stagedDBPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: sqlite VACUUM INTO: %w", op, err)
	}

	checksumHex, sizeBytes, err := computeFileSHA256AndSync(stagedDBPath)
	if err != nil {
		_ = os.Remove(stagedDBPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: checksum/fsync staged snapshot: %w", op, err)
	}

	sidecarBytes, err := json.Marshal(snapshotSidecar{
		ID:             snapID,
		SizeBytes:      sizeBytes,
		ChecksumSHA256: checksumHex,
		CreatedAt:      createdAt,
	})
	if err != nil {
		_ = os.Remove(stagedDBPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: encode snapshot metadata: %w", op, err)
	}
	if err := os.WriteFile(stagedMetaPath, sidecarBytes, 0o600); err != nil {
		_ = os.Remove(stagedDBPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: write staged metadata: %w", op, err)
	}
	if err := os.Rename(stagedMetaPath, finalMetaPath); err != nil {
		_ = os.Remove(stagedDBPath)
		_ = os.Remove(stagedMetaPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: rename snapshot metadata: %w", op, err)
	}
	if err := os.Rename(stagedDBPath, finalDBPath); err != nil {
		_ = os.Remove(stagedDBPath)
		_ = os.Remove(finalMetaPath)
		return SnapshotMetadata{}, fmt.Errorf("%s: atomic rename snapshot: %w", op, err)
	}
	_ = syncDirectory(a.layout.SnapshotsDir)

	return SnapshotMetadata{
		ID:             snapID,
		FilePath:       finalDBPath,
		SizeBytes:      sizeBytes,
		ChecksumSHA256: checksumHex,
		CreatedAt:      createdAt,
	}, nil
}

// ListSnapshots returns all finalized `.db` snapshots in SnapshotsDir ordered newest-first.
func (a *Adapter) ListSnapshots(ctx context.Context) ([]SnapshotMetadata, error) {
	const op = "volume.SnapshotExporter.ListSnapshots"
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	entries, err := os.ReadDir(a.layout.SnapshotsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: read snapshots dir: %w", op, err)
	}

	var out []SnapshotMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		snapID := strings.TrimSuffix(entry.Name(), ".db")
		dbPath := filepath.Join(a.layout.SnapshotsDir, entry.Name())
		metaPath := filepath.Join(a.layout.SnapshotsDir, snapID+".meta.json")

		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}

		meta := SnapshotMetadata{
			ID:        snapID,
			FilePath:  dbPath,
			SizeBytes: info.Size(),
			CreatedAt: info.ModTime().UTC(),
		}

		if rawSidecar, readErr := os.ReadFile(filepath.Clean(metaPath)); readErr == nil {
			var sc snapshotSidecar
			if jsonErr := json.Unmarshal(rawSidecar, &sc); jsonErr == nil {
				if sc.ID != "" {
					meta.ID = sc.ID
				}
				if sc.ChecksumSHA256 != "" {
					meta.ChecksumSHA256 = sc.ChecksumSHA256
				}
				if !sc.CreatedAt.IsZero() {
					meta.CreatedAt = sc.CreatedAt.UTC()
				}
			}
		}
		if meta.ChecksumSHA256 == "" {
			if sum, sumErr := computeFileSHA256ReadOnly(dbPath); sumErr == nil {
				meta.ChecksumSHA256 = sum
			}
		}
		out = append(out, meta)
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// PruneSnapshots deletes older snapshots exceeding keepLatest and returns the count removed.
func (a *Adapter) PruneSnapshots(ctx context.Context, keepLatest int) (int, error) {
	const op = "volume.SnapshotExporter.PruneSnapshots"
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if keepLatest < 0 {
		return 0, errs.Validation(op, "keepLatest must be non-negative", errs.FieldViolation{
			Field:       "keep_latest",
			Description: "keepLatest retention threshold must be >= 0",
		})
	}

	release, err := a.Locker().AcquireExclusive(ctx, snapshotLockName)
	if err != nil {
		return 0, fmt.Errorf("%s: acquire snapshot lock: %w", op, err)
	}
	defer func() { _ = release() }()

	snapshots, err := a.ListSnapshots(ctx)
	if err != nil {
		return 0, err
	}
	if len(snapshots) <= keepLatest {
		return 0, nil
	}

	removed := 0
	for _, old := range snapshots[keepLatest:] {
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("%s: %w", op, err)
		}
		if err := os.Remove(old.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("%s: remove snapshot %q: %w", op, old.FilePath, err)
		}
		metaPath := strings.TrimSuffix(old.FilePath, ".db") + ".meta.json"
		_ = os.Remove(metaPath)
		removed++
	}
	_ = syncDirectory(a.layout.SnapshotsDir)
	return removed, nil
}
