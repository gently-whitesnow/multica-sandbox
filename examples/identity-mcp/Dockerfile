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
