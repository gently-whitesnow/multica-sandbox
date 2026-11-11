# Native live steering fixture

The opt-in sandbox transport uses one authenticated OpenCode 2.2.5 HTTP service
per attempt. The default ACP adapter remains available. See
[ADR 0036](../../adrs/0036-live-steering-contract-gates.md) for delivery
semantics, source pins and the compared alternatives.

Multica v0.7.2 neither admits OpenCode supplements nor lets a daemon read its own
delivery receipts. Three patches against the pinned revision close exactly that:

| Patch | Change |
| --- | --- |
| `native-admission.patch` | Three provider checks in source SQL and their generated Go query strings. |
| `native-receipts.patch` | `GET /api/daemon/tasks/{id}/supplements[?comment_id=]`: one read-only query, its sqlc counterpart, one handler and one route. Apply after admission with `git apply --unidiff-zero`. |
| `native-routing.patch` | Preserve the member’s current-run routing choice in `comment.suppressed_agent_ids` at create/bind, so a deleted receipt cannot become an automatic follow-up. Apply last. |

None migrates a database, implements a native Multica daemon adapter or
modifies a deployment. `scripts/test-upstream.sh` applies them only with
`VERIFY_STEERING_NATIVE_PATCH=1`, after verifying the exact pinned revision and a
clean disposable clone. The source checkout remains unchanged. The ordinary
verification path always uses an unmodified server, where the controller offers
no steering even when `live_steering` is set.

## Verification

Docker is required; nothing uses an inference account. Native negotiation,
receipt inspection, a two-transaction delete/claim lock regression, a 5000-comment
history, vanished receipts, lost claim and
acknowledgement responses, late acknowledgement and cancellation, with the real
client against the patched server:

```sh
./scripts/test-live-steering-native.sh
```

The real controller and OpenCode 2.2.5: same-run consumption and order, clean
resume, cancellation, controller SIGKILL, HTTP→ACP isolation, MCP rotation, and
behind the fault proxy lost claim/acknowledgement responses, a resent admission
under its stable ID, a vanished receipt and SIGKILL between proven consumption
and acknowledgement:

```sh
VERIFY_SERVICE=1 VERIFY_OPENCODE=1 \
UPSTREAM_TEST_FILTER='^TestUpstreamLifecycle$/^live-steering-(service|faults|mcp)$' \
./scripts/test-live-steering-native.sh
```

Request deadlines, ephemeral body cleanup, idle SSE, stdin EOF, native
withdrawal and a lost admission response against the real owner:

```sh
VERIFY_HTTP_RUNTIME=1 go test -race ./internal/opencode -run '^TestHTTPRuntime' -count=1 -timeout=180s
```

The wrapper enables the Go race detector. Fixtures create only generic local
tasks and disposable containers; images are built or digest-pinned by the
upstream script. `go run ./examples/acp-spike -http-steering-probe -version 2.2.5`
remains a comparison probe: a positive probe alone admits no image version.
Ordinary `./verify.sh` runs none of these real paths.

## Activation

The controller side and all three patches are tested against disposable servers.
This PR deploys neither: `live_steering` is off by default. Enabling it is a server change followed by a
controller configuration change. Their code/configuration can be rolled back
separately; persisted routing choices remain.

1. Review and merge all three patches into the Multica build the deployment runs
   (v0.7.2 at the pinned revision, or re-derive them for a newer one and rerun the
   commands above against it). Regenerate `supplement.sql.go` with the server's
   `make sqlc` and compare it with the patch. Deploy the server. Routing writes
   start immediately for member steering across all admitted providers, including
   codex/claude/grok/cursor/kiro, regardless of the sandbox flag. OpenCode steering
   still requires a daemon to offer the capability.
2. Confirm the agent image passes the steering image gate (`wget`, `sync`,
   `timeout`, OpenCode 2.2.5) and that no custom `command` replaces native
   `opencode serve` readiness.
3. In an infrastructure window with an idle queue, set `"live_steering": true`
   in the controller's `opencode` configuration and restart it. Sessions last
   owned by ACP start fresh with the lost-session notice.
4. Verify on a disposable issue: add guidance to a running task and check the
   receipt becomes `delivered` in the same task and session; watch the
   controller log for `supplement consumption quarantined` and
   `supplements/quarantine` in its state volume, which should stay empty.

Roll back in reverse: unset `live_steering` and restart (HTTP-owned sessions are
retired before ACP boots), then revert the server patches. Rollback leaves existing
`comment.suppressed_agent_ids` values in place: they suppress replay only for the
agent whose turn the member explicitly chose. No data cleanup is required.
Editing the comment's content after its receipt is gone recomputes that field
from the edit request. A vanished receipt before consumption can leave the member
input with no delivery, follow-up or visible receipt; the server cannot reconstruct
consumption. The flag without server patches selects the HTTP owner without steering.
