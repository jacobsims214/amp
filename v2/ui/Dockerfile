FROM node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
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
