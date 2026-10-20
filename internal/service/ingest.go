package service

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"git.benmandrew.com/me/margins/internal/gitx"
	"git.benmandrew.com/me/margins/internal/store"
)

// fetch updates the mirror and processes whatever changed. A webhook push and
// the Poll timer both end up here, so a missed delivery costs one interval.
func (s *Service) fetch(ctx context.Context) error {
	if err := s.Mirror.Fetch(ctx); err != nil {
		return err
	}
	return s.sync(ctx)
}

// Tracked returns the tracked branches and the review tag pattern, read from
// the default branch tip's .typst-review.toml.
func (s *Service) Tracked(ctx context.Context) (branches []string, reviewPattern string) {
	branches, reviewPattern = s.Cfg.Defaults.Branches, s.Cfg.Defaults.ReviewTags
	tip, err := s.Store.GetRef(ctx, "refs/heads/"+s.Cfg.Repo.DefaultBranch)
	if err != nil || tip.Gone {
		return
	}
	bc, err := s.BuildConfig(ctx, tip.SHA)
	if err != nil {
		return
	}
	return bc.Branches, bc.ReviewTags
}

// IsReviewTag reports whether a tag name matches the review pattern.
func IsReviewTag(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}

// sync compares the mirror's refs with the stored ones, records new commits,
// queues their builds, flags commits a force push left behind, and queues the
// eager diff for a new review tag.
func (s *Service) sync(ctx context.Context) error {
	refs, err := s.Mirror.Refs(ctx)
	if err != nil {
		return err
	}
	stored, err := s.Store.Refs(ctx, "")
	if err != nil {
		return err
	}
	old := map[string]store.Ref{}
	for _, r := range stored {
		old[r.Name] = r
	}
	live := map[string]bool{}
	var tips []string
	for _, r := range refs {
		live[r.Name] = true
		tips = append(tips, r.SHA)
	}

	// Record refs first, so Tracked reads the new default-branch config.
	for _, r := range refs {
		kind := "branch"
		if strings.HasPrefix(r.Name, "refs/tags/") {
			kind = "tag"
		}
		if prev, ok := old[r.Name]; ok && prev.SHA == r.SHA && !prev.Gone {
			continue
		}
		if err := s.Store.PutRef(ctx, store.Ref{Name: r.Name, SHA: r.SHA, Kind: kind, TagDate: r.Date}); err != nil {
			return err
		}
	}
	for name, r := range old {
		if !live[name] && !r.Gone {
			// Deleted on Gitea. Its builds stay.
			if err := s.Store.MarkRefGone(ctx, name); err != nil {
				return err
			}
			s.Log.Info("ref deleted", "ref", name)
		}
	}

	branches, reviewPattern := s.Tracked(ctx)
	var newTags []gitx.Ref
	for _, r := range refs {
		prev, known := old[r.Name]
		if known && prev.SHA == r.SHA && !prev.Gone {
			continue
		}
		if short, ok := strings.CutPrefix(r.Name, "refs/tags/"); ok {
			if IsReviewTag(reviewPattern, short) {
				newTags = append(newTags, r)
			}
			if err := s.recordCommits(ctx, []string{r.SHA}, store.PriorityBuild); err != nil {
				return err
			}
			continue
		}
		branch := strings.TrimPrefix(r.Name, "refs/heads/")
		if !slices.Contains(branches, branch) {
			continue
		}
		var shas []string
		switch {
		case !known || prev.Gone || !s.Mirror.HasCommit(ctx, prev.SHA):
			// A new branch: build only the tip; Backfill picks up its
			// ancestors.
			shas = []string{r.SHA}
		default:
			shas, err = s.Mirror.RevList(ctx, r.SHA, []string{prev.SHA}, s.Cfg.Limits.BackfillDepth)
			if err != nil {
				return err
			}
			if !slices.Contains(shas, r.SHA) {
				// A rewind to an older commit: nothing is new, but the
				// new tip may never have been built.
				shas = append(shas, r.SHA)
			}
			fastForward, err := s.Mirror.IsAncestor(ctx, prev.SHA, r.SHA)
			if err != nil {
				return err
			}
			if !fastForward {
				// A force push. Commits only the old tip reached are kept,
				// flagged unreachable.
				gone, err := s.Mirror.RevList(ctx, prev.SHA, tips, s.Cfg.Limits.BackfillDepth)
				if err != nil {
					return err
				}
				if err := s.Store.MarkUnreachable(ctx, gone); err != nil {
					return err
				}
				s.Log.Info("force push", "branch", branch, "unreachable", len(gone))
			}
		}
		if err := s.recordCommits(ctx, shas, store.PriorityBuild); err != nil {
			return err
		}
	}

	for _, t := range newTags {
		if err := s.eagerDiff(ctx, t, reviewPattern); err != nil {
			return err
		}
	}
	return nil
}

