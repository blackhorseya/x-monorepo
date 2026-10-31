# Used by goreleaser (dockers_v2) only: the binary is already cross-compiled, this just wraps it.
# Build context layout: <os>/<arch>/<binary>; APP picks which one (rpc | catalog-rpc | worker | migrate).
# distroless/static has CA certs + tzdata and no shell — TLS to Postgres/Redis/OTLP works,
# `docker exec sh` does not. Healthcheck from outside (GET /api/readyz), not HEALTHCHECK.
# The base is pinned tag@sha256 (the multi-arch index, so linux/amd64 and linux/arm64 both resolve);
# dependabot's docker ecosystem moves the tag and the digest together.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM
ARG APP
COPY $TARGETPLATFORM/$APP /app
# Config: APP_* env or mount a YAML and set APP_CONFIG=/etc/x-monorepo/config.yaml.
ENTRYPOINT ["/app"]
