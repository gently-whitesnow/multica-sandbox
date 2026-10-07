# ADR 0002: Custom base images and tool sets

Status: Accepted
Date: 2026-10-04

## Context

Teams need different SDKs, agent tools and approved images. A mandatory project
image or hardcoded tool catalogue would make adoption depend on maintaining forks.

## Decision

Provide an optional maintained base image. Users may extend it with their own
Dockerfile or supply an independently built compatible OCI image, including from
a private registry. Customization is native configuration, not a code extension.

Define a versioned environment manifest with an image reference, agent adapter
selection and tool declarations. Each tool is either present in the image or
delivered as an immutable bundle with a digest, mount destination and PATH entries.
Allow user-owned bundles; do not require registration in a central tool catalogue.
Installing a new agent tool still requires a compatible agent adapter.

Document and validate the image/runner contract: OS, architecture, required ABI,
execution identity, writable paths and launch behavior. Inject the runner as a
separate pinned artifact. Do not require inheritance from the project image or
promise support for arbitrary images. Report incompatibilities before execution.

Mount tool bundles read-only; keep writable tool state private to the run.
Installation scripts run during image build or isolated preparation, never on
the controller host. Environment manifests cannot override host-enforced sandbox
policy or expose controller credentials and sockets.

Resolve image and bundle references to digests before use. Include those digests,
the runner version and effective environment manifest in preparation cache keys.
Keep registry credentials and runtime secrets outside images, manifests and caches.
Docker and Kubernetes backends implement the same environment contract.
With the Multica relay (ADR 0014), the OpenCode adapter also requires the upstream
`multica` CLI from the pinned Multica revision on `PATH`; `examples/agent-image`
adds it to the pinned OpenCode image without credentials.

Refinement (#4, 2026-10-07). Upstream Multica has no image concept: its daemon
runs on the user's host and executes agent CLIs from `PATH` above a minimum version,
branching on major versions (`pkg/agent/version.go`, `opencode_v2.go` at `b4ca5b4`).
We keep that ownership: the image contains the agent and its runtime. Injecting
agents into foreign images is rejected because every future agent would bring its
own libc and runtime constraints. Adapters list conformance-verified agent versions,
starting from the latest stable release, and refuse others; there is no override.
The controller never builds Dockerfiles; images come from the user's build system by
digest. It inspects the image once at startup in a hardened offline container and
reports every incompatibility; the digest makes per-attempt checks redundant. Image
`ENV`, files and metadata that would override adapter projection are incompatibilities,
not policy inputs. The `multica` CLI belongs to the controller's Multica contract
and moves from the image to controller delivery in a later slice.

## Consequences

Users can bring their own images and tools without modifying the runtime. We own
the default image, manifest validation and runner compatibility checks; users own
their custom artifacts and dependencies. The OpenCode image contract is implemented
(`internal/opencode/README.md`); the environment manifest, tool bundles and
published base image remain planned.
