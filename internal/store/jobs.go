package store

import (
	"context"
	"database/sql"
	"errors"
)

// Job priorities: lower runs first. A push is fetched before anything it
// causes is built, and work a person is waiting for comes before history.
const (
	PriorityFetch     = 0
	PriorityRequested = 1
	PriorityBuild     = 2
	PriorityHistory   = 3
)

type Job struct {
	ID       int64
	Kind     string
	Key      string
	Payload  string
	Priority int
}

// Enqueue adds a job unless one with the same key is already queued or
// running. It reports whether a job was added.
func (s *Store) Enqueue(ctx context.Context, kind, key, payload string, priority int) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO jobs (kind, key, payload, state, priority, created_at) VALUES (?, ?, ?, 'queued', ?, ?)",
		kind, key, payload, priority, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Claim takes the next queued job and marks it running. It returns
// ErrNotFound when the queue is empty.
func (s *Store) Claim(ctx context.Context) (Job, error) {
	var j Job
	err := s.db.QueryRowContext(ctx, `
UPDATE jobs SET state = 'running'
WHERE id = (SELECT id FROM jobs WHERE state = 'queued' ORDER BY priority, id LIMIT 1)
RETURNING id, kind, key, payload, priority`).Scan(&j.ID, &j.Kind, &j.Key, &j.Payload, &j.Priority)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

// Done removes a finished job, whatever its outcome. Outcomes live on the
// build, diff and annotated records.
func (s *Store) Done(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", id)
	return err
}

// ResetRunning returns jobs a crash left running to the queue. Call it once
// at boot, before any worker starts.
func (s *Store) ResetRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET state = 'queued' WHERE state = 'running'")
	return err
}

// CountJobs counts queued and running jobs of a kind.
func (s *Store) CountJobs(ctx context.Context, kind string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE kind = ?", kind).Scan(&n)
	return n, err
}
