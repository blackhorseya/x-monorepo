# Used by goreleaser (dockers_v2) only: the binary is already cross-compiled, this just wraps it.
# Build context layout: <os>/<arch>/<binary>; APP picks which one (rpc | worker | migrate).
# distroless/static has CA certs + tzdata and no shell — TLS to Postgres/Redis/OTLP works,
# `docker exec sh` does not. Healthcheck from outside (GET /api/readyz), not HEALTHCHECK.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
ARG APP
COPY $TARGETPLATFORM/$APP /app
# Config: APP_* env or mount a YAML and set APP_CONFIG=/etc/x-monorepo/config.yaml.
ENTRYPOINT ["/app"]
