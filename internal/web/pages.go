package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/service"
	"git.benmandrew.com/me/margins/internal/store"
)

// resolve turns a commit name from a URL into a full SHA the service knows.
// canonical is false when the name was a short SHA or a ref, so the caller
// redirects to the full-SHA URL.
func (s *Server) resolve(ctx context.Context, name string) (sha string, canonical bool, err error) {
	if config.SHA.MatchString(name) {
		if _, err := s.Store.GetCommit(ctx, name); err != nil {
			return "", false, err
		}
		return name, true, nil
	}
	if config.ShortSHA.MatchString(name) {
		if sha, err := s.Store.ResolveSHA(ctx, name); err == nil {
			return sha, false, nil
		}
	}
	if config.ValidRef(name) {
		for _, prefix := range []string{"refs/tags/", "refs/heads/"} {
			ref, err := s.Store.GetRef(ctx, prefix+name)
			if err == nil && !ref.Gone {
				if _, err := s.Store.GetCommit(ctx, ref.SHA); err == nil {
					return ref.SHA, false, nil
				}
			}
		}
	}
	return "", false, store.ErrNotFound
}

// entryFor picks the entrypoint a URL names, or the commit's first one.
func (s *Server) entryFor(ctx context.Context, sha, name string) (string, error) {
	bc, err := s.Svc.BuildConfig(ctx, sha)
	if err != nil {
		bc = s.Cfg.Defaults
	}
	if name == "" {
		return bc.Entrypoints[0], nil
	}
	if slices.Contains(bc.Entrypoints, name+".typ") {
		return name + ".typ", nil
	}
	return "", store.ErrNotFound
}

func entrySuffix(entry, first string) string {
	if entry == first {
		return ""
	}
	return "/" + service.EntryName(entry)
}

func redirectMoving(w http.ResponseWriter, r *http.Request, to string) {
	if r.URL.RawQuery != "" && !strings.Contains(to, "?") {
		to += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, to, http.StatusFound)
}

// --- Timeline ---

type timelineRow struct {
	Commit     store.Commit
	Tags       []string
	Builds     []store.Build
	FirstEntry string
	Notes      int
	Parent     string
}

type timelineData struct {
	Branch   string
	Branches []string
	Rows     []timelineRow
	Missing  bool
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	branches, _ := s.Svc.Tracked(ctx)
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = s.Cfg.Repo.DefaultBranch
	}
	data := timelineData{Branch: branch, Branches: branches}
	ref, err := s.Store.GetRef(ctx, "refs/heads/"+branch)
	if !config.ValidRef(branch) || err != nil || ref.Gone {
		data.Missing = true
		s.render(w, r, http.StatusOK, "timeline", "Timeline", data)
		return
	}
	shas, err := s.Svc.Mirror.RevList(ctx, ref.SHA, nil, s.Cfg.Limits.BackfillDepth)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tags, err := s.Store.Refs(ctx, "tag")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tagsAt := map[string][]string{}
	for _, t := range tags {
		tagsAt[t.SHA] = append(tagsAt[t.SHA], t.Short())
	}
	counts, err := s.Store.OpenNoteCounts(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, sha := range shas {
		c, err := s.Svc.Commit(ctx, sha)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		builds, err := s.latestBuilds(ctx, sha)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		row := timelineRow{Commit: c, Tags: tagsAt[sha], Builds: builds, Notes: counts[sha]}
		if len(c.Parents) > 0 {
			row.Parent = c.Parents[0]
		}
		if len(builds) > 0 {
			row.FirstEntry = builds[0].Entry
		}
		data.Rows = append(data.Rows, row)
	}
	s.render(w, r, http.StatusOK, "timeline", "Timeline", data)
}

