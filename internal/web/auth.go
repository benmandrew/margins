package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"git.benmandrew.com/me/margins/internal/config"
	"git.benmandrew.com/me/margins/internal/store"
)

const (
	sessionCookie   = "__Host-session"
	ceremonyCookie  = "__Host-ceremony"
	sessionLifetime = 30 * 24 * time.Hour
	ceremonyTTL     = 5 * time.Minute
	maxAuthBody     = 64 << 10
)

// waUser adapts a configured user to the WebAuthn library. The user handle
// is the user ID, which the config validates and which never changes.
type waUser struct {
	user  config.User
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return []byte(u.user.ID) }
func (u waUser) WebAuthnName() string                       { return u.user.ID }
func (u waUser) WebAuthnDisplayName() string                { return u.user.Name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (s *Server) loadUser(ctx context.Context, id string) (waUser, error) {
	u, ok := s.Cfg.User(id)
	if !ok {
		return waUser{}, errors.New("unknown user")
	}
	stored, err := s.Store.Credentials(ctx, id)
	if err != nil {
		return waUser{}, err
	}
	wu := waUser{user: u}
	for _, c := range stored {
		var cred webauthn.Credential
		if err := json.Unmarshal([]byte(c.Data), &cred); err != nil {
			return waUser{}, err
		}
		wu.creds = append(wu.creds, cred)
	}
	return wu, nil
}

// ceremony is the server half of a WebAuthn ceremony in progress, keyed by a
// random ID in a short-lived cookie.
type ceremony struct {
	data      webauthn.SessionData
	userID    string // enrolment only
	enrolHash string // enrolment only
	expires   time.Time
}

type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func (c *ceremonies) put(w http.ResponseWriter, secure bool, cer ceremony) {
	id := randomToken()
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]ceremony{}
	}
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	cer.expires = now.Add(ceremonyTTL)
	c.m[id] = cer
	c.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: id, Path: "/", MaxAge: int(ceremonyTTL.Seconds()),
		Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// take removes and returns the ceremony named by the request's cookie, so
// each challenge is answered at most once.
func (c *ceremonies) take(w http.ResponseWriter, r *http.Request, secure bool) (ceremony, bool) {
	ck, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: "", Path: "/", MaxAge: -1, Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[ck.Value]
	delete(c.m, ck.Value)
	if !ok || time.Now().After(cer.expires) {
		return ceremony{}, false
	}
	return cer, true
}

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

func newWebAuthn(cfg config.Config) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPID:                  u.Hostname(),
		RPDisplayName:         "Thesis review",
		RPOrigins:             []string{cfg.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
}

// --- Enrolment ---

type enrolBegin struct {
	Secret string `json:"secret"`
}

func (s *Server) enrolBegin(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "enrol", "") {
		return
	}
	id := r.PathValue("id")
	var req enrolBegin
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAuthBody)).Decode(&req); err != nil || len(req.Secret) > 512 {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	u, ok := s.Cfg.User(id)
	hash := hashToken(req.Secret)
	// Compare against a fixed string for unknown users too, so the timing
	// does not reveal which IDs exist.
	want := strings.Repeat("0", 64)
	if ok {
		want = u.EnrolHash
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(want)) != 1 || !ok || req.Secret == "" {
		jsonError(w, http.StatusForbidden, "This enrolment link is not valid.")
		return
	}
	used, err := s.Store.EnrolmentUsed(r.Context(), hash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if used {
		jsonError(w, http.StatusGone, "This enrolment link has already been used. Ask for a new one.")
		return
	}
	wu, err := s.loadUser(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range wu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, data, err := s.wa.BeginRegistration(wu, webauthn.WithExclusions(exclude))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ceremonies.put(w, s.secure, ceremony{data: *data, userID: id, enrolHash: hash})
	writeJSON(w, creation)
}

func (s *Server) enrolFinish(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "enrol", "") {
		return
	}
	cer, ok := s.ceremonies.take(w, r, s.secure)
	if !ok || cer.userID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "The enrolment timed out. Reload the page and try again.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBody))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "The passkey response could not be read.")
		return
	}
	wu, err := s.loadUser(r.Context(), cer.userID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cred, err := s.wa.CreateCredential(wu, cer.data, parsed)
	if err != nil {
		s.Log.Info("enrolment rejected", "user", cer.userID, "err", err)
		jsonError(w, http.StatusBadRequest, "The passkey was not accepted.")
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.Store.CompleteEnrolment(r.Context(), cer.enrolHash, store.Credential{ID: cred.ID, UserID: cer.userID, Data: string(data)})
	if errors.Is(err, store.ErrEnrolmentUsed) {
		jsonError(w, http.StatusGone, "This enrolment link has already been used. Ask for a new one.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("passkey enrolled", "user", cer.userID)
	if err := s.startSession(w, r, cer.userID); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"redirect": "/"})
}

// --- Login ---

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "login", "") {
		return
	}
	assertion, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ceremonies.put(w, s.secure, ceremony{data: *data})
	writeJSON(w, assertion)
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "login", "") {
		return
	}
	cer, ok := s.ceremonies.take(w, r, s.secure)
	if !ok {
		jsonError(w, http.StatusBadRequest, "The login timed out. Try again.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBody))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "The passkey response could not be read.")
		return
	}
	var userID string
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		c, err := s.Store.CredentialByID(r.Context(), rawID)
		if err != nil {
			return nil, errors.New("unknown credential")
		}
		if !bytes.Equal(userHandle, []byte(c.UserID)) {
			return nil, errors.New("user handle does not match the credential")
		}
		wu, err := s.loadUser(r.Context(), c.UserID)
		if err != nil {
			return nil, err
		}
		userID = c.UserID
		return wu, nil
	}
	_, cred, err := s.wa.ValidatePasskeyLogin(handler, cer.data, parsed)
	if err != nil {
		s.Log.Info("login rejected", "err", err)
		jsonError(w, http.StatusUnauthorized, "That passkey is not registered here.")
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Store the new sign count and flags, as the library asks.
	if err := s.Store.UpdateCredential(r.Context(), store.Credential{ID: cred.ID, UserID: userID, Data: string(data)}); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.startSession(w, r, userID); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"redirect": safeNext(r.URL.Query().Get("next"))})
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

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token := randomToken()
	err := s.Store.CreateSession(r.Context(), store.Session{
		Hash: hashToken(token), UserID: userID, CSRF: randomToken(),
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
	u, ok := s.Cfg.User(sess.UserID)
	if !ok {
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

// requireOrigin guards the login and enrolment POSTs, which run before a
// session exists.
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
	"enrol": 10,
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
