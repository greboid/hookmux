package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/greboid/hookmux/internal/routes"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const pushConfig = `
routes:
  - name: pushes
    match: '.event == "push"'
    forward:
      - url: http://target
        headers:
          X-Token: secret
`

// testHarness: configs use the "http://target" placeholder, rewritten to the
// in-memory recorder server.
type testHarness struct {
	handler    *Webhook
	dispatcher *Dispatcher
	forwarded  chan *http.Request
	bodies     sync.Map // request pointer -> body bytes
}

func (h *testHarness) capture(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.bodies.Store(r, body)
	h.forwarded <- r
}

func (h *testHarness) bodyOf(t *testing.T, req *http.Request) string {
	t.Helper()
	v, ok := h.bodies.Load(req)
	if !ok {
		t.Fatal("no recorded body for request")
	}
	return string(v.([]byte))
}

func newHarness(t *testing.T, config string) *testHarness {
	t.Helper()
	h := &testHarness{forwarded: make(chan *http.Request, 16)}
	target := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.capture(r)
		w.WriteHeader(http.StatusOK)
	}))
	// Client() must be called before reading target.URL, which it initializes.
	client := target.Client()

	set, err := routes.Parse([]byte(strings.ReplaceAll(config, "http://target", target.URL)))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	h.dispatcher = NewDispatcher(5*time.Second, client)
	h.handler = NewWebhook(set, h.dispatcher, 1024*1024)
	return h
}

func post(t *testing.T, handler http.Handler, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestForward(t *testing.T) {
	h := newHarness(t, pushConfig)

	w := post(t, h.handler, "/webhook", "application/json", `{"event":"push","repository":{"name":"hookmux"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if w.Body.String() != "accepted\n" {
		t.Errorf("response body = %q, want %q", w.Body.String(), "accepted\n")
	}

	select {
	case req := <-h.forwarded:
		if req.Method != http.MethodPost {
			t.Errorf("forward method = %s, want POST", req.Method)
		}
		if req.Header.Get("X-Hookmux-Route") != "pushes" {
			t.Errorf("X-Hookmux-Route = %q, want pushes", req.Header.Get("X-Hookmux-Route"))
		}
		if req.Header.Get("X-Token") != "secret" {
			t.Error("configured header was not forwarded")
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if got := h.bodyOf(t, req); got != `{"event":"push","repository":{"name":"hookmux"}}` {
			t.Errorf("forwarded body = %q", got)
		}
		if req.Header.Get("X-Forwarded-For") == "" {
			t.Error("X-Forwarded-For was not set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestNoopRoute(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: discard
    match: '.event == "push"'
    noop: true
  - name: other
    match: '.event == "issues"'
    forward:
      - url: http://target
`)

	w := post(t, h.handler, "/", "application/json", `{"event":"push"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	select {
	case req := <-h.forwarded:
		t.Errorf("unexpected forward from noop route: %s", req.URL)
	case <-time.After(200 * time.Millisecond):
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestForwardOverHTTPS(t *testing.T) {
	h := &testHarness{forwarded: make(chan *http.Request, 4)}
	tlsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.capture(r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsTarget.Close)

	set, err := routes.Parse([]byte(strings.ReplaceAll(`
routes:
  - name: secure
    match: '.event == "push"'
    forward:
      - url: https://target
        headers:
          X-Token: tls-secret
`, "https://target", tlsTarget.URL+"/secure")))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	h.dispatcher = NewDispatcher(5*time.Second, tlsTarget.Client())
	h.handler = NewWebhook(set, h.dispatcher, 1024*1024)

	w := post(t, h.handler, "/", "application/json", `{"event":"push"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	select {
	case req := <-h.forwarded:
		if req.TLS == nil {
			t.Error("forward was not made over TLS")
		}
		if got := h.bodyOf(t, req); got != `{"event":"push"}` {
			t.Errorf("forwarded body = %q", got)
		}
		if req.Header.Get("X-Token") != "tls-secret" {
			t.Error("configured header was not forwarded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestFanOutToMultipleRoutesAndTargets(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "fanout.txt")
	h := newHarness(t, fmt.Sprintf(`
routes:
  - name: route-a
    match: '.event == "push"'
    forward:
      - url: http://target/one
      - url: http://target/two
        method: PUT
  - name: route-b
    match: '.event == "push"'
    exec:
      - command: ["sh", "-c", "echo fanout > %[1]s"]
  - name: route-c
    match: '.event == "issues"'
    forward:
      - url: http://target/three
`, outFile))

	w := post(t, h.handler, "/", "application/json", `{"event":"push"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	methods := map[string]string{}
	for range 2 {
		select {
		case req := <-h.forwarded:
			switch {
			case strings.HasSuffix(req.URL.Path, "/one"):
				methods["/one"] = req.Method
			case strings.HasSuffix(req.URL.Path, "/two"):
				methods["/two"] = req.Method
			default:
				t.Errorf("unexpected forward to %s", req.URL.Path)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for forwards")
		}
	}
	if methods["/one"] != http.MethodPost || methods["/two"] != http.MethodPut {
		t.Errorf("forward methods = %v, want /one POST and /two PUT", methods)
	}

	select {
	case req := <-h.forwarded:
		t.Errorf("unexpected forward to %s: route-c should not have matched", req.URL)
	case <-time.After(200 * time.Millisecond):
	}

	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("command did not write payload: %v", err)
	}
	if string(data) != "fanout\n" {
		t.Errorf("command output = %q, want %q", data, "fanout\n")
	}
}

func TestNoMatch(t *testing.T) {
	h := newHarness(t, pushConfig)

	w := post(t, h.handler, "/webhook", "application/json", `{"event":"issues"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	select {
	case req := <-h.forwarded:
		t.Fatalf("unexpected forward for unmatched route: %s", req.URL)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestForwardWithTransform(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: trimmed
    match: '.event == "push"'
    forward:
      - url: http://target
        body: '{event: .event, repo: .repository.name}'
`)

	post(t, h.handler, "/webhook", "application/json", `{"event":"push","repository":{"name":"hookmux"},"extra":"ignored"}`)

	select {
	case req := <-h.forwarded:
		if want := `{"event":"push","repo":"hookmux"}`; h.bodyOf(t, req) != want {
			t.Errorf("transformed body = %q, want %q", h.bodyOf(t, req), want)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestLargeIntegerPreservedEndToEnd(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: ids
    match: '.id != null'
    forward:
      - url: http://target
        body: '{id: .id}'
`)

	post(t, h.handler, "/", "application/json", `{"id":9007199254740993}`)

	select {
	case req := <-h.forwarded:
		if want := `{"id":9007199254740993}`; h.bodyOf(t, req) != want {
			t.Errorf("forwarded body = %s, want %s (integer must not be rounded)", h.bodyOf(t, req), want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestForwardHeadersOverrideDefaults(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: overridden
    forward:
      - url: http://target
        headers:
          Content-Type: application/xml
          X-Token: abc
`)

	post(t, h.handler, "/", "application/json", `{"event":"push"}`)

	select {
	case req := <-h.forwarded:
		if ct := req.Header.Get("Content-Type"); ct != "application/xml" {
			t.Errorf("Content-Type = %q, want application/xml (configured headers must win)", ct)
		}
		if req.Header.Get("X-Token") != "abc" {
			t.Error("configured header was not forwarded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestXForwardedForAppendsCaller(t *testing.T) {
	h := newHarness(t, pushConfig)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"event":"push"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	select {
	case fwd := <-h.forwarded:
		want := "203.0.113.7, 192.0.2.1:1234"
		if got := fwd.Header.Get("X-Forwarded-For"); got != want {
			t.Errorf("X-Forwarded-For = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestNonJSONPayloadReachesCatchAllRoute(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: catcher
    forward:
      - url: http://target
`)

	w := post(t, h.handler, "/webhook", "text/plain", "not json at all")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	select {
	case req := <-h.forwarded:
		if got := h.bodyOf(t, req); got != "not json at all" {
			t.Errorf("forwarded body = %q, want raw payload", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}
	h.dispatcher.Shutdown(context.Background())
}

func TestForwardDestinationErrorStillAccepted(t *testing.T) {
	h := &testHarness{forwarded: make(chan *http.Request, 4)}
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.capture(r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	client := srv.Client() // also initializes srv.URL
	set, err := routes.Parse([]byte(strings.ReplaceAll(pushConfig, "http://target", srv.URL)))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	h.dispatcher = NewDispatcher(5*time.Second, client)
	h.handler = NewWebhook(set, h.dispatcher, 1024*1024)

	if w := post(t, h.handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d despite destination error", w.Code, http.StatusAccepted)
	}
	select {
	case <-h.forwarded:
	case <-time.After(5 * time.Second):
		t.Fatal("destination did not receive the request")
	}
	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestForwardUnreachableStillAccepted(t *testing.T) {
	set, err := routes.Parse([]byte(`
routes:
  - name: nowhere
    forward:
      - url: http://127.0.0.1:1/hook
`))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	d := NewDispatcher(5*time.Second, nil)
	handler := NewWebhook(set, d, 1024*1024)

	if w := post(t, handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d despite unreachable destination", w.Code, http.StatusAccepted)
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestForwardTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &testHarness{forwarded: make(chan *http.Request, 4)}
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Hour) // never responds in time (virtual time)
			h.capture(r)
			w.WriteHeader(http.StatusOK)
		}))
		client := srv.Client() // also initializes srv.URL
		set, err := routes.Parse([]byte(strings.ReplaceAll(pushConfig, "http://target", srv.URL)))
		if err != nil {
			t.Fatalf("routes.Parse() error = %v", err)
		}
		h.dispatcher = NewDispatcher(150*time.Millisecond, client)
		h.handler = NewWebhook(set, h.dispatcher, 1024*1024)

		start := time.Now()
		if w := post(t, h.handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		if err := h.dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
		if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 200*time.Millisecond {
			t.Errorf("elapsed = %v, want the 150ms action timeout", elapsed)
		}
		select {
		case req := <-h.forwarded:
			t.Errorf("unexpected completed forward to %s", req.URL)
		default:
		}
		srv.CloseClientConnections()
	})
}

func TestShutdownWaitsForInFlightForward(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var forwarded atomic.Bool
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Second) // slow destination
			forwarded.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		client := srv.Client() // also initializes srv.URL
		set, err := routes.Parse([]byte(strings.ReplaceAll(pushConfig, "http://target", srv.URL)))
		if err != nil {
			t.Fatalf("routes.Parse() error = %v", err)
		}
		d := NewDispatcher(time.Minute, client)
		handler := NewWebhook(set, d, 1024*1024)

		if w := post(t, handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		done := make(chan error, 1)
		go func() {
			done <- d.Shutdown(context.Background())
		}()

		time.Sleep(500 * time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("Shutdown returned at 500ms while forward still in flight (err=%v)", err)
		default:
		}

		time.Sleep(2 * time.Second)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}
		default:
			t.Fatal("Shutdown still pending after forward completed")
		}
		if !forwarded.Load() {
			t.Error("destination did not record the forward")
		}
		srv.CloseClientConnections()
	})
}

func TestExec(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "payload.txt")
	h := newHarness(t, fmt.Sprintf(`
routes:
  - name: local
    match: '.event == "push"'
    exec:
      - command: ["sh", "-c", "cat > %[1]s"]
`, outFile))

	post(t, h.handler, "/webhook", "application/json", `{"event":"push"}`)

	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("command did not write payload: %v", err)
	}
	if string(got) != `{"event":"push"}` {
		t.Errorf("command stdin = %q, want webhook body", got)
	}
}

func TestExecReceivesRouteEnvironment(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "route.txt")
	h := newHarness(t, fmt.Sprintf(`
routes:
  - name: env-route
    exec:
      - command: ["sh", "-c", "printf '%%s' \"$HOOKMUX_ROUTE\" > %[1]s"]
`, outFile))

	post(t, h.handler, "/", "application/json", `{"event":"push"}`)

	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("command did not write output: %v", err)
	}
	if string(got) != "env-route" {
		t.Errorf("HOOKMUX_ROUTE = %q, want env-route", got)
	}
}

func TestExecFailureStillAccepted(t *testing.T) {
	h := newHarness(t, `
routes:
  - name: failing
    exec:
      - command: ["sh", "-c", "echo boom >&2; exit 7"]
`)

	if w := post(t, h.handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d despite command failure", w.Code, http.StatusAccepted)
	}
	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestExecTimesOut(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "late.txt")
	set, err := routes.Parse([]byte(fmt.Sprintf(`
routes:
  - name: slow
    exec:
      - command: ["sh", "-c", "sleep 2; touch %[1]s"]
`, outFile)))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	d := NewDispatcher(250*time.Millisecond, nil)
	handler := NewWebhook(set, d, 1024*1024)

	start := time.Now()
	if w := post(t, handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("Shutdown took %v, want the command to be killed at the 250ms timeout", elapsed)
	}
	if _, err := os.ReadFile(outFile); !os.IsNotExist(err) {
		t.Error("timed-out command should not have written output")
	}
}

func TestExecLogsCommandOutput(t *testing.T) {
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	})

	h := newHarness(t, `
routes:
  - name: chatty
    exec:
      - command: ["sh", "-c", "printf '%s' '  hello world  '"]
`)

	if w := post(t, h.handler, "/", "application/json", `{"event":"push"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	if err := h.dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	logs := buf.String()
	for _, want := range []string{`msg="Command output"`, "route=chatty", "command=sh", `output="hello world"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q; got:\n%s", want, logs)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, pushConfig)
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow header = %q, want it to contain POST", allow)
	}
}

func TestBodyTooLarge(t *testing.T) {
	set, err := routes.Parse([]byte(pushConfig))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}
	handler := NewWebhook(set, NewDispatcher(time.Second, nil), 8)
	w := post(t, handler, "/webhook", "application/json", `{"event":"push","lots":"of data here"}`)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

type closeTrackingBody struct {
	r      io.Reader
	closed bool
}

func (c *closeTrackingBody) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *closeTrackingBody) Close() error {
	c.closed = true
	return nil
}

func TestRequestBodyClosedOnEveryPath(t *testing.T) {
	set, err := routes.Parse([]byte(pushConfig))
	if err != nil {
		t.Fatalf("routes.Parse() error = %v", err)
	}

	serve := func(handler *Webhook, body *closeTrackingBody) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/webhook", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	t.Run("success", func(t *testing.T) {
		tracker := &closeTrackingBody{r: strings.NewReader(`{"event":"push"}`)}
		handler := NewWebhook(set, NewDispatcher(5*time.Second, nil), 1024*1024)
		if w := serve(handler, tracker); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
		}
		if !tracker.closed {
			t.Error("request body was not closed")
		}
	})

	t.Run("body too large", func(t *testing.T) {
		tracker := &closeTrackingBody{r: strings.NewReader(`{"event":"push","lots":"of data here"}`)}
		handler := NewWebhook(set, NewDispatcher(time.Second, nil), 8)
		if w := serve(handler, tracker); w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
		if !tracker.closed {
			t.Error("request body was not closed after hitting the size limit")
		}
	})
}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		valid bool
	}{
		{"object", `{"a":1}`, true},
		{"array", `[1,2,3]`, true},
		{"string scalar", `"hello"`, true},
		{"empty body", "", false},
		{"whitespace body", "   \n", false},
		{"invalid", "{nope", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, valid := parseJSON([]byte(tt.body))
			if valid != tt.valid {
				t.Errorf("parseJSON(%q) valid = %v, want %v", tt.body, valid, tt.valid)
			}
		})
	}
}
