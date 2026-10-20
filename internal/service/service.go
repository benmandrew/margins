// Package service is the build-and-review pipeline: it turns pushes into
// fetches, builds, diffs, annotated builds and carried-forward notes, all
// through one job queue in the database, and runs the maintenance timers.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.benmandrew.com/me/thesis/internal/config"
	"git.benmandrew.com/me/thesis/internal/gitx"
	"git.benmandrew.com/me/thesis/internal/store"
	"git.benmandrew.com/me/thesis/internal/typst"
)

// Tools names the external programs. Each is looked up on PATH unless set.
type Tools struct {
	Typdiff   string
	DiffPDF   string
	PDFToText string
	Git       string
}

func (t *Tools) fill() {
	if t.Typdiff == "" {
		t.Typdiff = "typdiff"
	}
	if t.DiffPDF == "" {
		t.DiffPDF = "diff-pdf"
	}
	if t.PDFToText == "" {
		t.PDFToText = "pdftotext"
	}
	if t.Git == "" {
		t.Git = "git"
	}
}

type Service struct {
	Cfg    config.Config
	Store  *store.Store
	Mirror *gitx.Mirror
	Typst  *typst.Manager
	Tools  Tools
	Log    *slog.Logger

	// Scratch holds worktrees. In the container it is a tmpfs.
	Scratch string

	wake chan struct{}

	cfgMu    sync.Mutex
	cfgCache map[string]buildConfig

	activeMu sync.Mutex
	active   map[string]bool // scratch directories in use
}

// buildConfig is a commit's parsed .typst-review.toml, or the error that
// stopped it parsing.
type buildConfig struct {
	config.Build
	Err error
}

func New(cfg config.Config, st *store.Store, secrets config.Secrets, log *slog.Logger) *Service {
	s := &Service{
		Cfg:   cfg,
		Store: st,
		Log:   log,
		Mirror: &gitx.Mirror{
			Dir:   filepath.Join(cfg.DataDir, "mirror.git"),
			URL:   cfg.Repo.CloneURL,
			Token: secrets.GiteaToken,
			Home:  filepath.Join(cfg.DataDir, "home"),
		},
		Typst: &typst.Manager{
			Dir:    filepath.Join(cfg.DataDir, "typst"),
			Target: typst.HostTarget(),
			Hashes: cfg.TypstHashes,
		},
		Scratch: filepath.Join(os.TempDir(), "thesis-work"),
	}
	return s
}

func (s *Service) init() {
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	if s.cfgCache == nil {
		s.cfgCache = map[string]buildConfig{}
	}
	if s.active == nil {
		s.active = map[string]bool{}
	}
	s.Tools.fill()
}

// Paths under the data directory.
func (s *Service) home() string     { return filepath.Join(s.Cfg.DataDir, "home") }
func (s *Service) packages() string { return filepath.Join(s.Cfg.DataDir, "packages") }

// EntryName is an entrypoint as it appears in URLs: its path without ".typ".
func EntryName(entry string) string { return strings.TrimSuffix(entry, ".typ") }

// fileName is an entrypoint as a single path component. '%' cannot appear in
// an entrypoint, so the mapping is one-to-one.
func fileName(entry string) string { return strings.ReplaceAll(EntryName(entry), "/", "%2F") }

func (s *Service) BuildPath(sha, entry string) string {
	return filepath.Join(s.Cfg.DataDir, "builds", sha, fileName(entry)+".pdf")
}

func (s *Service) BuildLogPath(sha, entry string) string {
	return filepath.Join(s.Cfg.DataDir, "builds", sha, fileName(entry)+".log")
}

func (s *Service) diffDir(a, b string) string {
	return filepath.Join(s.Cfg.DataDir, "diffs", a+".."+b)
}

func (s *Service) DiffPath(a, b, entry, method string) string {
	return filepath.Join(s.diffDir(a, b), fileName(entry)+"."+method+".pdf")
}

func (s *Service) DiffLogPath(a, b, entry string) string {
	return filepath.Join(s.diffDir(a, b), fileName(entry)+".log")
}

func (s *Service) TextDiffPath(a, b, entry string) string {
	return filepath.Join(s.diffDir(a, b), fileName(entry)+".txt.diff")
}

func (s *Service) AnnotatedPath(sha, entry, hash string) string {
	return filepath.Join(s.Cfg.DataDir, "annotated", sha, fileName(entry)+"."+hash+".pdf")
}

func (s *Service) AnnotatedLogPath(sha, entry, hash string) string {
	return filepath.Join(s.Cfg.DataDir, "annotated", sha, fileName(entry)+"."+hash+".log")
}

func (s *Service) SnapshotPath() string {
	return filepath.Join(s.Cfg.DataDir, "backup", "db.sqlite")
}

