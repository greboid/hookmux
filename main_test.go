package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greboid/hookmux/internal/routes"
	"github.com/greboid/hookmux/internal/server"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type recorder struct {
	forwarded chan *http.Request
	bodies    sync.Map // request pointer -> body bytes
}

func newRecorder() *recorder {
	return &recorder{forwarded: make(chan *http.Request, 16)}
}

func (r *recorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.bodies.Store(req, body)
	r.forwarded <- req
	w.WriteHeader(http.StatusOK)
}

func (r *recorder) bodyOf(t *testing.T, req *http.Request) string {
	t.Helper()
	v, ok := r.bodies.Load(req)
	if !ok {
		t.Fatal("no recorded body for request")
	}
	return string(v.([]byte))
}

func TestNewHandler(t *testing.T) {
	rec := newRecorder()
	target := httptest.NewTestServer(t, http.HandlerFunc(rec.serveHTTP))
	client := target.Client() // also initializes target.URL

	routeSet, err := routes.Parse([]byte(`
routes:
  - name: pushes
    match: '.event == "push"'
    forward:
      - url: ` + target.URL + `/hook
`))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}

	handler := newHandler(routeSet, server.NewDispatcher(5*time.Second, client), 64*1024)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	waitForward := func(t *testing.T) *http.Request {
		t.Helper()
		select {
		case req := <-rec.forwarded:
			return req
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for forward")
			return nil
		}
	}

	t.Run("health check", func(t *testing.T) {
		if w := do(http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
			t.Errorf("GET /healthz status = %d, want 200", w.Code)
		}
	})

	t.Run("webhook at root", func(t *testing.T) {
		w := do(http.MethodPost, "/", `{"event":"push"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("POST / status = %d, want %d", w.Code, http.StatusAccepted)
		}
		req := waitForward(t)
		if got := rec.bodyOf(t, req); got != `{"event":"push"}` {
			t.Errorf("forwarded body = %q", got)
		}
	})

	t.Run("webhook at sub-path", func(t *testing.T) {
		if w := do(http.MethodPost, "/github/services/hook", `{"event":"push"}`); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		waitForward(t)
	})

	t.Run("webhook with trailing slash", func(t *testing.T) {
		if w := do(http.MethodPost, "/trailing/", `{"event":"push"}`); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		waitForward(t)
	})

	t.Run("landing page", func(t *testing.T) {
		w := do(http.MethodGet, "/", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET / status = %d, want %d", w.Code, http.StatusOK)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if !strings.Contains(w.Body.String(), "hookmux") {
			t.Error("landing page does not mention hookmux")
		}
	})

	t.Run("favicon", func(t *testing.T) {
		w := do(http.MethodGet, "/favicon.svg", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /favicon.svg status = %d, want %d", w.Code, http.StatusOK)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
			t.Errorf("Content-Type = %q, want image/svg+xml", ct)
		}
		if !strings.Contains(w.Body.String(), "<svg") {
			t.Error("favicon body is not an SVG document")
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		w := do(http.MethodDelete, "/", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE / status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if allow := w.Header().Get("Allow"); !strings.Contains(allow, "POST") {
			t.Errorf("Allow header = %q, want it to contain POST", allow)
		}
		if w := do(http.MethodGet, "/webhook", ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET /webhook status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("payload without matching route still accepted", func(t *testing.T) {
		if w := do(http.MethodPost, "/", `{"event":"issues"}`); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		select {
		case req := <-rec.forwarded:
			t.Errorf("unexpected forward for unmatched payload: %s", req.URL)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("post to healthz is routed as a webhook", func(t *testing.T) {
		w := do(http.MethodPost, "/healthz", `{"event":"push"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("POST /healthz status = %d, want %d (falls through to webhook endpoint)", w.Code, http.StatusAccepted)
		}
		waitForward(t)
	})

	t.Run("body size limit", func(t *testing.T) {
		big := strings.Repeat("a", 128*1024)
		w := do(http.MethodPost, "/", `{"event":"push","pad":"`+big+`"}`)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
	})
}
