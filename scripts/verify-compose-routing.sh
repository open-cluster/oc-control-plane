#!/bin/sh
set -eu

network="opencluster-routing-$$"
backend="opencluster-routing-backend-$$"
frontend="opencluster-routing-frontend-$$"
root_body="/tmp/opencluster-routing-root-$$"

cleanup() {
  docker rm --force "$frontend" "$backend" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -f "$root_body"
}
trap cleanup EXIT INT TERM

docker network create "$network" >/dev/null
docker run --detach --rm --name "$backend" --network "$network" \
  --network-alias control-plane nginxinc/nginx-unprivileged:1.27-alpine >/dev/null
docker run --detach --rm --name "$frontend" --network "$network" \
  --publish 127.0.0.1::8080 opencluster-frontend:ci >/dev/null

port="$(docker port "$frontend" 8080/tcp | sed -n 's/.*://p' | head -n 1)"
base="http://127.0.0.1:$port"
attempt=0
until curl --fail --silent "$base/" >"$root_body"; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "frontend did not become ready" >&2
    exit 1
  fi
  sleep 1
done

grep -q "OpenCluster" "$root_body"

for path in /api/v1/probe /webhooks/v1/probe /healthz /readyz /metrics; do
  status="$(curl --silent --output /dev/null --write-out '%{http_code}' "$base$path")"
  if [ "$status" != "404" ]; then
    echo "$path returned $status; expected the backend's 404" >&2
    exit 1
  fi
done