// Start prepares the data directory and the mirror after a boot or a crash.
func (s *Service) Start(ctx context.Context) error {
	s.init()
	for _, d := range []string{s.home(), s.packages(), s.Typst.Dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// Worktrees from a previous run are dead; their builds are re-queued.
	os.RemoveAll(s.Scratch)
	if err := os.MkdirAll(s.Scratch, 0o700); err != nil {
		return err
	}
	if err := s.Mirror.Ensure(ctx); err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	if err := s.Mirror.PruneWorktrees(ctx); err != nil {
		return err
	}
	if err := s.Store.ResetRunning(ctx); err != nil {
		return err
	}
	if err := s.Store.ResetInterrupted(ctx); err != nil {
		return err
	}
	keep := make([]string, len(s.Cfg.Users))
	for i, u := range s.Cfg.Users {
		keep[i] = u.ID
	}
	n, err := s.Store.RemoveUsersExcept(ctx, keep)
	if err != nil {
		return err
	}
	if n > 0 {
		s.Log.Info("removed passkeys and sessions of users no longer in the config", "rows", n)
	}
	return s.Store.DeleteExpiredSessions(ctx)
}

// Job kinds and their payloads.
const (
	jobFetch    = "fetch"
	jobBuild    = "build"
	jobDiff     = "diff"
	jobAnnotate = "annotate"
	jobCarry    = "carry"
	jobBackfill = "backfill"
	jobRetry    = "retry"
	jobReview   = "review-diffs"
	jobPrune    = "prune"
	jobSnapshot = "snapshot"
)

type shaPayload struct {
	SHA string `json:"sha"`
}

type idPayload struct {
	ID int64 `json:"id"`
}

func (s *Service) enqueue(ctx context.Context, kind, key string, payload any, priority int) error {
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	added, err := s.Store.Enqueue(ctx, kind, key, string(p), priority)
	if err != nil {
		return err
	}
	if added {
		s.Wake()
	}
	return nil
}

// Wake tells an idle worker to look for a job.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// EnqueueFetch is what a verified push does.
func (s *Service) EnqueueFetch(ctx context.Context) error {
	return s.enqueue(ctx, jobFetch, jobFetch, struct{}{}, store.PriorityFetch)
}

func (s *Service) enqueueBuild(ctx context.Context, sha string, priority int) error {
	return s.enqueue(ctx, jobBuild, "build:"+sha, shaPayload{sha}, priority)
}

// Run starts the workers and the timers, and returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.init()
	var wg sync.WaitGroup
	for range s.Cfg.Limits.Workers {
		wg.Go(func() { s.worker(ctx) })
	}
	wg.Go(func() { s.timers(ctx) })
	wg.Wait()
}

func (s *Service) worker(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := s.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			s.Log.Error("job queue", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-time.After(30 * time.Second):
		}
	}
}

// RunOne claims and runs one job. It reports false when the queue is empty.
func (s *Service) RunOne(ctx context.Context) (bool, error) {
	s.init()
	job, err := s.Store.Claim(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	start := time.Now()
	err = s.dispatch(ctx, job)
	if err != nil {
		s.Log.Error("job failed", "kind", job.Kind, "key", job.Key, "err", err)
	} else {
		s.Log.Debug("job done", "kind", job.Kind, "key", job.Key, "took", time.Since(start).Round(time.Millisecond))
	}
	// The job is done even when it failed: the outcome is in its build, diff
	// or annotated row, and the Retry timer decides whether to run it again.
	// A cancelled context leaves the job running, so ResetRunning picks it up
	// at the next boot.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, s.Store.Done(context.WithoutCancel(ctx), job.ID)
}

// RunUntilIdle runs jobs until the queue is empty. Tests use it in place of
// the worker pool.
func (s *Service) RunUntilIdle(ctx context.Context) error {
	for {
		ran, err := s.RunOne(ctx)
		if err != nil {
			return err
		}
		if !ran {
			return nil
		}
	}
}

func (s *Service) dispatch(ctx context.Context, job store.Job) error {
	switch job.Kind {
	case jobFetch:
		return s.fetch(ctx)
	case jobBuild:
		var p shaPayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			return err
		}
		return s.buildCommit(ctx, p.SHA)
	case jobDiff:
		var p idPayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			return err
		}
		return s.runDiff(ctx, p.ID)
	case jobAnnotate:
		var p idPayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			return err
		}
		return s.runAnnotated(ctx, p.ID)
	case jobCarry:
		var p shaPayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			return err
		}
		return s.carryNotes(ctx, p.SHA)
	case jobBackfill:
		return s.backfill(ctx)
	case jobRetry:
		return s.retry(ctx)
	case jobReview:
		return s.reviewDiffs(ctx)
	case jobPrune:
		return s.prune(ctx)
	case jobSnapshot:
		return s.snapshot(ctx)
	}
	return fmt.Errorf("unknown job kind %q", job.Kind)
}

// BuildConfig returns a commit's build configuration. It is a pure function
// of the commit, so it is cached.
func (s *Service) BuildConfig(ctx context.Context, sha string) (config.Build, error) {
	s.init()
	s.cfgMu.Lock()
	bc, ok := s.cfgCache[sha]
	s.cfgMu.Unlock()
	if ok {
		return bc.Build, bc.Err
	}
	data, _, err := s.Mirror.ReadFile(ctx, sha, config.BuildFile)
	if err != nil {
		return config.Build{}, err
	}
	b, perr := config.ParseBuild(data, s.Cfg.Defaults)
	s.cfgMu.Lock()
	s.cfgCache[sha] = buildConfig{b, perr}
	s.cfgMu.Unlock()
	return b, perr
}

// scratchDir makes a fresh directory for one worktree and marks it in use.
func (s *Service) scratchDir(prefix string) (string, func(), error) {
	dir, err := os.MkdirTemp(s.Scratch, prefix+"-")
	if err != nil {
		return "", nil, err
	}
	// git worktree add wants to create the directory itself.
	os.Remove(dir)
	s.activeMu.Lock()
	s.active[dir] = true
	s.activeMu.Unlock()
	return dir, func() {
		if err := s.Mirror.RemoveWorktree(dir); err != nil {
			s.Log.Warn("removing worktree", "dir", dir, "err", err)
		}
		s.activeMu.Lock()
		delete(s.active, dir)
		s.activeMu.Unlock()
	}, nil
}

// writeFileAtomic writes data next to path and renames it into place.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