// recordCommits stores commit metadata and queues a build for each commit.
func (s *Service) recordCommits(ctx context.Context, shas []string, priority int) error {
	var missing []string
	for _, sha := range shas {
		if _, err := s.Store.GetCommit(ctx, sha); err == store.ErrNotFound {
			missing = append(missing, sha)
		} else if err != nil {
			return err
		}
	}
	infos, err := s.Mirror.Commits(ctx, missing)
	if err != nil {
		return err
	}
	for _, c := range infos {
		if err := s.Store.PutCommit(ctx, store.Commit{
			SHA: c.SHA, Parents: c.Parents, Author: c.Author, Subject: c.Subject, CommittedAt: c.CommittedAt,
		}); err != nil {
			return err
		}
	}
	// Oldest first, so with one worker history builds in order.
	for i := len(shas) - 1; i >= 0; i-- {
		if err := s.enqueueBuild(ctx, shas[i], priority); err != nil {
			return err
		}
	}
	return nil
}

// ReviewTags returns the live review tags, oldest first: by tagger date for
// annotated tags, else by the tagged commit's date.
func (s *Service) ReviewTags(ctx context.Context) ([]store.Ref, error) {
	_, pattern := s.Tracked(ctx)
	tags, err := s.Store.Refs(ctx, "tag")
	if err != nil {
		return nil, err
	}
	var out []store.Ref
	for _, t := range tags {
		if IsReviewTag(pattern, t.Short()) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TagDate != out[j].TagDate {
			return out[i].TagDate < out[j].TagDate
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// previousReviewTag finds the newest review tag older than t whose commit is
// an ancestor of t's, so the pair lies on one branch.
func (s *Service) previousReviewTag(ctx context.Context, t store.Ref) (store.Ref, bool, error) {
	tags, err := s.ReviewTags(ctx)
	if err != nil {
		return store.Ref{}, false, err
	}
	idx := slices.IndexFunc(tags, func(r store.Ref) bool { return r.Name == t.Name })
	for i := idx - 1; i >= 0; i-- {
		if tags[i].SHA == t.SHA {
			continue
		}
		ok, err := s.Mirror.IsAncestor(ctx, tags[i].SHA, t.SHA)
		if err != nil {
			return store.Ref{}, false, err
		}
		if ok {
			return tags[i], true, nil
		}
	}
	return store.Ref{}, false, nil
}

func (s *Service) eagerDiff(ctx context.Context, t gitx.Ref, pattern string) error {
	ref, err := s.Store.GetRef(ctx, t.Name)
	if err != nil {
		return err
	}
	prev, ok, err := s.previousReviewTag(ctx, ref)
	if err != nil || !ok {
		return err
	}
	s.Log.Info("review tag", "tag", ref.Short(), "previous", prev.Short())
	return s.requestDiffs(ctx, prev.SHA, ref.SHA, store.PriorityBuild)
}

// requestDiffs creates and queues the diff of (a, b) for every entrypoint of
// b. A diff that is already done or queued is left alone.
func (s *Service) requestDiffs(ctx context.Context, a, b string, priority int) error {
	bc, err := s.BuildConfig(ctx, b)
	if err != nil {
		return fmt.Errorf("build config of %s: %w", b, err)
	}
	for _, e := range bc.Entrypoints {
		if err := s.requestDiff(ctx, a, b, e, priority); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) requestDiff(ctx context.Context, a, b, entry string, priority int) error {
	d, _, err := s.Store.EnsureDiff(ctx, a, b, entry)
	if err != nil {
		return err
	}
	if d.Status != store.Queued && !(d.Pruned && d.Status == store.OK) {
		return nil
	}
	if d.Pruned {
		if err := s.Store.RequeueDiff(ctx, d.ID); err != nil {
			return err
		}
	}
	return s.enqueue(ctx, jobDiff, fmt.Sprintf("diff:%d", d.ID), idPayload{d.ID}, priority)
}
