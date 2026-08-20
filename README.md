# OpenCluster Control Plane

OpenCluster investigates incidents across connected operational systems and records the
reads behind each finding. This repository contains the multi-tenant control plane:
organizations, integrations, alerts, incidents, investigations, and the protocol
endpoint used by the separate OpenCluster Relay.

The project is under active development and does not yet publish a stable release from
this repository.

Proprietary. See [LICENSE](./LICENSE).

## Develop

You need Go 1.26.6. The complete test suite also needs a reachable Docker daemon for
PostgreSQL test containers.

```bash
make tools       # install pinned development tools
make test-short  # unit tests without Docker
make verify      # the complete local CI gate
```

## Run locally

Start PostgreSQL, write its DSN to a file, and run the control plane:

```bash
printf '%s' 'postgres://user:password@localhost:5432/opencluster?sslmode=disable' \
  > /tmp/opencluster.dsn

OC_HTTP_ADDRESS=127.0.0.1:8080 \
OC_PLACEMENTS=shared=/tmp/opencluster.dsn \
OC_DEFAULT_PLACEMENT=shared \
go run ./cmd/controlplane
```

This minimal configuration exposes health, readiness, and metrics. See the
[self-hosted configuration](./docs/self-hosted/configuration.mdx) to enable operator
access, alert intake, Relays, integrations, and investigations.

## Documentation

- [Product documentation](./docs/index.mdx) — setup, integrations, investigations,
  security, and self-hosted operations
- [CONTEXT.md](./CONTEXT.md) — domain vocabulary
- [AGENTS.md](./AGENTS.md) — repository boundaries and required verification

The Mintlify site is authored in `docs/`. Every shipped integration must have a
navigable product page.

## Contributing

Read [AGENTS.md](./AGENTS.md) before changing code. Use the terms in
[CONTEXT.md](./CONTEXT.md), add tests at observable boundaries, and run `make verify`
before submitting a change.
