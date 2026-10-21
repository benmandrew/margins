package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/service"
	"git.benmandrew.com/me/margins/internal/store"
)

var ctx = context.Background()

const origin = "https://thesis.example"

var secrets = map[string]string{"ben": "ben-secret", "alice": "alice-secret", "bob": "bob-secret"}

type fixture struct {
	svc  *service.Service
	web  *Server
	srv  *httptest.Server
	repo string
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
	if out, err := exec.Command("bash", "../../scripts/make-fixture.sh", repo).CombinedOutput(); err != nil {
		t.Fatalf("make-fixture: %v\n%s", err, out)
	}
	rel := httptest.NewServer(http.FileServer(http.Dir(releases)))
	t.Cleanup(rel.Close)
	hashes := map[string]string{}
	for _, v := range []string{"0.15.1", "0.14.2"} {
		data, err := os.ReadFile(filepath.Join(releases, "v"+v, "typst-"+target+".tar.xz"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		hashes[v+"/"+target] = hex.EncodeToString(sum[:])
	}
	var users []config.User
	for _, u := range []struct {
		id, name string
		role     config.Role
	}{{"ben", "Ben", config.RoleAuthor}, {"alice", "Alice", config.RoleSupervisor}, {"bob", "Bob", config.RoleSupervisor}} {
		users = append(users, config.User{ID: u.id, Name: u.name, Role: u.role, SecretHash: hashToken(secrets[u.id])})
	}
	data := t.TempDir()
	cfg := config.Config{
		DataDir:        data,
		Origin:         origin,
		ClientIPHeader: "X-Real-IP",
		Repo:           config.Repo{FullName: "me/thesis", CloneURL: repo, DefaultBranch: "main"},
		Defaults: config.Build{
			Typst: "0.15.1", Entrypoints: []string{"main.typ"}, Branches: []string{"main"}, ReviewTags: "review-*",
		},
		Limits: config.Limits{
			BuildTimeout: 60 * time.Second, Workers: 1, MaxOutputBytes: 50 << 20, DiffQueue: 10,
			MaxAttempts: 3, BackfillDepth: 200, DiskBudget: 4 << 30, NoteMaxChars: 4000, NotesPerCommit: 500,
		},
		Timers:      config.Timers{SnapshotAt: "01:30"},
		TypstHashes: hashes,
		Users:       users,
		Location:    time.UTC,
	}
	st, err := store.Open(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	sec := config.Secrets{WebhookSecret: "hook"}
	svc := service.New(cfg, st, sec, log)
	svc.Mirror.AllowFile = true
	svc.Typst.BaseURL = rel.URL
	svc.Typst.Target = target
	svc.Scratch = filepath.Join(t.TempDir(), "work")
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := New(svc, sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(s.Handler())
	t.Cleanup(srv.Close)
	f := &fixture{svc: svc, web: s, srv: srv, repo: repo}
	out, err := exec.Command("git", "-C", repo, "rev-list", "--reverse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	f.shas = strings.Fields(string(out))
	if err := svc.EnqueueFetch(ctx); err != nil {
		t.Fatal(err)
	}
	f.idle(t)
	// The timers do not run here; queue the backfill that records c2 to c4.
	if _, err := st.Enqueue(ctx, "backfill", "backfill", "{}", store.PriorityHistory); err != nil {
		t.Fatal(err)
	}
	f.idle(t)
	return f
}

func (f *fixture) idle(t *testing.T) {
	t.Helper()
	if err := f.svc.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// --- A browser ---

type browser struct {
	t  *testing.T
	f  *fixture
	c  *http.Client
	ip string
}

func (f *fixture) browser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	shared := *f.srv.Client()
	c := &shared
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &browser{t: t, f: f, c: c, ip: "192.0.2.1"}
}

func (b *browser) do(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	b.t.Helper()
	req, err := http.NewRequest(method, b.f.srv.URL+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("X-Real-IP", b.ip)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	return res
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	res := b.do("GET", path, nil, nil)
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// postForm sends a form with the session's CSRF token, read from a page.
func (b *browser) postForm(path string, form url.Values) *http.Response {
	b.t.Helper()
	_, page := b.get("/notes")
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatal("no CSRF token on /notes")
	}
	form.Set("csrf", m[1])
	res := b.do("POST", path, strings.NewReader(form.Encode()), map[string]string{"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"})
	res.Body.Close()
	return res
}

// login submits the login form, as the page or a login link does.
func (b *browser) login(secret, next string) *http.Response {
	b.t.Helper()
	form := url.Values{"secret": {secret}, "next": {next}}
	res := b.do("POST", "/login", strings.NewReader(form.Encode()), map[string]string{"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"})
	res.Body.Close()
	return res
}

func (f *fixture) loggedIn(t *testing.T, id string) *browser {
	t.Helper()
	b := f.browser(t)
	if res := b.login(secrets[id], "/"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s: %s", id, res.Status)
	}
	return b
}

// --- Tests ---

func TestAuth(t *testing.T) {
	f := newFixture(t)
	anon := f.browser(t)

	res, _ := anon.get("/healthz")
	if res.StatusCode != http.StatusOK {
		t.Errorf("healthz: %s", res.Status)
	}
	for _, p := range []string{"/", "/c/" + f.shas[0] + ".pdf", "/no/such/page"} {
		res, _ := anon.get(p)
		if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), "/login?next=") {
			t.Errorf("anonymous GET %s: %s to %q", p, res.Status, res.Header.Get("Location"))
		}
		if res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("anonymous GET %s: Cache-Control %q", p, res.Header.Get("Cache-Control"))
		}
	}
	if res := anon.do("POST", "/notes", nil, map[string]string{"Origin": origin}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST: %s", res.Status)
	}
	form := url.Values{"secret": {secrets["alice"]}}.Encode()
	if res := anon.do("POST", "/login", strings.NewReader(form), map[string]string{"Origin": "https://evil.example", "Content-Type": "application/x-www-form-urlencoded"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin login: %s", res.Status)
	}

	// A wrong or empty secret fails.
	for _, secret := range []string{"wrong", ""} {
		if res := anon.login(secret, "/"); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("secret %q: %s", secret, res.Status)
		}
	}
	if res, _ := anon.get("/"); res.StatusCode != http.StatusFound {
		t.Errorf("timeline after a failed login: %s", res.Status)
	}

	// The secret alone names the user, and next is followed only on this site.
	alice := f.browser(t)
	res = alice.login(secrets["alice"], "/notes")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/notes" {
		t.Fatalf("login: %s to %q", res.Status, res.Header.Get("Location"))
	}
	res, body := alice.get("/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Alice") {
		t.Errorf("timeline after login: %s", res.Status)
	}
	if res := f.browser(t).login(secrets["bob"], "//evil.example"); res.Header.Get("Location") != "/" {
		t.Errorf("offsite next: redirected to %q", res.Header.Get("Location"))
	}
	if res, _ := alice.get("/login"); res.StatusCode != http.StatusFound {
		t.Errorf("login page when logged in: %s", res.Status)
	}

	// A POST without the CSRF token, then logout.
	if res := alice.do("POST", "/notes", strings.NewReader("sha=x"), map[string]string{"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF: %s", res.Status)
	}
	if res := alice.postForm("/logout", url.Values{}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("logout: %s", res.Status)
	}
	if res, _ := alice.get("/"); res.StatusCode != http.StatusFound {
		t.Errorf("after logout: %s", res.Status)
	}
}

func TestNewSecretEndsSessions(t *testing.T) {
	f := newFixture(t)
	bob := f.loggedIn(t, "bob")
	for i, u := range f.web.Cfg.Users {
		if u.ID == "bob" {
			f.web.Cfg.Users[i].SecretHash = hashToken("bob-new-secret")
		}
	}
	if res, _ := bob.get("/"); res.StatusCode != http.StatusFound {
		t.Errorf("session after a new secret: %s", res.Status)
	}
	if res := f.browser(t).login(secrets["bob"], "/"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old secret: %s", res.Status)
	}
	if res := f.browser(t).login("bob-new-secret", "/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("new secret: %s", res.Status)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	b := f.browser(t)
	var last *http.Response
	for range 25 {
		last = b.login("wrong", "/")
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Errorf("25th login: %s", last.Status)
	}
	// Another address is not limited.
	b.ip = "192.0.2.2"
	if res := b.login(secrets["ben"], "/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("other address: %s", res.Status)
	}
}

func TestPDFURLs(t *testing.T) {
	f := newFixture(t)
	b := f.loggedIn(t, "alice")
	c1, c5 := f.shas[0], f.shas[4]

	res, _ := b.get("/c/" + c1[:7] + ".pdf")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/c/"+c1+".pdf" || res.Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("short SHA: %s to %q, %q", res.Status, res.Header.Get("Location"), res.Header.Get("Cache-Control"))
	}
	res, _ = b.get("/c/review-1.pdf")
	if res.Header.Get("Location") != "/c/"+c1+".pdf" {
		t.Errorf("tag: %q", res.Header.Get("Location"))
	}
	res, body := b.get("/c/" + c1 + ".pdf")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(body, "%PDF") {
		t.Fatalf("full SHA: %s %q", res.Status, res.Header.Get("Content-Type"))
	}
	if cc := res.Header.Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Errorf("full-SHA Cache-Control %q", cc)
	}
	if csp := res.Header.Get("Content-Security-Policy"); csp != "frame-ancestors 'self'" {
		t.Errorf("PDF CSP %q", csp)
	}
	res, _ = b.get("/latest.pdf")
	if res.Header.Get("Location") != "/c/"+c5+".pdf" {
		t.Errorf("latest: %q", res.Header.Get("Location"))
	}
	res, _ = b.get("/review/latest.pdf")
	if res.Header.Get("Location") != "/c/"+c5+".pdf" {
		t.Errorf("review latest: %q", res.Header.Get("Location"))
	}
	res, _ = b.get("/review/changes.pdf")
	if res.Header.Get("Location") != "/diff/"+c1+".."+c5+".pdf" {
		t.Errorf("review changes: %q", res.Header.Get("Location"))
	}
	// The review-tag diff was built eagerly.
	res, body = b.get("/diff/" + c1 + ".." + c5 + ".pdf")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "%PDF") {
		t.Errorf("review diff: %s", res.Status)
	}
	// Unknown commits are refused; a new pair is queued, then served.
	if res, _ := b.get("/diff/" + strings.Repeat("a", 40) + ".." + c5 + ".pdf"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown SHA diff: %s", res.Status)
	}
	pair := "/diff/" + f.shas[1] + ".." + f.shas[2] + ".pdf"
	res = b.do("GET", pair, nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
	res.Body.Close()
	if n, _ := f.svc.Store.CountJobs(ctx, "diff"); n != 0 {
		t.Errorf("a cross-site link queued %d diffs", n)
	}
	res, body = b.get(pair)
	if res.StatusCode != http.StatusAccepted || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("new diff: %s", res.Status)
	}
	f.idle(t)
	if res, _ := b.get(pair); res.StatusCode != http.StatusOK {
		t.Errorf("new diff after the queue ran: %s", res.Status)
	}
	// The failing commit shows its log.
	res, body = b.get("/c/" + f.shas[3] + ".pdf")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "The build failed") {
		t.Errorf("failed build: %s", res.Status)
	}
	// Pages render.
	for _, p := range []string{"/", "/c/" + c5, "/compare", "/compare?a=review-1&b=review-2", "/source/" + c5, "/notes"} {
		res, body := b.get(p)
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusFound {
			t.Errorf("GET %s: %s\n%s", p, res.Status, body)
		}
		if res.StatusCode == http.StatusOK && res.Header.Get("Cache-Control") != "private, no-cache" {
			t.Errorf("GET %s: Cache-Control %q", p, res.Header.Get("Cache-Control"))
		}
	}

	// A full queue answers 503.
	for i := range 10 {
		f.svc.Store.Enqueue(ctx, "diff", "dummy"+string(rune('a'+i)), "{}", 0)
	}
	res, _ = b.get("/diff/" + f.shas[0] + ".." + f.shas[2] + ".pdf")
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Errorf("full queue: %s", res.Status)
	}
}

