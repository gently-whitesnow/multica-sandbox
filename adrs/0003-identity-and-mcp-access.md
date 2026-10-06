# ADR 0003: Run identity and MCP-mediated access

Status: Accepted
Date: 2026-10-04

## Context

Agent-generated code can inspect its environment, files and accessible processes.
Hiding a credential from the model is not a security boundary. MCP standardizes
calls but does not make a tool or its arguments safe. This decision supersedes
ADR 0001's per-run service credential delivery.

## Decision

Treat the entire sandbox, custom image, tool bundle and preparation script as
untrusted. Only short-lived proof of run identity may enter it. Keep upstream
credentials, Multica task/daemon tokens, registry credentials and signing keys
in trusted services outside the sandbox. Never deliver them through environment
variables, files, mounts, prompts, logs, snapshots or credential-bearing sidecars
reachable by agent code. Local MCP helpers may hold no upstream credentials.
ADR 0014 records one narrow exception: an opaque per-attempt credential for the
controller's Multica API relay; the `mat_` task token itself stays in controller memory.

Use an existing workload identity issuer, such as SPIRE or deployment OIDC.
Bind attested workload identity to tenant, agent, task and execution attempt in
trusted server state. Never trust caller-supplied task IDs as authorization.
Identity proofs need narrow audiences and short lifetimes; gateways must reject
ended or revoked runs without waiting for expiry. A readable identity credential
can still be stolen: it must never grant broader or longer access than the run.

External tool and service access goes through trusted remote MCP services. They
validate identity and current grants for every invocation, including concrete
resources and arguments. Obtain downstream credentials server-side; never forward
the sandbox identity token as an upstream API token or return service credentials.
Use established MCP authorization libraries. Fail closed on policy/auth failures.
No unrestricted fetch, shell or credential lookup tool may bypass resource policy.
Apply outbound destination validation at the MCP service too, including redirects
and DNS resolution, to prevent credential leakage and SSRF.

Enforce default-deny network policy outside agent control. Allow only configured
identity, MCP, protected inference and narrow controller endpoints; prevent direct Internet/internal
API access, cloud metadata, host sockets and Kubernetes API access. Remove default
service-account mounts. The controller channel accepts only that run's lifecycle
events. External Git and package operations also require mediated access; local
Git, compilers and filesystem operations remain local. An adapter requiring raw
provider credentials is unsupported. ADR 0009 selects a protected OpenAI-compatible inference gateway with separate
audience and attempt authorization. Direct provider access remains forbidden.

Audit principal, task, attempt, tool, target, policy decision and outcome outside
the sandbox; redact sensitive inputs/results. Treat tool results as untrusted.
Secret-free access still permits abuse of granted operations and data: constrain
outputs and destinations as well as inputs. Credential storage alone cannot
prevent exfiltration through an authorized tool.

## Consequences

Images and tools remain customizable, but cannot relax access policy. Credentials
are protected by a separate trust boundary; this is a planned contract, not a
claim of implemented protection. MCP services and identity infrastructure become
security-critical dependencies. Existing CLIs may need adapters or may not fit.

## References

- [Anthropic: decoupling execution and credentials](https://www.anthropic.com/engineering/managed-agents)
- [MCP security best practices](https://modelcontextprotocol.io/specification/latest/basic/security_best_practices)
- [SPIRE workload attestation](https://spiffe.io/docs/latest/spire-about/spire-concepts/)
