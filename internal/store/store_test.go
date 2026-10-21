package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var ctx = context.Background()

func TestDeliveries(t *testing.T) {
	s := open(t)
	for i, want := range []bool{false, true} {
		seen, err := s.SeenDelivery("d1")
		if err != nil || seen != want {
			t.Errorf("call %d: seen = %v, %v; want %v", i, seen, err, want)
		}
	}
}

func TestJobsPriorityAndDedupe(t *testing.T) {
	s := open(t)
	s.Enqueue(ctx, "build", "build:a", "{}", PriorityHistory)
	s.Enqueue(ctx, "fetch", "fetch", "{}", PriorityFetch)
	if added, _ := s.Enqueue(ctx, "fetch", "fetch", "{}", PriorityFetch); added {
		t.Error("duplicate key was added")
	}
	j, err := s.Claim(ctx)
	if err != nil || j.Kind != "fetch" {
		t.Fatalf("first claim = %+v, %v", j, err)
	}
	// A running job still blocks a duplicate.
	if added, _ := s.Enqueue(ctx, "fetch", "fetch", "{}", PriorityFetch); added {
		t.Error("duplicate of a running job was added")
	}
	s.Done(ctx, j.ID)
	if added, _ := s.Enqueue(ctx, "fetch", "fetch", "{}", PriorityFetch); !added {
		t.Error("key not reusable after Done")
	}
	if n, _ := s.CountJobs(ctx, "build"); n != 1 {
		t.Errorf("build jobs = %d", n)
	}
}

func TestResetRunning(t *testing.T) {
	s := open(t)
	s.Enqueue(ctx, "build", "k", "{}", PriorityBuild)
	s.Claim(ctx)
	if _, err := s.Claim(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("running job claimed twice")
	}
	s.ResetRunning(ctx)
	if _, err := s.Claim(ctx); err != nil {
		t.Fatal("reset job not claimable")
	}
}

// A database from before secret logins keeps its data, and its passkey
// sessions end.
func TestMigrateFromPasskeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		"PRAGMA user_version = 1",
		"INSERT INTO sessions (hash, user_id, csrf, created_at, expires_at) VALUES ('old', 'ben', 'c', 0, 9999999999)",
		"INSERT INTO deliveries (id, received_at) VALUES ('d1', 0)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if seen, err := s.SeenDelivery("d1"); err != nil || !seen {
		t.Errorf("delivery lost: %v, %v", seen, err)
	}
	if sess, err := s.GetSession(ctx, "old"); err != nil || sess.SecretHash != "" {
		t.Errorf("old session = %+v, %v; want one with no secret hash", sess, err)
	}
}

func TestSessionsExpireAndUsersRemoved(t *testing.T) {
	s := open(t)
	s.CreateSession(ctx, Session{Hash: "old", UserID: "ben", CSRF: "c", ExpiresAt: 1})
	if _, err := s.GetSession(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Error("expired session returned")
	}
	s.CreateSession(ctx, Session{Hash: "a", UserID: "alice", CSRF: "c", ExpiresAt: now() + 100})
	s.CreateSession(ctx, Session{Hash: "b", UserID: "ben", CSRF: "c", ExpiresAt: now() + 100})
	if _, err := s.RemoveUsersExcept(ctx, []string{"ben"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Error("removed user's session survived")
	}
	if _, err := s.GetSession(ctx, "b"); err != nil {
		t.Error("kept user's session removed")
	}
}

func TestNoteCapAndAnchors(t *testing.T) {
	s := open(t)
	n := Note{UserID: "alice", Body: "hi", Origin: Anchor{SHA: "abc", File: "ch1.typ", Line: 3, Col: 1}, Selected: "x"}
	n1, err := s.CreateNote(ctx, n, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNote(ctx, n, 1); !errors.Is(err, ErrTooManyNotes) {
		t.Errorf("cap not enforced: %v", err)
	}
	s.PutAnchor(ctx, n1.ID, Anchor{SHA: "def", File: "ch1.typ", Line: 5, Col: 1})
	at, _ := s.NotesAt(ctx, "def", NoteOpen)
	if len(at) != 1 || at[0].At.Line != 5 || at[0].Origin.Line != 3 {
		t.Errorf("NotesAt = %+v", at)
	}
	if id, err := ParseNoteRef(n1.Ref()); err != nil || id != n1.ID {
		t.Errorf("ParseNoteRef(%q) = %d, %v", n1.Ref(), id, err)
	}
	for _, bad := range []string{"n", "n01", "x1", "n-1", "n1;"} {
		if _, err := ParseNoteRef(bad); err == nil {
			t.Errorf("ParseNoteRef(%q) accepted", bad)
		}
	}
}

func TestSnapshotIntegrity(t *testing.T) {
	s := open(t)
	s.SeenDelivery("d1")
	path := filepath.Join(t.TempDir(), "backup", "db.sqlite")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	snap, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if res, err := snap.IntegrityCheck(ctx); err != nil || res != "ok" {
		t.Errorf("integrity = %q, %v", res, err)
	}
	if seen, _ := snap.SeenDelivery("d1"); !seen {
		t.Error("snapshot lost a row")
	}
}

func TestResolveSHA(t *testing.T) {
	s := open(t)
	s.PutCommit(ctx, Commit{SHA: "abcdef0123456789abcdef0123456789abcdef01", Author: "a", Subject: "s"})
	s.PutCommit(ctx, Commit{SHA: "abcdff0123456789abcdef0123456789abcdef01", Author: "a", Subject: "s"})
	if sha, err := s.ResolveSHA(ctx, "abcdef0"); err != nil || sha[:7] != "abcdef0" {
		t.Errorf("ResolveSHA = %q, %v", sha, err)
	}
	if _, err := s.ResolveSHA(ctx, "abcd"); !errors.Is(err, ErrNotFound) {
		t.Error("ambiguous prefix resolved")
	}
}
