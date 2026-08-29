# syntax=docker/dockerfile:1
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY deploy/compose/frontend-nginx.conf /etc/nginx/nginx.conf
COPY web/ /usr/share/nginx/html/

EXPOSE 8080