// latestBuilds returns the newest build of each entrypoint the commit's
// config names, in config order.
func (s *Server) latestBuilds(ctx context.Context, sha string) ([]store.Build, error) {
	bc, err := s.Svc.BuildConfig(ctx, sha)
	if err != nil {
		bc = s.Cfg.Defaults
	}
	var out []store.Build
	for _, e := range bc.Entrypoints {
		b, err := s.Store.CurrentBuild(ctx, sha, e)
		if errors.Is(err, store.ErrNotFound) {
			out = append(out, store.Build{SHA: sha, Entry: e, Status: store.Queued})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// --- Commit routes: page, PDF, annotated PDF, log ---

func (s *Server) commitRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var kind string
	for _, suffix := range []string{".notes.pdf", ".pdf", ".log"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			name, kind = base, suffix
			break
		}
	}
	entryName := r.PathValue("entry")
	if kind == "" && entryName != "" {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	sha, canonical, err := s.resolve(ctx, name)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if !canonical {
		to := "/c/" + sha + kind
		if entryName != "" {
			to += "/" + entryName
		}
		redirectMoving(w, r, to)
		return
	}
	if kind == "" {
		s.commitPage(w, r, sha)
		return
	}
	entry, err := s.entryFor(ctx, sha, entryName)
	if err != nil {
		s.notFound(w, r)
		return
	}
	switch kind {
	case ".pdf":
		s.buildPDF(w, r, sha, entry)
	case ".notes.pdf":
		s.annotatedPDF(w, r, sha, entry)
	case ".log":
		s.serveLog(w, r, s.Svc.BuildLogPath(sha, entry))
	}
}

func (s *Server) buildPDF(w http.ResponseWriter, r *http.Request, sha, entry string) {
	ctx := r.Context()
	b, err := s.Store.CurrentBuild(ctx, sha, entry)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	switch {
	case err == nil && b.Status == store.OK && !b.Pruned:
		s.servePDF(w, r, s.Svc.BuildPath(sha, entry), pdfName(sha, entry, ""), "private, max-age=31536000, immutable")
		return
	case err == nil && (b.Status == store.Failed || b.Status == store.Timeout && b.Attempts >= s.Cfg.Limits.MaxAttempts):
		s.showFailure(w, r, "The build failed", fmt.Sprintf("Commit %s, %s, Typst %s: %s.", sha[:7], entry, b.Typst, b.Status), s.Svc.BuildLogPath(sha, entry))
		return
	}
	if !sameSiteRequest(r) {
		s.confirm(w, r, "Build this commit?", "/build", map[string]string{"sha": sha, "entry": entry}, "Build")
		return
	}
	if _, err := s.Svc.RequestBuild(ctx, sha, entry); err != nil {
		s.fail(w, r, err)
		return
	}
	s.pending(w, r, "Building "+sha[:7], "This PDF is being built. The page reloads until it is ready.")
}

func (s *Server) annotatedPDF(w http.ResponseWriter, r *http.Request, sha, entry string) {
	ctx := r.Context()
	if b, err := s.Store.CurrentBuild(ctx, sha, entry); err != nil || b.Status != store.OK {
		// The annotated build needs the commit to compile at all.
		s.buildPDF(w, r, sha, entry)
		return
	}
	a, ok, err := s.Svc.RequestAnnotated(ctx, sha, entry, sameSiteRequest(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		redirectMoving(w, r, "/c/"+sha+".pdf"+entrySuffix(entry, s.firstEntry(ctx, sha)))
		return
	}
	switch {
	case a.Status == store.OK && !a.Pruned:
		// Notes change, so the URL does too: revalidate every time.
		s.servePDF(w, r, s.Svc.AnnotatedPath(sha, entry, a.NotesHash), pdfName(sha, entry, "-notes"), "private, no-cache")
	case a.Status == store.Failed:
		s.showFailure(w, r, "The annotated build failed", "Every placement fallback failed to compile.", s.Svc.AnnotatedLogPath(sha, entry, a.NotesHash))
	case !sameSiteRequest(r):
		s.confirm(w, r, "Build the annotated PDF?", "/build", map[string]string{"sha": sha, "entry": entry, "notes": "1"}, "Build")
	default:
		s.pending(w, r, "Building the annotated PDF", "The notes are being placed. The page reloads until the PDF is ready.")
	}
}

func (s *Server) firstEntry(ctx context.Context, sha string) string {
	e, _ := s.entryFor(ctx, sha, "")
	return e
}

func pdfName(sha, entry, suffix string) string {
	name := "thesis-" + sha[:7]
	if entry != "main.typ" {
		name += "-" + strings.ReplaceAll(service.EntryName(entry), "/", "-")
	}
	return name + suffix + ".pdf"
}

func (s *Server) servePDF(w http.ResponseWriter, r *http.Request, path, filename, cache string) {
	f, err := os.Open(path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	disposition := "inline"
	if r.URL.Query().Has("download") {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Disposition", disposition+`; filename="`+filename+`"`)
	h.Set("Cache-Control", cache)
	// The Commit and Compare pages frame PDFs from this site.
	h.Set("Content-Security-Policy", "frame-ancestors 'self'")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	http.ServeContent(w, r, filename, fi.ModTime(), f)
}

func (s *Server) serveLog(w http.ResponseWriter, r *http.Request, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Write(data)
}

func (s *Server) pending(w http.ResponseWriter, r *http.Request, heading, text string) {
	s.renderPage(w, r, http.StatusAccepted, "message", page{
		Title: heading, Viewer: viewerOf(r), Refresh: 5,
		Data: message{Heading: heading, Text: text},
	})
}

func (s *Server) showFailure(w http.ResponseWriter, r *http.Request, heading, text, logPath string) {
	log, _ := os.ReadFile(logPath)
	s.render(w, r, http.StatusOK, "message", heading, message{Heading: heading, Text: text, Log: string(log)})
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, heading, action string, fields map[string]string, label string) {
	s.render(w, r, http.StatusOK, "message", heading, message{
		Heading: heading,
		Text:    "You followed a link from another site. Confirm to start the build.",
		Form:    &requestForm{Action: action, Fields: fields, Label: label},
	})
}

// buildRequest is the confirmation POST for a build or an annotated build.
func (s *Server) buildRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sha := r.PostFormValue("sha")
	if !config.SHA.MatchString(sha) {
		s.notFound(w, r)
		return
	}
	if _, err := s.Store.GetCommit(ctx, sha); err != nil {
		s.notFound(w, r)
		return
	}
	entry, err := s.entryFor(ctx, sha, service.EntryName(r.PostFormValue("entry")))
	if err != nil {
		s.notFound(w, r)
		return
	}
	to := "/c/" + sha + ".pdf"
	if r.PostFormValue("notes") != "" {
		to = "/c/" + sha + ".notes.pdf"
		if _, _, err := s.Svc.RequestAnnotated(ctx, sha, entry, true); err != nil {
			s.fail(w, r, err)
			return
		}
	} else if _, err := s.Svc.RequestBuild(ctx, sha, entry); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to+entrySuffix(entry, s.firstEntry(ctx, sha)), http.StatusSeeOther)
}

type entryView struct {
	Entry     string
	Name      string
	Suffix    string
	Build     store.Build
	HasBuild  bool
	Log       string
	Annotated *store.Annotated
}

type commitData struct {
	Commit   store.Commit
	Tags     []string
	Entries  []entryView
	Selected entryView
	Parent   string
	Notes    []store.PlacedNote
	Branch   bool
}

func (s *Server) commitPage(w http.ResponseWriter, r *http.Request, sha string) {
	ctx := r.Context()
	c, err := s.Svc.Commit(ctx, sha)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := commitData{Commit: c}
	if len(c.Parents) > 0 {
		data.Parent = c.Parents[0]
	}
	tags, _ := s.Store.Refs(ctx, "tag")
	for _, t := range tags {
		if t.SHA == sha {
			data.Tags = append(data.Tags, t.Short())
		}
	}
	builds, err := s.latestBuilds(ctx, sha)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	want := r.URL.Query().Get("entry")
	first := ""
	for i, b := range builds {
		if i == 0 {
			first = b.Entry
		}
		v := entryView{Entry: b.Entry, Name: service.EntryName(b.Entry), Suffix: entrySuffix(b.Entry, first), Build: b, HasBuild: b.ID != 0}
		data.Entries = append(data.Entries, v)
	}
	if len(data.Entries) == 0 {
		s.notFound(w, r)
		return
	}
	data.Selected = data.Entries[0]
	for _, v := range data.Entries {
		if v.Name == want {
			data.Selected = v
		}
	}
	sel := &data.Selected
	if sel.Build.Status == store.Failed || sel.Build.Status == store.Timeout {
		log, _ := os.ReadFile(s.Svc.BuildLogPath(sha, sel.Entry))
		sel.Log = string(log)
	}
	if a, err := s.Store.LatestAnnotated(ctx, sha, sel.Entry); err == nil {
		sel.Annotated = &a
	}
	if data.Notes, err = s.Svc.AnnotationNotes(ctx, sha); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "commit", "Commit "+sha[:7], data)
}

// --- Source and notes on it ---

type sourceLine struct {
	N     int
	Text  string
	Notes []store.PlacedNote
}

type sourceData struct {
	Commit   store.Commit
	Files    []string
	File     string
	Lines    []sourceLine
	CanWrite bool
	MaxChars int
	Error    string
}

func (s *Server) sourcePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sha, canonical, err := s.resolve(ctx, r.PathValue("sha"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if !canonical {
		redirectMoving(w, r, "/source/"+sha)
		return
	}
	c, err := s.Svc.Commit(ctx, sha)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.Svc.Mirror.TypFiles(ctx, sha)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := sourceData{Commit: c, Files: files, MaxChars: s.Cfg.Limits.NoteMaxChars, CanWrite: true, Error: r.URL.Query().Get("error")}
	file := r.URL.Query().Get("file")
	if file == "" {
		file = s.firstEntry(ctx, sha)
	}
	if !slices.Contains(files, file) {
		if len(files) == 0 {
			s.render(w, r, http.StatusOK, "source", "Source "+sha[:7], data)
			return
		}
		file = files[0]
	}
	data.File = file
	content, _, err := s.Svc.Mirror.ReadFile(ctx, sha, file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	notes, err := s.Store.NotesAt(ctx, sha, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byLine := map[int][]store.PlacedNote{}
	for _, n := range notes {
		if n.At.File == file {
			byLine[n.At.Line] = append(byLine[n.At.Line], n)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		data.Lines = append(data.Lines, sourceLine{N: n, Text: sc.Text(), Notes: byLine[n]})
	}
	s.render(w, r, http.StatusOK, "source", file+" at "+sha[:7], data)
}

type noteView struct {
	store.Note
	User      string
	Placement string
	Mine      bool
}

type notesData struct {
	Notes []noteView
	State string
	User  string
	SHA   string
	Users []config.User
}

func (s *Server) notesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = string(store.NoteOpen)
	}
	filter := store.NoteState(state)
	if state == "all" {
		filter = ""
	}
	user := q.Get("user")
	if user != "" && !config.UserID.MatchString(user) {
		user = ""
	}
	sha := q.Get("sha")
	if sha != "" && !config.SHA.MatchString(sha) {
		sha = ""
	}
	notes, err := s.Store.Notes(ctx, filter, user, sha)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := viewerOf(r)
	data := notesData{State: state, User: user, SHA: sha, Users: s.Cfg.Users}
	placements := map[string]map[string]string{}
	for _, n := range notes {
		nv := noteView{Note: n, User: n.UserID, Mine: n.UserID == v.ID}
		if u, ok := s.Cfg.User(n.UserID); ok {
			nv.User = u.Name
		}
		p, ok := placements[n.Origin.SHA]
		if !ok {
			p = map[string]string{}
			if a, err := s.Store.LatestAnnotated(ctx, n.Origin.SHA, s.firstEntry(ctx, n.Origin.SHA)); err == nil {
				p = parsePlacements(a.Placements)
			}
			placements[n.Origin.SHA] = p
		}
		nv.Placement = p[n.Ref()]
		data.Notes = append(data.Notes, nv)
	}
	s.render(w, r, http.StatusOK, "notes", "Notes", data)
}

// --- Stable moving URLs ---

func (s *Server) latest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tip, err := s.Store.GetRef(ctx, "refs/heads/"+s.Cfg.Repo.DefaultBranch)
	if err != nil || tip.Gone {
		s.notFound(w, r)
		return
	}
	shas, err := s.Svc.Mirror.RevList(ctx, tip.SHA, nil, s.Cfg.Limits.BackfillDepth)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := r.PathValue("entry")
	for _, sha := range shas {
		entry, err := s.entryFor(ctx, sha, name)
		if err != nil {
			continue
		}
		if b, err := s.Store.CurrentBuild(ctx, sha, entry); err == nil && b.Status == store.OK {
			redirectMoving(w, r, "/c/"+sha+".pdf"+entrySuffix(entry, s.firstEntry(ctx, sha)))
			return
		}
	}
	s.render(w, r, http.StatusNotFound, "message", "No build yet", message{Heading: "No build yet", Text: "No commit on the default branch has built successfully yet."})
}

func (s *Server) reviewLatest(w http.ResponseWriter, r *http.Request) {
	tags, err := s.Svc.ReviewTags(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(tags) == 0 {
		s.render(w, r, http.StatusNotFound, "message", "No review tag", message{Heading: "No review tag", Text: "No review tag has been pushed yet."})
		return
	}
	suffix := ""
	if e := r.PathValue("entry"); e != "" {
		suffix = "/" + e
	}
	redirectMoving(w, r, "/c/"+tags[len(tags)-1].SHA+".pdf"+suffix)
}

func (s *Server) reviewChanges(w http.ResponseWriter, r *http.Request) {
	tags, err := s.Svc.ReviewTags(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(tags) < 2 {
		s.render(w, r, http.StatusNotFound, "message", "Not enough review tags", message{Heading: "Not enough review tags", Text: "The changes need two review tags."})
		return
	}
	suffix := ""
	if e := r.PathValue("entry"); e != "" {
		suffix = "/" + e
	}
	a, b := tags[len(tags)-2], tags[len(tags)-1]
	redirectMoving(w, r, "/diff/"+a.SHA+".."+b.SHA+".pdf"+suffix)
}

// --- Diffs ---

// parsePair splits "<a>..<b><suffix>".
func parsePair(name string) (a, b, kind string, ok bool) {
	for _, suffix := range []string{".pdf", ".txt", ".log"} {
		if base, found := strings.CutSuffix(name, suffix); found {
			a, b, ok = strings.Cut(base, "..")
			return a, b, suffix, ok && a != "" && b != ""
		}
	}
	return "", "", "", false
}

func (s *Server) diffRoute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	aName, bName, kind, ok := parsePair(r.PathValue("name"))
	if !ok {
		s.notFound(w, r)
		return
	}
	a, canonA, errA := s.resolve(ctx, aName)
	b, canonB, errB := s.resolve(ctx, bName)
	if errA != nil || errB != nil {
		s.render(w, r, http.StatusNotFound, "message", "Unknown commit", message{
			Heading: "Unknown commit", Text: "A diff can only compare commits and tags the service already knows.",
		})
		return
	}
	entryName := r.PathValue("entry")
	if !canonA || !canonB {
		to := "/diff/" + a + ".." + b + kind
		if entryName != "" {
			to += "/" + entryName
		}
		redirectMoving(w, r, to)
		return
	}
	entry, err := s.entryFor(ctx, b, entryName)
	if err != nil {
		s.notFound(w, r)
		return
	}
	switch kind {
	case ".log":
		s.serveLog(w, r, s.Svc.DiffLogPath(a, b, entry))
		return
	case ".txt":
		redirectMoving(w, r, "/compare?a="+a+"&b="+b+"&entry="+service.EntryName(entry))
		return
	}
	d, err := s.Store.FindDiff(ctx, a, b, entry)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if err == nil && d.Status == store.OK && !d.Pruned {
		switch d.Method {
		case service.MethodTypdiff, service.MethodDiffPDF:
			name := fmt.Sprintf("diff-%s..%s.pdf", a[:7], b[:7])
			s.servePDF(w, r, s.Svc.DiffPath(a, b, entry, d.Method), name, "private, max-age=31536000, immutable")
		default:
			redirectMoving(w, r, "/compare?a="+a+"&b="+b+"&entry="+service.EntryName(entry))
		}
		return
	}
	if err == nil && (d.Status == store.Failed || d.Status == store.Timeout && d.Attempts >= s.Cfg.Limits.MaxAttempts) {
		s.showFailure(w, r, "The diff failed", "No diff method succeeded. The log below has each method's output.", s.Svc.DiffLogPath(a, b, entry))
		return
	}
	if !sameSiteRequest(r) {
		s.confirm(w, r, "Build this diff?", "/diff", map[string]string{"a": a, "b": b, "entry": entry}, "Build the diff")
		return
	}
	if _, err := s.Svc.RequestDiff(ctx, a, b, entry); err != nil {
		s.diffRequestError(w, r, err)
		return
	}
	s.pending(w, r, "Building the diff", "The diff is being built. The page reloads until it is ready.")
}

func (s *Server) diffRequestError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, service.ErrQueueFull) {
		w.Header().Set("Retry-After", "60")
		s.render(w, r, http.StatusServiceUnavailable, "message", "Busy", message{
			Heading: "Too many diffs queued", Text: "Try again in a minute.",
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	s.fail(w, r, err)
}

// diffRequest is the POST that starts a diff from the Compare page or a
// cross-site confirmation.
func (s *Server) diffRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, _, errA := s.resolve(ctx, r.PostFormValue("a"))
	b, _, errB := s.resolve(ctx, r.PostFormValue("b"))
	if errA != nil || errB != nil {
		s.render(w, r, http.StatusNotFound, "message", "Unknown commit", message{
			Heading: "Unknown commit", Text: "A diff can only compare commits and tags the service already knows.",
		})
		return
	}
	entry, err := s.entryFor(ctx, b, service.EntryName(r.PostFormValue("entry")))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if _, err := s.Svc.RequestDiff(ctx, a, b, entry); err != nil {
		s.diffRequestError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/compare?a="+a+"&b="+b+"&entry="+service.EntryName(entry), http.StatusSeeOther)
}

type choice struct {
	Value, Label string
}

type diffLine struct {
	Class, Text string
}

type compareData struct {
	Choices   []choice
	A, B      string
	AName     string
	BName     string
	Entry     string
	EntryName string
	Entries   []string
	Diff      *store.Diff
	Method    string
	PDFURL    string
	Text      []diffLine
	TextCut   bool
	Log       string
	Pending   bool
}

func (s *Server) comparePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	data := compareData{AName: q.Get("a"), BName: q.Get("b")}
	data.Choices = s.choices(ctx)
	if data.AName == "" || data.BName == "" {
		tags, _ := s.Svc.ReviewTags(ctx)
		if len(tags) >= 2 && data.AName == "" && data.BName == "" {
			data.AName, data.BName = tags[len(tags)-2].Short(), tags[len(tags)-1].Short()
		} else {
			s.render(w, r, http.StatusOK, "compare", "Compare", data)
			return
		}
	}
	a, canonA, errA := s.resolve(ctx, data.AName)
	b, canonB, errB := s.resolve(ctx, data.BName)
	if errA != nil || errB != nil {
		s.render(w, r, http.StatusNotFound, "message", "Unknown commit", message{
			Heading: "Unknown commit", Text: "A diff can only compare commits and tags the service already knows.",
		})
		return
	}
	entry, err := s.entryFor(ctx, b, q.Get("entry"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if !canonA || !canonB {
		redirectMoving(w, r, "/compare?a="+a+"&b="+b+"&entry="+service.EntryName(entry))
		return
	}
	data.A, data.B, data.Entry, data.EntryName = a, b, entry, service.EntryName(entry)
	if bc, err := s.Svc.BuildConfig(ctx, b); err == nil {
		data.Entries = bc.Entrypoints
	}
	d, err := s.Store.FindDiff(ctx, a, b, entry)
	if err == nil {
		data.Diff = &d
		data.Method = d.Method
	}
	suffix := entrySuffix(entry, s.firstEntry(ctx, b))
	switch {
	case data.Diff == nil:
		// Not requested yet: the page offers a button.
	case d.Status == store.OK && !d.Pruned:
		if d.Method == service.MethodTypdiff || d.Method == service.MethodDiffPDF {
			data.PDFURL = "/diff/" + a + ".." + b + ".pdf" + suffix
		}
		data.Text, data.TextCut = readTextDiff(s.Svc.TextDiffPath(a, b, entry))
	case d.Status == store.Failed, d.Status == store.Timeout && d.Attempts >= s.Cfg.Limits.MaxAttempts:
		log, _ := os.ReadFile(s.Svc.DiffLogPath(a, b, entry))
		data.Log = string(log)
	default:
		data.Pending = true
	}
	p := page{Title: "Compare", Viewer: viewerOf(r), Data: data}
	if data.Pending {
		p.Refresh = 5
	}
	s.renderPage(w, r, http.StatusOK, "compare", p)
}

const maxTextDiffLines = 5000

func readTextDiff(path string) ([]diffLine, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var out []diffLine
	for i, l := range strings.Split(string(data), "\n") {
		if i < 4 && (strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ")) {
			continue
		}
		if len(out) >= maxTextDiffLines {
			return out, true
		}
		class := "ctx"
		switch {
		case strings.HasPrefix(l, "@@"):
			class = "hunk"
		case strings.HasPrefix(l, "+"):
			class = "add"
		case strings.HasPrefix(l, "-"):
			class = "del"
		}
		out = append(out, diffLine{class, l})
	}
	return out, false
}

// choices are the commits the Compare page offers: review tags, branch tips
// and recent commits on the default branch.
func (s *Server) choices(ctx context.Context) []choice {
	var out []choice
	if tags, err := s.Svc.ReviewTags(ctx); err == nil {
		for i := len(tags) - 1; i >= 0; i-- {
			out = append(out, choice{tags[i].Short(), "tag " + tags[i].Short()})
		}
	}
	branches, _ := s.Svc.Tracked(ctx)
	for _, br := range branches {
		if ref, err := s.Store.GetRef(ctx, "refs/heads/"+br); err == nil && !ref.Gone {
			out = append(out, choice{ref.SHA, "branch " + br + " (" + ref.SHA[:7] + ")"})
		}
	}
	if tip, err := s.Store.GetRef(ctx, "refs/heads/"+s.Cfg.Repo.DefaultBranch); err == nil {
		shas, _ := s.Svc.Mirror.RevList(ctx, tip.SHA, nil, 50)
		for _, sha := range shas {
			c, err := s.Svc.Commit(ctx, sha)
			if err != nil {
				continue
			}
			subject := c.Subject
			if len(subject) > 60 {
				subject = subject[:57] + "…"
			}
			out = append(out, choice{sha, sha[:7] + " " + subject})
		}
	}
	return out
}

func parsePlacements(js string) map[string]string {
	out := map[string]string{}
	json.Unmarshal([]byte(js), &out)
	return out
}
