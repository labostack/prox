package action

import (
	"net/http"
	"strings"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/resource"
	"github.com/dortanes/prox/internal/router"
)

// Static returns a fixed response with pre-computed body, headers, and status.
// If the body contains template variables like {domain}, {match.domain}, {path},
// {match.path}, {method}, they are interpolated at request time.
type Static struct {
	status     int
	headers    map[string]string
	body       []byte
	isTemplate bool // true if body contains {…} placeholders
}

// NewStatic creates a static response handler.
func NewStatic(act *config.Action, resolver *resource.Resolver) (*Static, error) {
	s := &Static{
		status:  act.Status,
		headers: act.Headers,
	}

	if act.BodyRef.Name != "" {
		body, err := resolver.Resolve(act.BodyRef.Name)
		if err != nil {
			return nil, err
		}
		s.body = body
	}

	// Detect template placeholders.
	if s.body != nil && strings.Contains(string(s.body), "{") {
		s.isTemplate = true
	}

	return s, nil
}

func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, v := range s.headers {
		w.Header().Set(k, v)
	}

	w.WriteHeader(s.status)

	if s.body == nil {
		return
	}

	if !s.isTemplate {
		_, _ = w.Write(s.body)
		return
	}

	// Template interpolation from match context.
	out := string(s.body)

	mr := router.GetMatchResult(r)
	if mr != nil {
		out = strings.ReplaceAll(out, "{domain}", mr.Domain)
		out = strings.ReplaceAll(out, "{domain.pattern}", mr.DomainPattern)
		out = strings.ReplaceAll(out, "{match.domain}", mr.MatchDomain)
		out = strings.ReplaceAll(out, "{match.glob}", mr.MatchGlob)
		out = strings.ReplaceAll(out, "{path}", mr.Path)
		out = strings.ReplaceAll(out, "{match.path}", mr.MatchPath)
	}

	// Request-level variables (always available).
	out = strings.ReplaceAll(out, "{method}", r.Method)
	out = strings.ReplaceAll(out, "{host}", r.Host)

	_, _ = w.Write([]byte(out))
}
