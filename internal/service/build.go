package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"git.benmandrew.com/me/margins/internal/store"
	"git.benmandrew.com/me/margins/internal/typst"
)

// buildCommit builds every entrypoint of a commit that has no finished build
// for its cache key. All entrypoints share one worktree.
func (s *Service) buildCommit(ctx context.Context, sha string) error {
	commit, err := s.Commit(ctx, sha)
	if err != nil {
		return err
	}

	bc, cfgErr := s.BuildConfig(ctx, sha)
	if cfgErr != nil {
		// A broken .typst-review.toml fails the commit's builds visibly, under
		// the service's default entrypoints and version.
		return s.failConfig(ctx, sha, cfgErr)
	}

	var worktree string
	release := func() {}
	defer func() { release() }()
	built := true
	for _, entry := range bc.Entrypoints {
		b, _, err := s.Store.EnsureBuild(ctx, sha, entry, bc.Typst)
		if err != nil {
			return err
		}
		if !s.needsBuild(b) {
			built = built && b.Status == store.OK
			continue
		}
		if worktree == "" {
			if worktree, release, err = s.checkout(ctx, "build", sha); err != nil {
				return err
			}
		}
		st, err := s.compileEntry(ctx, b, worktree, bc.Fonts, commit.CommittedAt)
		if err != nil {
			return err
		}
		built = built && st == store.OK
	}
	if built {
		return s.afterBuild(ctx, sha)
	}
	return nil
}

// Commit returns a commit's metadata, recording it from the mirror first if
// the store does not have it yet.
func (s *Service) Commit(ctx context.Context, sha string) (store.Commit, error) {
	c, err := s.Store.GetCommit(ctx, sha)
	if !errors.Is(err, store.ErrNotFound) {
		return c, err
	}
	infos, err := s.Mirror.Commits(ctx, []string{sha})
	if err != nil {
		return c, err
	}
	if len(infos) == 0 {
		return c, fmt.Errorf("commit %s: %w", sha, store.ErrNotFound)
	}
	i := infos[0]
	c = store.Commit{SHA: i.SHA, Parents: i.Parents, Author: i.Author, Subject: i.Subject, CommittedAt: i.CommittedAt}
	return c, s.Store.PutCommit(ctx, c)
}

// needsBuild reports whether a build row should be (re)run now: it is
// queued, or its PDF was pruned, or it timed out with attempts left.
func (s *Service) needsBuild(b store.Build) bool {
	switch {
	case b.Status == store.Queued, b.Status == store.Building:
		return true
	case b.Status == store.OK && b.Pruned:
		return true
	case b.Status == store.Timeout && b.Attempts < s.Cfg.Limits.MaxAttempts:
		return true
	}
	return false
}

// checkout makes a detached worktree of sha in a fresh scratch directory.
// The caller must call release on every exit path.
func (s *Service) checkout(ctx context.Context, prefix, sha string) (dir string, release func(), err error) {
	dir, release, err = s.scratchDir(prefix)
	if err != nil {
		return "", func() {}, err
	}
	if err := s.Mirror.AddWorktree(ctx, dir, sha); err != nil {
		release()
		return "", func() {}, err
	}
	return dir, release, nil
}

func (s *Service) compileEntry(ctx context.Context, b store.Build, worktree, fonts string, committedAt int64) (store.Status, error) {
	if err := s.Store.StartBuild(ctx, b.ID); err != nil {
		return "", err
	}
	out := s.BuildPath(b.SHA, b.Entry)
	logPath := s.BuildLogPath(b.SHA, b.Entry)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	res, err := s.compile(ctx, b.Typst, worktree, b.Entry, fonts, committedAt, out+".tmp")
	if err != nil {
		// The service failed, not the document: put the build back so the
		// next Backfill tries again.
		s.Store.RequeueBuild(context.WithoutCancel(ctx), b.ID)
		return "", err
	}
	st := store.Status(res.Status)
	if st == store.OK {
		if err := os.Rename(out+".tmp", out); err != nil {
			return "", err
		}
	} else {
		os.Remove(out + ".tmp")
	}
	if err := writeFileAtomic(logPath, res.Log); err != nil {
		return "", err
	}
	s.Log.Info("build", "sha", b.SHA[:12], "entry", b.Entry, "typst", b.Typst, "status", st, "took", res.Duration.Round(1e6))
	return st, s.Store.FinishBuild(context.WithoutCancel(ctx), b.ID, st, res.Duration.Milliseconds(), res.Size)
}

