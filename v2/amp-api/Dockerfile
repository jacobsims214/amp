FROM golang:1.25-alpine AS builder
WORKDIR /app
# Install any CA certs dropped into the repo-root certs/ directory.
# This handles corporate/VPN SSL inspection (e.g. Zscaler).
# The directory is gitignored — add your .pem/.crt files there locally.
RUN apk add --no-cache ca-certificates
COPY certs/ /tmp/certs/
RUN cat /tmp/certs/*.pem /tmp/certs/*.crt > /usr/local/share/ca-certificates/custom.crt 2>/dev/null || true \
    && update-ca-certificates
COPY amp-api/go.mod amp-api/go.sum ./
RUN go mod download
COPY amp-api/ .
RUN CGO_ENABLED=0 go build -o amp-api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 amp
WORKDIR /app
COPY --from=builder /app/amp-api .
RUN chown amp:amp /app/amp-api
USER 1000
EXPOSE 8000 3001
CMD ["./amp-api"]
