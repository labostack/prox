# TLS & Certificates

prox supports TLS with both manual certificate management and automatic certificate issuance via ACME (e.g., Let's Encrypt).

## Manual Certificates

### Single File

Provide the path to a certificate and its private key:

```json5
{
  services: {
    web: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/example.crt",
      tls_key: "/etc/prox/certs/example.key",
      routes: [...]
    },
  },
}
```

The certificate file may be PEM-encoded `.crt` or `.pem`. The key file must be the corresponding PEM-encoded private key.

### Directory Mode

Point `tls_cert` to a directory instead of a single file. prox discovers all certificate/key pairs automatically:

```json5
{
  services: {
    web: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/",
      routes: [...]
    },
  },
}
```

When `tls_cert` is a directory, `tls_key` is not required. prox scans for `.crt` and `.pem` files and automatically pairs each with its corresponding `.key` file (matched by filename). The correct certificate is selected at runtime via SNI — the TLS ClientHello indicates which domain the client is connecting to, and prox serves the matching certificate.

!!! tip
    Directory mode is ideal for multi-domain setups. Drop new certificate pairs into the directory and reload — no config changes needed.

## Automatic Certificates (ACME)

prox can automatically obtain and renew TLS certificates using the ACME protocol. This eliminates manual certificate management entirely.

When `acme` is configured, prox uses [CertMagic](https://github.com/caddyserver/certmagic) to handle the full certificate lifecycle — issuance, renewal, and OCSP stapling — with no manual intervention required.

### Quick Start

The minimal ACME configuration requires only an email address. prox enables TLS automatically when `acme` is present:

```json5
{
  services: {
    web: {
      listen: ":443",
      acme: {
        email: "certs@example.com",
      },
      routes: [
        { match: { domain: "example.com", path: "/*" }, action: "site" },
        { match: { domain: "api.example.com", path: "/*" }, action: "api" },
      ],
    },
  },
  actions: {
    site: { type: "serve", root: "./public" },
    api:  { type: "proxy", upstream: "localhost:3000" },
  },
}
```

With this configuration, prox automatically:

1. Enables TLS on the service
2. Discovers `example.com` and `api.example.com` from the route domains
3. Obtains certificates from Let's Encrypt
4. Renews certificates before expiration
5. Enables OCSP stapling

### Configuration Reference

| Field | Default | Description |
|-------|---------|-------------|
| `email` | *(required)* | ACME account email, used for certificate expiration notices |
| `ca` | `""` (Let's Encrypt) | CA directory URL or shorthand: `"staging"`, `"zerossl"`, `"letsencrypt"` |
| `cas` | `[]` | Fallback CAs, tried in order. Mutually exclusive with `ca` |
| `challenge` | `"alpn"` | Challenge type: `"alpn"` (TLS-ALPN-01), `"http"` (HTTP-01), or `"dns"` (DNS-01) |
| `dns` | — | DNS provider config, required when `challenge` is `"dns"` |
| `dns.provider` | *(required)* | DNS provider name: `"cloudflare"` |
| `dns.token` | *(env var)* | API token. Falls back to provider env var if empty |
| `dns.discover` | `false` | Fetch all zones from provider account and manage certificates automatically |
| `dns.resolvers` | `["1.1.1.1:53", "8.8.8.8:53"]` | DNS resolvers for zone detection and propagation checks. Override to use custom nameservers |
| `storage_type` | `"file"` | Storage backend: `"file"` (local filesystem) or `"s3"` (S3-compatible) |
| `storage` | `"acme/"` | Storage path for certificates and account data (file backend) |
| `s3` | — | S3 storage config, required when `storage_type` is `"s3"` |
| `s3.bucket` | *(required)* | S3 bucket name |
| `s3.region` | `"us-east-1"` | AWS region |
| `s3.endpoint` | — | Custom endpoint for S3-compatible providers (MinIO, R2, Spaces) |
| `s3.access_key` | — | Static access key. Falls back to AWS credential chain if empty |
| `s3.secret_key` | — | Static secret key. Must be set together with `access_key` |
| `s3.prefix` | `""` (root) | Key prefix within the bucket |
| `s3.use_path_style` | `false` | Force path-style URLs (required for MinIO) |
| `domains` | *(auto)* | Explicit domain list. If empty, auto-discovered from routes |

### Challenge Types

ACME uses challenges to verify domain ownership. prox supports all three standard challenge types.

#### TLS-ALPN-01 (default)

The default challenge type. The CA connects to port 443 and performs a TLS handshake with a special ALPN protocol to verify domain control.

```json5
acme: {
  email: "certs@example.com",
  // challenge defaults to "alpn"
}
```

!!! note
    TLS-ALPN-01 requires port 443 to be publicly accessible and routed to prox. This is the simplest option when prox is the edge server.

#### HTTP-01

The CA makes an HTTP request to port 80 to verify domain control. prox handles the `/.well-known/acme-challenge/` path automatically.

```json5
acme: {
  email: "certs@example.com",
  challenge: "http",
}
```

!!! warning
    HTTP-01 requires port 80 to be publicly accessible and routed to prox. This challenge type does **not** support wildcard certificates.

#### DNS-01

The CA verifies domain control by checking a DNS TXT record. This is the only challenge type that supports wildcard certificates and does not require any inbound port access.

```json5
acme: {
  email: "certs@example.com",
  challenge: "dns",
  dns: {
    provider: "cloudflare",
  },
}
```

!!! tip
    DNS-01 is the best choice when prox is behind a firewall, NAT, or CDN — the CA never needs to connect to your server directly.

### DNS Providers

#### Cloudflare

To use Cloudflare as the DNS provider, create an API token with the following permissions:

- **Zone → DNS → Edit** — allows creating and deleting TXT records for challenge validation
- **Zone → Zone → Read** — allows listing zones to find the correct zone ID

Set the token via environment variable (recommended):

```bash
export CF_DNS_API_TOKEN="your-api-token"
```

Or directly in the configuration:

```json5
acme: {
  email: "certs@example.com",
  challenge: "dns",
  dns: {
    provider: "cloudflare",
    token: "your-api-token",  // or use CF_DNS_API_TOKEN env var
  },
}
```

!!! tip
    Using the environment variable `CF_DNS_API_TOKEN` keeps secrets out of your configuration files.

#### Automatic Domain Discovery

When `dns.discover` is enabled, prox fetches all active zones from the provider account and manages certificates for each zone automatically — both the apex domain and wildcard (`example.com` + `*.example.com`).

```json5
acme: {
  email: "certs@example.com",
  challenge: "dns",
  dns: {
    provider: "cloudflare",
    discover: true,
  },
  storage: "/mnt/ssl",
}
```

With this configuration, prox:

1. Calls the Cloudflare API to list all active zones in the account
2. Issues certificates for each zone and its wildcard
3. On reload, re-fetches zones and issues certificates for any new domains
4. Only requests new certificates when none exist or renewal is due

!!! tip
    This is ideal for multi-domain setups where domains are added frequently. Add a new domain in Cloudflare, reload prox — the certificate is issued automatically.

### Wildcard Certificates

Wildcard certificates cover all subdomains of a domain (e.g., `*.example.com`). They require the DNS-01 challenge type.

```json5
{
  services: {
    web: {
      listen: ":443",
      acme: {
        email: "certs@example.com",
        challenge: "dns",
        dns: { provider: "cloudflare" },
        domains: ["example.com", "*.example.com"],
      },
      routes: [
        { match: { domain: "*.example.com", path: "/*" }, action: "app" },
        { match: { domain: "example.com", path: "/*" }, action: "site" },
      ],
    },
  },
  actions: {
    app:  { type: "proxy", upstream: "localhost:3000" },
    site: { type: "serve", root: "./public" },
  },
}
```

!!! warning
    Wildcard domains in `acme.domains` require `challenge: "dns"`. Validation will reject wildcard domains with other challenge types.

### Multiple Certificate Authorities

Use `cas` to define fallback certificate authorities. If the first CA is unavailable or rate-limited, prox tries the next one in order:

```json5
acme: {
  email: "certs@example.com",
  cas: [
    "letsencrypt",
    "zerossl",
  ],
}
```

The following shorthand values are recognized:

| Shorthand | CA |
|-----------|-----|
| `"letsencrypt"` or `"production"` or `""` | Let's Encrypt Production |
| `"staging"` | Let's Encrypt Staging |
| `"zerossl"` | ZeroSSL Production |

Any other value is treated as a full ACME directory URL.

!!! note
    `ca` and `cas` are mutually exclusive. Use `ca` for a single CA, or `cas` for ordered fallback.

### Mixed Mode

Manual certificates and ACME can coexist on the same service. This is useful for gradual migration or when some certificates are managed externally:

```json5
{
  services: {
    web: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/",   // manually managed certs
      acme: {
        email: "certs@example.com",   // automatic certs for everything else
      },
      routes: [...]
    },
  },
}
```

In mixed mode, prox first checks the manual certificate store. If no manual certificate matches the SNI domain, the ACME manager handles the request — obtaining a certificate on demand if needed.

### Staging

Use the Let's Encrypt staging environment for testing. Staging certificates are **not** trusted by browsers but have much higher rate limits:

```json5
acme: {
  email: "certs@example.com",
  ca: "staging",
}
```

!!! tip
    Always test your ACME configuration with `ca: "staging"` first. Let's Encrypt production has [strict rate limits](https://letsencrypt.org/docs/rate-limits/) — exceeding them can block certificate issuance for your domain for up to a week.

### Certificate Storage

ACME certificates, private keys, and account data need a persistent storage backend. prox supports two storage backends: **file** (local filesystem, default) and **s3** (S3-compatible object storage).

#### File Storage (default)

The default backend stores data in a local directory. The default location is an `acme/` directory next to the configuration file.

```
config.json5
acme/
├── acme-v02.api.letsencrypt.org-directory/
│   ├── users/
│   │   └── certs@example.com/
│   └── certificates/
│       ├── example.com.crt
│       └── example.com.key
```

To customize the storage path:

```json5
acme: {
  email: "certs@example.com",
  storage: "/var/lib/prox/acme",
}
```

Relative paths are resolved from the directory of the configuration file. Absolute paths are used as-is.

!!! warning
    The storage directory contains private keys. Ensure appropriate file permissions (`700` for the directory, `600` for key files).

#### S3 Storage

For multi-server deployments, prox can store ACME data in any S3-compatible object storage — AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces, etc.

Set `storage_type: "s3"` and provide the `s3` configuration block:

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "my-certificates",
    prefix: "prox/acme/",  // optional, default is bucket root
  },
}
```

prox uses advisory locking via S3 object metadata to coordinate certificate issuance across multiple instances, preventing duplicate requests and rate limit issues.

##### AWS S3

With IAM roles (EC2/ECS/EKS), no credentials are needed — prox uses the default AWS credential chain:

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "my-certs",
    region: "eu-west-1",
  },
}
```

With static credentials:

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "my-certs",
    region: "us-east-1",
    access_key: "AKIAIOSFODNN7EXAMPLE",
    secret_key: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  },
}
```

!!! tip
    Use IAM roles or environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) instead of hardcoding credentials in the config.

##### MinIO

MinIO requires path-style addressing and a custom endpoint:

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "certificates",
    endpoint: "https://minio.internal:9000",
    access_key: "minioadmin",
    secret_key: "minioadmin",
    use_path_style: true,
  },
}
```

