package routes

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const sampleConfig = `
routes:
  - name: pushes
    match: '.event == "push"'
    forward:
      - url: https://ci.example.com/hook
        headers:
          X-Token: secret
      - url: http://mirror.example.com/hook
        body: '{event: .event}'
  - name: everything
    exec:
      - command: ["/usr/local/bin/notify", "-chan", "ops"]
`

func parseConfig(t *testing.T, config string) *Set {
	t.Helper()
	set, err := Parse([]byte(config))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return set
}

func jsonNumber(s string) json.Number {
	return json.Number(s)
}

func TestParseValidConfig(t *testing.T) {
	set := parseConfig(t, sampleConfig)
	if set.Len() != 2 {
		t.Errorf("Len() = %d, want 2", set.Len())
	}

	pushes := set.routes[0]
	if pushes.route.Name != "pushes" {
		t.Errorf("name = %q, want pushes", pushes.route.Name)
	}
	if pushes.match == nil {
		t.Error("match expression was not compiled")
	}
	if len(pushes.route.Forward) != 2 {
		t.Fatalf("forwards = %d, want 2", len(pushes.route.Forward))
	}
	if pushes.route.Forward[0].Headers["X-Token"] != "secret" {
		t.Error("forward header not parsed")
	}
	if pushes.transforms[0] != nil {
		t.Error("first forward should have no transform")
	}
	if pushes.transforms[1] == nil {
		t.Error("second forward transform was not compiled")
	}

	everything := set.routes[1]
	if everything.match != nil {
		t.Error("route without match should have nil match code")
	}
	if len(everything.route.Exec) != 1 || everything.route.Exec[0].Command[0] != "/usr/local/bin/notify" {
		t.Error("exec action not parsed")
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"invalid match jq": `
routes:
  - name: bad
    match: '.event ==='
    exec:
      - command: ["true"]
`,
		"invalid body jq": `
routes:
  - name: bad
    forward:
      - url: https://example.com/hook
        body: '{unclosed'
`,
		"forward without url": `
routes:
  - name: bad
    forward:
      - headers:
          X-A: b
`,
		"exec without command": `
routes:
  - name: bad
    exec:
      - command: []
`,
		"route without actions": `
routes:
  - name: bad
    match: '.event == "push"'
`,
		"no routes at all":     "",
		"empty routes list":    "routes: []\n",
		"routes key only":      "routes:\n",
		"top level not a map":  "- just a string\n",
		"route is not a table": "routes: [nope]\n",
		"invalid yaml": `
routes: [ this is : not : valid
`,
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(config)); err == nil {
				t.Errorf("Parse() succeeded, want error for %s", name)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.yml")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if set.Len() != 2 {
		t.Errorf("Len() = %d, want 2", set.Len())
	}

	if _, err := Load(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("Load() of missing file succeeded, want error")
	}
}

func TestMatches(t *testing.T) {
	set := parseConfig(t, sampleConfig)

	pushEvent := map[string]any{"event": "push", "repository": map[string]any{"name": "hookmux"}}
	matched := set.Matches(pushEvent, true)
	if len(matched) != 2 || matched[0].Route.Name != "pushes" || matched[1].Route.Name != "everything" {
		t.Errorf("push event matched %v, want [pushes everything]", routeNames(matched))
	}

	otherEvent := map[string]any{"event": "issue"}
	matched = set.Matches(otherEvent, true)
	if len(matched) != 1 || matched[0].Route.Name != "everything" {
		t.Errorf("issue event matched %v, want [everything]", routeNames(matched))
	}

	matched = set.Matches(nil, false)
	if len(matched) != 1 || matched[0].Route.Name != "everything" {
		t.Errorf("non-JSON payload matched %v, want [everything]", routeNames(matched))
	}
}

