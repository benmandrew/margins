// Package gitx runs git against the service's bare mirror of the thesis
// repository. Every argument is passed as an array, never through a shell,
// and every caller-supplied name is validated before it gets here.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"git.benmandrew.com/me/thesis/internal/config"
)

type Mirror struct {
	Dir   string // the bare repository
	URL   string
	Token string // Gitea token, sent as an HTTP header, never in the URL
	Home  string // HOME for git, so no user config is read
	// AllowFile permits file:// and path remotes. Tests use it; the service
	// fetches over HTTP only.
	AllowFile bool

	mu sync.Mutex // serialises fetches and worktree changes
}

func (m *Mirror) env() []string {
	env := []string{
		"HOME=" + m.Home,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"LC_ALL=C",
	}
	if f := os.Getenv("SSL_CERT_FILE"); f != "" {
		env = append(env, "SSL_CERT_FILE="+f, "GIT_SSL_CAINFO="+f)
	}
	if m.Token != "" {
		// Passed through the environment so it never shows in a process
		// listing or in the repository's config file.
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: token "+m.Token)
	}
	return env
}

// git runs a git command in the mirror and returns stdout.
func (m *Mirror) git(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{
		"--git-dir=" + m.Dir,
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.allow=never",
		"-c", "protocol.http.allow=always",
		"-c", "protocol.https.allow=always",
	}
	if m.AllowFile {
		full = append(full, "-c", "protocol.file.allow=always")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = m.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.Args[0], e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Ensure creates the bare mirror if it does not exist yet.
func (m *Mirror) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if _, err := m.git(ctx, "init", "--bare", "--quiet"); err != nil {
		return err
	}
	for _, kv := range [][]string{
		{"remote.origin.url", m.URL},
		{"remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"},
		{"gc.auto", "0"},
	} {
		if _, err := m.git(ctx, append([]string{"config"}, kv...)...); err != nil {
			return err
		}
	}
	return nil
}

// Fetch updates every branch and tag from Gitea, removing refs deleted there.
func (m *Mirror) Fetch(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.git(ctx, "config", "remote.origin.url", m.URL); err != nil {
		return err
	}
	_, err := m.git(ctx, "fetch", "--quiet", "--prune", "--prune-tags", "--force", "origin")
	return err
}

type Ref struct {
	Name string // refs/heads/main, refs/tags/review-1
	SHA  string // the commit, with tags peeled
	Date int64  // tagger date for annotated tags, otherwise the commit date
}

// Refs lists branches and tags. Refs with names that fail validation are
// skipped, so nothing hostile reaches a path or an argument later.
func (m *Mirror) Refs(ctx context.Context) ([]Ref, error) {
	out, err := m.git(ctx, "for-each-ref",
		"--format=%(refname)%00%(objecttype)%00%(objectname)%00%(*objectname)%00%(taggerdate:unix)%00%(committerdate:unix)%00%(*committerdate:unix)",
		"refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 7 {
			continue
		}
		name, typ := f[0], f[1]
		short := strings.TrimPrefix(strings.TrimPrefix(name, "refs/heads/"), "refs/tags/")
		if !config.ValidRef(short) {
			continue
		}
		r := Ref{Name: name, SHA: f[2]}
		switch typ {
		case "commit":
			r.Date, _ = strconv.ParseInt(f[5], 10, 64)
		case "tag":
			r.SHA = f[3]
			r.Date, _ = strconv.ParseInt(f[4], 10, 64)
			if r.Date == 0 {
				r.Date, _ = strconv.ParseInt(f[6], 10, 64)
			}
		default:
			continue
		}
		if !config.SHA.MatchString(r.SHA) {
			// A tag of a tag or of a tree. Not something to build.
			continue
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// RevList returns commits reachable from to and not from any of exclude,
// newest first, at most max (0 means no limit).
func (m *Mirror) RevList(ctx context.Context, to string, exclude []string, max int) ([]string, error) {
	if err := checkSHA(to); err != nil {
		return nil, err
	}
	args := []string{"rev-list"}
	if max > 0 {
		args = append(args, "--max-count="+strconv.Itoa(max))
	}
	args = append(args, to)
	for _, e := range exclude {
		if err := checkSHA(e); err != nil {
			return nil, err
		}
		args = append(args, "^"+e)
	}
	out, err := m.git(ctx, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// IsAncestor reports whether a is an ancestor of b (or equal to it).
func (m *Mirror) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if err := checkSHA(a, b); err != nil {
		return false, err
	}
	_, err := m.git(ctx, "merge-base", "--is-ancestor", a, b)
	var ge *Error
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return err == nil, err
}

// HasCommit reports whether the mirror holds a commit.
func (m *Mirror) HasCommit(ctx context.Context, sha string) bool {
	if checkSHA(sha) != nil {
		return false
	}
	_, err := m.git(ctx, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

type CommitInfo struct {
	SHA         string
	Parents     []string
	Author      string
	Subject     string
	CommittedAt int64
}

func (m *Mirror) Commits(ctx context.Context, shas []string) ([]CommitInfo, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	if err := checkSHA(shas...); err != nil {
		return nil, err
	}
	args := append([]string{"log", "--no-walk=unsorted", "--format=%H%x00%P%x00%an%x00%s%x00%ct%x1e"}, shas...)
	out, err := m.git(ctx, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	var infos []CommitInfo
	for _, rec := range strings.Split(string(out), "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x00")
		if len(f) != 5 {
			continue
		}
		t, _ := strconv.ParseInt(f[4], 10, 64)
		infos = append(infos, CommitInfo{SHA: f[0], Parents: strings.Fields(f[1]), Author: f[2], Subject: f[3], CommittedAt: t})
	}
	return infos, nil
}

// ReadFile returns a file's content at a commit, and ok=false if the path does
// not exist there.
func (m *Mirror) ReadFile(ctx context.Context, sha, path string) (data []byte, ok bool, err error) {
	if err := checkSHA(sha); err != nil {
		return nil, false, err
	}
	// RelPath refuses dot-files, so the one the service reads is named.
	if !config.RelPath.MatchString(path) && path != config.BuildFile {
		return nil, false, fmt.Errorf("path %q", path)
	}
	out, err := m.git(ctx, "cat-file", "blob", sha+":"+path)
	if err != nil {
		if _, err2 := m.git(ctx, "cat-file", "-e", sha+":"+path); err2 != nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

// TypFiles lists the .typ files at a commit whose paths pass validation.
func (m *Mirror) TypFiles(ctx context.Context, sha string) ([]string, error) {
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, "ls-tree", "-r", "-z", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(p, ".typ") && config.RelPath.MatchString(p) {
			files = append(files, p)
		}
	}
	return files, nil
}

type Change struct {
	Status byte // 'A', 'M' or 'D'
	Path   string
}

// ChangedTyp lists the .typ files that differ between a and b. Renames are
// reported as a delete and an add.
func (m *Mirror) ChangedTyp(ctx context.Context, a, b string) ([]Change, error) {
	if err := checkSHA(a, b); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, "diff", "--no-renames", "--name-status", "-z", a, b, "--", "*.typ")
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var changes []Change
	for i := 0; i+1 < len(f); i += 2 {
		st, path := f[i], f[i+1]
		if st == "" || !config.RelPath.MatchString(path) {
			continue
		}
		changes = append(changes, Change{Status: st[0], Path: path})
	}
	return changes, nil
}

// Hunk is one -U0 diff hunk: OldLines lines from OldStart became NewLines
// lines from NewStart.
type Hunk struct {
	OldStart, OldLines, NewStart, NewLines int
}

// Hunks returns the zero-context hunks of one file between two commits.
func (m *Mirror) Hunks(ctx context.Context, a, b, path string) ([]Hunk, error) {
	if err := checkSHA(a, b); err != nil {
		return nil, err
	}
	if !config.RelPath.MatchString(path) {
		return nil, fmt.Errorf("path %q", path)
	}
	out, err := m.git(ctx, "diff", "--no-renames", "--no-color", "-U0", a, b, "--", path)
	if err != nil {
		return nil, err
	}
	var hunks []Hunk
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		h, ok := parseHunkHeader(line)
		if ok {
			hunks = append(hunks, h)
		}
	}
	return hunks, sc.Err()
}

func parseHunkHeader(line string) (Hunk, bool) {
	// @@ -12,3 +12,0 @@ optional section heading
	f := strings.Fields(line)
	if len(f) < 3 {
		return Hunk{}, false
	}
	oldStart, oldLines, ok1 := parseRange(strings.TrimPrefix(f[1], "-"))
	newStart, newLines, ok2 := parseRange(strings.TrimPrefix(f[2], "+"))
	return Hunk{oldStart, oldLines, newStart, newLines}, ok1 && ok2
}

func parseRange(s string) (start, n int, ok bool) {
	a, b, has := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	n = 1
	if has {
		if n, err = strconv.Atoi(b); err != nil {
			return 0, 0, false
		}
	}
	return start, n, true
}

// AddWorktree checks out a commit, detached, into dir. The caller must call
// RemoveWorktree on every exit path.
func (m *Mirror) AddWorktree(ctx context.Context, dir, sha string) error {
	if err := checkSHA(sha); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.git(ctx, "worktree", "add", "--detach", "--quiet", "--", dir, sha)
	return err
}

// RemoveWorktree deletes a worktree and its metadata. It uses a fresh context
// so cleanup still runs after a build's context was cancelled.
func (m *Mirror) RemoveWorktree(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.git(context.Background(), "worktree", "remove", "--force", "--", dir)
	if rmErr := os.RemoveAll(dir); err == nil {
		err = rmErr
	}
	m.git(context.Background(), "worktree", "prune")
	return err
}

// PruneWorktrees drops metadata for worktrees whose directories are gone,
// after a crash.
func (m *Mirror) PruneWorktrees(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.git(ctx, "worktree", "prune")
	return err
}

func checkSHA(shas ...string) error {
	for _, s := range shas {
		if !config.SHA.MatchString(s) {
			return fmt.Errorf("not a full SHA: %q", s)
		}
	}
	return nil
}
