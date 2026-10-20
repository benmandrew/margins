// Package store holds the service's state in one SQLite database: build and
// diff records, the job queue, notes, passkeys and sessions.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by single-row lookups that match nothing.
var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and brings its schema up to
// date.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises every statement. The load is a handful of
	// writes a minute, and it rules out SQLITE_BUSY between our own writers.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Snapshot writes a consistent copy of the database to path. VACUUM INTO
// writes a temporary file first, which is renamed into place, so a reader of
// path never sees a half-written snapshot.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var migrations = []string{
	`
CREATE TABLE deliveries (
	id TEXT PRIMARY KEY,
	received_at INTEGER NOT NULL
);

CREATE TABLE commits (
	sha TEXT PRIMARY KEY,
	parents TEXT NOT NULL,
	author TEXT NOT NULL,
	subject TEXT NOT NULL,
	committed_at INTEGER NOT NULL,
	unreachable INTEGER NOT NULL DEFAULT 0
);

-- Branches and tags as last seen. A deleted ref is kept with gone = 1.
CREATE TABLE refs (
	name TEXT PRIMARY KEY,
	sha TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('branch', 'tag')),
	tag_date INTEGER NOT NULL DEFAULT 0,
	gone INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE builds (
	id INTEGER PRIMARY KEY,
	sha TEXT NOT NULL,
	entry TEXT NOT NULL,
	typst TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'building', 'ok', 'failed', 'timeout')),
	attempts INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	pruned INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	UNIQUE (sha, entry, typst)
);
CREATE INDEX builds_sha ON builds (sha);

CREATE TABLE diffs (
	id INTEGER PRIMARY KEY,
	a TEXT NOT NULL,
	b TEXT NOT NULL,
	entry TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'building', 'ok', 'failed', 'timeout')),
	method TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	pruned INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	UNIQUE (a, b, entry)
);

CREATE TABLE jobs (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	key TEXT NOT NULL UNIQUE,
	payload TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('queued', 'running')),
	priority INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX jobs_next ON jobs (state, priority, id);

CREATE TABLE credentials (
	id BLOB PRIMARY KEY,
	user_id TEXT NOT NULL,
	data TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX credentials_user ON credentials (user_id);

CREATE TABLE used_enrolments (
	hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	used_at INTEGER NOT NULL
);

CREATE TABLE sessions (
	hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	csrf TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE notes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id TEXT NOT NULL,
	body TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('open', 'resolved', 'outdated')),
	sha TEXT NOT NULL,
	file TEXT NOT NULL,
	line INTEGER NOT NULL,
	col INTEGER NOT NULL,
	selected TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	resolved_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX notes_sha ON notes (sha);

-- Where each note sits in each commit it has been carried to, including the
-- commit it was written on.
CREATE TABLE note_anchors (
	note_id INTEGER NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
	sha TEXT NOT NULL,
	file TEXT NOT NULL,
	line INTEGER NOT NULL,
	col INTEGER NOT NULL,
	PRIMARY KEY (note_id, sha)
);
CREATE INDEX note_anchors_sha ON note_anchors (sha);

CREATE TABLE annotated (
	id INTEGER PRIMARY KEY,
	sha TEXT NOT NULL,
	entry TEXT NOT NULL,
	typst TEXT NOT NULL,
	notes_hash TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'building', 'ok', 'failed', 'timeout')),
	-- JSON object: note id -> "anchor", "paragraph" or "unplaced".
	placements TEXT NOT NULL DEFAULT '{}',
	size INTEGER NOT NULL DEFAULT 0,
	pruned INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	UNIQUE (sha, entry, typst, notes_hash)
);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

// IntegrityCheck runs PRAGMA integrity_check and returns its verdict.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var res string
	err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res)
	return res, err
}

// Ping checks the database answers a query, for the health check.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
