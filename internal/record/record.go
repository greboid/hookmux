// Package record writes received webhook payloads to disk for later
// inspection or replay.
package record

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Mode selects which payloads are recorded.
type Mode int

const (
	ModeNone Mode = iota
	ModeUnhandled
	ModeAll
)

// ParseMode converts a flag value into a Mode.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "none":
		return ModeNone, nil
	case "unhandled":
		return ModeUnhandled, nil
	case "all":
		return ModeAll, nil
	default:
		return ModeNone, fmt.Errorf("invalid record mode %q (want none, unhandled or all)", s)
	}
}

// Recorder writes one JSON file per recorded webhook into a directory.
// A nil Recorder is a valid no-op.
type Recorder struct {
	dir  string
	mode Mode
	seq  atomic.Uint64
}

// New creates the recording directory and returns a Recorder.
func New(dir string, mode Mode) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create recording directory: %w", err)
	}
	return &Recorder{dir: dir, mode: mode}, nil
}

// Entry is the JSON structure written for one recorded webhook.
type Entry struct {
	Timestamp  time.Time   `json:"timestamp"`
	Method     string      `json:"method"`
	URI        string      `json:"uri"`
	Remote     string      `json:"remote"`
	Headers    http.Header `json:"headers"`
	Matched    []string    `json:"matched"`
	Body       string      `json:"body"`
	BodyBase64 bool        `json:"body_base64,omitempty"`
}

// Record stores a webhook payload. matched holds the names of the routes
// that matched; an empty slice means no route handled it. Errors are
// logged, never propagated: recording must not break webhook handling.
func (r *Recorder) Record(req *http.Request, body []byte, matched []string) {
	if r == nil || r.mode == ModeNone {
		return
	}
	if r.mode == ModeUnhandled && len(matched) > 0 {
		return
	}
	e := Entry{
		Timestamp: time.Now(),
		Method:    req.Method,
		URI:       req.URL.RequestURI(),
		Remote:    req.RemoteAddr,
		Headers:   req.Header.Clone(),
		Matched:   matched,
		Body:      string(body),
	}
	if !utf8.Valid(body) {
		e.Body = base64.StdEncoding.EncodeToString(body)
		e.BodyBase64 = true
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		slog.Error("Unable to marshal recorded payload", "error", err)
		return
	}
	name := fmt.Sprintf("%s-%06d.json", e.Timestamp.Format("20060102-150405.000000000"), r.seq.Add(1))
	path := filepath.Join(r.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		slog.Error("Unable to write recorded payload", "path", path, "error", err)
		return
	}
	slog.Info("Recorded payload", "path", path, "matched", len(matched))
}
