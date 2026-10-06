# ADR 0009: Stable agent identity and attempt authorization

Status: Accepted
Date: 2026-10-04

## Context

One controller can execute tasks from multiple workspaces. Public runtime
visibility does not authorize access to workspace tools or models. A permanent
agent identity must not grant every attempt the same resource access.

## Decision

Keep the external OAuth service client/account mapped to one Multica agent.
Use issuer and subject as the principal key. Do not create an OIDC client per task.
Keep client secrets, private keys and refresh credentials outside the sandbox.
Use deployment-provided identity issuance and workload attestation rather than
building an issuer in this repository.

Authorize each attempt separately in trusted state: workspace, agent, task,
attempt, allowed resources, audience, expiry and active/revoked status. Bind the
proof delivered to the workload to that state; caller-supplied IDs are not proof.
Require short-lived access tokens, audience checks and online grant checks on
new operations. Revoke on completion, cancellation and recovery. Define bounded
leases for controller outages before enabling external access in the controller.

Tools use trusted remote MCP services. Inference uses a protected OpenAI-compatible
HTTP gateway with workspace model policy and budgets, reached through the
controller relay (ADR 0012). Its per-attempt grant lives in controller memory, so
attempt leases cover MCP only. This resolves ADR 0003's open inference transport
decision: MCP is not required for model calls.
Provider and tool credentials remain external. Reuse an existing inference gateway;
this project integrates the execution boundary, not provider routing or IAM.
An OpenAI-compatible API alone does not imply support for run authorization.

Start with an isolated contract fixture using Keycloak 26.8.0, the official MCP
Go SDK 1.8.0 and go-oidc 3.21.0. The verifier checks Keycloak access-token type and
client in addition to signature, issuer, audience and expiry. Test-only trusted
registration binds token fingerprints to grants; it is not workload attestation.
Dependencies are pinned in Go modules and image configuration. They introduce
no production controller dependency on Keycloak or a particular inference vendor.

## Consequences

The fixture exercises two attempts of one agent, resource/workspace boundaries
and immediate denial of new calls after revocation. It does not deliver identity
to a real sandbox or provide inference, distributed authorization or audit.
The example's in-memory registry fails closed on restart. Its admin endpoint is
fixture plumbing, not a proposed production authorization API. In-flight operation
cancellation requires a separate integration contract.

Production integration must verify identity bootstrap, tenant/resource isolation,
wrong-audience/expired-token rejection, lifecycle revocation and external egress
policy before advertising support. The experimental OpenCode path integrates an authenticated deployment-owned
authority through `internal/attempt`. Renewals register JWT fingerprints with an
exact MCP recipient and a maximum 15-second lease, checked every second. Completion,
cancellation and failed renewal revoke the attempt before terminal reporting; startup
recovers the controller namespace before claims. An authority outage still removes
the container and stops the controller; downstream access ends when its lease expires.
The authority must reject ended-attempt re-enrollment and fingerprint reassignment,
check leases on every operation and fail closed after restart. Its wire adapter is
experimental integration plumbing, not a runtime-owned IAM service.
No security capability is inferred from protocol compatibility.

## References

- [OAuth security BCP](https://www.rfc-editor.org/rfc/rfc9700.html)
- [OAuth resource indicators](https://www.rfc-editor.org/rfc/rfc8707.html)
- [Keycloak service accounts](https://www.keycloak.org/docs/latest/server_admin/index.html#_service_accounts)
- [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [go-oidc](https://github.com/coreos/go-oidc)
