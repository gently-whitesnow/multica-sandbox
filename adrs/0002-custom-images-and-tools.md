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
`multica` CLI from the pinned Multica revision; the controller delivers it as a bundle.

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
not policy inputs. The `multica` CLI belongs to the controller's Multica contract,
not to the image.

Refinement (#41, 2026-10-07). Controller artifacts are digest-pinned OCI images
mounted read-only with Docker image mounts (`--mount type=image`) under the reserved
`/opt/multica-sandbox/<name>`; each `bin` directory precedes the image `PATH`. The
first artifact is the `multica` CLI (`deploy/multica-cli.Dockerfile`); tool bundles
reuse the mechanism. Verified on Docker Engine 29.2.1 (containerd store, arm64):
with `--pull=never`, an absent digest fails at create and references resolve only
as `repository@digest`; the mount is always read-only, ignoring `readonly=false`;
binaries execute as the attempt user; `inspect` reports exactly the requested image
mount, so the policy accepts only that mount and its `PATH`. Docker sets neither
`nosuid` nor `nodev`: without `no-new-privileges` a setuid artifact binary gained
euid 0, with the attempt policy it did not. Zero capabilities and the device cgroup
cover devices. The CLI marks image mounts experimental; re-run conformance on engine
upgrades. Startup inspection runs with the mounts and requires `multica` to resolve
to the artifact built from the pinned revision.
Rejected: a named volume populated from the image (mutable, not bound to a digest
at mount time, needs its own lifecycle); streaming into `/workspace` (writable by
the agent, copied per attempt into the size-limited tmpfs, unsuitable for bundles);
`--volumes-from` (needs image `VOLUME`, yields writable copies); host bind mounts
(the controller runs in a container). Kubernetes `image` volumes (stable in 1.36,
read-only, `pullPolicy: Never`) are the counterpart for #6; their exec and `nosuid`
semantics must be verified on the selected runtime.

## Consequences

Users can bring their own images and tools without modifying the runtime. We own
the default image, manifest validation and runner compatibility checks; users own
their custom artifacts and dependencies. The OpenCode image contract and the
controller-delivered CLI are implemented (`internal/opencode/README.md`); the
environment manifest, user tool bundles and published base image remain planned.
