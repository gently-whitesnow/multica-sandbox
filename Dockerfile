FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# The source commit for sandbox_build_info (ADR 0025); images.yml passes it.
ARG SANDBOX_REVISION=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/gently-whitesnow/multica-sandbox/internal/metrics.Revision=${SANDBOX_REVISION}" -o /controller ./cmd/sandbox-controller \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sandbox-helper ./cmd/sandbox-helper

# Controller-owned attempt helper (ADR 0015): `docker build --target helper .`, then
# reference its digest as `helper`. It holds no credentials.
FROM scratch AS helper
COPY --from=build --chmod=0555 /sandbox-helper /sandbox-helper
ENTRYPOINT ["/sandbox-helper"]

FROM docker:29.2.1-cli@sha256:cab69e2d0a1a2ea9a1ce1060252f439e83483ae41ec09317aecb33b08a0656a5
COPY --from=build /controller /usr/local/bin/sandbox-controller
# Deploy contract 1 (ADR 0031): SIGUSR1 drains claims, SIGUSR2 resumes, the metrics
# listener answers /readyz. The host deploy controller reads this label.
LABEL io.multica-sandbox.deploy-contract="1"
ENTRYPOINT ["/usr/local/bin/sandbox-controller"]
CMD []
