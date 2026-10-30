# Example user-owned agent image on an unrelated base (issue #39): Debian with the
# released glibc OpenCode build, checksum-pinned per architecture. The context may be
# empty. Everything the agent needs is installed here; attempts have no network. The
# controller mounts the `multica` CLI (ADR 0014). No credentials are baked in.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS base

FROM base AS opencode-amd64
ADD --checksum=sha256:3a38b8c284450a5ca81a2c1ebbf29ebe05e618518f095da0066652167eb0eb33 https://github.com/anomalyco/opencode/releases/download/v2.2.2/opencode-linux-x64.tar.gz /opencode.tar.gz

FROM base AS opencode-arm64
ADD --checksum=sha256:dd866cebb72d11c91a7a7c4c3fd961ee06e03a9d0a5ff8eaa2a8ebebf25de7ba https://github.com/anomalyco/opencode/releases/download/v2.2.2/opencode-linux-arm64.tar.gz /opencode.tar.gz

FROM opencode-${TARGETARCH} AS opencode
RUN tar -xzf /opencode.tar.gz -C /usr/local/bin opencode

FROM base
# OpenCode searches with ripgrep and would otherwise try to download it at run time;
# git and gh work on repositories through the controller relays (ADR 0015).
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates gh git ripgrep && rm -rf /var/lib/apt/lists/*
COPY --from=opencode /usr/local/bin/opencode /usr/local/bin/opencode
