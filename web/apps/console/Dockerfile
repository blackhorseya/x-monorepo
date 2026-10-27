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
FROM nginxinc/nginx-unprivileged:1.27-alpine@sha256:65e3e85dbaed8ba248841d9d58a899b6197106c23cb0ff1a132b7bfe0547e4c0
ENV RPC_UPSTREAM=http://rpc:8080 \
    NGINX_ENVSUBST_FILTER=^RPC_ \
    NGINX_ENVSUBST_OUTPUT_DIR=/tmp
COPY deploy/default.conf.template /etc/nginx/templates/default.conf.template
COPY deploy/console.conf /etc/nginx/conf.d/default.conf
COPY deploy/security-headers.conf /etc/nginx/security-headers.conf
COPY dist/ /usr/share/nginx/html/
EXPOSE 8080
