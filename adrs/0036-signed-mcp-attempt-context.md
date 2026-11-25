# ADR 0036: Signed MCP attempt context

Status: Proposed.
Date: 2026-11-24

## Context

The PostgreSQL MCP audit records the authenticated IAM identity. GLaDOS can use
confirmed Multica claim metadata to correlate individual audited calls with a
sandbox attempt.

## Decision

Make signing opt in through the controller's `opencode.mcp_attempt_context`
configuration. The relay sends signed context only for the one exact configured
endpoint URL, with the existing explicit IAM connection and a recent running
confirmation for the relay grant's claim. The workspace, agent, task and attempt
come from that claim. Bind the context to the same access token sent upstream,
including after rotation. Calls without eligible context retain their current
IAM and routing behavior. Deployments that dedicate the issuer audience to the
pilot can also set `restrict_iam_to_target`. This rejects IAM-selected MCP
requests for all other URLs and preserves anonymous and static-header routes.

Reserve `X-Idp-Attempt-Context` at the MCP relay; drop caller-supplied and
configured connection values. The relay adds its own value in the form
`base64url(payload).base64url(HMAC-SHA256(base64url(payload)))`, both segments
unpadded. The HMAC input is the ASCII payload segment, and the key is the
controller-only file's trimmed text. The UTF-8 JSON payload has `v: 1`,
`aud`, `source: "multica-sandbox"`, `server`, `workspaceId`, `agentId`, `taskId`,
`attempt`, `tokenHash` (lowercase SHA-256 hex of the raw access token) and
`iat` (Unix seconds). `server` is the configured Multica base URL, including any
base path and with trailing slashes removed.

## Consequences

The receiver verifies the signature, audience, server, token fingerprint and
timestamp before recording attribution. IAM remains authoritative for permissions.
The signature protects integrity. Private HTTP transports expose the attribution
metadata to the same network observers that can already see the bearer. Use HTTPS
where available and retain existing private network controls for HTTP endpoints.

### Configuration

For the GLaDOS pilot, add the following object inside `opencode` in the controller
configuration. Leave it absent to disable signing.

```json
"mcp_attempt_context": {
  "url": "http://postgres-mcp.example.invalid:8080/mcp",
  "audience": "idp-postgres",
  "key_file": "/run/secrets/mcp-attempt-context",
  "restrict_iam_to_target": true
}
```

Keep `identity_file`, `mcp_relay`, and the connection's existing
`Authorization: Bearer {iam:access_token}` selection. Set `url` to the exact saved
PostgreSQL MCP connection URL, including the path and any query. Preserve the
connection's existing egress policy. In the receiver, use the same audience and
allow the controller's configured `server` without a trailing slash.

Mount the shared signing key into the controller only as a read-only secret
file. Use a random 32-byte key encoded as hex text and give the receiver the same
text. The controller trims surrounding whitespace and requires at least 32
visible ASCII characters in a file of up to 4096 bytes. The key is loaded on
startup. Coordinate key changes with the receiver and restart the controller.
