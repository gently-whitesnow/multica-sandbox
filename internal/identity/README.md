# Controller identity

Resolve credentials from trusted Multica configuration, obtain a signed JWT and
validate it before delivery. Long-lived credentials and JWTs stay in the controller.
OpenCode receives opaque, revocable relay grants; the adapter renews identity while
Multica reports the attempt running.

## Agent Environment

With `agent_env: true`, read `IAM_CLIENT_ID` and `IAM_CLIENT_SECRET` from the
claim's agent Environment. Both select that source; neither falls back to an
optional configured binding/resolver; one alone denies. Running attempts retain
their claim snapshot; changes apply to the next attempt. The controller consumes
these variables, so they never enter the agent process. No alternate names exist.

For Keycloak, these values are the service-account client ID and secret. For
OpenBao, they are the AppRole role ID and secret ID. Multica owns assigning
credentials to agents. Copying the pair to another agent gives it the same
external identity; ENV mode does not promise a unique identity per Multica UUID.

All agents in one service use one trusted issuer/source profile. A minimal OpenBao
configuration needs no list of agents or MCP URLs:

```json
{
  "version": 1,
  "agent_env": true,
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
  }
}
```

`ReadConfig` accepts strict version-1 JSON. Endpoints require HTTPS unless
`allow_http` explicitly permits a private deployment or fixture. Issuer endpoints,
secret files and controller credentials never come from workload input. HTTP
requests have a ten-second timeout and 64 KiB response bound, with redirects and
proxy ENV disabled. Credential and token formatting/serialization redact values.

## Token sources and verification

`TokenSource` separates credential exchange from common JWT validation. OAuth2
client credentials obtain only `access_token`; there is no automatic ID-token
substitution or opaque-token introspection. Configure `oauth2_client_credentials`
and `token_url`; `auth_method` selects `client_secret_post` or `client_secret_basic`.
The original `issuer.token_url` remains supported; when both URLs exist they must
match. OAuth token parameters may carry scopes/resource, never tool permissions.

OpenBao AppRole exchange authenticates using JSON `role_id`/`secret_id`, obtains
`auth.entity_id` from that authenticated login, requests the configured identity
JWT and revokes the temporary login token. Cleanup runs after failure/cancellation;
failed revocation denies delivery. All three endpoints must share an origin.
The AppRole policy permits the identity signing role and self-revocation. That
role owns audience, principal claims and TTL; OAuth scopes/resource are rejected.

The common verifier checks RS256/ES256 signature, exact issuer, nonempty subject,
audience when configured, expiry, issuance time, optional not-before and bounded
lifetime. `max_ttl_seconds` defaults to 300 and is at most 3600.

- `keycloak` checks `azp == client_id` and `typ == Bearer`. Set audience explicitly;
  omitted audience preserves recipient-owned checking for existing deployments.
- `jwt` requires audience and a named nonempty principal claim. A static binding or
  resolver supplies its expected principal. OpenBao additionally requires
  `JWT.sub == login.entity_id`; this trusted exchange proof permits ENV mode
  without a static principal. A generic OAuth/JWT ENV source without such proof
  is rejected. Principal claims are read only after signature validation.

A logical-agent JWT is not task attestation. OpenBao can issue identical JWTs for
one entity in one second. Isolation and cancellation use per-attempt relay grants;
optional fingerprint authority needs its own compatible identity contract.

## Other credential sources

Static `bindings` select a unique client per Multica agent across workspaces and
reference bounded regular absolute secret files, re-read on issuance. Generic
JWT bindings also supply expected `principal`. Secret values do not belong in JSON.

Alternatively, `external: {"url":"https://resolver.example/resolve",
"bearer_file":"/run/secrets/resolver"}` supplies an authenticated resolver. A POST
contains version 1 and the trusted server/workspace/agent reference. A 200 response
must echo that reference and return `client_id`, `client_secret`, optional expected
`principal` and token parameters. Malformed responses, outages and mismatches deny;
static and external sources do not silently fall back to one another.

## Delivery

An MCP explicitly selects identity through its ordinary native Multica header:
`Authorization: Bearer {iam:access_token}`. The adapter recognizes this directive,
then calls `AcquireForMCP`; identity policy keeps no endpoint list. The template
is not sent upstream or exposed as a real JWT in the attempt. Anonymous and
ordinary user-header connections never trigger identity issuance. Deployment
proxy/network policy owns reachability independently of the selected auth mode.

`http_services` remains an operator-owned contract for non-MCP HTTP consumers.
Services fix an origin and exact method/path routes, optionally a clean base path.
The shared MCP relay serves `/http/<name>/<path>` with the current JWT. The attempt
receives only opaque `SANDBOX_HTTP_<NAME>_URL/TOKEN`; no source credential or JWT.
An explicit `bearer_file` selects a controller-only static service credential and
requires an agent allowlist. `path_credential` supports APIs with credentials in
the path, withholding upstream errors. HTTP services work without MCP selection.
They do not grant recipient permissions, which the receiving service owns.

Run `go test -race ./internal/identity ./internal/opencode`,
`./scripts/test-identity.sh` for real Keycloak/MCP and
`./scripts/test-openbao-identity.sh` for disposable OpenBao issuance/rotation.
