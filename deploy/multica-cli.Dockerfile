# Upstream multica CLI as a read-only controller artifact (ADR 0014). The controller
# mounts it into attempts; agent images do not contain it. The `multica` context is
# Multica at multica.UpstreamRevision, e.g.
#   docker build -f deploy/multica-cli.Dockerfile \
#     --build-context multica=https://github.com/multica-ai/multica.git#b4ca5b4a23e68b26292a680dca7689a952bb1cd5 .
# Reference the resulting digest as `multica_cli`. It holds no credentials.
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG REVISION=b4ca5b4a23e68b26292a680dca7689a952bb1cd5
WORKDIR /src
COPY --from=multica /server/go.mod /server/go.sum ./
RUN go mod download
COPY --from=multica /server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.commit=$REVISION" -o /multica ./cmd/multica

FROM scratch
COPY --from=build --chmod=0555 /multica /bin/multica
