// Package web serves the review UI, the stable PDF URLs and the webhook.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/service"
	"git.benmandrew.com/me/margins/internal/store"
	"git.benmandrew.com/me/margins/internal/webhook"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	Svc     *service.Service
	Cfg     config.Config
	Store   *store.Store
	Log     *slog.Logger
	Webhook *webhook.Handler

	wa         *webauthn.WebAuthn
	ceremonies ceremonies
	limiter    limiter
	pages      map[string]*template.Template
	// secure marks cookies Secure. It is false only for a plain-HTTP
	// localhost origin in development.
	secure bool
}

func New(svc *service.Service, secrets config.Secrets) (*Server, error) {
	s := &Server{
		Svc:   svc,
		Cfg:   svc.Cfg,
		Store: svc.Store,
		Log:   svc.Log,
	}
	s.secure = strings.HasPrefix(s.Cfg.Origin, "https://")
	var err error
	if s.wa, err = newWebAuthn(s.Cfg); err != nil {
		return nil, err
	}
	if s.pages, err = parseTemplates(); err != nil {
		return nil, err
	}
	s.Webhook = &webhook.Handler{
		Forge:      webhook.Gitea{},
		Secret:     []byte(secrets.WebhookSecret),
		Repo:       s.Cfg.Repo.FullName,
		Deliveries: deliveries{s.Store},
		Enqueue: func(webhook.Push) error {
			// The payload only says something changed; the fetch works out
			// what from the mirror.
			return svc.EnqueueFetch(context.Background())
		},
		Log: s.Log,
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	auth := s.requireLogin

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("POST /hooks/push", s.Webhook)

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login/begin", s.requireOrigin(s.loginBegin))
	mux.HandleFunc("POST /login/finish", s.requireOrigin(s.loginFinish))
	mux.HandleFunc("GET /enrol/{id}", s.enrolPage)
	mux.HandleFunc("POST /enrol/{id}/begin", s.requireOrigin(s.enrolBegin))
	mux.HandleFunc("POST /enrol/{id}/finish", s.requireOrigin(s.enrolFinish))
	mux.HandleFunc("POST /logout", auth(s.logout))

	mux.HandleFunc("GET /{$}", auth(s.timeline))
	mux.HandleFunc("GET /c/{name}", auth(s.commitRoute))
	mux.HandleFunc("GET /c/{name}/{entry...}", auth(s.commitRoute))
	mux.HandleFunc("GET /source/{sha}", auth(s.sourcePage))
	mux.HandleFunc("GET /compare", auth(s.comparePage))
	mux.HandleFunc("GET /diff/{name}", auth(s.diffRoute))
	mux.HandleFunc("GET /diff/{name}/{entry...}", auth(s.diffRoute))
	mux.HandleFunc("POST /diff", auth(s.diffRequest))
	mux.HandleFunc("POST /build", auth(s.buildRequest))
	mux.HandleFunc("GET /latest.pdf", auth(s.latest))
	mux.HandleFunc("GET /latest.pdf/{entry...}", auth(s.latest))
	mux.HandleFunc("GET /review/latest.pdf", auth(s.reviewLatest))
	mux.HandleFunc("GET /review/latest.pdf/{entry...}", auth(s.reviewLatest))
	mux.HandleFunc("GET /review/changes.pdf", auth(s.reviewChanges))
	mux.HandleFunc("GET /review/changes.pdf/{entry...}", auth(s.reviewChanges))

	mux.HandleFunc("GET /notes", auth(s.notesPage))
	mux.HandleFunc("POST /notes", auth(s.noteCreate))
	mux.HandleFunc("POST /notes/{id}/edit", auth(s.noteEdit))
	mux.HandleFunc("POST /notes/{id}/delete", auth(s.noteDelete))
	mux.HandleFunc("POST /notes/{id}/resolve", auth(s.noteResolve))

	// Anything else still needs a login before it may even 404, so an
	// anonymous visitor learns nothing about which paths exist.
	mux.HandleFunc("/", auth(func(w http.ResponseWriter, r *http.Request) {
		s.notFound(w, r)
	}))
	return securityHeaders(mux)
}

// securityHeaders sets the headers every response carries. Handlers set
// Cache-Control; the default here is the strictest.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Cache-Control", "private, no-store")
		h.ServeHTTP(w, r)
	})
}

func staticHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static files are public: the login page needs them. They change
		// only with a new image, so an hour of caching is safe.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

type deliveries struct{ st *store.Store }

func (d deliveries) Seen(id string) (bool, error) { return d.st.SeenDelivery(id) }

// --- Templates ---

var funcs = template.FuncMap{
	"short": func(sha string) string {
		if len(sha) > 7 {
			return sha[:7]
		}
		return sha
	},
	"date": func(unix int64) string {
		if unix == 0 {
			return ""
		}
		return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04")
	},
	"entryName": service.EntryName,
	"esc":       url.PathEscape,
	"query":     url.QueryEscape,
	"list":      func(xs ...string) []string { return xs },
}

func parseTemplates() (map[string]*template.Template, error) {
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		pages[base] = t
	}
	return pages, nil
}

// page is what every template receives.
type page struct {
	Title  string
	Viewer viewer
	Data   any
	// Refresh, in seconds, reloads a page that waits on a job, with no
	// script.
	Refresh int
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, name, title string, data any) {
	s.renderPage(w, r, code, name, page{Title: title, Viewer: viewerOf(r), Data: data})
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.fail(w, r, errors.New("no template "+name))
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "message", "Not found", message{
		Heading: "Not found", Text: "There is nothing at this address.",
	})
}

type message struct {
	Heading string
	Text    string
	Log     string
	Form    *requestForm
}

// requestForm is a one-button POST that starts work a cross-site link may
// not start by itself.
type requestForm struct {
	Action string
	Fields map[string]string
	Label  string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); ok {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Log in", nil)
}

func (s *Server) enrolPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.Cfg.User(r.PathValue("id"))
	if !ok {
		s.render(w, r, http.StatusNotFound, "message", "Not found", message{Heading: "Not found", Text: "This enrolment link is not valid."})
		return
	}
	s.render(w, r, http.StatusOK, "enrol", "Set up your passkey", struct{ ID, Name string }{u.ID, u.Name})
}

// sameSiteRequest reports whether a GET that starts work came from this site
// or from the address bar. A link on another site gets a confirmation button
// instead, so only this site's pages and a person's own navigation start
// builds without a CSRF token.
func sameSiteRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}
