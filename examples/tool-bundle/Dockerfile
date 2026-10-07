# Example user-owned tool bundle (ADR 0002, issue #43): the static jq release,
# checksum-pinned per architecture. A bundle is a scratch image whose files are
# self-contained and relocatable; the controller mounts it read-only at
# /opt/multica-sandbox/tools/<name> and puts its `path` entries (default `bin`)
# on PATH. The build context may be empty. No credentials are baked in.
# Controller configuration, next to `image`:
#   "tools": [{"name": "jq", "image": "<repository>@sha256:<digest>", "check": ["bin/jq", "--version"]}]
FROM scratch AS jq-amd64
ADD --checksum=sha256:b1c22172dd303f3be49e935aa56aa48a8b7a46e0bc838b4997d3bb451495870f https://github.com/jqlang/jq/releases/download/jq-1.8.2/jq-linux-amd64 /jq

FROM scratch AS jq-arm64
ADD --checksum=sha256:8b85c817833814ddca00a144c33705546355afccf0cf39b188f3cdb48b852309 https://github.com/jqlang/jq/releases/download/jq-1.8.2/jq-linux-arm64 /jq

FROM jq-${TARGETARCH} AS jq

FROM scratch
COPY --from=jq --chmod=0555 /jq /bin/jq