func TestNotes(t *testing.T) {
	f := newFixture(t)
	alice := f.loggedIn(t, "alice")
	bob := f.loggedIn(t, "bob")
	ben := f.loggedIn(t, "ben")
	c1 := f.shas[0]

	note := func(b *browser, line, col int, selected, body string) *http.Response {
		return b.postForm("/notes", url.Values{
			"sha": {c1}, "file": {"ch1.typ"}, "line": {itoa(line)}, "col": {itoa(col)},
			"selected": {selected}, "body": {body},
		})
	}
	res := note(alice, 3, 1, "Wrong text", "x")
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Errorf("a selection that does not match: %q", res.Header.Get("Location"))
	}
	res = note(alice, 3, 1, "Programs", "Cite this.")
	if res.StatusCode != http.StatusSeeOther || strings.Contains(res.Header.Get("Location"), "error=") {
		t.Fatalf("create: %s %q", res.Status, res.Header.Get("Location"))
	}
	notes, _ := f.svc.Store.Notes(ctx, store.NoteOpen, "", "")
	if len(notes) != 1 {
		t.Fatalf("%d notes", len(notes))
	}
	id := itoa(int(notes[0].ID))

	if res := bob.postForm("/notes/"+id+"/edit", url.Values{"body": {"mine now"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("another supervisor's edit: %s", res.Status)
	}
	if res := bob.postForm("/notes/"+id+"/delete", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Errorf("another supervisor's delete: %s", res.Status)
	}
	if res := alice.postForm("/notes/"+id+"/resolve", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Errorf("supervisor resolve: %s", res.Status)
	}
	if res := alice.postForm("/notes/"+id+"/edit", url.Values{"body": {"Cite Knuth here."}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("own edit: %s", res.Status)
	}
	_, body := ben.get("/notes")
	if !strings.Contains(body, "Cite Knuth here.") || !strings.Contains(body, "/resolve") {
		t.Errorf("notes page for the author is missing the note or the resolve button")
	}

	// The annotated PDF builds on request.
	res, _ = alice.get("/c/" + c1 + ".notes.pdf")
	if res.StatusCode != http.StatusAccepted {
		t.Errorf("annotated before build: %s", res.Status)
	}
	f.idle(t)
	res, pdf := alice.get("/c/" + c1 + ".notes.pdf")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(pdf, "%PDF") || res.Header.Get("Cache-Control") != "private, no-cache" {
		t.Errorf("annotated: %s %q", res.Status, res.Header.Get("Cache-Control"))
	}

	if _, body := ben.get("/notes"); !strings.Contains(body, "placed anchor") {
		t.Errorf("notes page does not show the placement")
	}
	if res := ben.postForm("/notes/"+id+"/resolve", url.Values{}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("author resolve: %s", res.Status)
	}
	n, _ := f.svc.Store.GetNote(ctx, notes[0].ID)
	if n.State != store.NoteResolved || n.ResolvedBy != "ben" {
		t.Errorf("after resolve: %s by %q", n.State, n.ResolvedBy)
	}
	if res := alice.postForm("/notes/"+id+"/edit", url.Values{"body": {"late"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("edit of a resolved note: %s", res.Status)
	}
	// With no open notes the annotated URL falls back to the plain PDF.
	res, _ = alice.get("/c/" + c1 + ".notes.pdf")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/c/"+c1+".pdf" {
		t.Errorf("annotated with no notes: %s %q", res.Status, res.Header.Get("Location"))
	}
}

func TestLineColOffset(t *testing.T) {
	src := []byte("ab\nçd\n")
	for _, tt := range []struct {
		line, col, off int
		ok             bool
	}{
		{1, 1, 0, true}, {1, 3, 2, true}, {1, 4, 0, false},
		{2, 1, 3, true}, {2, 2, 0, false}, {2, 3, 5, true}, {3, 1, 7, true}, {4, 1, 0, false},
	} {
		off, ok := lineColOffsetBytes(src, tt.line, tt.col)
		if ok != tt.ok || ok && off != tt.off {
			t.Errorf("lineColOffsetBytes(%d, %d) = %d, %v", tt.line, tt.col, off, ok)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestWebhookNeedsSignatureNotLogin(t *testing.T) {
	f := newFixture(t)
	b := f.browser(t)
	res := b.do("POST", "/hooks/push", strings.NewReader(`{}`), map[string]string{
		"Content-Type": "application/json", "X-Gitea-Event": "push", "X-Gitea-Delivery": "d1",
	})
	res.Body.Close()
	if res.StatusCode == http.StatusFound || res.StatusCode == http.StatusOK || res.StatusCode == http.StatusAccepted {
		t.Errorf("unsigned webhook: %s", res.Status)
	}
}
