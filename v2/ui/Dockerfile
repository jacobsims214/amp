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

FROM nginx:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
# Route all paths to index.html for SPA
RUN echo 'server { \
  listen 80; \
  root /usr/share/nginx/html; \
  location / { try_files $uri $uri/ /index.html; } \
  location /api/ { proxy_pass http://amp-api:3001; proxy_http_version 1.1; \
    proxy_set_header Connection ""; proxy_buffering off; } \
}' > /etc/nginx/conf.d/default.conf
EXPOSE 80
