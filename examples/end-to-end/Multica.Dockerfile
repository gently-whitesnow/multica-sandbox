FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY --from=multica /server/go.mod /server/go.sum ./
RUN go mod download
COPY --from=multica /server/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /server ./cmd/server && CGO_ENABLED=0 go build -trimpath -o /migrate ./cmd/migrate
FROM alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
WORKDIR /app
COPY --from=build /server /migrate ./
COPY --from=multica /server/migrations/ ./migrations/
COPY --from=multica /LICENSE /NOTICE ./
COPY --from=multica /docker/entrypoint.sh ./entrypoint.sh
RUN chmod +x entrypoint.sh
ENTRYPOINT ["/app/entrypoint.sh"]
