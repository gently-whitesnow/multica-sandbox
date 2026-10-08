FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY examples/identity-mcp/ ./examples/identity-mcp/
RUN CGO_ENABLED=0 go build -trimpath -o /identity-example ./examples/identity-mcp
FROM scratch AS fixture
COPY --from=build /identity-example /identity-example
ENTRYPOINT ["/identity-example"]

FROM docker:29.2.1-cli@sha256:cab69e2d0a1a2ea9a1ce1060252f439e83483ae41ec09317aecb33b08a0656a5 AS rotation
COPY --from=build /identity-example /identity-example
ENTRYPOINT ["/identity-example"]

# Git smart HTTP fixture on the Debian base of examples/agent-image (ADR 0015 tests).
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS git
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
COPY --from=build /identity-example /identity-example
ENTRYPOINT ["/identity-example"]
