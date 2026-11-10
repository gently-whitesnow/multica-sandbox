# Native live steering fixture

The opt-in sandbox transport uses one authenticated OpenCode 2.2.5 HTTP service
per attempt. The default ACP adapter remains available. See
[ADR 0036](../../adrs/0036-live-steering-contract-gates.md) for bounded delivery
semantics, source pins, the maintained alternatives and restart limitations.

Multica v0.7.2 does not admit OpenCode supplements. `native-admission.patch`
contains the minimal server admission change: three provider checks in source SQL
and their generated Go query strings. It does not implement a native Multica daemon
adapter, migrate a database or modify a deployment.

`scripts/test-upstream.sh` applies this patch only with
`VERIFY_STEERING_NATIVE_PATCH=1`, after verifying the exact pinned revision and a
clean disposable clone. The source checkout remains unchanged. The ordinary
verification path always uses an unmodified server.

Run native negotiation, author/ownership/receipt inspection and durable client
checks (Docker required):

```sh
./scripts/test-live-steering-native.sh
```

Run the real controller, same-run input consumption, clean resume, cancellation,
controller SIGKILL/recovery, restart isolation and selected MCP rotation:

```sh
VERIFY_SERVICE=1 VERIFY_OPENCODE=1 \
UPSTREAM_TEST_FILTER='^TestUpstreamLifecycle$/^live-steering-(service|mcp)$' \
./scripts/test-live-steering-native.sh
```

The wrapper enables the Go race detector for integration tests. Fixtures create
only generic local tasks and disposable containers. All required images are
built or digest-pinned by the upstream script. Normal upstream verification still
checks unsupported OpenCode negotiation and native follow-up fallback.

Additional comparison probes:

```sh
go run ./examples/acp-spike -http-steering-probe -version 2.2.5
go run ./examples/acp-spike -plugin-steering-probe -version 2.2.5
```

The plugin probe uses the supported server SDK inside the original ACP owner.
The selected HTTP transport additionally exposes inbox inspection/withdrawal for
completion races. A positive transport probe alone does not admit another image
version. No fixture patch is applied to an installed or production server.
