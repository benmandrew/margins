package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// SeenDelivery records a webhook delivery id and reports whether it was
// recorded before.
func (s *Store) SeenDelivery(id string) (bool, error) {
	res, err := s.db.Exec("INSERT OR IGNORE INTO deliveries (id, received_at) VALUES (?, ?)", id, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 0, err
}

type Commit struct {
	SHA         string
	Parents     []string
	Author      string
	Subject     string
	CommittedAt int64
	Unreachable bool
}

func (s *Store) PutCommit(ctx context.Context, c Commit) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO commits (sha, parents, author, subject, committed_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (sha) DO UPDATE SET unreachable = 0`,
		c.SHA, strings.Join(c.Parents, " "), c.Author, c.Subject, c.CommittedAt)
	return err
}

func (s *Store) GetCommit(ctx context.Context, sha string) (Commit, error) {
	var c Commit
	var parents string
	err := s.db.QueryRowContext(ctx,
		"SELECT sha, parents, author, subject, committed_at, unreachable FROM commits WHERE sha = ?", sha).
		Scan(&c.SHA, &parents, &c.Author, &c.Subject, &c.CommittedAt, &c.Unreachable)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Parents = strings.Fields(parents)
	return c, err
}

// ResolveSHA expands a short SHA to the one known commit it prefixes.
func (s *Store) ResolveSHA(ctx context.Context, prefix string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT sha FROM commits WHERE sha >= ? AND sha < ? LIMIT 2", prefix, prefix+"g")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return "", err
		}
		shas = append(shas, sha)
	}
	if len(shas) != 1 {
		return "", ErrNotFound
	}
	return shas[0], rows.Err()
}

// MarkUnreachable flags commits that a force push removed from every branch.
// Their builds stay.
func (s *Store) MarkUnreachable(ctx context.Context, shas []string) error {
	for _, sha := range shas {
		if _, err := s.db.ExecContext(ctx, "UPDATE commits SET unreachable = 1 WHERE sha = ?", sha); err != nil {
			return err
		}
	}
	return nil
}

type Ref struct {
	Name    string // full ref name, refs/heads/... or refs/tags/...
	SHA     string
	Kind    string // "branch" or "tag"
	TagDate int64
	Gone    bool
}

func (r Ref) Short() string {
	return strings.TrimPrefix(strings.TrimPrefix(r.Name, "refs/heads/"), "refs/tags/")
}

func (s *Store) PutRef(ctx context.Context, r Ref) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO refs (name, sha, kind, tag_date, gone, updated_at) VALUES (?, ?, ?, ?, 0, ?)
ON CONFLICT (name) DO UPDATE SET sha = excluded.sha, kind = excluded.kind,
	tag_date = excluded.tag_date, gone = 0, updated_at = excluded.updated_at`,
		r.Name, r.SHA, r.Kind, r.TagDate, now())
	return err
}

func (s *Store) MarkRefGone(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE refs SET gone = 1, updated_at = ? WHERE name = ?", now(), name)
	return err
}

// Refs returns live refs of a kind ("branch", "tag" or "" for both).
func (s *Store) Refs(ctx context.Context, kind string) ([]Ref, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, sha, kind, tag_date, gone FROM refs WHERE gone = 0 AND (? = '' OR kind = ?) ORDER BY name",
		kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.Name, &r.SHA, &r.Kind, &r.TagDate, &r.Gone); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

func (s *Store) GetRef(ctx context.Context, name string) (Ref, error) {
	var r Ref
	err := s.db.QueryRowContext(ctx,
		"SELECT name, sha, kind, tag_date, gone FROM refs WHERE name = ?", name).
		Scan(&r.Name, &r.SHA, &r.Kind, &r.TagDate, &r.Gone)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
