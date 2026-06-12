package acme

import (
	"fmt"
	"os"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/labostack/prox/internal/config"
)

// providerEnvVars maps provider names to their default environment
// variable for the API token.
var providerEnvVars = map[string]string{
	"cloudflare": "CF_DNS_API_TOKEN",
}

// buildDNSSolver creates a DNS-01 solver for the configured provider.
// If no token is set in config, it reads from the provider's environment variable.
func buildDNSSolver(cfg *config.ACMEDNSConfig) (*certmagic.DNS01Solver, error) {
	token := cfg.Token
	if token == "" {
		envVar, ok := providerEnvVars[cfg.Provider]
		if !ok {
			return nil, fmt.Errorf("unknown DNS provider %q", cfg.Provider)
		}
		token = os.Getenv(envVar)
		if token == "" {
			return nil, fmt.Errorf(
				"DNS provider %q: API token not set (set %s environment variable or acme.dns.token in config)",
				cfg.Provider, envVar,
			)
		}
	}

	provider, err := newDNSProvider(cfg.Provider, token)
	if err != nil {
		return nil, err
	}

	return &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider: provider,
			Resolvers:   cfg.Resolvers,
		},
	}, nil
}

// newDNSProvider creates a libdns-compatible DNS provider by name.
func newDNSProvider(name, token string) (certmagic.DNSProvider, error) {
	switch name {
	case "cloudflare":
		return &cloudflare.Provider{APIToken: token}, nil
	default:
		return nil, fmt.Errorf("unsupported DNS provider %q", name)
	}
}
