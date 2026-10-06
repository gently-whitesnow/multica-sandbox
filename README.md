# multica-sandbox

**The execution boundary for Multica agents.**

Multica's [security model](https://multica.ai/docs/security-model) states:

> So Multica does not pretend to be the boundary. Put one around it.

multica-sandbox is being built to provide that boundary: disposable environments
for running AI coding agents in your infrastructure.

**Status:** experimental controller with offline probes and opt-in OpenCode MCP
identity rotation; a separate subscription-backed lab exists.
Docker/runc tested; Sysbox and Kubernetes remain planned. Independent runtime, no Multica fork.

## Design

- Connect directly to Multica's Daemon API; no custom runtime profiles.
- Run preparation and agent execution in isolated environments for each run.
- Restrict mounts, network access, credentials, resources and execution time.
- Reuse immutable toolchains and prepared workspace caches; give each run its
  own writable state. Keep resumable sessions separate from disposable caches.
- Give sandboxes only short-lived run identity; keep service credentials outside.
- Route external tool access through authorized MCP services; enforce egress
  outside the sandbox. Identity alone does not authorize an action.
- Use the project's optional base image or your own compatible OCI image.
- Declare your own tool set in configuration: tools baked into the image or
  pinned bundles mounted read-only, without changing sandbox code.

Multica owns tasks and retries. The controller manages execution environments;
the runner executes one task through a supported agent adapter.

Execution backends, agent adapters and inference connections are separate.
Multica selects the runtime and model; trusted bindings select the inference gateway.
Provider credentials stay outside the sandbox. The picker shows each workspace runtime's
advisory catalog; the gateway enforces per-agent access. Full event/session adapters remain open.

Containers share the host kernel; isolation depends on policy and granted credentials.

Architecture: [ADR 0001](adrs/0001-isolated-task-runtime.md). Images/tools:
[ADR 0002](adrs/0002-custom-images-and-tools.md). Access boundaries: [ADR 0003](adrs/0003-identity-and-mcp-access.md); component selection and acceptance
gates: [ADR 0004](adrs/0004-reuse-and-security-gates.md).

## Controller service

Use isolated test workspaces. The default runtime executes configured test
commands; the opt-in [OpenCode path](internal/opencode/README.md) projects agent
instructions and selected remote MCP connections. Requires Linux Docker with cgroup v2,
builtin seccomp and resource controllers; Docker Desktop is tested for development.
The controller creates sibling workloads through the host socket.
Socket access is host-administrative authority; prefer a dedicated worker host.

```sh
mkdir -p var
cp deploy/controller.example.json var/controller.json
```

Edit the server origin (HTTPS; `allow_http` is only for isolated fixtures) and a unique, stable daemon UUID.
Set a preloaded image pinned by digest, an absolute executable/arguments and timeout.
Place the Multica controller token in `var/multica-token` with restrictive permissions.
Both files stay outside Git. The token is mounted only into the trusted controller.

```sh
docker pull alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
docker compose up -d --build
docker compose logs -f controller
```

One controller serves all accessible workspaces with a shared `concurrency` limit (default 1, maximum 32) and at most one active attempt per workspace. Every attempt gets a fresh non-root,
networkless container with bounded resources and temporary storage. Custom images
need no inheritance; image-declared volumes are rejected. No service tokens or
host mounts enter execution. Workload output/files are discarded; completion is a
fixed test result. The opt-in OpenCode path uses per-attempt internal networks, native MCP OAuth
rotation and external attempt leases. Optional [inference identity](internal/inference/README.md)
uses a separate JWT and provider auth hook. Native messages, reported usage, terminal
text/session IDs and safe repository references are mapped to existing Multica APIs.
See the [capability matrix and walkthrough](internal/opencode/README.md); direct repository checkout, retained sessions and artifact publication remain unsupported.

