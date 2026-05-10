// Package action implements request handlers (proxy, static, serve, pass, drop).
package action

import (
	"fmt"
	"net/http"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/resource"
)

// Registry maps action names to their http.Handler implementations.
type Registry struct {
	handlers map[string]http.Handler
}

// RouteHints maps action names to route paths for prefix stripping.
type RouteHints struct {
	PathByAction map[string]string
}

// Build constructs all action handlers from config.
// svcCfg is optional service-level configuration for transport tuning.
func Build(actions map[string]*config.Action, resolver *resource.Resolver, hints *RouteHints, svcCfg *config.ServerConfig) (*Registry, error) {
	handlers := make(map[string]http.Handler, len(actions))

	for name, act := range actions {
		routePath := ""
		if hints != nil {
			routePath = hints.PathByAction[name]
		}

		h, err := buildHandler(act, resolver, routePath, svcCfg)
		if err != nil {
			return nil, fmt.Errorf("building action %q: %w", name, err)
		}
		handlers[name] = h
	}

	// Wire fallback handlers (second pass to allow forward references).
	for name, act := range actions {
		if act.Fallback == "" {
			continue
		}
		fb, ok := handlers[act.Fallback]
		if !ok {
			return nil, fmt.Errorf("action %q: fallback %q not found", name, act.Fallback)
		}
		if p, ok := handlers[name].(*Proxy); ok {
			p.SetFallback(fb)
		}
	}

	return &Registry{handlers: handlers}, nil
}

// Get returns the handler for a named action.
func (reg *Registry) Get(name string) http.Handler {
	return reg.handlers[name]
}

func buildHandler(act *config.Action, resolver *resource.Resolver, routePath string, svcCfg *config.ServerConfig) (http.Handler, error) {
	switch act.Type {
	case config.ActionTypeProxy:
		return NewProxy(act, svcCfg)
	case config.ActionTypeStatic:
		return NewStatic(act, resolver)
	case config.ActionTypeServe:
		return NewServe(act, routePath)
	case config.ActionTypePass:
		// Pass actions are handled at L4 by the dispatcher — they never reach HTTP.
		// This is a safety net in case of misconfiguration.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}), nil
	case config.ActionTypeDrop:
		return &Drop{}, nil
	default:
		return nil, fmt.Errorf("unknown action type %q", act.Type)
	}
}
