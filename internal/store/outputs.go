package store

import (
	"context"
	"database/sql"
	"errors"
)

// Status is the life cycle of a build, a diff or an annotated build.
type Status string

const (
	Queued   Status = "queued"
	Building Status = "building"
	OK       Status = "ok"
	Failed   Status = "failed"
	Timeout  Status = "timeout"
)

type Build struct {
	ID         int64
	SHA        string
	Entry      string
	Typst      string
	Status     Status
	Attempts   int
	DurationMS int64
	Size       int64
	Pruned     bool
	UpdatedAt  int64
}

const buildCols = "id, sha, entry, typst, status, attempts, duration_ms, size, pruned, updated_at"

func scanBuild(row interface{ Scan(...any) error }) (Build, error) {
	var b Build
	err := row.Scan(&b.ID, &b.SHA, &b.Entry, &b.Typst, &b.Status, &b.Attempts, &b.DurationMS, &b.Size, &b.Pruned, &b.UpdatedAt)
	return b, err
}

// EnsureBuild returns the build for this cache key, creating it as queued.
// created reports whether it was new.
func (s *Store) EnsureBuild(ctx context.Context, sha, entry, typst string) (b Build, created bool, err error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO builds (sha, entry, typst, status, updated_at) VALUES (?, ?, ?, 'queued', ?)",
		sha, entry, typst, now())
	if err != nil {
		return b, false, err
	}
	n, _ := res.RowsAffected()
	b, err = scanBuild(s.db.QueryRowContext(ctx,
		"SELECT "+buildCols+" FROM builds WHERE sha = ? AND entry = ? AND typst = ?", sha, entry, typst))
	return b, n == 1, err
}

// StartBuild marks a build as building and counts the attempt.
func (s *Store) StartBuild(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE builds SET status = 'building', attempts = attempts + 1, updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) FinishBuild(ctx context.Context, id int64, st Status, durationMS, size int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE builds SET status = ?, duration_ms = ?, size = ?, pruned = 0, updated_at = ? WHERE id = ?",
		st, durationMS, size, now(), id)
	return err
}