##### Cloudflare R2

R2 uses the S3-compatible API with an account-specific endpoint:

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "prox-certs",
    endpoint: "https://<account-id>.r2.cloudflarestorage.com",
    access_key: "...",
    secret_key: "...",
  },
}
```

##### DigitalOcean Spaces

```json5
acme: {
  email: "certs@example.com",
  storage_type: "s3",
  s3: {
    bucket: "prox-certs",
    region: "nyc3",
    endpoint: "https://nyc3.digitaloceanspaces.com",
    access_key: "...",
    secret_key: "...",
  },
}
```

!!! warning
    The S3 bucket contains private keys. Ensure the bucket is not publicly accessible and has appropriate IAM/access policies.

### OCSP Stapling

prox automatically enables OCSP stapling for all certificates — both manually loaded and ACME-managed. OCSP responses are fetched from the CA's OCSP responder and stapled to the TLS handshake, eliminating the need for clients to contact the CA separately.

No configuration is needed. OCSP stapling is always active when certificates include an OCSP responder URL.

### Domain Auto-Discovery

When `acme.domains` is not set, prox automatically discovers domains from the `match.domain` patterns in the service's routes:

```json5
{
  services: {
    web: {
      listen: ":443",
      acme: { email: "certs@example.com" },
      routes: [
        { match: { domain: "example.com" }, action: "site" },
        { match: { domain: "api.example.com" }, action: "api" },
      ],
    },
  },
}
```

prox extracts `example.com` and `api.example.com` from the route configuration and manages certificates for both automatically.

!!! note
    Wildcard domain patterns like `*.example.com` in routes are **not** auto-discovered for ACME — wildcard certificates require explicit `acme.domains` with the `dns` challenge type.

### Multi-Server Deployment

When running prox on multiple servers with the same domains, each server would independently request certificates — which can exhaust rate limits and cause DNS-01 challenge conflicts.

#### S3 Storage (recommended)

Use S3-compatible storage to share ACME data across all prox instances. prox uses advisory locking via S3 object metadata to ensure only one instance issues a certificate at a time:

```json5
acme: {
  email: "certs@example.com",
  challenge: "dns",
  dns: { provider: "cloudflare" },
  storage_type: "s3",
  s3: {
    bucket: "prox-certs",
    region: "us-east-1",
  },
}
```

!!! tip
    S3 storage is the recommended approach for multi-server deployments. No shared filesystem infrastructure is needed — any S3-compatible provider works.

#### Shared Filesystem

Alternatively, mount a shared filesystem (NFS, EFS) as the ACME storage path across all servers:

```json5
acme: {
  email: "certs@example.com",
  challenge: "dns",
  dns: { provider: "cloudflare" },
  storage: "/mnt/shared/prox-acme/",  // NFS mount
}
```

CertMagic uses file-based locking — only one server obtains the certificate, and the others use it from the shared storage.

#### Hybrid Approach

Use ACME on a single primary server and distribute certificates to the rest using the manual `tls_cert` directory mode:

1. **Primary server** — runs with `acme` config, obtains and renews certificates
2. **Other servers** — use `tls_cert` pointing to a synced copy of the primary's ACME storage directory

Synchronize the ACME storage directory using `rsync`, configuration management (Ansible, Chef), or a CI/CD pipeline.

!!! warning
    Let's Encrypt allows **5 duplicate certificates per domain per week** and **50 certificates per registered domain per week**. Running ACME independently on many servers without shared storage will quickly hit these limits.

### Troubleshooting

**Rate limits**

Let's Encrypt enforces rate limits on certificate issuance. If you hit a limit, issuance will fail with an error indicating the limit type. Use `ca: "staging"` for testing to avoid production rate limits.

**DNS zone detection failures**

In containerized environments (Docker, Kubernetes), ACME zone detection may fail with errors like `expected 1 zone, got 0`. This happens when the container's DNS resolver cannot properly return SOA records, causing certmagic to misidentify the DNS zone (e.g., detecting `xyz.` instead of `example.xyz`). prox defaults to using public DNS resolvers (`1.1.1.1` and `8.8.8.8`) to avoid this, but you can override them if needed:

```json5
dns: {
  provider: "cloudflare",
  resolvers: ["1.1.1.1:53", "8.8.8.8:53"],
}
```

**DNS propagation**

DNS-01 challenges require TXT records to propagate before the CA can verify them. If challenges fail with timeout errors, the DNS provider may have slow propagation. prox uses public DNS resolvers by default for propagation checks, which typically see Cloudflare records within seconds. If you still experience timeouts, check your provider's TTL settings.

**Firewall rules**

- **TLS-ALPN-01**: Port 443 must accept inbound connections from the CA
- **HTTP-01**: Port 80 must accept inbound connections from the CA
- **DNS-01**: No inbound ports required — only outbound API access to the DNS provider

**Certificate not renewing**

Certificates are renewed automatically before expiration. If renewal fails, check the logs for errors. Common causes:

- DNS provider token expired or revoked
- Firewall rules changed, blocking CA access
- Domain no longer points to the server (for ALPN/HTTP challenges)

**Storage permissions**

If prox cannot write to the storage directory, certificate operations will fail. Ensure the prox process has read/write access to the storage path.

```bash
# Check storage directory permissions
ls -la /path/to/acme/
```

