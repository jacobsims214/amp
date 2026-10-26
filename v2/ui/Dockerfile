FROM node:22-alpine AS builder
WORKDIR /app
# Install any CA certs dropped into the repo-root certs/ directory.
# This handles corporate/VPN SSL inspection (e.g. Zscaler).
# The directory is gitignored — add your .pem/.crt files there locally.
RUN apk add --no-cache ca-certificates
COPY certs/ /tmp/certs/
# Merge all certs into one bundle for system (Go/curl) and set NODE_EXTRA_CA_CERTS for npm/node
RUN cat /tmp/certs/*.pem /tmp/certs/*.crt > /usr/local/share/ca-certificates/custom.crt 2>/dev/null || true \
    && update-ca-certificates
ENV NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/custom.crt
COPY ui/package*.json ./
RUN npm ci
COPY ui/ .
RUN npm run build

FROM nginxinc/nginx-unprivileged:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
# nginx.conf: every path falls back to index.html (SPA); /sessions/ also gets the
# remote session page's strict CSP. nginx-unprivileged already listens on 8080 as
# a non-root UID (101) — needed to run under a "restricted" Pod Security Standard
# securityContext (runAsNonRoot, no privileged ports).
COPY ui/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 8080
