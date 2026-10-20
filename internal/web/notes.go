package web

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/store"
)

const maxSelected = 2000

// noteCreate saves a note from the Source page. The browser sends the
// anchor's line, its UTF-8 byte column and the selected text; the server
// checks the text is really there before storing anything.
func (s *Server) noteCreate(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	if !s.allow(w, r, "notes", v.ID) {
		return
	}
	ctx := r.Context()
	sha, file := r.PostFormValue("sha"), r.PostFormValue("file")
	back := func(msg string) {
		to := "/source/" + sha + "?file=" + url.QueryEscape(file)
		if msg != "" {
			to += "&error=" + url.QueryEscape(msg)
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
	if !config.SHA.MatchString(sha) || !config.RelPath.MatchString(file) || !strings.HasSuffix(file, ".typ") {
		http.Error(w, "bad note request", http.StatusBadRequest)
		return
	}
	if _, err := s.Store.GetCommit(ctx, sha); err != nil {
		s.notFound(w, r)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	selected := r.PostFormValue("selected")
	line, errL := strconv.Atoi(r.PostFormValue("line"))
	col, errC := strconv.Atoi(r.PostFormValue("col"))
	switch {
	case body == "":
		back("The note is empty.")
		return
	case utf8.RuneCountInString(body) > s.Cfg.Limits.NoteMaxChars:
		back("The note is longer than " + strconv.Itoa(s.Cfg.Limits.NoteMaxChars) + " characters.")
		return
	case !utf8.ValidString(body) || !utf8.ValidString(selected):
		back("The note is not valid UTF-8.")
		return
	case errL != nil || errC != nil || line < 1 || col < 1:
		back("Select some text in the source first.")
		return
	case len(selected) > maxSelected:
		back("The selection is too long. Select at most a paragraph.")
		return
	}
	content, ok, err := s.Svc.Mirror.ReadFile(ctx, sha, file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	off, inRange := lineColOffsetBytes(content, line, col)
	if !ok || !inRange || !bytes.HasPrefix(content[off:], []byte(selected)) {
		back("The selection does not match the file. Reload the page and select again.")
		return
	}
	n, err := s.Store.CreateNote(ctx, store.Note{
		UserID: v.ID, Body: body, Selected: selected,
		Origin: store.Anchor{SHA: sha, File: file, Line: line, Col: col},
	}, s.Cfg.Limits.NotesPerCommit)
	if errors.Is(err, store.ErrTooManyNotes) {
		back("This commit already has the maximum number of open notes.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("note created", "note", n.Ref(), "user", v.ID, "sha", sha[:12])
	back("")
}

// lineColOffsetBytes turns a 1-based line and byte column into an offset.
// A column one past the end of the line is valid: it anchors at the newline.
func lineColOffsetBytes(content []byte, line, col int) (int, bool) {
	start := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(content[start:], '\n')
		if i < 0 {
			return 0, false
		}
		start += i + 1
	}
	end := len(content)
	if i := bytes.IndexByte(content[start:], '\n'); i >= 0 {
		end = start + i
	}
	off := start + col - 1
	if off > end || !utf8.Valid(content[start:off]) {
		return 0, false
	}
	return off, true
}

// ownOpenNote loads the note a URL names, if the viewer wrote it and it is
// still open: the rule for edit and delete.
func (s *Server) ownOpenNote(w http.ResponseWriter, r *http.Request) (store.Note, bool) {
	v := viewerOf(r)
	if !s.allow(w, r, "notes", v.ID) {
		return store.Note{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return store.Note{}, false
	}
	n, err := s.Store.GetNote(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return store.Note{}, false
	}
	if n.UserID != v.ID || n.State != store.NoteOpen {
		http.Error(w, "you can only change your own open notes", http.StatusForbidden)
		return store.Note{}, false
	}
	return n, true
}

func (s *Server) noteEdit(w http.ResponseWriter, r *http.Request) {
	n, ok := s.ownOpenNote(w, r)
	if !ok {
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" || !utf8.ValidString(body) || utf8.RuneCountInString(body) > s.Cfg.Limits.NoteMaxChars {
		http.Error(w, "the note must be between 1 and "+strconv.Itoa(s.Cfg.Limits.NoteMaxChars)+" characters", http.StatusBadRequest)
		return
	}
	if err := s.Store.UpdateNoteBody(r.Context(), n.ID, body); err != nil {
		s.fail(w, r, err)
		return
	}
	s.backToNotes(w, r)
}

func (s *Server) noteDelete(w http.ResponseWriter, r *http.Request) {
	n, ok := s.ownOpenNote(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteNote(r.Context(), n.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("note deleted", "note", n.Ref(), "user", n.UserID)
	s.backToNotes(w, r)
}

func (s *Server) noteResolve(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	if !s.allow(w, r, "notes", v.ID) {
		return
	}
	if !v.IsAuthor() {
		http.Error(w, "only the author resolves notes", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	n, err := s.Store.GetNote(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if n.State != store.NoteResolved {
		if err := s.Store.SetNoteState(r.Context(), n.ID, store.NoteResolved, v.ID); err != nil {
			s.fail(w, r, err)
			return
		}
		s.Log.Info("note resolved", "note", n.Ref(), "by", v.ID)
	}
	s.backToNotes(w, r)
}

// backToNotes returns to the page the form was on, if it is on this site.
func (s *Server) backToNotes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, safeNext(r.PostFormValue("back")), http.StatusSeeOther)
}
