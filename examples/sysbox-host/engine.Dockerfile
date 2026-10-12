# Minimal attempt image with a nested engine for sysbox-runc (ADR 0019): the static
# Docker 29.7.1 of docker:dind without its `VOLUME /var/lib/docker` (the image contract
# forbids volumes) and iptables. The VERIFY_SYSBOX=1 integration test builds it; agent
# images add the same files. Build context: examples/sysbox-host.
FROM docker@sha256:e8faad5a8dc5279dff929afc5449f2791736912fff9f99351d742db2fad01b4c AS dind
FROM alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates iptables ip6tables
COPY --from=dind /usr/local/bin/ /usr/local/bin/
COPY --from=dind /usr/local/libexec/docker/ /usr/local/libexec/docker/
