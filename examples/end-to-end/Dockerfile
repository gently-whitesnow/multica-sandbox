FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY examples/end-to-end/ ./examples/end-to-end/
RUN CGO_ENABLED=0 go build -trimpath -o /e2e ./examples/end-to-end
FROM docker:29.2.1-cli@sha256:cab69e2d0a1a2ea9a1ce1060252f439e83483ae41ec09317aecb33b08a0656a5 AS control
COPY --from=build /e2e /e2e
ENTRYPOINT ["/e2e"]
FROM ghcr.io/anomalyco/opencode:1.18.34@sha256:b34342987ca889fc2cc19cbc046eefc2418e5980a3d696e209fbb401a288f631 AS agent
COPY --from=build /e2e /e2e
ENV HOME=/home/agent XDG_CONFIG_HOME=/home/agent/.config XDG_DATA_HOME=/home/agent/.local/share XDG_CACHE_HOME=/home/agent/.cache
ENV OPENCODE_DISABLE_AUTOUPDATE=true OPENCODE_DISABLE_MODELS_FETCH=true OPENCODE_DISABLE_DEFAULT_PLUGINS=true OPENCODE_DISABLE_LSP_DOWNLOAD=true
ENTRYPOINT ["/e2e", "agent"]
