package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"git.benmandrew.com/me/thesis/internal/store"
)

// ErrQueueFull means too many diffs are already queued; the caller answers
// 503 with Retry-After.
var ErrQueueFull = errors.New("the diff queue is full")

// RequestBuild queues a build of sha for a visitor waiting on it. It is a
// no-op when the build is done or already queued.
func (s *Service) RequestBuild(ctx context.Context, sha, entry string) (store.Build, error) {
	bc, err := s.BuildConfig(ctx, sha)
	if err != nil {
		return store.Build{}, err
	}
	if !slices.Contains(bc.Entrypoints, entry) {
		return store.Build{}, fmt.Errorf("%s: %w", entry, store.ErrNotFound)
	}
	b, _, err := s.Store.EnsureBuild(ctx, sha, entry, bc.Typst)
	if err != nil || !s.needsBuild(b) {
		return b, err
	}
	if b.Status != store.Queued {
		if err := s.Store.RequeueBuild(ctx, b.ID); err != nil {
			return b, err
		}
		b.Status = store.Queued
	}
	return b, s.enqueueBuild(ctx, sha, store.PriorityRequested)
}

// RequestDiff returns the diff of (a, b, entry), queuing it if it is new or
// pruned. Both commits must already be known. A full queue refuses new work.
func (s *Service) RequestDiff(ctx context.Context, a, b, entry string) (store.Diff, error) {
	for _, sha := range []string{a, b} {
		if _, err := s.Store.GetCommit(ctx, sha); err != nil {
			return store.Diff{}, err
		}
	}
	d, err := s.Store.FindDiff(ctx, a, b, entry)
	if err == nil && !(d.Status == store.Queued || d.Status == store.OK && d.Pruned) {
		return d, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return d, err
	}
	n, err := s.Store.CountJobs(ctx, jobDiff)
	if err != nil {
		return d, err
	}
	if n >= s.Cfg.Limits.DiffQueue {
		return d, ErrQueueFull
	}
	if err := s.requestDiff(ctx, a, b, entry, store.PriorityRequested); err != nil {
		return d, err
	}
	return s.Store.FindDiff(ctx, a, b, entry)
}
