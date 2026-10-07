# Example OpenCode agent image with the upstream Multica CLI (ADR 0014, issue #4).
# Context: a directory containing a static linux `multica` binary built from the
# pinned Multica revision's server/cmd/multica. The CLI holds no credentials;
# the controller supplies MULTICA_SERVER_URL and an opaque relay MULTICA_TOKEN.
FROM ghcr.io/anomalyco/opencode:1.18.35@sha256:ae90ec960c871b0ea6df9abc1c2ec1e6b36464e8eca9b0e71e1ed3b553f47aa2
COPY multica /usr/local/bin/multica
