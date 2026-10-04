# ADR 0008: Shared controller, workspace-scoped authority

Status: Proposed
Date: 2026-10-04

## Context

A deployment may serve 100 workspaces without 100 machines or controller processes.
The current controller deliberately binds one state volume to one workspace.
PR #11 is a single-workspace baseline, not a multi-tenant implementation.

Research inspected upstream main at b4ca5b4a23e68b26292a680dca7689a952bb1cd5.
These are source findings, not a new multi-workspace integration or load test:

- Runtime rows belong to one workspace. Public visibility shares them with that
  workspace's members; it does not make a runtime server-global.
- The native daemon discovers memberships and registers runtimes per workspace.
- PAT/JWT authentication follows user membership; an mdt_ daemon token is bound
  to one workspace and daemon. A dedicated user PAT can cover many memberships.
- Batch claims accept runtime IDs and free slots, authorize each runtime and cap
  returned tasks at 32 per request. That is not a workspace-count limit.
- Agent custom environment can contain inference keys and reaches agent execution.
  This mechanism is incompatible with ADR 0003's credential boundary.

## Decision

Propose one controller process per worker, one stable daemon identity, and separate
runtime registrations for each authorized workspace and supported adapter.
Keep runtime visibility independent from controller admission and inference policy.
Public runtimes are optional sharing inside each workspace, never tenant isolation.

Start with a dedicated service user using the existing PAT path, membership-based
workspace discovery and an explicit operator allowlist. Do not silently opt into
every workspace on the server. Membership is required even for the service user;
this is not a new upstream service-account credential type. A shared PAT broadens
controller compromise impact. Evaluate workspace-scoped token sets when separate
revocation or smaller authority is required, without adding controller processes.

Use upstream batch claims and heartbeat mechanisms with bounded global capacity.
Reserve capacity before claiming, and verify per-workspace fairness and limits;
no second durable task queue. Target 100 registered workspaces, not 100 simultaneous
sandboxes. Measure idle API load, claim latency and revocation behavior before
claiming that scale. One busy workspace must not starve the others.

Bind each attempt to server, workspace, runtime, task and attempt identity from
trusted control-plane data. Partition writable state, authorization, cache and
telemetry accordingly. Discovering a workspace does not authorize future actions.
Removal must stop new claims and revoke/drain its work without disturbing others.
Cleanup must retain ownership records for removed workspaces and must not run a
worker-wide sweep during individual workspace registration. Migrate ADR 0007's
single-workspace identity binding explicitly; never silently reuse its state.

Inference credentials are independent of Multica control-plane credentials.
A trusted service maps validated run identity to workspace policy and a secret
reference. It selects the provider credential, model permissions and budget;
never trust a caller's workspace header, endpoint URL or credential selector.
No provider key or reusable workspace proxy key enters the sandbox. Key rotation
must not require rebuilding images, and cancellation must revoke inference access.

Keep external actions MCP-mediated as in ADR 0003. Evaluate maintained gateways
and identity components, but do not assume an OpenAI-compatible gateway supplies
MCP inference, run attestation or immediate revocation. Inference transport remains
open: prove a compatible MCP path or separately review a change to ADR 0003 before
supporting agents that require native provider HTTP. Do not build a general LLM
proxy or identity issuer inside the controller.

## Consequences

Implement multi-workspace discovery, isolation and bounded capacity before the
first real-agent integration. Test cross-workspace denial, revoked membership,
public runtime scope, restart cleanup and noisy-neighbor behavior against upstream.
Then prove two workspaces use distinct inference credentials without exposing
those credentials to either sandbox. Shared worker trust is explicit; stronger
host/tenant isolation remains a deployment and backend concern.

## References

- [Runtime scope and visibility](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/pkg/db/queries/runtime.sql)
- [Membership discovery](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/daemon_workspace.go)
- [Authorization, batch claims and task delivery](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/handler/daemon.go)
- [Native daemon and custom environment](https://github.com/multica-ai/multica/blob/b4ca5b4a23e68b26292a680dca7689a952bb1cd5/server/internal/daemon/daemon.go)
- [SPIRE identity attestation](https://spiffe.io/docs/latest/spire-about/spire-concepts/)
- [LiteLLM credential boundary](https://docs.litellm.ai/docs/proxy/client_setup/overview)
