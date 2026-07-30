---
name: docker-engineering
description: Use when writing Dockerfiles, docker-compose configs, or troubleshooting container builds — covers multi-stage builds, layer caching, security hardening, health checks, and corporate CA certificate injection.
---

# Docker Engineering

**References:** [Examples](examples.md)

## Images

| Rule | Do | Don't |
|------|-----|-------|
| Base image | `alpine:3.x` or distroless for runtime | `ubuntu`, `debian` in production |
| Tags | Pin exact version `alpine:3.20` | `latest` or branch tags |
| Layers | Chain with `&&` in one `RUN` | One `RUN` per command |

## Multi-Stage Builds

> [Full example](examples.md#go-dockerfile)

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/server ./cmd/server

FROM alpine:3.20
COPY --from=builder /app/server /app/server
CMD ["/app/server"]
```

| Rule | Do | Don't |
|------|-----|-------|
| Runtime content | Copy only compiled binary | Copy entire source tree |
| Build tools | Install in builder stage only | Install build tools in runtime |

## Layer Caching

| Rule | Do | Don't |
|------|-----|-------|
| Dep files first | `COPY go.sum ./` before `COPY . .` | `COPY . .` first |
| Source last | `COPY . .` after dep install | Mix deps and source in same layer |
| Group RUNs | Chain commands with `&&` | Separate `RUN` for each `apt-get` |

## Security

```dockerfile
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser
```

| Rule | Do | Don't |
|------|-----|-------|
| User | `adduser -D appuser && USER appuser` | Run as root |
| Secrets | Build args for build-time; runtime env for runtime | Hardcode in `ENV` |
| Filesystem | `--read-only` flag where possible | Writable filesystem by default |

## Health Checks

```dockerfile
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1
```

Use `wget` on Alpine (available by default). Replace `8080` with your service port.

## Corporate CA Certificates

| Distro | Command |
|--------|---------|
| Alpine | `COPY cert.pem /usr/local/share/ca-certificates/cert.crt` + `RUN update-ca-certificates` |
| Debian/Ubuntu | Same path and command as Alpine |
| RHEL/Amazon Linux | `/etc/pki/ca-trust/source/anchors/cert.crt` + `RUN update-ca-trust` |

Add cert steps to the **runtime stage**. Builder-stage-only cert injection won't help runtime HTTP calls.

## Compose

> [Full example](examples.md#compose)

| Rule | Do | Don't |
|------|-----|-------|
| Health deps | `condition: service_healthy` | Bare `depends_on: [service]` |
| Persistence | Named volumes | Bind mounts in production |
| Restart | `restart: unless-stopped` | No restart policy |

```yaml
depends_on:
  db:
    condition: service_healthy
```

## Do / Don't

| Do | Don't |
|----|-------|
| Pin every base image tag | Use `latest` |
| Non-root user in runtime stage | Run containers as root |
| `--rm` on `docker compose run` one-offs | Let stopped containers accumulate |
| Health check on every HTTP service | Ship without `HEALTHCHECK` |
| Named volumes for stateful data | Bind mounts for production state |
