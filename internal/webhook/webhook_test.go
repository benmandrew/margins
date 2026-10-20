package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var secret = []byte("s3cret")

const payload = `{"ref":"refs/heads/main","before":"` + ZeroSHA + `","after":"1111111111111111111111111111111111111111","repository":{"full_name":"me/thesis"}}`

type memDeliveries map[string]bool

func (m memDeliveries) Seen(id string) (bool, error) {
	seen := m[id]
	m[id] = true
	return seen, nil
}

func sign(body string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func newHandler() (*Handler, *[]Push) {
	var pushes []Push
	return &Handler{
		Forge:      Gitea{},
		Secret:     secret,
		Repo:       "me/thesis",
		Deliveries: memDeliveries{},
		Enqueue:    func(p Push) error { pushes = append(pushes, p); return nil },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &pushes
}

func request(body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hooks/push", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func giteaHeaders(body string, key []byte, delivery string) map[string]string {
	return map[string]string{
		"X-Gitea-Event":     "push",
		"X-Gitea-Delivery":  delivery,
		"X-Gitea-Signature": sign(body, key),
	}
}

func TestHandler(t *testing.T) {
	tampered := strings.Replace(payload, "1111", "2222", 1)
	otherRepo := strings.Replace(payload, "me/thesis", "me/other", 1)
	tests := []struct {
		name    string
		body    string
		headers map[string]string
		want    int
		pushes  int
	}{
		{"valid", payload, giteaHeaders(payload, secret, "d1"), http.StatusAccepted, 1},
		{"tampered body", tampered, giteaHeaders(payload, secret, "d1"), http.StatusUnauthorized, 0},
		{"wrong secret", payload, giteaHeaders(payload, []byte("nope"), "d1"), http.StatusUnauthorized, 0},
		{"missing signature", payload, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Delivery": "d1"}, http.StatusUnauthorized, 0},
		{"missing event headers", payload, map[string]string{"X-Gitea-Signature": sign(payload, secret)}, http.StatusBadRequest, 0},
		{"prefixed signature", payload, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Delivery": "d1", "X-Gitea-Signature": "sha256=" + sign(payload, secret)}, http.StatusUnauthorized, 0},
		{"other repository", otherRepo, giteaHeaders(otherRepo, secret, "d1"), http.StatusForbidden, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, pushes := newHandler()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request(tt.body, tt.headers))
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
			if len(*pushes) != tt.pushes {
				t.Errorf("enqueued %d pushes, want %d", len(*pushes), tt.pushes)
			}
		})
	}
}

func TestDuplicateDeliveryIsNoOp(t *testing.T) {
	h, pushes := newHandler()
	for range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request(payload, giteaHeaders(payload, secret, "same")))
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
	}
	if len(*pushes) != 1 {
		t.Errorf("enqueued %d pushes, want 1", len(*pushes))
	}
}

func TestNonPushEventIgnored(t *testing.T) {
	h, pushes := newHandler()
	headers := giteaHeaders(payload, secret, "d1")
	headers["X-Gitea-Event"] = "issues"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(payload, headers))
	if w.Code != http.StatusNoContent || len(*pushes) != 0 {
		t.Errorf("status = %d with %d pushes, want 204 with none", w.Code, len(*pushes))
	}
}

func TestPushPayloadParsed(t *testing.T) {
	h, pushes := newHandler()
	h.ServeHTTP(httptest.NewRecorder(), request(payload, giteaHeaders(payload, secret, "d1")))
	if len(*pushes) != 1 {
		t.Fatal("no push enqueued")
	}
	p := (*pushes)[0]
	if p.Ref != "refs/heads/main" || p.Before != ZeroSHA || p.After != strings.Repeat("1", 40) {
		t.Errorf("parsed %+v", p)
	}
}
