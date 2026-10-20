package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"

	"git.benmandrew.com/me/thesis/internal/config"
	"git.benmandrew.com/me/thesis/internal/store"
)

//go:embed assets/review-notes.typ
var reviewNotesTyp []byte

const reviewDir = ".thesis-review"

// Placements a note can get in an annotated build.
const (
	PlacedAnchor    = "anchor"
	PlacedParagraph = "paragraph"
	PlacedUnplaced  = "unplaced"
)

// userColours are distinct on white and in greyscale print.
var userColours = []string{"#c01c28", "#1a5fb4", "#26a269", "#a347ba", "#c64600", "#5e5c64"}

// AnnotationNotes returns the notes an annotated build of sha shows: the open
// notes anchored there, and the outdated notes written on it.
func (s *Service) AnnotationNotes(ctx context.Context, sha string) ([]store.PlacedNote, error) {
	all, err := s.Store.NotesAt(ctx, sha, "")
	if err != nil {
		return nil, err
	}
	var out []store.PlacedNote
	for _, n := range all {
		if n.State == store.NoteOpen || n.State == store.NoteOutdated && n.Origin.SHA == sha {
			out = append(out, n)
		}
	}
	return out, nil
}

type noteJSON struct {
	Number string `json:"number"`
	Name   string `json:"name"`
	Colour string `json:"colour"`
	Text   string `json:"text"`
	File   string `json:"file"`
	Line   int    `json:"line"`
}

