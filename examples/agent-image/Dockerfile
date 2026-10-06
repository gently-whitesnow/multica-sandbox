# Example OpenCode agent image with the upstream Multica CLI (ADR 0014, issue #4).
# Context: a directory containing a static linux `multica` binary built from the
# pinned Multica revision's server/cmd/multica. The CLI holds no credentials;
# the controller supplies MULTICA_SERVER_URL and an opaque relay MULTICA_TOKEN.
FROM ghcr.io/anomalyco/opencode:1.18.34@sha256:b34342987ca889fc2cc19cbc046eefc2418e5980a3d696e209fbb401a288f631
COPY multica /usr/local/bin/multica
