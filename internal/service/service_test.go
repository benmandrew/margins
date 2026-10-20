package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.benmandrew.com/me/thesis/internal/config"
	"git.benmandrew.com/me/thesis/internal/store"
)

var ctx = context.Background()

// fixture is a service wired to a fresh copy of the fixture repository, with
// Typst served from the release archives the dev shell fetched.
type fixture struct {
	*Service
	repo string // the "Gitea" repository, a plain git repo
	shas []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	releases, target := os.Getenv("THESIS_TEST_TYPST_RELEASES"), os.Getenv("THESIS_TEST_TYPST_TARGET")
	if releases == "" {
		t.Skip("THESIS_TEST_TYPST_RELEASES is unset; run inside nix develop")
	}
	for _, tool := range []string{"git", "typdiff", "diff-pdf", "pdftotext"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; run inside nix develop", tool)
		}
	}
	repo := filepath.Join(t.TempDir(), "thesis")
	out, err := exec.Command("bash", "../../scripts/make-fixture.sh", repo).CombinedOutput()
	if err != nil {
		t.Fatalf("make-fixture: %v\n%s", err, out)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(releases)))
	t.Cleanup(srv.Close)

	hashes := map[string]string{}
	for _, v := range []string{"0.15.1", "0.14.2"} {
		data, err := os.ReadFile(filepath.Join(releases, "v"+v, "typst-"+target+".tar.xz"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		hashes[v+"/"+target] = hex.EncodeToString(sum[:])
	}

	data := t.TempDir()
	cfg := config.Config{
		DataDir: data,
		Origin:  "https://thesis.example",
		Repo:    config.Repo{FullName: "me/thesis", CloneURL: repo, DefaultBranch: "main"},
		Defaults: config.Build{
			Typst: "0.15.1", Entrypoints: []string{"main.typ"}, Branches: []string{"main"}, ReviewTags: "review-*",
		},
		Limits: config.Limits{
			BuildTimeout: 60 * time.Second, Workers: 1, MaxOutputBytes: 50 << 20, DiffQueue: 10,
			MaxAttempts: 3, BackfillDepth: 200, DiskBudget: 4 << 30, NoteMaxChars: 4000, NotesPerCommit: 500,
		},
		Timers:      config.Timers{SnapshotAt: "01:30"},
		TypstHashes: hashes,
		Users: []config.User{
			{ID: "ben", Name: "Ben", Role: config.RoleAuthor},
			{ID: "alice", Name: "Alice", Role: config.RoleSupervisor},
		},
		Location: time.UTC,
	}
	st, err := store.Open(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := New(cfg, st, config.Secrets{}, log)
	s.Mirror.AllowFile = true
	s.Typst.BaseURL = srv.URL
	s.Typst.Target = target
	s.Scratch = filepath.Join(t.TempDir(), "work")
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{Service: s, repo: repo}
	f.shas = f.revList(t)
	return f
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// revList returns the fixture's commits, oldest first: c1 .. c5.
func (f *fixture) revList(t *testing.T) []string {
	out, err := exec.Command("git", "-C", f.repo, "rev-list", "--reverse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func (f *fixture) idle(t *testing.T) {
	t.Helper()
	if err := f.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+f.repo+"/.home",
		"GIT_AUTHOR_NAME=Ben", "GIT_AUTHOR_EMAIL=ben@example.com",
		"GIT_COMMITTER_NAME=Ben", "GIT_COMMITTER_EMAIL=ben@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) build(t *testing.T, sha string) store.Build {
	t.Helper()
	b, err := f.Store.CurrentBuild(ctx, sha, "main.typ")
	if err != nil {
		t.Fatalf("build of %s: %v", sha[:7], err)
	}
	return b
}

func pdfText(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("pdftotext", "-layout", path, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext %s: %v", path, err)
	}
	return string(out)
}

func TestPipeline(t *testing.T) {
	f := newFixture(t)
	c1, c2, c3, c4, c5 := f.shas[0], f.shas[1], f.shas[2], f.shas[3], f.shas[4]

	// A first fetch sees a new branch: only its tip and the tags build.
	if err := f.EnqueueFetch(ctx); err != nil {
		t.Fatal(err)
	}
	f.idle(t)
	if _, err := f.Store.CurrentBuild(ctx, c3, "main.typ"); err == nil {
		t.Error("a new branch built more than its tip")
	}
	if b := f.build(t, c5); b.Status != store.OK {
		t.Fatalf("tip: %s", b.Status)
	}

	// Backfill builds the rest of the history.
	f.enqueue(ctx, jobBackfill, jobBackfill, struct{}{}, store.PriorityHistory)
	f.idle(t)
	want := map[string]store.Status{c1: store.OK, c2: store.OK, c3: store.OK, c4: store.Failed, c5: store.OK}
	for sha, st := range want {
		if b := f.build(t, sha); b.Status != st {
			t.Errorf("%s: status %s, want %s", sha[:7], b.Status, st)
		}
	}
	if b := f.build(t, c2); b.Typst != "0.14.2" {
		t.Errorf("c2 built with Typst %s, want the pinned 0.14.2", b.Typst)
	}
	log, _ := os.ReadFile(f.BuildLogPath(c4, "main.typ"))
	if !bytes.Contains(log, []byte("this-function-does-not-exist")) {
		t.Errorf("failed build log lacks the error:\n%s", log)
	}

	// The failed build is not retried.
	f.enqueue(ctx, jobRetry, jobRetry, struct{}{}, store.PriorityHistory)
	f.idle(t)
	if b := f.build(t, c4); b.Attempts != 1 {
		t.Errorf("failed build ran %d times", b.Attempts)
	}

	// Rebuilding gives the same bytes.
	before, _ := os.ReadFile(f.BuildPath(c3, "main.typ"))
	b3 := f.build(t, c3)
	f.Store.MarkBuildPruned(ctx, b3.ID)
	os.Remove(f.BuildPath(c3, "main.typ"))
	if _, err := f.EnsureBuilt(ctx, c3, "main.typ"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.BuildPath(c3, "main.typ"))
	if len(before) == 0 || !bytes.Equal(before, after) {
		t.Error("rebuild is not byte-identical")
	}

	// The second review tag gave an eager typdiff diff.
	d, err := f.Store.FindDiff(ctx, c1, c5, "main.typ")
	if err != nil {
		t.Fatalf("eager review diff: %v", err)
	}
	if d.Status != store.OK || d.Method != MethodTypdiff {
		dlog, _ := os.ReadFile(f.DiffLogPath(c1, c5, "main.typ"))
		t.Fatalf("review diff: %s by %q\n%s", d.Status, d.Method, dlog)
	}
	text := pdfText(t, f.DiffPath(c1, c5, "main.typ", MethodTypdiff))
	for _, s := range []string{"Results", "mainly", "slightly larger"} {
		if !strings.Contains(text, s) {
			t.Errorf("markup diff lacks %q", s)
		}
	}
	if _, err := os.Stat(f.TextDiffPath(c1, c5, "main.typ")); err != nil {
		t.Errorf("no text diff: %v", err)
	}
}

func TestDiffFallsBackToDiffPDF(t *testing.T) {
	f := newFixture(t)
	f.Tools.Typdiff = "false" // every typdiff run fails
	f.EnqueueFetch(ctx)
	f.idle(t)
	c1, c5 := f.shas[0], f.shas[4]
	d, err := f.Store.FindDiff(ctx, c1, c5, "main.typ")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.OK || d.Method != MethodDiffPDF {
		t.Errorf("diff: %s by %q, want ok by diff-pdf", d.Status, d.Method)
	}
}

func TestForcePushAndDeletion(t *testing.T) {
	f := newFixture(t)
	f.EnqueueFetch(ctx)
	f.idle(t)
	c4, c5 := f.shas[3], f.shas[4]
	// Rewrite main to drop the last commit, and push a new branch.
	f.git(t, "reset", "--hard", c4)
	f.git(t, "branch", "draft")
	f.git(t, "tag", "-d", "review-2")
	f.EnqueueFetch(ctx)
	f.idle(t)
	c, err := f.Store.GetCommit(ctx, c5)
	if err != nil || !c.Unreachable {
		t.Errorf("force-pushed-away commit: %+v, %v", c, err)
	}
	if r, err := f.Store.GetRef(ctx, "refs/tags/review-2"); err != nil || !r.Gone {
		t.Errorf("deleted tag: %+v, %v", r, err)
	}
	if b := f.build(t, c5); b.Status != store.OK {
		t.Error("builds of a gone commit were not kept")
	}
	if b := f.build(t, c4); b.Status != store.Failed {
		t.Errorf("new tip c4: %s", b.Status)
	}
}

func TestNotesAnnotatedAndCarried(t *testing.T) {
	f := newFixture(t)
	f.EnqueueFetch(ctx)
	f.idle(t)
	c1, c3, c5 := f.shas[0], f.shas[2], f.shas[4]
	if b := f.build(t, c1); b.Status != store.OK {
		t.Fatal("c1 did not build")
	}

	hostile := `#read("/etc/passwd") #while true {} ] " \ [`
	add := func(line, col int, selected, body string) store.Note {
		t.Helper()
		n, err := f.Store.CreateNote(ctx, store.Note{
			UserID: "alice", Body: body, Selected: selected,
			Origin: store.Anchor{SHA: c1, File: "ch1.typ", Line: line, Col: col},
		}, 500)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	prose := add(3, 1, "Programs are written", hostile)
	maths := add(6, 3, "integral_0^1 x^2", "Check this integral.")
	table := add(11, 31, "Output", "Is this value right?")
	raw := add(16, 1, "print", "Raw block note.")

	a, ok, err := f.RequestAnnotated(ctx, c1, "main.typ", true)
	if err != nil || !ok {
		t.Fatalf("RequestAnnotated: %v, %v", ok, err)
	}
	f.idle(t)
	a, _ = f.Store.GetAnnotated(ctx, a.ID)
	alog, _ := os.ReadFile(f.AnnotatedLogPath(c1, "main.typ", a.NotesHash))
	if a.Status != store.OK {
		t.Fatalf("annotated build: %s\n%s", a.Status, alog)
	}
	var placements map[string]string
	json.Unmarshal([]byte(a.Placements), &placements)
	t.Logf("placements: %v\n%s", placements, alog)
	if placements[prose.Ref()] != PlacedAnchor {
		t.Errorf("prose note placed at %q", placements[prose.Ref()])
	}
	if placements[maths.Ref()] == PlacedAnchor || placements[raw.Ref()] == PlacedAnchor {
		t.Errorf("a note inside maths or raw text was placed at its anchor: %v", placements)
	}
	if placements[table.Ref()] == "" {
		t.Errorf("table note has no placement")
	}
	text := pdfText(t, f.AnnotatedPath(c1, "main.typ", a.NotesHash))
	flat := strings.Join(strings.Fields(text), "")
	if !strings.Contains(flat, strings.Join(strings.Fields(hostile), "")) {
		t.Errorf("hostile note text is not verbatim in the PDF:\n%s", text)
	}

	// Carry to c3, which rewrites the lines around the prose and table notes
	// and the integral: the prose and table notes move, the maths note is
	// outdated.
	if err := f.carryNotes(ctx, c3); err != nil {
		t.Fatal(err)
	}
	at3, _ := f.Store.NotesAt(ctx, c3, store.NoteOpen)
	moved := map[int64]store.Anchor{}
	for _, n := range at3 {
		moved[n.ID] = n.At
	}
	if _, ok := moved[prose.ID]; !ok {
		t.Error("prose note did not move to c3")
	}
	if n, _ := f.Store.GetNote(ctx, maths.ID); n.State != store.NoteOutdated {
		t.Errorf("maths note state %s, want outdated", n.State)
	}
	if err := f.carryNotes(ctx, c5); err != nil {
		t.Fatal(err)
	}
	at5, _ := f.Store.NotesAt(ctx, c5, store.NoteOpen)
	if len(at5) != 3 {
		t.Errorf("notes at c5: %d, want 3", len(at5))
	}
	// The outdated note still shows on the commit it was written on.
	notes1, _ := f.AnnotationNotes(ctx, c1)
	if len(notes1) != 4 {
		t.Errorf("notes on c1: %d, want 4", len(notes1))
	}
}

func TestPrune(t *testing.T) {
	f := newFixture(t)
	f.EnqueueFetch(ctx)
	f.idle(t)
	f.enqueue(ctx, jobBackfill, jobBackfill, struct{}{}, store.PriorityHistory)
	f.idle(t)
	used, _ := f.DiskUsage()
	f.Cfg.Limits.DiskBudget = used - 1
	if err := f.prune(ctx); err != nil {
		t.Fatal(err)
	}
	c1, c2, c5 := f.shas[0], f.shas[1], f.shas[4]
	if b := f.build(t, c2); !b.Pruned {
		t.Error("the oldest unprotected build was kept")
	}
	for _, sha := range []string{c1, c5} {
		if b := f.build(t, sha); b.Pruned {
			t.Errorf("protected build %s was pruned", sha[:7])
		}
	}
}

func TestSnapshot(t *testing.T) {
	f := newFixture(t)
	if err := f.snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.SnapshotPath()); err != nil {
		t.Fatal(err)
	}
}