func TestMatchesJqTruthiness(t *testing.T) {
	set := parseConfig(t, `
routes:
  - name: truthy
    match: '.value'
    exec:
      - command: ["true"]
`)

	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"zero is truthy", jsonNumber("0"), true},
		{"empty string is truthy", "", true},
		{"non-zero number is truthy", jsonNumber("3"), true},
		{"object is truthy", map[string]any{}, true},
		{"false is falsy", false, false},
		{"null is falsy", nil, false},
		{"missing key is falsy", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := map[string]any{}
			if tt.name != "missing key is falsy" {
				input["value"] = tt.value
			}
			if got := len(set.Matches(input, true)) > 0; got != tt.want {
				t.Errorf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchesMultipleResults(t *testing.T) {
	set := parseConfig(t, `
routes:
  - name: any-item-ok
    match: '.items[] | .ok'
    exec:
      - command: ["true"]
`)

	tests := []struct {
		name  string
		input any
		want  bool
	}{
		{"one truthy result matches", map[string]any{"items": []any{map[string]any{"ok": false}, map[string]any{"ok": true}}}, true},
		{"no truthy results", map[string]any{"items": []any{map[string]any{"ok": false}}}, false},
		{"empty array yields nothing", map[string]any{"items": []any{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(set.Matches(tt.input, true)) > 0; got != tt.want {
				t.Errorf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchesExpressionErrors(t *testing.T) {
	// error() compiles fine but fails at evaluation time.
	set := parseConfig(t, `
routes:
  - name: bad
    match: 'error("deliberate failure")'
    exec:
      - command: ["true"]
`)
	matched := set.Matches(map[string]any{"event": "push"}, true)
	if len(matched) != 0 {
		t.Errorf("failing expression should not match, got %v", routeNames(matched))
	}
}

func TestRenderBody(t *testing.T) {
	set := parseConfig(t, sampleConfig)
	pushes := &Match{Route: set.routes[0].route, c: set.routes[0]}
	original := []byte(`{"event":"push","before":"abc"}`)
	input := map[string]any{"event": "push", "before": "abc"}

	got, err := pushes.RenderBody(0, original, input)
	if err != nil {
		t.Fatalf("RenderBody(0) error = %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("RenderBody(0) = %q, want original %q", got, original)
	}

	got, err = pushes.RenderBody(1, original, input)
	if err != nil {
		t.Fatalf("RenderBody(1) error = %v", err)
	}
	if string(got) != `{"event":"push"}` {
		t.Errorf("RenderBody(1) = %q, want %q", got, `{"event":"push"}`)
	}

	if _, err := pushes.RenderBody(1, original, nil); err == nil {
		t.Error("RenderBody(1) with nil input should fail")
	}
}

func TestRenderBodyTakesFirstResult(t *testing.T) {
	set := parseConfig(t, `
routes:
  - name: multi
    forward:
      - url: https://example.com/hook
        body: '.a, .b'
`)
	m := &Match{Route: set.routes[0].route, c: set.routes[0]}
	got, err := m.RenderBody(0, []byte("original"), map[string]any{
		"a": jsonNumber("1"),
		"b": jsonNumber("2"),
	})
	if err != nil {
		t.Fatalf("RenderBody() error = %v", err)
	}
	if string(got) != "1" {
		t.Errorf("RenderBody() = %q, want %q (first result only)", got, "1")
	}
}

func TestHasTransform(t *testing.T) {
	set := parseConfig(t, sampleConfig)
	m := &Match{Route: set.routes[0].route, c: set.routes[0]}
	if m.HasTransform(0) {
		t.Error("HasTransform(0) = true, want false (forward has no body expression)")
	}
	if !m.HasTransform(1) {
		t.Error("HasTransform(1) = false, want true (forward has a body expression)")
	}
}

func TestRenderBodyTransformErrors(t *testing.T) {
	set := parseConfig(t, `
routes:
  - name: bad-transforms
    forward:
      - url: https://example.com/hook
        body: 'empty'
      - url: https://example.com/hook
        body: 'error("deliberate")'
`)
	m := &Match{Route: set.routes[0].route, c: set.routes[0]}

	if _, err := m.RenderBody(0, []byte("original"), map[string]any{"a": jsonNumber("1")}); err == nil || !strings.Contains(err.Error(), "no output") {
		t.Errorf("RenderBody() error = %v, want no-output error", err)
	}
	if _, err := m.RenderBody(1, []byte("original"), map[string]any{"a": jsonNumber("1")}); err == nil || !strings.Contains(err.Error(), "deliberate") {
		t.Errorf("RenderBody() error = %v, want wrapped transform error", err)
	}
}

func TestRenderBodyNumberPrecision(t *testing.T) {
	set := parseConfig(t, `
routes:
  - name: numbers
    forward:
      - url: https://example.com/hook
        body: '{count: .count}'
`)
	input := map[string]any{"count": jsonNumber("9007199254740993")}
	got, err := (&Match{Route: set.routes[0].route, c: set.routes[0]}).RenderBody(0, nil, input)
	if err != nil {
		t.Fatalf("RenderBody() error = %v", err)
	}
	if !strings.Contains(string(got), "9007199254740993") {
		t.Errorf("RenderBody() = %s, want unrounded integer", got)
	}
}

func routeNames(matches []*Match) []string {
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Route.Name
	}
	return names
}
