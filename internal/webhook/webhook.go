// Package webhook receives Gitea push events. It verifies and enqueues, and
// does no Git or Typst work in the request, so a slow build never times out
// Gitea's delivery.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxBody caps the payload. Gitea truncates the commits list long before a
// push event reaches this size.
const maxBody = 1 << 20

// ZeroSHA is what Gitea sends as `before` for a new ref and as `after` for a
// deleted one.
const ZeroSHA = "0000000000000000000000000000000000000000"

// Push is the part of a Gitea push payload the service reads. The commits list
// is deliberately absent: it can be truncated, so new SHAs come from
// `git rev-list before..after` against the mirror.
type Push struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Forge verifies a request from one kind of forge. Only Gitea exists in v1;
// GitHub is future work and would be a second implementation.
type Forge interface {
	// Event returns the event name and delivery id, and ok=false when the
	// request does not carry this forge's headers.
	Event(h http.Header) (event, delivery string, ok bool)
	Verify(h http.Header, body []byte, secret []byte) bool
}

type Gitea struct{}

func (Gitea) Event(h http.Header) (string, string, bool) {
	event, delivery := h.Get("X-Gitea-Event"), h.Get("X-Gitea-Delivery")
	return event, delivery, event != "" && delivery != ""
}

// Verify checks X-Gitea-Signature, the bare hex HMAC-SHA256 of the raw body.
func (Gitea) Verify(h http.Header, body []byte, secret []byte) bool {
	return validHMAC(h.Get("X-Gitea-Signature"), body, secret)
}

func validHMAC(sigHex string, body, secret []byte) bool {
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Deliveries records delivery ids. Seen reports whether id was recorded
// before, and records it if not, in one step.
type Deliveries interface {
	Seen(id string) (bool, error)
}

// Handler serves POST /hooks/push.
type Handler struct {
	Forge      Forge
	Secret     []byte
	Repo       string // Gitea full name; payloads for any other are rejected
	Deliveries Deliveries
	// Enqueue schedules a fetch for the push. It must return quickly.
	Enqueue func(Push) error
	Log     *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	event, delivery, ok := h.Forge.Event(r.Header)
	if !ok {
		http.Error(w, "missing forge headers", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Verify before reading anything else from the body, and before the
	// delivery id touches the database.
	if !h.Forge.Verify(r.Header, body, h.Secret) {
		h.Log.Warn("webhook signature rejected", "delivery", delivery)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if event != "push" {
		// Accepted and ignored, so a misconfigured hook does not show as
		// failing deliveries in Gitea.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var p Push
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(p.Repository.FullName, h.Repo) {
		h.Log.Warn("webhook for another repository", "repo", p.Repository.FullName, "delivery", delivery)
		http.Error(w, "unknown repository", http.StatusForbidden)
		return
	}
	seen, err := h.Deliveries.Seen(delivery)
	if err != nil {
		h.Log.Error("recording delivery", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if seen {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// The id is recorded before the enqueue, so a failed enqueue makes a
	// redelivery a no-op. The poll timer fetches the same commits anyway.
	if err := h.Enqueue(p); err != nil {
		h.Log.Error("enqueueing push", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
