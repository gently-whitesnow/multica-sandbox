# Upstream multica CLI as a read-only controller artifact (ADR 0014). The controller
# mounts it into attempts; agent images do not contain it. The `multica` context is
# Multica at multica.UpstreamRevision, e.g.
#   docker build -f deploy/multica-cli.Dockerfile \
#     --build-context multica=https://github.com/multica-ai/multica.git#f05a525ac8e82f5e9169ad078e44f166fad6d5c8 .
# Reference the resulting digest as `multica_cli`. It holds no credentials.
# For a server at another compatible revision, use its source as the context, pass
# --build-arg REVISION=<its commit> and set the same commit as `multica_cli_revision`.
FROM golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build
ARG REVISION=f05a525ac8e82f5e9169ad078e44f166fad6d5c8
WORKDIR /src
COPY --from=multica /server/go.mod /server/go.sum ./
RUN go mod download
COPY --from=multica /server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.commit=$REVISION" -o /multica ./cmd/multica

FROM scratch
COPY --from=build --chmod=0555 /multica /bin/multica
