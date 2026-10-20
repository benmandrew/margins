package service

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"git.benmandrew.com/me/thesis/internal/store"
)

// timers starts each maintenance task at boot and then on its interval. A
// task only enqueues its job, and the job's unique key stops a run from
// overlapping one still queued or running.
func (s *Service) timers(ctx context.Context) {
	every := func(interval time.Duration, kind string) {
		for {
			if err := s.enqueue(ctx, kind, kind, struct{}{}, store.PriorityHistory); err != nil && ctx.Err() == nil {
				s.Log.Error("timer", "task", kind, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}
	tasks := []struct {
		interval time.Duration
		kind     string
	}{
		{s.Cfg.Timers.Poll, jobFetch},
		{s.Cfg.Timers.Backfill, jobBackfill},
		{s.Cfg.Timers.Retry, jobRetry},
		{s.Cfg.Timers.ReviewDiffs, jobReview},
		{s.Cfg.Timers.Prune, jobPrune},
	}
	done := make(chan struct{})
	for _, t := range tasks {
		go func() {
			every(t.interval, t.kind)
			done <- struct{}{}
		}()
	}
	go func() {
		s.daily(ctx)
		done <- struct{}{}
	}()
	for range len(tasks) + 1 {
		<-done
	}
}

// daily enqueues the snapshot at snapshot_at local time each day.
func (s *Service) daily(ctx context.Context) {
	for {
		wait := time.Until(NextAt(time.Now(), s.Cfg.Timers.SnapshotAt, s.Cfg.Location))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := s.enqueue(ctx, jobSnapshot, jobSnapshot, struct{}{}, store.PriorityFetch); err != nil && ctx.Err() == nil {
			s.Log.Error("timer", "task", jobSnapshot, "err", err)
		}
	}
}

// NextAt is the next time after now that the wall clock in loc reads hhmm.
func NextAt(now time.Time, hhmm string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return now.Add(24 * time.Hour)
	}
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if !next.After(n) {
		next = time.Date(n.Year(), n.Month(), n.Day()+1, t.Hour(), t.Minute(), 0, 0, loc)
	}
	return next
}

// backfill queues a build for every commit on the tracked branches and every
// review tag with no build yet, newest first, up to the configured depth.
func (s *Service) backfill(ctx context.Context) error {
	branches, _ := s.Tracked(ctx)
	var shas []string
	seen := map[string]bool{}
	add := func(sha string) {
		if !seen[sha] {
			seen[sha] = true
			shas = append(shas, sha)
		}
	}
	for _, br := range branches {
		ref, err := s.Store.GetRef(ctx, "refs/heads/"+br)
		if err != nil || ref.Gone {
			continue
		}
		list, err := s.Mirror.RevList(ctx, ref.SHA, nil, s.Cfg.Limits.BackfillDepth)
		if err != nil {
			return err
		}
		for _, sha := range list {
			add(sha)
		}
	}
	tags, err := s.ReviewTags(ctx)
	if err != nil {
		return err
	}
	for _, t := range tags {
		add(t.SHA)
	}
	var need []string
	for _, sha := range shas {
		builds, err := s.Store.BuildsForCommit(ctx, sha)
		if err != nil {
			return err
		}
		missing := len(builds) == 0
		for _, b := range builds {
			if b.Status == store.Queued || b.Status == store.Building {
				missing = true
			}
		}
		if missing {
			need = append(need, sha)
		}
	}
	if len(need) > 0 {
		s.Log.Info("backfill", "commits", len(need))
	}
	// recordCommits queues oldest first; reverse so newest history comes
	// first, as the brief asks.
	for i := len(need) - 1; i >= 0; i-- {
		if err := s.recordCommits(ctx, need[i:i+1], store.PriorityHistory); err != nil {
			return err
		}
	}
	return nil
}

// retry re-queues builds and diffs that timed out and have attempts left. A
// failed compile is deterministic and never retried. Diffs left queued with
// no job, after a crash, are queued again too.
func (s *Service) retry(ctx context.Context) error {
	max := s.Cfg.Limits.MaxAttempts
	timedOut, err := s.Store.BuildsWithStatus(ctx, store.Timeout)
	if err != nil {
		return err
	}
	for _, b := range timedOut {
		if b.Attempts < max {
			if err := s.enqueueBuild(ctx, b.SHA, store.PriorityHistory); err != nil {
				return err
			}
		}
	}
	for _, st := range []store.Status{store.Timeout, store.Queued} {
		diffs, err := s.Store.DiffsWithStatus(ctx, st)
		if err != nil {
			return err
		}
		for _, d := range diffs {
			if st == store.Timeout && d.Attempts >= max {
				continue
			}
			if err := s.enqueue(ctx, jobDiff, fmt.Sprintf("diff:%d", d.ID), idPayload{d.ID}, store.PriorityHistory); err != nil {
				return err
			}
		}
	}
	return nil
}

// reviewDiffs queues any missing diff between consecutive review tags.
func (s *Service) reviewDiffs(ctx context.Context) error {
	tags, err := s.ReviewTags(ctx)
	if err != nil {
		return err
	}
	for _, t := range tags {
		prev, ok, err := s.previousReviewTag(ctx, t)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := s.requestDiffs(ctx, prev.SHA, t.SHA, store.PriorityHistory); err != nil {
			s.Log.Warn("review diff", "tag", t.Short(), "err", err)
		}
	}
	return nil
}

func (s *Service) snapshot(ctx context.Context) error {
	start := time.Now()
	if err := s.Store.Snapshot(ctx, s.SnapshotPath()); err != nil {
		return err
	}
	s.Log.Info("database snapshot", "path", s.SnapshotPath(), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// DiskUsage sums the sizes of every regular file under the data directory.
func (s *Service) DiskUsage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.Cfg.DataDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total, err
}

// protected returns the commits a branch tip or a review tag points at.
func (s *Service) protected(ctx context.Context) (map[string]bool, error) {
	refs, err := s.Store.Refs(ctx, "")
	if err != nil {
		return nil, err
	}
	_, pattern := s.Tracked(ctx)
	keep := map[string]bool{}
	for _, r := range refs {
		if r.Kind == "branch" || IsReviewTag(pattern, r.Short()) {
			keep[r.SHA] = true
		}
	}
	return keep, nil
}

// prune keeps the data directory under the disk budget. Annotated PDFs go
// first, since they rebuild from the notes; then the oldest builds and
// diffs, skipping the protected ones. Notes, the database and its snapshot
// are never touched. It also removes worktrees a crash left behind.
func (s *Service) prune(ctx context.Context) error {
	s.removeStaleWorktrees(ctx)
	used, err := s.DiskUsage()
	if err != nil {
		return err
	}
	budget := s.Cfg.Limits.DiskBudget
	if used <= budget {
		return nil
	}
	start := used
	remove := func(paths ...string) int64 {
		var freed int64
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil {
				if os.Remove(p) == nil {
					freed += fi.Size()
				}
			}
		}
		return freed
	}

	annotated, err := s.Store.AllAnnotated(ctx)
	if err != nil {
		return err
	}
	sort.Slice(annotated, func(i, j int) bool { return annotated[i].UpdatedAt < annotated[j].UpdatedAt })
	for _, a := range annotated {
		if used <= budget {
			break
		}
		if a.Pruned || a.Status != store.OK {
			continue
		}
		used -= remove(s.AnnotatedPath(a.SHA, a.Entry, a.NotesHash))
		if err := s.Store.MarkAnnotatedPruned(ctx, a.ID); err != nil {
			return err
		}
	}

	keep, err := s.protected(ctx)
	if err != nil {
		return err
	}
	type item struct {
		at     int64
		build  *store.Build
		diff   *store.Diff
		commit int64
	}
	var items []item
	builds, err := s.Store.AllBuilds(ctx)
	if err != nil {
		return err
	}
	for i := range builds {
		b := &builds[i]
		if b.Pruned || b.Status != store.OK || keep[b.SHA] {
			continue
		}
		c, _ := s.Store.GetCommit(ctx, b.SHA)
		items = append(items, item{at: c.CommittedAt, build: b})
	}
	diffs, err := s.Store.AllDiffs(ctx)
	if err != nil {
		return err
	}
	for i := range diffs {
		d := &diffs[i]
		if d.Pruned || d.Status != store.OK || keep[d.A] && keep[d.B] {
			continue
		}
		c, _ := s.Store.GetCommit(ctx, d.B)
		items = append(items, item{at: c.CommittedAt, diff: d})
	}
	// Oldest commit first.
	sort.SliceStable(items, func(i, j int) bool { return items[i].at < items[j].at })
	for _, it := range items {
		if used <= budget {
			break
		}
		if it.build != nil {
			used -= remove(s.BuildPath(it.build.SHA, it.build.Entry))
			if err := s.Store.MarkBuildPruned(ctx, it.build.ID); err != nil {
				return err
			}
			continue
		}
		d := it.diff
		used -= remove(
			s.DiffPath(d.A, d.B, d.Entry, MethodTypdiff),
			s.DiffPath(d.A, d.B, d.Entry, MethodDiffPDF),
			s.TextDiffPath(d.A, d.B, d.Entry))
		if err := s.Store.MarkDiffPruned(ctx, d.ID); err != nil {
			return err
		}
	}
	s.Log.Info("prune", "before", start, "after", used, "budget", budget)
	if used > budget {
		s.Log.Warn("still over the disk budget after pruning everything unprotected", "used", used, "budget", budget)
	}
	return nil
}

// removeStaleWorktrees deletes scratch directories no job is using.
func (s *Service) removeStaleWorktrees(ctx context.Context) {
	entries, err := os.ReadDir(s.Scratch)
	if err != nil {
		return
	}
	s.activeMu.Lock()
	var stale []string
	for _, e := range entries {
		p := filepath.Join(s.Scratch, e.Name())
		if !s.active[p] {
			stale = append(stale, p)
		}
	}
	s.activeMu.Unlock()
	for _, p := range stale {
		// Directories still in use by a job in another worker are in
		// active; anything else is left over from a crash.
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.RemoveAll(p)
		}
	}
	s.Mirror.PruneWorktrees(ctx)
}