// NotesJSON is the notes.json an annotated build reads, and its SHA-256,
// which is part of the build's cache key.
func (s *Service) NotesJSON(notes []store.PlacedNote) ([]byte, string, error) {
	doc := map[string]noteJSON{}
	for i, n := range notes {
		name, colour := n.UserID, userColours[len(userColours)-1]
		if idx := slices.IndexFunc(s.Cfg.Users, func(u config.User) bool { return u.ID == n.UserID }); idx >= 0 {
			name = s.Cfg.Users[idx].Name
			colour = userColours[idx%len(userColours)]
		}
		text := n.Body
		if n.State == store.NoteOutdated {
			text = "(outdated) " + text
		}
		doc[n.Ref()] = noteJSON{
			Number: strconv.Itoa(i + 1), Name: name, Colour: colour, Text: text,
			File: n.At.File, Line: n.At.Line,
		}
	}
	// encoding/json sorts map keys, so the bytes and the hash are stable.
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// RequestAnnotated returns the annotated build of (sha, entry) for its
// current notes, and queues it if needed and queue is set. ok is false when
// the commit has no notes to show.
func (s *Service) RequestAnnotated(ctx context.Context, sha, entry string, queue bool) (a store.Annotated, ok bool, err error) {
	notes, err := s.AnnotationNotes(ctx, sha)
	if err != nil || len(notes) == 0 {
		return a, false, err
	}
	_, hash, err := s.NotesJSON(notes)
	if err != nil {
		return a, false, err
	}
	bc, err := s.BuildConfig(ctx, sha)
	if err != nil {
		return a, false, err
	}
	a, _, err = s.Store.EnsureAnnotated(ctx, sha, entry, bc.Typst, hash)
	if err != nil {
		return a, false, err
	}
	if queue && (a.Status == store.Queued || a.Status == store.Timeout || a.Status == store.OK && a.Pruned) {
		if a.Status != store.Queued {
			if err := s.Store.RequeueAnnotated(ctx, a.ID); err != nil {
				return a, false, err
			}
			a.Status = store.Queued
		}
		err = s.enqueue(ctx, jobAnnotate, fmt.Sprintf("annotate:%d", a.ID), idPayload{a.ID}, store.PriorityRequested)
	}
	return a, true, err
}

type marker struct {
	note   store.PlacedNote
	offset int // byte offset in the file
	place  string
}

func (s *Service) runAnnotated(ctx context.Context, id int64) error {
	a, err := s.Store.GetAnnotated(ctx, id)
	if err != nil {
		return err
	}
	if !(a.Status == store.Queued || a.Status == store.Building || a.Status == store.Timeout || a.Status == store.OK && a.Pruned) {
		return nil
	}
	if err := s.Store.StartAnnotated(ctx, a.ID); err != nil {
		return err
	}
	logPath := s.AnnotatedLogPath(a.SHA, a.Entry, a.NotesHash)
	finish := func(st store.Status, placements map[string]string, size int64, log []byte) error {
		if err := writeFileAtomic(logPath, log); err != nil {
			return err
		}
		p, _ := json.Marshal(placements)
		s.Log.Info("annotated build", "sha", a.SHA[:12], "entry", a.Entry, "status", st)
		return s.Store.FinishAnnotated(context.WithoutCancel(ctx), a.ID, st, string(p), size)
	}

	notes, err := s.AnnotationNotes(ctx, a.SHA)
	if err != nil {
		return err
	}
	notesJSON, hash, err := s.NotesJSON(notes)
	if err != nil {
		return err
	}
	if hash != a.NotesHash {
		// The notes changed after this build was queued. The next request
		// queues a build for the new set; this one is left for Prune.
		return finish(store.Failed, nil, 0, []byte("[thesis] the notes changed before this build ran\n"))
	}
	bc, err := s.BuildConfig(ctx, a.SHA)
	if err != nil {
		return finish(store.Failed, nil, 0, []byte("[thesis] "+err.Error()+"\n"))
	}
	commit, err := s.Commit(ctx, a.SHA)
	if err != nil {
		return err
	}

	tree, release, err := s.checkout(ctx, "notes", a.SHA)
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Lstat(filepath.Join(tree, reviewDir)); err == nil {
		return finish(store.Failed, nil, 0, []byte("[thesis] the commit has its own "+reviewDir+"/, so it cannot be annotated\n"))
	}
	if err := os.MkdirAll(filepath.Join(tree, reviewDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tree, reviewDir, "notes.json"), notesJSON, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tree, reviewDir, "review-notes.typ"), reviewNotesTyp, 0o644); err != nil {
		return err
	}

	// Read every file a note sits in, and the entrypoint, before any change.
	originals := map[string][]byte{}
	for _, f := range append(notesFiles(notes), a.Entry) {
		if _, done := originals[f]; done {
			continue
		}
		p := filepath.Join(tree, f)
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		originals[f] = data
	}

	plans := placementPlans(notes, originals)
	var log bytes.Buffer
	out := s.AnnotatedPath(a.SHA, a.Entry, a.NotesHash)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	timedOut := false
	for i, plan := range plans {
		if err := writePlan(tree, a.Entry, originals, plan); err != nil {
			return err
		}
		res, err := s.compile(ctx, bc.Typst, tree, a.Entry, bc.Fonts, commit.CommittedAt, out+".tmp")
		if err != nil {
			s.Store.RequeueAnnotated(context.WithoutCancel(ctx), a.ID)
			return err
		}
		fmt.Fprintf(&log, "[thesis] attempt %d (%s): %s\n", i+1, summarise(plan), res.Status)
		log.Write(res.Log)
		if res.Status == "ok" {
			if err := os.Rename(out+".tmp", out); err != nil {
				return err
			}
			placements := map[string]string{}
			for _, m := range plan {
				placements[m.note.Ref()] = m.place
			}
			return finish(store.OK, placements, res.Size, log.Bytes())
		}
		os.Remove(out + ".tmp")
		if res.Status == "timeout" {
			timedOut = true
			break
		}
	}
	if timedOut {
		return finish(store.Timeout, nil, 0, log.Bytes())
	}
	return finish(store.Failed, nil, 0, log.Bytes())
}

func notesFiles(notes []store.PlacedNote) []string {
	var files []string
	for _, n := range notes {
		if !slices.Contains(files, n.At.File) {
			files = append(files, n.At.File)
		}
	}
	return files
}

