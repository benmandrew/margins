package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Credential is a stored passkey. Data is the library's credential record as
// JSON, so the store does not depend on the WebAuthn library's types.
type Credential struct {
	ID     []byte
	UserID string
	Data   string
}

func (s *Store) AddCredential(ctx context.Context, c Credential) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO credentials (id, user_id, data, created_at) VALUES (?, ?, ?, ?)",
		c.ID, c.UserID, c.Data, now())
	return err
}

func (s *Store) Credentials(ctx context.Context, userID string) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, user_id, data FROM credentials WHERE user_id = ? ORDER BY created_at", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.UserID, &c.Data); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CredentialByID(ctx context.Context, id []byte) (Credential, error) {
	var c Credential
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, data FROM credentials WHERE id = ?", id).
		Scan(&c.ID, &c.UserID, &c.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// UpdateCredential stores the credential after a login, which advances its
// signature counter.
func (s *Store) UpdateCredential(ctx context.Context, c Credential) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE credentials SET data = ?, last_used_at = ? WHERE id = ?", c.Data, now(), c.ID)
	return err
}

// EnrolmentUsed reports whether an enrolment hash has been used.
func (s *Store) EnrolmentUsed(ctx context.Context, hash string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM used_enrolments WHERE hash = ?", hash).Scan(&n)
	return n > 0, err
}

// CompleteEnrolment records the hash as used and stores the passkey in one
// transaction, so a hash cannot be spent twice by two racing ceremonies.
func (s *Store) CompleteEnrolment(ctx context.Context, hash string, c Credential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO used_enrolments (hash, user_id, used_at) VALUES (?, ?, ?)", hash, c.UserID, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrEnrolmentUsed
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO credentials (id, user_id, data, created_at) VALUES (?, ?, ?, ?)",
		c.ID, c.UserID, c.Data, now()); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrEnrolmentUsed = errors.New("enrolment secret already used")

type Session struct {
	Hash      string
	UserID    string
	CSRF      string
	ExpiresAt int64
}

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (hash, user_id, csrf, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
		sess.Hash, sess.UserID, sess.CSRF, now(), sess.ExpiresAt)
	return err
}

// GetSession returns a live session. An expired one is deleted and reported
// as not found.
func (s *Store) GetSession(ctx context.Context, hash string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx,
		"SELECT hash, user_id, csrf, expires_at FROM sessions WHERE hash = ?", hash).
		Scan(&sess.Hash, &sess.UserID, &sess.CSRF, &sess.ExpiresAt)
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

// RemoveUsersExcept deletes the passkeys and sessions of every user not in
// keep. Their notes stay: they are feedback, and the author still reads them.
func (s *Store) RemoveUsersExcept(ctx context.Context, keep []string) (int64, error) {
	if len(keep) == 0 {
		keep = []string{""}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, k := range keep {
		args[i] = k
	}
	var total int64
	for _, table := range []string{"credentials", "sessions"} {
		res, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE user_id NOT IN ("+ph+")", args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
