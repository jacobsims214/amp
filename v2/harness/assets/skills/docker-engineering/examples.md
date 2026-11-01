# Docker Engineering — Examples

## Go Dockerfile

Complete multi-stage Go Dockerfile with non-root user, health check, and CA cert injection.

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dep manifests first — maximizes layer cache
COPY go.mod go.sum ./
RUN go mod download

# Copy source after deps are cached
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server

# ──────────────────────────────────────────────
# Runtime stage — minimal Alpine, no Go toolchain
FROM alpine:3.20

WORKDIR /app

# Corporate CA certificate (required for HTTPS in restricted networks)
COPY certs/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt
RUN update-ca-certificates

# Non-root user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

# Copy only the compiled binary
COPY --from=builder /app/server /app/server

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

CMD ["/app/server"]
```

---

## Compose

Docker Compose service with health check and `depends_on` with condition.

```yaml
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: appdb
    volumes:
      - db-data:/var/lib/postgresql/data
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 10s

  api:
    build: .
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://app:secret@db:5432/appdb
    restart: unless-stopped
    depends_on:
      db:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"]
      interval: 30s
      timeout: 3s
      retries: 3
      start_period: 10s

volumes:
  db-data:
```
