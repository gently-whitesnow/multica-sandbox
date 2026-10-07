# Example user-owned agent image on an unrelated base (issue #39): Debian with the
# released glibc OpenCode build, checksum-pinned per architecture. The context may be
# empty. Everything the agent needs is installed here; attempts have no network. The
# controller mounts the `multica` CLI (ADR 0014). No credentials are baked in.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS base

FROM base AS opencode-amd64
ADD --checksum=sha256:c8f888b451f5494a18f858fffb0e0b68f4e4baa9c241761c5f206884f0fa640d https://github.com/anomalyco/opencode/releases/download/v1.18.35/opencode-linux-x64.tar.gz /opencode.tar.gz

FROM base AS opencode-arm64
ADD --checksum=sha256:f7f2ba59ee8aa94d388f9696575a32d20e71c2ee48def9f80fc693a60fec6c72 https://github.com/anomalyco/opencode/releases/download/v1.18.35/opencode-linux-arm64.tar.gz /opencode.tar.gz

FROM opencode-${TARGETARCH} AS opencode
RUN tar -xzf /opencode.tar.gz -C /usr/local/bin opencode

FROM base
# OpenCode searches with ripgrep and would otherwise try to download it at run time.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep && rm -rf /var/lib/apt/lists/*
COPY --from=opencode /usr/local/bin/opencode /usr/local/bin/opencode
