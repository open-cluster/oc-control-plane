#!/bin/sh
set -eu

image='redocly/cli@sha256:cd93790e9ad4455d9d77977f98749e00b3fe995e81e2514cf34241d31b44fcfb'
output="${1:-.openapi.tmp.yaml}"
if [ "$#" -eq 0 ]; then
  trap 'rm -f "$output"' EXIT
fi

docker run --rm --volume "$(pwd):/work" --workdir /work "$image" \
  lint api/openapi.yaml --format stylish
docker run --rm --volume "$(pwd):/work" --workdir /work "$image" \
  bundle api/openapi.yaml --output "$output"

# Mintlify needs an absolute server to render copyable request examples. Keep the canonical
# contract deployment-neutral and specialize only its generated documentation input.
sed -i '0,/^  - url: \/$/s||  - url: http://localhost:8080\n    description: Local self-hosted deployment|' "$output"
