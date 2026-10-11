package routes

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/greboid/hookmux/internal/verify"
	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
)

type Forward struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

type Exec struct {
	Command []string `yaml:"command"`
}

type Route struct {
	Name      string    `yaml:"name"`
	Match     string    `yaml:"match"`
	MatchPath MatchPath `yaml:"matchpath"`
	Verify    *Verify   `yaml:"verify"`
	Forward   []Forward `yaml:"forward"`
	Exec      []Exec    `yaml:"exec"`
	Noop      bool      `yaml:"noop"`
}

// MatchPath is one or more URL path prefixes a route is restricted to.
// YAML accepts either a single string or a list.
type MatchPath []string

func (p *MatchPath) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!seq" {
		return value.Decode((*[]string)(p))
	}
	var single string
	if err := value.Decode(&single); err != nil {
		return err
	}
	*p = MatchPath{single}
	return nil
}

// Verify describes signature checking for a route; routes with verification
// only match requests whose signature validates.
type Verify struct {
	Type     string `yaml:"type"`
	Secret   string `yaml:"secret"`
	Header   string `yaml:"header"`
	Encoding string `yaml:"encoding"`
	URL      string `yaml:"url"`
}

type fileConfig struct {
	Routes []Route `yaml:"routes"`
}

type compiled struct {
	route      Route
	match      *gojq.Code
	matchPaths []string
	transforms []*gojq.Code
	verify     verify.Verifier
}

type Set struct {
	routes []compiled
}

func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read routing config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Set, error) {
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse routing config: %w", err)
	}

	set := &Set{}
	if len(fc.Routes) == 0 {
		return nil, errors.New("routing config defines no routes")
	}
	for i, r := range fc.Routes {
		label := r.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		c := compiled{route: r, transforms: make([]*gojq.Code, len(r.Forward))}

		for _, p := range r.MatchPath {
			normalized, err := normalizePath(p)
			if err != nil {
				return nil, fmt.Errorf("route %s: invalid matchpath %q: %w", label, p, err)
			}
			c.matchPaths = append(c.matchPaths, normalized)
		}

		if r.Match != "" {
			code, err := compileJQ(r.Match)
			if err != nil {
				return nil, fmt.Errorf("route %s: invalid match expression: %w", label, err)
			}
			c.match = code
		}

		for j, f := range r.Forward {
			if f.URL == "" {
				return nil, fmt.Errorf("route %s: forward %d has no url", label, j)
			}
			if f.Body != "" {
				code, err := compileJQ(f.Body)
				if err != nil {
					return nil, fmt.Errorf("route %s: invalid body expression for forward %d: %w", label, j, err)
				}
				c.transforms[j] = code
			}
		}

		for j, e := range r.Exec {
			if len(e.Command) == 0 {
				return nil, fmt.Errorf("route %s: exec %d has an empty command", label, j)
			}
		}

		if r.Verify != nil {
			v, err := verify.New(verify.Config{
				Type:     r.Verify.Type,
				Secret:   r.Verify.Secret,
				Header:   r.Verify.Header,
				Encoding: r.Verify.Encoding,
				URL:      r.Verify.URL,
			})
			if err != nil {
				return nil, fmt.Errorf("route %s: %w", label, err)
			}
			c.verify = v
		}

		if len(r.Forward) == 0 && len(r.Exec) == 0 && !r.Noop {
			return nil, fmt.Errorf("route %s has no forward, exec or noop actions", label)
		}

		set.routes = append(set.routes, c)
	}
	return set, nil
}

func (s *Set) Len() int {
	return len(s.routes)
}

type Match struct {
	Route Route
	c     compiled
}

func (m *Match) HasTransform(i int) bool {
	return m.c.transforms[i] != nil
}

func (m *Match) RenderBody(i int, original []byte, input any) ([]byte, error) {
	code := m.c.transforms[i]
	if code == nil {
		return original, nil
	}
	if input == nil {
		return nil, errors.New("body transform requires a JSON payload")
	}
	v, ok := code.Run(input).Next()
	if !ok {
		return nil, errors.New("body transform produced no output")
	}
	if err, isErr := v.(error); isErr {
		return nil, fmt.Errorf("evaluate body transform: %w", err)
	}
	out, err := gojq.Marshal(v)
	if err != nil {
		// Unreachable today: Run only yields normalized values (NaN -> null,
		// +/-Inf -> clamped to MaxFloat64); kept as insurance against gojq changes.
		return nil, fmt.Errorf("marshal transformed body: %w", err)
	}
	return out, nil
}

// Matches returns every route whose match expression and signature
// verification (if any) the webhook satisfies.
func (s *Set) Matches(r *http.Request, body []byte, input any, jsonValid bool) []*Match {
	var matched []*Match
	for _, c := range s.routes {
		if len(c.matchPaths) > 0 && (r == nil || !pathMatches(c.matchPaths, r.URL.Path)) {
			continue
		}
		if !jsonValid && c.match != nil {
			continue
		}
		if c.verify != nil {
			if err := c.verify.Verify(r, body); err != nil {
				slog.Warn("Webhook verification failed", "route", c.route.Name, "error", err)
				continue
			}
		}
		ok, err := matches(&c, input)
		if err != nil {
			slog.Error("Failed to evaluate match expression", "route", c.route.Name, "error", err)
			continue
		}
		if ok {
			matched = append(matched, &Match{Route: c.route, c: c})
		}
	}
	return matched
}

func matches(c *compiled, input any) (bool, error) {
	if c.match == nil {
		return true, nil
	}
	iter := c.match.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			return false, nil
		}
		if err, isErr := v.(error); isErr {
			return false, fmt.Errorf("evaluate match expression: %w", err)
		}
		if v != nil && v != false {
			return true, nil
		}
	}
}

func compileJQ(expr string) (*gojq.Code, error) {
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, err
	}
	return gojq.Compile(query)
}

func normalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is empty")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		p = "/"
	}
	return p, nil
}

func pathMatches(prefixes []string, path string) bool {
	for _, p := range prefixes {
		if p == "/" || path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
