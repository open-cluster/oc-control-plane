#!/bin/sh
set -eu

image='redocly/cli@sha256:cd93790e9ad4455d9d77977f98749e00b3fe995e81e2514cf34241d31b44fcfb'
output="${1:-/tmp/openapi.yaml}"

docker run --rm --volume "$(pwd):/work" --workdir /work "$image" \
  lint api/openapi.yaml --format stylish
docker run --rm --volume "$(pwd):/work" --workdir /work "$image" \
  bundle api/openapi.yaml --output "$output"
