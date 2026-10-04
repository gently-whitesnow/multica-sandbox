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
- Keep controller credentials and host runtime sockets outside agent containers.

Multica owns tasks and retries. The controller manages execution environments;
the runner executes one task with Codex or Claude Code. Containers share the
host kernel: isolation depends on the runtime, policy and granted credentials.

See [ADR 0001](adrs/0001-isolated-task-runtime.md) for the architecture and scope.

## Contributing

Use English and keep documentation concise. Install the pinned
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