Docker restarts a crashed controller. On startup it removes its old executions
before asking Multica to recover tasks. Persistent state binds the shared lock to
the daemon, server and Docker Engine. Preserve the named volume across
updates, use one controller per identity, and do not scale this Compose service.
Never remove the state volume while the controller is active.

Use a dedicated user PAT. Add that user to each workspace; the controller discovers
membership every 10 seconds and registers a runtime. The runtime owner manually
sets public visibility in Multica. Public does not share across workspaces.
An mdt_ token discovers only its bound workspace; registration never sweeps active containers.

Workspace discovery is always enabled. A single accessible workspace uses the same
execution path as many workspaces; `concurrency: 1` limits global execution to one
attempt at a time. Membership determines which workspaces are served.

`docker compose stop` gracefully removes active execution and intentionally leaves
the controller stopped. Restart with `docker compose up -d`; interrupted tasks are
reconciled then. No independent deadline or automatic live-hang recovery exists.
Docker unavailability, invalid config or repeated crashes can delay cleanup.
See [ADR 0007](adrs/0007-containerized-controller-service.md) for deployment limits
and [ADR 0006](adrs/0006-offline-container-execution.md) for execution policy.

## One-attempt probe

For explicit experiments, use `go run ./cmd/sandbox-probe` with `-server`,
`-workspace`, `-daemon`, an absolute `-lock` path and `MULTICA_PROBE_TOKEN`.
Without `-image` it runs a timer. Add `-image IMAGE@sha256:DIGEST -duration 30s --
/bin/sh -c 'echo test > /workspace/result'` for an offline command.
`-recover-only` reaps/reconciles without claiming; `-fail` simulates timer failure.
Reuse the same identity, backend and lock. See [ADR 0005](adrs/0005-upstream-lifecycle-probe.md).

## Upstream verification

`./scripts/test-upstream.sh` builds the pinned, unmodified Multica revision and
runs disposable PostgreSQL/server containers. Tests seed generic tasks directly
in the database; UI task creation is not covered. Requires Go 1.26.4+, Docker,
Git, curl and OpenSSL; upstream may download its required Go toolchain.
Set `VERIFY_SERVICE=1` to build and test the actual controller image through Compose.
The service fixture shares only the disposable server's network namespace to use
loopback HTTP; deployment configuration requires HTTPS for non-loopback origins.

[Identity/MCP](examples/identity-mcp/README.md) and [end-to-end lab](examples/end-to-end/README.md):
Compose fixtures and trust-boundary diagrams. The identity fixture also verifies
the controller OpenCode path with deterministic inference. `VERIFY_INFERENCE=1`
adds Keycloak/LiteLLM rotation; combine all three flags for controller inference.

## Contributing

Install the pinned [Harness CLI](https://github.com/gently-whitesnow/harness-cli) and verify:

```sh
./scripts/install-harness.sh
./verify.sh
```

`verify.sh` runs Harness, formatting, vet, race tests and build. Set
`VERIFY_UPSTREAM=1` to include the disposable upstream suite, or `VERIFY_SERVICE=1`
to also build and test the Compose controller. Set
`VERIFY_CONTAINERS=1` for hostile-container conformance (preload the image above).
Set `VERIFY_IDENTITY=1` for the disposable identity/MCP fixture.
Set `VERIFY_OPENCODE=1` for native OpenCode/JWT rotation and failure tests; combine
with `VERIFY_SERVICE=1` for the real Multica → Compose controller task.
Set `VERIFY_COMMIT_RANGE=master..HEAD` to validate published commit messages.
All available Harness checks for Go, YAML and repository documentation are required.
Harness does not execute tests or toolchains; `verify.sh` runs those locally.
CI is paused during development. The workflow supports manual dispatch only;
enable it explicitly when ready; stage new files before verification.
Run `.git/harness/bin/harness commit-message template` for the required format.

## License

[Apache-2.0](LICENSE). Independent project; not an official Multica component.
