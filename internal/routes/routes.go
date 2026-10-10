package routes

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

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
	Name    string    `yaml:"name"`
	Match   string    `yaml:"match"`
	Forward []Forward `yaml:"forward"`
	Exec    []Exec    `yaml:"exec"`
	Noop    bool      `yaml:"noop"`
}

type fileConfig struct {
	Routes []Route `yaml:"routes"`
}

type compiled struct {
	route      Route
	match      *gojq.Code
	transforms []*gojq.Code
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

func (s *Set) Matches(input any, jsonValid bool) []*Match {
	var matched []*Match
	for _, c := range s.routes {
		if !jsonValid && c.match != nil {
			continue
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
