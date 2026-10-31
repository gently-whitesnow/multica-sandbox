# Controller identity

Resolves a trusted Multica server/workspace/agent reference, obtains a signed JWT
and validates it before credential delivery. Long-lived credentials and JWTs stay
outside attempts. The OpenCode adapter renews them while Multica reports the task
running; attempts use opaque, revocable relay credentials.

## Configuration

`ReadConfig` parses strict version-1 JSON; `New(config, controllerServer)` validates
policy. The server comes from controller configuration. Workloads cannot select
issuers, secret files, principals or recipients. Endpoints must use HTTPS unless
`allow_http` explicitly permits a private fixture or deployment network. Requests
have a ten-second timeout and 64 KiB response bound; redirects and proxy ENV are
disabled. Ordinary formatting redacts credentials; JSON serialization refuses them.

All agents in one identity service use one issuer/source profile. Bindings select
a unique client per agent across workspaces. Secret files must be regular absolute
paths, are bounded and are read on every issuance. Atomic replacement and projected
secret symlinks support rotation. Secret values never belong in configuration JSON.

An OpenBao installation uses only standard configuration:

```json
{
  "version": 1,
  "issuer": {
    "profile": "jwt",
    "url": "https://bao.example.com/v1/identity/oidc",
    "jwks_url": "https://bao.example.com/v1/identity/oidc/.well-known/keys",
    "audience": "configured-oidc-role-client-id",
    "principal_claim": "service_principal",
    "max_ttl_seconds": 300
  },
  "token_source": {
    "type": "openbao_approle",
    "login_url": "https://bao.example.com/v1/auth/approle/login",
    "token_url": "https://bao.example.com/v1/identity/oidc/token/tools",
    "revoke_url": "https://bao.example.com/v1/auth/token/revoke-self"
  },
  "bindings": [{
    "workspace_id": "10000000-0000-4000-8000-000000000001",
    "agent_id": "20000000-0000-4000-8000-000000000001",
    "client_id": "approle-role-id",
    "secret_file": "/run/secrets/agent-secret-id",
    "principal": "builder"
  }],
  "mcp": ["https://tools.example.com/mcp"],
  "http_services": [{
    "name": "control",
    "origin": "https://control.example.com",
    "routes": [
      {"method": "GET", "path": "/api/v1/settings"},
      {"method": "POST", "path_prefix": "/api/v1/deployment-access/"}
    ]
  }]
}
```

The source authenticates using JSON `role_id`/`secret_id`, requests the identity JWT,
then revokes the temporary login token, including on issuance failure/cancellation.
Failed cleanup denies delivery. All three endpoints must share an origin. The
AppRole policy must allow the named identity role and `auth/token/revoke-self`.
The signing role controls JWT claims, audience and TTL. Configure the exact JWT
issuer from discovery, including its path; OpenBao's issuer-base setting differs.
OAuth scope/resource request parameters are rejected for this source.

## OAuth and JWT profiles

`TokenSource` separates credential exchange from common verification. For OAuth,
use `token_source.type: "oauth2_client_credentials"` and its `token_url`. Original
`issuer.token_url` remains compatible; when both are present they must match.
OpenBao must use only `token_source.token_url`. OAuth `auth_method` selects
`client_secret_post` (legacy default) or `client_secret_basic`; unknown methods deny. OAuth obtains only `access_token`;
there is no automatic ID-token substitution or opaque-token introspection.

`issuer.profile: "keycloak"` checks `azp == client_id` and `typ == Bearer`.
Set `issuer.audience` explicitly. An omitted profile preserves the original
Keycloak contract, including recipient-owned audience checking if it is absent.
A Keycloak binding uses its service-client ID and a secret-file reference; it
needs no `principal`. Optional binding `token` carries OAuth `scopes` and `resource`
issuance parameters, never tool permissions.

`issuer.profile: "jwt"` requires `audience`, `principal_claim` and a trusted
binding/resolver `principal`. It assumes no Keycloak claims. Both profiles verify
RS256/ES256 signature, exact issuer, nonempty subject, expiry, issuance time,
optional not-before and bounded lifetime. `max_ttl_seconds` defaults to 300 and is
at most 3600; it validates rather than changes issuer configuration. The receiving
service still authorizes audiences, roles and concrete resources.

A logical-agent JWT is not task attestation. In particular, OpenBao can issue
identical JWTs for the same agent within one second. Attempt isolation uses opaque
relay grants; an optional external fingerprint authority requires its own compatible
identity contract and must not assume each issuance is unique.

## Credential resolution

With `agent_env: true`, Keycloak credentials may come from trusted claim
`IDP_IAM_CLIENT_ID` and `IDP_IAM_CLIENT_SECRET`. Both names select that source;
neither falls back to the configured resolver; one alone denies. Renewal uses the
attempt's snapshot. `env_allow` reserves these names. This mode is unavailable for
`profile: "jwt"`: the pair supplies no independent expected principal.

Replace `bindings` with `external: {"url":"https://resolver.example/resolve",
"bearer_file":"/run/secrets/resolver"}` for an authenticated resolver, with no
fallback between static and external sources. The controller POSTs
`{"version":1,"agent":{"server":"https://multica.example","workspace_id":"…","agent_id":"…"}}`.
A 200 response must echo that exact reference and return `client_id`, `client_secret`,
optional `principal` and optional `token` issuance parameters. Unknown fields,
wrong references/versions, malformed credentials or non-200 responses deny. The
resolver owns agent/client consistency and cannot supply credential endpoints.

## Recipients and relay lifecycle

`mcp` approves exact URLs selected by native Multica claims; it adds no connection
or tool. `mcp_anonymous` approves disjoint exact URLs that receive no token.
`AcquireForMCP` checks the recipient before contacting the source. Ongoing renewal,
status checks and cleanup belong to the adapter, not a background issuer loop.

`http_services` separately names operator-approved HTTP recipients and method/path
routes. An origin may have a clean base path, never a query. Each route has an exact `path` or a slash-ended
`path_prefix`; whole-origin wildcards and CONNECT are unavailable. The shared MCP
relay serves `/http/<name>/<path>` with the current agent JWT. It preserves API
query parameters, strips caller credentials, rejects traversal/redirects and
revokes in-flight access with the attempt. No source/JWT enters the workload.
The attempt receives `SANDBOX_HTTP_<NAME>_URL` and `SANDBOX_HTTP_<NAME>_TOKEN` (opaque).
These managed environment names cannot be overridden through `env_allow`.
An explicit service `bearer_file` instead uses a controller-held static HTTP API
credential. It requires an `agent_ids` allowlist; other agents receive no service
environment/route and cannot acquire it. The file is re-read on renewal with a
30-second internal lease. Missing files fail closed, without JWT fallback. Optional
trusted `X-*` `headers` replace caller values. This mode is separate from JWT
`TokenSource` verification and never places the service credential in an attempt.
HTTP recipients work even when the agent selects no MCP connections. Recipient
permissions remain server-owned; this route does not grant production access.

Run `go test -race ./internal/identity ./internal/opencode` for contracts and relay
adversarial tests, `./scripts/test-identity.sh` for real Keycloak/MCP and
`./scripts/test-openbao-identity.sh` for disposable OpenBao issuance and rotation.
`VERIFY_OPENCODE=1 ./verify.sh` exercises native task renewal separately.
