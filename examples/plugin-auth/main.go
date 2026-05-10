// Example auth plugin that demonstrates the request-response hook API.
//
// This plugin validates Bearer tokens on incoming requests and injects
// an identity header on success.
//
// Build:
//
//	go build -o auth ./examples/plugin-auth
//
// Config:
//
//	{
//	  match: { domain: "*.example.com", path: "/api/*" },
//	  plugins: ["./auth"],
//	  plugin_timeout: "2s",
//	  action: { type: "proxy", upstream: "localhost:3000" },
//	}
package main

import (
	"log"
	"strings"

	"github.com/dortanes/prox/sdk"
)

func main() {
	p := sdk.New()

	p.OnConfigure(func(route sdk.Route) {
		log.Printf("configured for route %s (domain=%s, path=%s)",
			route.ID, route.Domain, route.Path)
	})

	p.OnRequest(func(req *sdk.Request) *sdk.Response {
		token := req.Header("Authorization")
		token = strings.TrimPrefix(token, "Bearer ")

		if token == "" {
			return sdk.Deny(401, "Unauthorized")
		}

		// Validate token here (JWT, database lookup, external service, etc.)
		userID := validateToken(token)
		if userID == "" {
			return sdk.Deny(403, "Forbidden")
		}

		return sdk.Allow(
			sdk.WithHeader("X-User-ID", userID),
		)
	})

	p.OnResponse(func(req *sdk.Request, resp *sdk.UpstreamResponse) *sdk.ResponseMod {
		return sdk.ModifyResponse(
			sdk.WithResponseHeader("X-Powered-By", "prox"),
			sdk.RemoveResponseHeader("Server"),
		)
	})

	p.Run()
}

func validateToken(token string) string {
	// Placeholder — replace with actual validation logic.
	if len(token) > 0 {
		return "user-" + token[:min(len(token), 8)]
	}
	return ""
}
