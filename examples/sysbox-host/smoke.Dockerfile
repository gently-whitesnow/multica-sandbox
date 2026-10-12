# Minimal attempt image for the Sysbox smoke: the static Docker 29.7.1 from docker:dind
# without its `VOLUME /var/lib/docker` (the image contract forbids volumes) plus the dind
# supervisor prototype. Build context: examples/sysbox-host.
FROM docker@sha256:e8faad5a8dc5279dff929afc5449f2791736912fff9f99351d742db2fad01b4c AS dind
FROM alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates iptables ip6tables
COPY --from=dind /usr/local/bin/ /usr/local/bin/
COPY --from=dind /usr/local/libexec/docker/ /usr/local/libexec/docker/
COPY --chmod=0755 dind /usr/local/libexec/dind