// placementPlans returns up to three plans, each a placement for every note:
// at the anchor where the scanner allows it, then at paragraph starts, then
// all in the "Unplaced notes" list.
func placementPlans(notes []store.PlacedNote, originals map[string][]byte) [][]marker {
	classes := map[string][]uint8{}
	for f, data := range originals {
		classes[f] = scanMarkup(data)
	}
	var first, second, third []marker
	for _, n := range notes {
		src, ok := originals[n.At.File]
		anchor, para := -1, -1
		if ok {
			class := classes[n.At.File]
			if off, ok := lineColOffset(src, n.At.Line, n.At.Col); ok {
				if class[off] == posInline {
					anchor = off
				}
				if p := paragraphStart(src, off); class[p] != posOther {
					para = p
				}
			}
		}
		unplaced := marker{note: n, offset: -1, place: PlacedUnplaced}
		switch {
		case anchor >= 0:
			first = append(first, marker{n, anchor, PlacedAnchor})
		case para >= 0:
			first = append(first, marker{n, para, PlacedParagraph})
		default:
			first = append(first, unplaced)
		}
		if para >= 0 {
			second = append(second, marker{n, para, PlacedParagraph})
		} else {
			second = append(second, unplaced)
		}
		third = append(third, unplaced)
	}
	plans := [][]marker{first}
	if !samePlan(first, second) {
		plans = append(plans, second)
	}
	if !samePlan(plans[len(plans)-1], third) {
		plans = append(plans, third)
	}
	return plans
}

func samePlan(a, b []marker) bool {
	return slices.EqualFunc(a, b, func(x, y marker) bool { return x.place == y.place && x.offset == y.offset })
}

func summarise(plan []marker) string {
	count := map[string]int{}
	for _, m := range plan {
		count[m.place]++
	}
	return fmt.Sprintf("%d at anchor, %d at paragraph start, %d unplaced",
		count[PlacedAnchor], count[PlacedParagraph], count[PlacedUnplaced])
}

// writePlan rewrites the tree's files from their originals with one plan's
// markers. Note IDs are checked against their pattern before they become
// source; note text never does.
func writePlan(tree, entry string, originals map[string][]byte, plan []marker) error {
	byFile := map[string][]marker{}
	var unplaced []string
	for _, m := range plan {
		ref := m.note.Ref()
		if !config.NoteID.MatchString(ref) {
			return fmt.Errorf("note id %q", ref)
		}
		if m.place == PlacedUnplaced {
			unplaced = append(unplaced, ref)
			continue
		}
		byFile[m.note.At.File] = append(byFile[m.note.At.File], m)
	}
	for f, orig := range originals {
		data := slices.Clone(orig)
		ms := byFile[f]
		// Insert from the end, so earlier offsets stay valid.
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].offset > ms[j].offset })
		for _, m := range ms {
			ins := `#review-note("` + m.note.Ref() + `");`
			if m.place == PlacedParagraph {
				ins += "\n"
			}
			data = slices.Insert(data, m.offset, []byte(ins)...)
		}
		if len(ms) > 0 {
			data = append([]byte("#import \"/"+reviewDir+"/review-notes.typ\": review-note\n"), data...)
		}
		if f == entry && len(unplaced) > 0 {
			list, _ := json.Marshal(unplaced)
			// A Typst array of strings, from a JSON array of IDs that all
			// matched ^n[0-9]+$.
			data = append(data, fmt.Sprintf("\n#import \"/%s/review-notes.typ\": review-unplaced\n#review-unplaced((%s,))\n",
				reviewDir, bytes.Trim(list, "[]"))...)
		}
		if err := os.WriteFile(filepath.Join(tree, f), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// lineColOffset converts a 1-based line and byte column to an offset.
func lineColOffset(src []byte, line, col int) (int, bool) {
	if line < 1 || col < 1 {
		return 0, false
	}
	off := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(src[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
	}
	end := len(src)
	if i := bytes.IndexByte(src[off:], '\n'); i >= 0 {
		end = off + i
	}
	if off+col-1 > end {
		return 0, false
	}
	return off + col - 1, true
}

// paragraphStart is the start of the first line after the nearest blank line
// before off, or of the file.
func paragraphStart(src []byte, off int) int {
	lineStart := bytes.LastIndexByte(src[:off], '\n') + 1
	for lineStart > 0 {
		prevStart := bytes.LastIndexByte(src[:lineStart-1], '\n') + 1
		if len(bytes.TrimSpace(src[prevStart:lineStart-1])) == 0 {
			return lineStart
		}
		lineStart = prevStart
	}
	return 0
}