// RequeueBuild puts a build back to queued, for a retry or after its PDF was
// pruned and someone asked for it again.
func (s *Store) RequeueBuild(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE builds SET status = 'queued', updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) GetBuild(ctx context.Context, id int64) (Build, error) {
	b, err := scanBuild(s.db.QueryRowContext(ctx, "SELECT "+buildCols+" FROM builds WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// BuildsForCommit returns every build of a commit, newest first.
func (s *Store) BuildsForCommit(ctx context.Context, sha string) ([]Build, error) {
	return s.queryBuilds(ctx, "SELECT "+buildCols+" FROM builds WHERE sha = ? ORDER BY updated_at DESC, id DESC", sha)
}

// CurrentBuild is the most recent build of (sha, entry), whichever Typst
// version it used.
func (s *Store) CurrentBuild(ctx context.Context, sha, entry string) (Build, error) {
	b, err := scanBuild(s.db.QueryRowContext(ctx,
		"SELECT "+buildCols+" FROM builds WHERE sha = ? AND entry = ? ORDER BY id DESC LIMIT 1", sha, entry))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

func (s *Store) BuildsWithStatus(ctx context.Context, st Status) ([]Build, error) {
	return s.queryBuilds(ctx, "SELECT "+buildCols+" FROM builds WHERE status = ? ORDER BY id", st)
}

func (s *Store) AllBuilds(ctx context.Context) ([]Build, error) {
	return s.queryBuilds(ctx, "SELECT "+buildCols+" FROM builds ORDER BY id")
}

func (s *Store) queryBuilds(ctx context.Context, q string, args ...any) ([]Build, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) MarkBuildPruned(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE builds SET pruned = 1 WHERE id = ?", id)
	return err
}

type Diff struct {
	ID        int64
	A, B      string
	Entry     string
	Status    Status
	Method    string
	Attempts  int
	Size      int64
	Pruned    bool
	UpdatedAt int64
}

const diffCols = "id, a, b, entry, status, method, attempts, size, pruned, updated_at"

func scanDiff(row interface{ Scan(...any) error }) (Diff, error) {
	var d Diff
	err := row.Scan(&d.ID, &d.A, &d.B, &d.Entry, &d.Status, &d.Method, &d.Attempts, &d.Size, &d.Pruned, &d.UpdatedAt)
	return d, err
}

func (s *Store) EnsureDiff(ctx context.Context, a, b, entry string) (d Diff, created bool, err error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO diffs (a, b, entry, status, updated_at) VALUES (?, ?, ?, 'queued', ?)",
		a, b, entry, now())
	if err != nil {
		return d, false, err
	}
	n, _ := res.RowsAffected()
	d, err = scanDiff(s.db.QueryRowContext(ctx,
		"SELECT "+diffCols+" FROM diffs WHERE a = ? AND b = ? AND entry = ?", a, b, entry))
	return d, n == 1, err
}

func (s *Store) GetDiff(ctx context.Context, id int64) (Diff, error) {
	d, err := scanDiff(s.db.QueryRowContext(ctx, "SELECT "+diffCols+" FROM diffs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (s *Store) FindDiff(ctx context.Context, a, b, entry string) (Diff, error) {
	d, err := scanDiff(s.db.QueryRowContext(ctx,
		"SELECT "+diffCols+" FROM diffs WHERE a = ? AND b = ? AND entry = ?", a, b, entry))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (s *Store) StartDiff(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE diffs SET status = 'building', attempts = attempts + 1, updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) FinishDiff(ctx context.Context, id int64, st Status, method string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE diffs SET status = ?, method = ?, size = ?, pruned = 0, updated_at = ? WHERE id = ?",
		st, method, size, now(), id)
	return err
}

func (s *Store) RequeueDiff(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE diffs SET status = 'queued', updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) DiffsWithStatus(ctx context.Context, st Status) ([]Diff, error) {
	return s.queryDiffs(ctx, "SELECT "+diffCols+" FROM diffs WHERE status = ? ORDER BY id", st)
}

func (s *Store) AllDiffs(ctx context.Context) ([]Diff, error) {
	return s.queryDiffs(ctx, "SELECT "+diffCols+" FROM diffs ORDER BY id")
}

func (s *Store) queryDiffs(ctx context.Context, q string, args ...any) ([]Diff, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Diff
	for rows.Next() {
		d, err := scanDiff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) MarkDiffPruned(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE diffs SET pruned = 1 WHERE id = ?", id)
	return err
}

type Annotated struct {
	ID         int64
	SHA        string
	Entry      string
	Typst      string
	NotesHash  string
	Status     Status
	Placements string // JSON object: note id -> placement
	Size       int64
	Pruned     bool
	UpdatedAt  int64
}

const annotatedCols = "id, sha, entry, typst, notes_hash, status, placements, size, pruned, updated_at"

func scanAnnotated(row interface{ Scan(...any) error }) (Annotated, error) {
	var a Annotated
	err := row.Scan(&a.ID, &a.SHA, &a.Entry, &a.Typst, &a.NotesHash, &a.Status, &a.Placements, &a.Size, &a.Pruned, &a.UpdatedAt)
	return a, err
}

func (s *Store) EnsureAnnotated(ctx context.Context, sha, entry, typst, notesHash string) (a Annotated, created bool, err error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO annotated (sha, entry, typst, notes_hash, status, updated_at) VALUES (?, ?, ?, ?, 'queued', ?)",
		sha, entry, typst, notesHash, now())
	if err != nil {
		return a, false, err
	}
	n, _ := res.RowsAffected()
	a, err = scanAnnotated(s.db.QueryRowContext(ctx,
		"SELECT "+annotatedCols+" FROM annotated WHERE sha = ? AND entry = ? AND typst = ? AND notes_hash = ?",
		sha, entry, typst, notesHash))
	return a, n == 1, err
}

func (s *Store) GetAnnotated(ctx context.Context, id int64) (Annotated, error) {
	a, err := scanAnnotated(s.db.QueryRowContext(ctx, "SELECT "+annotatedCols+" FROM annotated WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// LatestAnnotated is the newest successful annotated build of (sha, entry),
// used to show note placements.
func (s *Store) LatestAnnotated(ctx context.Context, sha, entry string) (Annotated, error) {
	a, err := scanAnnotated(s.db.QueryRowContext(ctx,
		"SELECT "+annotatedCols+" FROM annotated WHERE sha = ? AND entry = ? AND status = 'ok' ORDER BY updated_at DESC, id DESC LIMIT 1",
		sha, entry))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) StartAnnotated(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE annotated SET status = 'building', updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) FinishAnnotated(ctx context.Context, id int64, st Status, placements string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE annotated SET status = ?, placements = ?, size = ?, pruned = 0, updated_at = ? WHERE id = ?",
		st, placements, size, now(), id)
	return err
}

func (s *Store) RequeueAnnotated(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE annotated SET status = 'queued', updated_at = ? WHERE id = ?", now(), id)
	return err
}

func (s *Store) AllAnnotated(ctx context.Context) ([]Annotated, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+annotatedCols+" FROM annotated ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Annotated
	for rows.Next() {
		a, err := scanAnnotated(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) MarkAnnotatedPruned(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE annotated SET pruned = 1 WHERE id = ?", id)
	return err
}
