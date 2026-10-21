package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/store"
)

const (
	sessionCookie   = "__Host-session"
	sessionLifetime = 30 * 24 * time.Hour
	maxAuthBody     = 64 << 10
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// --- Login ---

type loginData struct {
	Next  string
	Error string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Log in", loginData{Next: safeNext(r.URL.Query().Get("next"))})
}

// login checks the secret from the login form. The secret alone names the
// user, as the config gives each user a different one.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "login", "") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	next := safeNext(r.PostFormValue("next"))
	u, ok := s.userBySecret(strings.TrimSpace(r.PostFormValue("secret")))
	if !ok {
		s.Log.Info("login rejected")
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, r, http.StatusUnauthorized, "login", "Log in", loginData{Next: next, Error: "That secret is not valid."})
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("logged in", "user", u.ID)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// userBySecret compares the secret's hash with every user's, so the time
// taken does not depend on which user it belongs to.
func (s *Server) userBySecret(secret string) (config.User, bool) {
	if secret == "" || len(secret) > 512 {
		return config.User{}, false
	}
	hash := []byte(hashToken(secret))
	var found config.User
	ok := false
	for _, u := range s.Cfg.Users {
		if subtle.ConstantTimeCompare(hash, []byte(u.SecretHash)) == 1 {
			found, ok = u, true
		}
	}
	return found, ok
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		s.Store.DeleteSession(r.Context(), hashToken(ck.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: s.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps a post-login redirect on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// --- Sessions ---

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u config.User) error {
	token := randomToken()
	err := s.Store.CreateSession(r.Context(), store.Session{
		Hash: hashToken(token), UserID: u.ID, CSRF: randomToken(), SecretHash: u.SecretHash,
		ExpiresAt: time.Now().Add(sessionLifetime).Unix(),
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionLifetime.Seconds()),
		Secure: s.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

type ctxKey struct{}

// viewer is the logged-in user of a request.
type viewer struct {
	config.User
	CSRF string
}

func (v viewer) IsAuthor() bool { return v.Role == config.RoleAuthor }

func viewerOf(r *http.Request) viewer {
	v, _ := r.Context().Value(ctxKey{}).(viewer)
	return v
}

func (s *Server) session(r *http.Request) (viewer, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" || len(ck.Value) > 128 {
		return viewer{}, false
	}
	sess, err := s.Store.GetSession(r.Context(), hashToken(ck.Value))
	if err != nil {
		return viewer{}, false
	}
	// A user removed from the config, or given a new secret, is logged out.
	u, ok := s.Cfg.User(sess.UserID)
	if !ok || sess.SecretHash != u.SecretHash {
		return viewer{}, false
	}
	return viewer{User: u, CSRF: sess.CSRF}, true
}

// requireLogin wraps a handler that needs a session. A GET without one is
// sent to the login page; anything else gets 401. Every POST must also carry
// the session's CSRF token and this site's Origin.
func (s *Server) requireLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.session(r)
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(v.CSRF)) != 1 {
				http.Error(w, "missing or wrong CSRF token", http.StatusForbidden)
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, v)))
	}
}

// requireOrigin guards the login POST, which runs before a session exists.
func (s *Server) requireOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.Cfg.Origin
}

// --- Rate limits ---

// limiter is a token bucket per key: rate per minute, with a burst of the
// same size.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

var limits = map[string]float64{
	"login": 20,
	"notes": 30,
}

func (l *limiter) allow(key string, perMinute float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	now := time.Now()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: perMinute, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * perMinute
	if b.tokens > perMinute {
		b.tokens = perMinute
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP is the address nginx put in the configured header. Without that
// header (a direct request in development) it is the peer address.
func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.ClientIPHeader != "" {
		if ip := strings.TrimSpace(r.Header.Get(s.Cfg.ClientIPHeader)); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allow applies an action's rate limit per client address and, when userID
// is set, per user. It writes 429 and returns false when over.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, action, userID string) bool {
	rate := limits[action]
	ok := s.limiter.allow(action+"|ip|"+s.clientIP(r), rate)
	if ok && userID != "" {
		ok = s.limiter.allow(action+"|user|"+userID, rate)
	}
	if !ok {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests; try again in a minute", http.StatusTooManyRequests)
	}
	return ok
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
