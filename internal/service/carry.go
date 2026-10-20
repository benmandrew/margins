package service

import (
	"bytes"
	"context"

	"git.benmandrew.com/me/margins/internal/gitx"
	"git.benmandrew.com/me/margins/internal/store"
)

// carryNotes anchors every open note at sha, from the note's nearest anchor
// on an ancestor. A note whose selected text has gone becomes outdated and
// stays on the commits it was already anchored at.
func (s *Service) carryNotes(ctx context.Context, sha string) error {
	notes, err := s.Store.OpenNotes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		anchors, err := s.Store.Anchors(ctx, n.ID)
		if err != nil {
			return err
		}
		src, found := store.Anchor{}, false
		var srcTime int64
		for _, a := range anchors {
			if a.SHA == sha {
				found = false
				break
			}
			ok, err := s.Mirror.IsAncestor(ctx, a.SHA, sha)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			c, err := s.Commit(ctx, a.SHA)
			if err != nil {
				return err
			}
			if !found || c.CommittedAt > srcTime {
				src, srcTime, found = a, c.CommittedAt, true
			}
		}
		if !found {
			continue
		}
		dst, ok, err := s.mapAnchor(ctx, src, sha, n.Selected)
		if err != nil {
			return err
		}
		if !ok {
			s.Log.Info("note outdated", "note", n.Ref(), "at", sha[:12])
			if err := s.Store.SetNoteState(ctx, n.ID, store.NoteOutdated, ""); err != nil {
				return err
			}
			continue
		}
		if err := s.Store.PutAnchor(ctx, n.ID, dst); err != nil {
			return err
		}
	}
	return nil
}

// mapAnchor moves an anchor from its commit to dst: through the line diff of
// its file, then by checking the selected text is there, then by searching
// the file for it.
func (s *Service) mapAnchor(ctx context.Context, src store.Anchor, dst, selected string) (store.Anchor, bool, error) {
	content, ok, err := s.Mirror.ReadFile(ctx, dst, src.File)
	if err != nil || !ok {
		return store.Anchor{}, false, err
	}
	hunks, err := s.Mirror.Hunks(ctx, src.SHA, dst, src.File)
	if err != nil {
		return store.Anchor{}, false, err
	}
	line, _ := mapLine(src.Line, hunks)
	out := store.Anchor{SHA: dst, File: src.File, Line: line, Col: src.Col}
	if selected == "" {
		if _, ok := lineColOffset(content, line, 1); ok {
			out.Col = 1
			if _, ok := lineColOffset(content, line, src.Col); ok {
				out.Col = src.Col
			}
			return out, true, nil
		}
		return store.Anchor{}, false, nil
	}
	if off, ok := lineColOffset(content, line, src.Col); ok && bytes.HasPrefix(content[off:], []byte(selected)) {
		return out, true, nil
	}
	// Search the whole file and take the occurrence nearest the mapped line.
	best, bestDist := -1, 0
	for from := 0; ; {
		i := bytes.Index(content[from:], []byte(selected))
		if i < 0 {
			break
		}
		off := from + i
		l := bytes.Count(content[:off], []byte{'\n'}) + 1
		dist := l - line
		if dist < 0 {
			dist = -dist
		}
		if best < 0 || dist < bestDist {
			best, bestDist = off, dist
		}
		from = off + 1
	}
	if best < 0 {
		return store.Anchor{}, false, nil
	}
	lineStart := bytes.LastIndexByte(content[:best], '\n') + 1
	out.Line = bytes.Count(content[:best], []byte{'\n'}) + 1
	out.Col = best - lineStart + 1
	return out, true, nil
}

// mapLine maps an old line number through zero-context hunks. changed is
// true when a hunk rewrote the line itself.
func mapLine(line int, hunks []gitx.Hunk) (int, bool) {
	delta := 0
	for _, h := range hunks {
		if h.OldLines == 0 {
			// An insertion after old line OldStart.
			if h.OldStart < line {
				delta += h.NewLines
				continue
			}
			break
		}
		end := h.OldStart + h.OldLines - 1
		switch {
		case end < line:
			delta += h.NewLines - h.OldLines
		case h.OldStart <= line:
			return line + delta, true
		default:
			return line + delta, false
		}
	}
	return line + delta, false
}
