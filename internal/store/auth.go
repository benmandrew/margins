package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type Session struct {
	Hash   string
	UserID string
	CSRF   string
	// SecretHash is the user's secret hash when the session started. The
	// session is valid only while the config still has that hash.
	SecretHash string
	ExpiresAt  int64
}

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (hash, user_id, csrf, secret_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		sess.Hash, sess.UserID, sess.CSRF, sess.SecretHash, now(), sess.ExpiresAt)
	return err
}

// GetSession returns a live session. An expired one is deleted and reported
// as not found.
func (s *Store) GetSession(ctx context.Context, hash string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx,
		"SELECT hash, user_id, csrf, secret_hash, expires_at FROM sessions WHERE hash = ?", hash).
		Scan(&sess.Hash, &sess.UserID, &sess.CSRF, &sess.SecretHash, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	if sess.ExpiresAt <= now() {
		s.DeleteSession(ctx, hash)
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE hash = ?", hash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now())
	return err
}

// RemoveUsersExcept deletes the sessions of every user not in keep. Their
// notes stay: they are feedback, and the author still reads them.
func (s *Store) RemoveUsersExcept(ctx context.Context, keep []string) (int64, error) {
	if len(keep) == 0 {
		keep = []string{""}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, k := range keep {
		args[i] = k
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id NOT IN ("+ph+")", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