// compile runs one typst compile in a tree. A version that cannot be
// fetched, or has no pinned hash, is a failed build with the reason in its
// log, so a bad pin shows in the UI.
func (s *Service) compile(ctx context.Context, version, root, entry, fonts string, timestamp int64, out string) (typst.Result, error) {
	bin, err := s.Typst.Binary(ctx, version)
	if err != nil {
		if errors.Is(err, typst.ErrNoHash) || strings.Contains(err.Error(), "SHA-256 mismatch") {
			return typst.Result{Status: "failed", Log: []byte("[thesis] " + err.Error() + "\n")}, nil
		}
		return typst.Result{}, err
	}
	c := typst.Compile{
		Binary:       bin,
		Root:         root,
		Entry:        entry,
		Out:          out,
		Timestamp:    timestamp,
		PackageCache: s.packages(),
		Home:         s.home(),
		Timeout:      s.Cfg.Limits.BuildTimeout,
		MaxOutput:    s.Cfg.Limits.MaxOutputBytes,
	}
	if fonts != "" {
		c.FontPaths = []string{filepath.Join(root, fonts)}
	}
	return c.Run(ctx)
}

// failConfig records an unreadable .typst-review.toml as a failed build.
func (s *Service) failConfig(ctx context.Context, sha string, cfgErr error) error {
	version := s.Cfg.Defaults.Typst
	if version == "" {
		version = "unknown"
	}
	for _, entry := range s.Cfg.Defaults.Entrypoints {
		b, _, err := s.Store.EnsureBuild(ctx, sha, entry, version)
		if err != nil {
			return err
		}
		if b.Status == store.Failed {
			continue
		}
		if err := s.Store.StartBuild(ctx, b.ID); err != nil {
			return err
		}
		if err := writeFileAtomic(s.BuildLogPath(sha, entry), []byte("[thesis] "+cfgErr.Error()+"\n")); err != nil {
			return err
		}
		if err := s.Store.FinishBuild(ctx, b.ID, store.Failed, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

// afterBuild carries open notes to a commit on a tracked branch once all its
// entrypoints have built.
func (s *Service) afterBuild(ctx context.Context, sha string) error {
	onBranch, err := s.OnTrackedBranch(ctx, sha)
	if err != nil || !onBranch {
		return err
	}
	return s.enqueue(ctx, jobCarry, "carry:"+sha, shaPayload{sha}, store.PriorityBuild)
}

// OnTrackedBranch reports whether a commit is reachable from a tracked
// branch tip.
func (s *Service) OnTrackedBranch(ctx context.Context, sha string) (bool, error) {
	branches, _ := s.Tracked(ctx)
	for _, br := range branches {
		ref, err := s.Store.GetRef(ctx, "refs/heads/"+br)
		if err != nil || ref.Gone {
			continue
		}
		ok, err := s.Mirror.IsAncestor(ctx, sha, ref.SHA)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// EnsureBuilt builds (sha, entry) now if it has no finished build, and
// returns the build. Diffs and annotated builds use it, so a request never
// waits on the queue for a build it depends on.
func (s *Service) EnsureBuilt(ctx context.Context, sha, entry string) (store.Build, error) {
	bc, err := s.BuildConfig(ctx, sha)
	if err != nil {
		return store.Build{}, err
	}
	if !slices.Contains(bc.Entrypoints, entry) {
		return store.Build{}, fmt.Errorf("%s is not an entrypoint at %s", entry, sha[:12])
	}
	b, _, err := s.Store.EnsureBuild(ctx, sha, entry, bc.Typst)
	if err != nil {
		return b, err
	}
	if s.needsBuild(b) {
		if err := s.buildCommit(ctx, sha); err != nil {
			return b, err
		}
		return s.Store.GetBuild(ctx, b.ID)
	}
	return b, nil
}
