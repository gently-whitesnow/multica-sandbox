# multica-sandbox

Multica's [security model](https://multica.ai/docs/security-model) states:

> So Multica does not pretend to be the boundary. Put one around it.

multica-sandbox provides that boundary: disposable environments for running AI
coding agents in your infrastructure.

**Status:** experimental controller with offline probes and opt-in OpenCode MCP
identity rotation; a separate subscription-backed lab exists. Docker runc/Sysbox
tested; Kubernetes planned. Independent runtime, no Multica fork.

## Design

- Connect directly to Multica's Daemon API; no custom runtime profiles.
- Run preparation and agent execution in isolated per-run environments with restricted
  mounts, network access, credentials, resources and execution time.
- Reuse immutable toolchains and prepared caches; give each run its own writable state
  and keep resumable sessions separate from disposable caches.
- Give sandboxes only short-lived run identity; keep service credentials outside.
- Route external tool access through authorized MCP services and enforce egress outside
  the sandbox. Identity alone does not authorize an action.
- Use the optional base image or a compatible OCI image; declare tools in config
  (baked into the image or mounted read-only as pinned bundles) without changing sandbox code.

Multica owns tasks and retries; the controller manages environments and runs tasks
through an agent adapter. Backends, adapters and inference connections stay separate.
Multica selects the runtime and model; trusted workspace bindings select the gateway
and key, which stay outside the sandbox. The picker shows the advisory catalog; the
gateway enforces key grants. See the OpenCode [conformance report](internal/opencode/README.md#conformance).

Containers share the host kernel; isolation depends on policy and granted credentials.
Docs: architecture ([ADR 0001](adrs/0001-isolated-task-runtime.md)), images/tools
([ADR 0002](adrs/0002-custom-images-and-tools.md)), access boundaries
([ADR 0003](adrs/0003-identity-and-mcp-access.md)), reuse/security gates
([ADR 0004](adrs/0004-reuse-and-security-gates.md)).

## Controller service

Use isolated test workspaces. The default runtime executes configured test commands;
the opt-in [OpenCode path](internal/opencode/README.md) projects agent instructions
and selected remote MCP connections. Requires Linux Docker with cgroup v2, builtin
seccomp and resource controllers (Docker Desktop is tested for development). Because
the controller creates sibling workloads via the host Docker socket (host-administrative
authority), prefer a dedicated worker host.

```sh
mkdir -p var
cp deploy/controller.example.json var/controller.json
```

Configure the HTTPS server URL (`allow_http` is for isolated fixtures only; base paths
like `https://example.com/multica` are preserved), a unique stable daemon UUID, a
digest-pinned preloaded image, command and timeout. Put the controller token in
`var/multica-token` with restrictive permissions. Keep both files outside Git; the
token mounts only into the trusted controller.

```sh
docker pull alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
docker compose up -d --build
docker compose logs -f controller
```

One controller serves all accessible workspaces (discovery is always enabled).
Concurrency is bounded only by each Multica agent's `max_concurrent_tasks`, enforced
by Multica at claim; the controller claims one task per workspace per round until
Multica returns none, pool admission refuses or a deploy drains it, and rejects configs
setting `concurrency` or `workspace_concurrency`. Each attempt uses one Docker network,
so size `default-address-pools` for peak concurrency (e.g. `10.200.0.0/16`, size 24).

Offline attempts run in fresh non-root, networkless containers with bounded resources
and temporary storage; OpenCode under Sysbox starts a namespaced root engine supervisor
and runs agent commands as uid 65532. Custom images need no inheritance; image-declared
volumes are rejected. No service tokens or host mounts enter execution. The opt-in
OpenCode path adds per-attempt internal networks, native MCP OAuth rotation, external
attempt leases, optional [inference](internal/inference/README.md) via a controller
relay swapping opaque attempt credentials for workspace keys, and native Multica
reporting. Repository checkout, mediated push/PR and retained issue/chat sessions are
opt-in; artifact publication is unsupported ([OpenCode README](internal/opencode/README.md)).

For host-sized execution, derive the shared attempt pool from host capacity reserving
memory for trusted services. `resources.cpu_mode: shared` removes per-attempt CPU
quotas; `pool.auto_admission: true` with read-only `disk_path` adds cold-start pacing,
observed headroom and disk/inode watermarks to PSI admission. Below the pause watermark,
idle sessions are pruned then reclaimed with reference mirrors in LRU order
(`sessions.artifact_age`, optional `sessions.budget`; [ADR 0028](adrs/0028-auto-host-resources.md),
ADR 0034, ADR 0035).

Docker restarts a crashed controller; on startup it removes its old executions before
asking Multica to recover tasks. Persistent state binds the shared lock to daemon,
server and Docker Engine. Preserve the named volume across updates, use one controller
per identity, never scale the Compose service and never remove the state volume while
active.

Use a dedicated user PAT (workspace admin is allowed for controller operations). Tasks
use Multica's native task-token permissions through the relay (ADR 0026) without
receiving the controller PAT; at v0.7.2 a task token inherits the runtime owner's
workspace role (the sandbox does not downgrade it). Add that user to each workspace:
the controller discovers membership every 10 s and registers a runtime (public visibility
is set manually in Multica and never crosses workspaces). An `mdt_` token discovers
only its bound workspace.

Logs are JSON lines; each attempt ends with `attempt finished` (status, reason, timings,
continuity). Relay refusals, transport failures, upstream 429/5xx and failed attempts
log at `WARN`; ordinary upstream 4xx and sampled summaries log at `DEBUG`. Metrics
count every relay outcome regardless of log level. `"debug": true` adds identity,
inference and failed Multica diagnostics (never secrets). `"metrics_listen": "0.0.0.0:9464"`
serves `/metrics` and `/readyz` ([ADR 0025](adrs/0025-quality-metrics-and-structured-logs.md)).

`docker compose stop` removes active execution and stops the controller; restart with
`docker compose up -d` to reconcile interrupted tasks. No independent deadline or
live-hang recovery exists; Docker unavailability, invalid config or repeated crashes
can delay cleanup. See [ADR 0006](adrs/0006-offline-container-execution.md) and
[ADR 0007](adrs/0007-containerized-controller-service.md).

## One-attempt probe

Run `go run ./cmd/sandbox-probe` with `-server`, `-workspace`, `-daemon`, an absolute
`-lock` path and `MULTICA_PROBE_TOKEN`. Without `-image` it runs a timer; add
`-image IMAGE@sha256:DIGEST -duration 30s -- /bin/sh -c 'echo test > /workspace/result'`
for an offline command. `-recover-only` reconciles without claiming; `-fail` simulates
failure. Reuse the same identity, backend and lock ([ADR 0005](adrs/0005-upstream-lifecycle-probe.md)).

## Upstream verification

`./scripts/test-upstream.sh` builds unmodified Multica v0.7.2 (ADR 0026) in disposable
PostgreSQL/server containers and seeds generic tasks in the database (UI creation is
not covered). Requires Go 1.26.9+, Docker, Git, curl and OpenSSL. Set `VERIFY_SERVICE=1`
to build and test the Compose controller (loopback HTTP is fixture-only; deployments
require HTTPS). See [identity/MCP](examples/identity-mcp/README.md) and
[end-to-end lab](examples/end-to-end/README.md); `VERIFY_INFERENCE=1` tests OpenCode
through the relay to LiteLLM keys.

## Contributing

Install the pinned [Harness CLI](https://github.com/gently-whitesnow/harness-cli) and verify:

```sh
./scripts/install-harness.sh
./verify.sh
```

`verify.sh` runs Harness, formatting, vet, race tests and build. Optional flags:
`VERIFY_UPSTREAM=1` (disposable upstream suite), `VERIFY_SERVICE=1` (Compose controller),
`VERIFY_CONTAINERS=1` (hostile-container conformance; preload the Alpine image above),
`VERIFY_IDENTITY=1` (identity/MCP fixture), `VERIFY_OPENCODE=1` (OpenCode/JWT rotation
and failure tests), `VERIFY_INFERENCE=1` (inference relay), `VERIFY_DYNAMIC_MCP=1`
(dynamic public MCP), `VERIFY_COMMIT_RANGE=master..HEAD` (commit checks).

All Harness checks are required; stage new files before verification. CI is paused
(workflows run on manual dispatch): `images.yml` publishes images for one platform;
`deploy.yml` rolls the controller ([AGENTS.md](AGENTS.md#controller-release)). Run
`.git/harness/bin/harness commit-message template` for the required commit format.

License: [Apache-2.0](LICENSE). Independent project; not an official Multica component.
