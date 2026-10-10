package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/greboid/hookmux/internal/routes"
)

var acceptedMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch}

type Webhook struct {
	routes     *routes.Set
	dispatcher *Dispatcher
	maxBody    int64
}

func NewWebhook(rs *routes.Set, d *Dispatcher, maxBody int64) *Webhook {
	return &Webhook{routes: rs, dispatcher: d, maxBody: maxBody}
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyReader := http.MaxBytesReader(w, r.Body, h.maxBody)
	defer func(bodyReader io.ReadCloser) {
		_ = bodyReader.Close()
	}(bodyReader)

	if !slices.Contains(acceptedMethods, r.Method) {
		w.Header().Set("Allow", strings.Join(acceptedMethods, ", "))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(bodyReader)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unable to read request body", http.StatusBadRequest)
		return
	}

	input, jsonValid := parseJSON(body)
	matches := h.routes.Matches(r, body, input, jsonValid)
	if len(matches) == 0 {
		slog.Warn("No routes matched webhook",
			"method", r.Method,
			"remote", r.RemoteAddr,
			"bytes", len(body),
			"json", jsonValid,
		)
	} else {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Route.Name
		}
		slog.Info("Matched routes for webhook",
			"routes", strings.Join(names, ","),
			"method", r.Method,
			"remote", r.RemoteAddr,
			"bytes", len(body),
		)
	}

	for _, m := range matches {
		for i := range m.Route.Forward {
			h.dispatcher.Forward(m, i, r, body, input)
		}
		for i := range m.Route.Exec {
			h.dispatcher.Exec(m, i, body)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "accepted")
}

func parseJSON(body []byte) (any, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

type Dispatcher struct {
	client  *http.Client
	timeout time.Duration
	wg      sync.WaitGroup
}

func NewDispatcher(timeout time.Duration, client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{}
	}
	return &Dispatcher{
		client:  client,
		timeout: timeout,
	}
}

func (d *Dispatcher) Forward(m *routes.Match, i int, orig *http.Request, body []byte, input any) {
	f := m.Route.Forward[i]
	d.wg.Go(func() {
		d.doForward(m, i, f, orig, body, input)
	})
}

func (d *Dispatcher) doForward(m *routes.Match, i int, f routes.Forward, orig *http.Request, body []byte, input any) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	payload, err := m.RenderBody(i, body, input)
	if err != nil {
		slog.Error("Unable to render forwarded body", "route", m.Route.Name, "url", f.URL, "error", err)
		return
	}

	method := strings.ToUpper(strings.TrimSpace(f.Method))
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, f.URL, bytes.NewReader(payload))
	if err != nil {
		slog.Error("Unable to build forward request", "route", m.Route.Name, "url", f.URL, "error", err)
		return
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("User-Agent", "hookmux")

	if ct := f.Headers["Content-Type"]; ct != "" {
		req.Header.Set("Content-Type", ct)
	} else if m.HasTransform(i) {
		req.Header.Set("Content-Type", "application/json")
	} else if orig != nil {
		if ct := orig.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}
	if orig != nil {
		if xff := orig.Header.Get("X-Forwarded-For"); xff != "" {
			req.Header.Set("X-Forwarded-For", xff+", "+orig.RemoteAddr)
		} else {
			req.Header.Set("X-Forwarded-For", orig.RemoteAddr)
		}
	}
	req.Header.Set("X-Hookmux-Route", m.Route.Name)
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := d.client.Do(req)
	if err != nil {
		slog.Error("Forward failed", "route", m.Route.Name, "url", f.URL, "error", err)
		return
	}
	defer func(body io.ReadCloser) {
		if err := body.Close(); err != nil {
			slog.Error("Unable to close reader whilst forwarding request", "error", err)
		}
	}(resp.Body)
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= http.StatusBadRequest {
		slog.Error("Forward rejected by destination",
			"route", m.Route.Name, "url", f.URL, "status", resp.StatusCode, "duration", time.Since(start))
		return
	}
	slog.Info("Forwarded webhook",
		"route", m.Route.Name, "url", f.URL, "status", resp.StatusCode, "duration", time.Since(start))
}

func (d *Dispatcher) Exec(m *routes.Match, i int, body []byte) {
	e := m.Route.Exec[i]
	d.wg.Go(func() {
		d.doExec(m, e, body)
	})
}

func (d *Dispatcher) doExec(m *routes.Match, e routes.Exec, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	name := e.Command[0]
	cmd := exec.CommandContext(ctx, name, e.Command[1:]...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(), "HOOKMUX_ROUTE="+m.Route.Name)

	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Command failed",
			"route", m.Route.Name, "command", name, "error", err,
			"output", strings.TrimSpace(string(out)), "duration", time.Since(start))
		return
	}
	if len(out) > 0 {
		slog.Info("Command output",
			"route", m.Route.Name, "command", name, "output", strings.TrimSpace(string(out)))
	}
	slog.Info("Executed command", "route", m.Route.Name, "command", name, "duration", time.Since(start))
}

func (d *Dispatcher) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
