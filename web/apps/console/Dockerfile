# The console as a static site behind nginx, with /api proxied to the rpc. Build the site
# first (`task web-build` → dist/), this image only copies it — same split as the Go images
# (deployments/docker/Dockerfile wraps cross-compiled binaries). Build context: this directory.
#   task web-build && docker build -t console web/apps/console
#   docker run --rm -p 8081:8080 -e RPC_UPSTREAM=http://host.docker.internal:8080 console
# nginx-unprivileged: listens on 8080, pid / temp files under /tmp, no root at any point.
# At start the entrypoint renders deploy/default.conf.template (envsubst, RPC_UPSTREAM only)
# into /tmp (the entrypoint refuses an output dir that does not exist yet, and only /tmp does),
# which conf.d/console.conf includes — so /tmp is the ONE path that has to be
# writable: a read-only root filesystem with a tmpfs / emptyDir at /tmp is enough, whatever
# uid runs it (the Helm chart runs it as 65532 like the Go images).
# The base is pinned tag@sha256 (the multi-arch index, so linux/amd64 and linux/arm64 both resolve);
# dependabot's docker ecosystem moves the tag and the digest together.
FROM nginxinc/nginx-unprivileged:1.31-alpine@sha256:b5047b9b76845eac124358373c6852377c52e4b05834a47647e955dd91cb8f1e
# Runtime configuration (docs/identity-login.md §8): CONSOLE_OIDC_ISSUER / CONSOLE_OIDC_CLIENT_ID /
# CONSOLE_API_KEY_LOGIN become /config.json for the SPA, and the issuer's exact origin joins the CSP's
# connect-src. The envsubst filter is an exact whitelist: no other variable of the container is ever
# substituted into a template. 18-console-env.envsh (sourced first) validates the values.
ENV RPC_UPSTREAM=http://rpc:8080 \
    NGINX_ENVSUBST_FILTER=^(RPC_UPSTREAM|CONSOLE_OIDC_ISSUER|CONSOLE_OIDC_CLIENT_ID|CONSOLE_API_KEY_LOGIN|CONSOLE_CSP_CONNECT_SRC)$ \
    NGINX_ENVSUBST_OUTPUT_DIR=/tmp
COPY deploy/default.conf.template /etc/nginx/templates/default.conf.template
COPY deploy/config.json.template /etc/nginx/templates/config.json.template
COPY deploy/security-headers.inc.template /etc/nginx/templates/security-headers.inc.template
COPY deploy/console.conf /etc/nginx/conf.d/default.conf
COPY --chmod=755 deploy/18-console-env.envsh /docker-entrypoint.d/18-console-env.envsh
COPY dist/ /usr/share/nginx/html/
EXPOSE 8080
