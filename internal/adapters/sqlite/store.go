// Package sqlite is recond's storage adapter: an embedded, pure-Go SQLite
// (modernc.org/sqlite, no cgo) so the binary is self-contained and tests run
// against the real store in a temp directory instead of mocks.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store errors callers branch on.
var (
	ErrAlreadyIngested = errors.New("file already ingested")
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
)

// Store wraps the database. SQLite allows one writer at a time; writes are
// serialised in-process (wmu) so concurrent webhook deliveries queue on a
// mutex instead of spinning on SQLITE_BUSY, while WAL lets reads proceed.
type Store struct {
	db  *sql.DB
	wmu sync.Mutex
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies migrations.
// The page cache is capped at 256 MiB: ingestion inserts into indexes keyed by
// random ids, and a cache smaller than those indexes turns every insert into
// disk reads.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(ON)", "busy_timeout(10000)", "cache_size(-262144)", "temp_store(MEMORY)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{db: db, now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is reachable (readiness).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migrate applies embedded SQL files in lexical order, each in its own
// transaction. Forward-only: a fix is a new migration, never an edit.
func (s *Store) migrate(ctx context.Context) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if err := s.applyMigration(ctx, version, name); err != nil {
			return fmt.Errorf("%s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version, file string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	body, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

// writeTx runs fn in a write transaction under the writer mutex.
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nullTime(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

func fromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(0, n.Int64).UTC()
}

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
