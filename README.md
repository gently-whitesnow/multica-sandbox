# multica-sandbox

**The execution boundary for Multica agents.**

Multica's [security model](https://multica.ai/docs/security-model) states:

> So Multica does not pretend to be the boundary. Put one around it.

multica-sandbox is being built to provide that boundary: disposable environments
for running AI coding agents in your infrastructure.

**Status:** architecture and repository setup only; no runnable sandbox yet.
Docker/Sysbox first, Kubernetes next. Independent runtime, no Multica fork.

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
Multica selects the runtime and model. Planned adapters translate task context,
events and sessions without coupling sandbox policy to a model vendor. Provider
credentials stay outside the sandbox. The inference transport contract remains
open; adapters must demonstrate compatibility before being advertised.

Containers share the host kernel: isolation depends on the runtime, policy and
granted credentials.

See [ADR 0001](adrs/0001-isolated-task-runtime.md) for the architecture and scope.
Image customization and tool delivery follow
[ADR 0002](adrs/0002-custom-images-and-tools.md). Access boundaries follow
[ADR 0003](adrs/0003-identity-and-mcp-access.md); component selection and acceptance
gates follow [ADR 0004](adrs/0004-reuse-and-security-gates.md).

## Contributing

Install the pinned
[Harness CLI](https://github.com/gently-whitesnow/harness-cli) and verify:

```sh
./scripts/install-harness.sh
./verify.sh
```

Harness reads Git-tracked files; stage new files before verification.
The clone-local installation includes a commit-message hook. Run
`.git/harness/bin/harness commit-message template` for the required format.

## License

[Apache-2.0](LICENSE). Independent project; not an official Multica component.
