# ADR 0008: Shared controller, workspace-scoped authority

Status: Accepted
Date: 2026-10-04

## Context

A deployment may serve 100 workspaces without 100 machines or controller processes.
One controller discovers accessible memberships and keeps attempt authority
scoped to each workspace.

Research inspected upstream main at b4ca5b4a23e68b26292a680dca7689a952bb1cd5.
The following source findings informed the implementation:

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

Use one controller process per worker connected to one Multica server,
one stable daemon identity, and separate
runtime registrations for each authorized workspace and supported adapter.
Keep runtime visibility independent from controller admission and inference policy.
Public runtimes are optional sharing inside each workspace, never tenant isolation.

Start with a dedicated service user using the existing PAT path, membership-based
workspace discovery. Discovery is always enabled, without workspace-mode selectors.
One or many accessible workspaces use the same scheduler; concurrency controls
capacity independently of membership count. Leave public visibility
under the runtime owner's manual control. Serving the whole server requires provisioning membership
for existing and newly created workspaces. Membership is required for the service user;
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
worker-wide sweep during individual workspace registration. Persist controller
ownership by server, daemon and Docker Engine as specified in ADR 0007.

Inference credentials are independent of Multica control-plane credentials.
A trusted service maps validated run identity to workspace policy and a secret
reference. It selects the provider credential, model permissions and budget;
never trust a caller's workspace header, endpoint URL or credential selector.
No provider key or reusable workspace proxy key enters the sandbox. Key rotation
must not require rebuilding images, and cancellation must revoke inference access.

Keep external actions MCP-mediated as in ADR 0003. Evaluate maintained gateways
and identity components, but do not assume an OpenAI-compatible gateway supplies
MCP inference, run attestation or immediate revocation. ADRs 0009/0012 select
native provider HTTP through an external gateway with independent inference
identity and active-attempt checks. Do not build a general LLM proxy or identity
issuer inside the controller.

### Implementation bounds

The first fleet uses HTTP batch claims (one task per fair workspace round), a
configurable global limit of 1–32, and one active attempt per workspace. Discovery
runs every 10 seconds, idle heartbeats every 15 seconds, and claims every second.
Membership loss cancels its local attempt. HTTP 403/404 scope failures remove that
runtime; transport, protocol and cleanup uncertainty stop the entire controller.
Revocation is eventually observed, not instantaneous. Active tasks also check
server status every second; upstream membership caches can delay rejection.

Startup removes all containers labeled with the persisted daemon identity before
any claims, including executions whose workspace is no longer accessible. The
runtime registry retains removed workspace mappings. Server task recovery waits
for restored access or upstream recovery mechanisms when membership is absent.
Rejoining waits for local teardown before recovery. Registrations never auto-publish.

## Consequences

The standalone probe may select one workspace for a disposable diagnostic; it is
not a controller service mode.

Multi-workspace discovery, isolation and bounded capacity precede the first real
agent integration. Acceptance covers claim scope, revoked membership, manual
visibility, restart cleanup and workspace fairness. Integration measurements
belong to issue #12; a 100-workspace fixture is not a production capacity guarantee.
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
