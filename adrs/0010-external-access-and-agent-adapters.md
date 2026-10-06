# ADR 0010: External access services and selectable agent adapters

Status: Accepted
Date: 2026-10-04

## Context

An execution boundary must work with operator-provided images without becoming
an inference gateway, MCP platform or IAM product. Protocol compatibility does
not make every agent CLI interchangeable.

## Decision

The runtime owns workload execution, safe task context delivery, identity
bootstrap integration, lifecycle and cleanup. Model routing, provider credentials,
tool implementation, role/resource authorization and budgets belong to external
services. Deployment examples may assemble those services without making them
runtime-owned components. Lifecycle signals or bounded identity leases are still
needed for external services to revoke ended attempts under ADR 0009.

ADR 0012 makes a narrow credential-translation relay in the controller process the
single inference path: it swaps an opaque attempt credential for the workspace's
gateway key. Provider routing, model grants and budgets remain in the gateway.

Use OpenCode as the first end-to-end example adapter because it supports custom
providers, remote MCP authorization headers and machine-readable output. Keep
agent adapters separate from model sources and sandbox backends. Codex CLI,
Claude Code and custom agents remain selectable future adapters; do not require
OpenCode inheritance or promise their compatibility before conformance tests.
A custom image must meet a supported adapter/runner contract. Its own entrypoint,
toolchain and dependency choices cannot relax backend policy.

The integration lab uses unmodified Multica, Keycloak, LiteLLM and CLIProxyAPI.
Provider account authentication stays in the proxy's credential store. A
short-lived MCP audience identifies the agent to the lab's external MCP fixture,
which checks roles, the fixed agent/resource mapping and live Multica task state.
Inference uses the ADR 0012 relay in the lab controller with a LiteLLM virtual key
for the lab workspace; LiteLLM refuses other models and caller-supplied provider
overrides. This demonstrates an OSS integration route, not a production
authorization SDK. MCP does not have to transport inference requests.

The lab's single-task envelope and OpenCode JSON interpretation are experimental.
The persistent controller now has an experimental OpenCode MCP integration with
safe context, cancellation, leases and identity checks. Optional inference uses
the workspace-key relay in ADR 0012. Native events, cumulative reported usage, terminal text and disposable session
identifiers use the existing Multica APIs; see the adapter README capability
matrix. Production adversarial egress conformance remains deployment-owned.
Do not silently advertise missing Multica features or copy credential-bearing
claim fields, arbitrary environment or untrusted endpoint configuration.

## Consequences

Operators can reuse existing access infrastructure. This project needs adapters
and lifecycle/identity integration rather than a model router or tool registry.
OpenAI-compatible HTTP does not guarantee identical tool-calling, Responses,
Messages or session behavior; test each adapter/gateway combination.

The local lab permits real subscription-backed inference after user login. It
maps one principal to one fixed task and does not implement general concurrent
attempt authorization, budgets, multi-tenant inference or production workload
attestation. Private HTTP, fixture database credentials, shared trusted config
and Docker socket authority are explicit lab limitations.

## References

- [OpenCode providers](https://opencode.ai/docs/providers/)
- [OpenCode remote MCP](https://opencode.ai/docs/mcp-servers/)
- [OpenCode CLI](https://opencode.ai/docs/cli/)
- [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
