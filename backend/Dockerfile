FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Runtime image for the API (SPA static files are served by the ingress proxy).
FROM alpine:3.22
# ca-certificates: outbound HTTPS (deployment targets, approval webhooks, git clone)
# git:             AEGIS_BUILD_CLONE clones credential-free https repositories
# docker-cli:      AEGIS_BUILD_SANDBOX=container talks to the host Docker daemon
#                  through a mounted /var/run/docker.sock; see the comments below.
RUN apk add --no-cache ca-certificates git docker-cli \
    && addgroup -S aegis \
    && adduser -S -G aegis aegis
WORKDIR /app
COPY --from=build /out/server /app/server
# The container listens on all interfaces so the ingress proxy can reach it.
# Host exposure is controlled by the compose network and published ports, not by
# the process; plain `docker run` without `-p` still keeps it private.
# AEGIS_HTTP_ADDR is only a default: operators may pin a specific interface.
ENV AEGIS_HTTP_ADDR=0.0.0.0:8080
# BusyBox wget ships with the alpine base image; /healthz is process liveness
# only (it does not check PostgreSQL/Redis). Use /readyz for readiness.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz || exit 1
# Keep running as the unprivileged aegis account. AEGIS_BUILD_SANDBOX=container
# needs access to the host Docker socket, whose group id is host-specific and
# cannot be baked into the image. Add the socket's numeric GID at run time, e.g.:
#   docker run --group-add "$(stat -c '%g' /var/run/docker.sock)" \
#     -v /var/run/docker.sock:/var/run/docker.sock ...
# Mounting that socket is equivalent to giving the container control of the host
# Docker daemon (effectively host root); see "构建沙箱部署" in deploy/README.md.
USER aegis
EXPOSE 8080
ENTRYPOINT ["/app/server"]
