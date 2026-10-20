package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type NoteState string

const (
	NoteOpen     NoteState = "open"
	NoteResolved NoteState = "resolved"
	NoteOutdated NoteState = "outdated"
)

// Anchor is a position in a file at a commit. Line and Col count from 1; Col
// counts bytes.
type Anchor struct {
	SHA  string
	File string
	Line int
	Col  int
}

type Note struct {
	ID         int64
	UserID     string
	Body       string
	State      NoteState
	Origin     Anchor // where it was written
	Selected   string
	CreatedAt  int64
	UpdatedAt  int64
	ResolvedBy string
}

// Ref is the note's ID as Typst and the UI see it: "n" and the row id.
func (n Note) Ref() string { return "n" + strconv.FormatInt(n.ID, 10) }

// ParseNoteRef turns "n12" back into 12.
func ParseNoteRef(ref string) (int64, error) {
	if !strings.HasPrefix(ref, "n") {
		return 0, fmt.Errorf("note id %q", ref)
	}
	id, err := strconv.ParseInt(ref[1:], 10, 64)
	if err != nil || id <= 0 || "n"+strconv.FormatInt(id, 10) != ref {
		return 0, fmt.Errorf("note id %q", ref)
	}
	return id, nil
}

const noteCols = "id, user_id, body, state, sha, file, line, col, selected, created_at, updated_at, resolved_by"

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	err := row.Scan(&n.ID, &n.UserID, &n.Body, &n.State, &n.Origin.SHA, &n.Origin.File, &n.Origin.Line,
		&n.Origin.Col, &n.Selected, &n.CreatedAt, &n.UpdatedAt, &n.ResolvedBy)
	return n, err
}

// ErrTooManyNotes is returned when a commit already holds the cap of open
// notes.
var ErrTooManyNotes = errors.New("too many open notes on this commit")

// CreateNote stores a note and its anchor on its own commit, refusing it when
// the commit already has maxOpen open notes.
func (s *Store) CreateNote(ctx context.Context, n Note, maxOpen int) (Note, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return n, err
	}
	defer tx.Rollback()
	var open int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM note_anchors a JOIN notes n ON n.id = a.note_id
WHERE a.sha = ? AND n.state = 'open'`, n.Origin.SHA).Scan(&open); err != nil {
		return n, err
	}
	if open >= maxOpen {
		return n, ErrTooManyNotes
	}
	t := now()
	res, err := tx.ExecContext(ctx, `
INSERT INTO notes (user_id, body, state, sha, file, line, col, selected, created_at, updated_at)
VALUES (?, ?, 'open', ?, ?, ?, ?, ?, ?, ?)`,
		n.UserID, n.Body, n.Origin.SHA, n.Origin.File, n.Origin.Line, n.Origin.Col, n.Selected, t, t)
	if err != nil {
		return n, err
	}
	n.ID, _ = res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO note_anchors (note_id, sha, file, line, col) VALUES (?, ?, ?, ?, ?)",
		n.ID, n.Origin.SHA, n.Origin.File, n.Origin.Line, n.Origin.Col); err != nil {
		return n, err
	}
	n.State, n.CreatedAt, n.UpdatedAt = NoteOpen, t, t
	return n, tx.Commit()
}

func (s *Store) GetNote(ctx context.Context, id int64) (Note, error) {
	n, err := scanNote(s.db.QueryRowContext(ctx, "SELECT "+noteCols+" FROM notes WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

func (s *Store) UpdateNoteBody(ctx context.Context, id int64, body string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE notes SET body = ?, updated_at = ? WHERE id = ?", body, now(), id)
	return err
}

func (s *Store) DeleteNote(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notes WHERE id = ?", id)
	return err
}

func (s *Store) SetNoteState(ctx context.Context, id int64, st NoteState, by string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE notes SET state = ?, resolved_by = ?, updated_at = ? WHERE id = ?", st, by, now(), id)
	return err
}

// PlacedNote is a note with its anchor at one particular commit.
type PlacedNote struct {
	Note
	At Anchor
}

// NotesAt returns the notes anchored at a commit, in file and line order. An
// empty state returns every state.
func (s *Store) NotesAt(ctx context.Context, sha string, state NoteState) ([]PlacedNote, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT n.id, n.user_id, n.body, n.state, n.sha, n.file, n.line, n.col, n.selected, n.created_at, n.updated_at, n.resolved_by,
	a.sha, a.file, a.line, a.col
FROM note_anchors a JOIN notes n ON n.id = a.note_id
WHERE a.sha = ? AND (? = '' OR n.state = ?)
ORDER BY a.file, a.line, a.col, n.id`, sha, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlacedNote
	for rows.Next() {
		var p PlacedNote
		if err := rows.Scan(&p.ID, &p.UserID, &p.Body, &p.State, &p.Origin.SHA, &p.Origin.File, &p.Origin.Line,
			&p.Origin.Col, &p.Selected, &p.CreatedAt, &p.UpdatedAt, &p.ResolvedBy,
			&p.At.SHA, &p.At.File, &p.At.Line, &p.At.Col); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Notes lists notes for the Notes page, newest first. Empty filters match
// everything.
func (s *Store) Notes(ctx context.Context, state NoteState, userID, sha string) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+noteCols+` FROM notes
WHERE (? = '' OR state = ?) AND (? = '' OR user_id = ?) AND (? = '' OR sha = ?)
ORDER BY created_at DESC, id DESC`, state, state, userID, userID, sha, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// OpenNoteCounts maps commit SHA to the number of open notes anchored there.
func (s *Store) OpenNoteCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.sha, count(*) FROM note_anchors a JOIN notes n ON n.id = a.note_id
WHERE n.state = 'open' GROUP BY a.sha`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sha string
		var n int
		if err := rows.Scan(&sha, &n); err != nil {
			return nil, err
		}
		out[sha] = n
	}
	return out, rows.Err()
}

// PutAnchor records where a note sits at a commit.
func (s *Store) PutAnchor(ctx context.Context, noteID int64, a Anchor) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO note_anchors (note_id, sha, file, line, col) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (note_id, sha) DO UPDATE SET file = excluded.file, line = excluded.line, col = excluded.col`,
		noteID, a.SHA, a.File, a.Line, a.Col)
	return err
}

// Anchors returns every commit a note is anchored at.
func (s *Store) Anchors(ctx context.Context, noteID int64) ([]Anchor, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT sha, file, line, col FROM note_anchors WHERE note_id = ?", noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Anchor
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.SHA, &a.File, &a.Line, &a.Col); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OpenNotes returns every open note with all its anchors, for carrying notes
// forward to a new commit.
func (s *Store) OpenNotes(ctx context.Context) ([]Note, error) {
	return s.Notes(ctx, NoteOpen, "", "")
}
