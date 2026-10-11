package record

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/github/pushes?run=1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")
	return req
}

func readEntry(t *testing.T, dir, name string) Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", name, err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("recorded file is not valid JSON: %v", err)
	}
	return e
}

func TestParseMode(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Mode
	}{
		{"none", ModeNone},
		{"unhandled", ModeUnhandled},
		{"all", ModeAll},
	} {
		if got, err := ParseMode(tt.in); err != nil || got != tt.want {
			t.Errorf("ParseMode(%q) = %v, %v; want %v, nil", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"", "everything", "NONE"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) error = nil, want error", in)
		}
	}
}

func TestRecordAll(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, ModeAll)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec.Record(newTestRequest(`{"event":"push"}`), []byte(`{"event":"push"}`), []string{"github-pushes"})
	rec.Record(newTestRequest(`{"event":"ping"}`), []byte(`{"event":"ping"}`), nil)

	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("recorded %d files, want 2", len(names))
	}

	for _, name := range []string{names[0].Name(), names[1].Name()} {
		e := readEntry(t, dir, name)
		if e.Method != http.MethodPost {
			t.Errorf("%s: method = %q, want POST", name, e.Method)
		}
		if e.URI != "/github/pushes?run=1" {
			t.Errorf("%s: uri = %q, want /github/pushes?run=1", name, e.URI)
		}
		if e.Remote == "" {
			t.Errorf("%s: remote is empty", name)
		}
		if e.Headers.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type header = %q, want application/json", name, e.Headers.Get("Content-Type"))
		}
		if e.Timestamp.IsZero() {
			t.Errorf("%s: timestamp is zero", name)
		}
		if e.BodyBase64 {
			t.Errorf("%s: body unexpectedly base64", name)
		}
	}

	first, second := names[0].Name(), names[1].Name()
	if e := readEntry(t, dir, first); len(e.Matched) != 1 || e.Matched[0] != "github-pushes" {
		t.Errorf("%s: matched = %v, want [github-pushes]", first, e.Matched)
	}
	if e := readEntry(t, dir, second); len(e.Matched) != 0 {
		t.Errorf("%s: matched = %v, want none", second, e.Matched)
	}
}

func TestRecordUnhandledOnly(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, ModeUnhandled)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec.Record(newTestRequest(`{"event":"push"}`), []byte(`{"event":"push"}`), []string{"github-pushes"})
	rec.Record(newTestRequest(`{"event":"issues"}`), []byte(`{"event":"issues"}`), nil)

	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("recorded %d files, want 1 (only the unhandled payload)", len(names))
	}
	e := readEntry(t, dir, names[0].Name())
	if string(e.Body) != `{"event":"issues"}` {
		t.Errorf("body = %q, want unmatched payload", e.Body)
	}
}

func TestRecordNone(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, ModeNone)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec.Record(newTestRequest(`{}`), []byte(`{}`), nil)
	var nilRecorder *Recorder
	nilRecorder.Record(newTestRequest(`{}`), []byte(`{}`), nil)

	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(names) != 0 {
		t.Errorf("recorded %d files, want 0", len(names))
	}
}

func TestRecordBinaryBody(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, ModeAll)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	raw := []byte{0xff, 0xfe, 0x00, 0x01, 0x02}
	rec.Record(newTestRequest("binary"), raw, nil)

	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("recorded %d files, want 1", len(names))
	}
	e := readEntry(t, dir, names[0].Name())
	if !e.BodyBase64 {
		t.Fatal("body_base64 = false, want true for non-UTF-8 body")
	}
	got, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		t.Fatalf("body is not valid base64: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("decoded body = %x, want %x", got, raw)
	}
}

func TestUniqueFileNames(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir, ModeAll)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for range 5 {
		rec.Record(newTestRequest("x"), []byte("x"), nil)
	}

	seen := make(map[string]bool, 5)
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(names) != 5 {
		t.Fatalf("recorded %d files, want 5", len(names))
	}
	for _, n := range names {
		if seen[n.Name()] {
			t.Errorf("duplicate file name %q", n.Name())
		}
		seen[n.Name()] = true
	}
}
