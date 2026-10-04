FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY examples/identity-mcp/ ./examples/identity-mcp/
RUN CGO_ENABLED=0 go build -trimpath -o /identity-example ./examples/identity-mcp
FROM scratch
COPY --from=build /identity-example /identity-example
ENTRYPOINT ["/identity-example"]
