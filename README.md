# OpenCluster Control Plane

OpenCluster investigates incidents across connected operational systems and records the
reads behind each finding. This repository contains the multi-tenant control plane:
organizations, integrations, alerts, incidents, investigations, and the protocol
endpoint used by the separate OpenCluster Relay.

The project is under active development and does not yet publish a stable release from
this repository.

Licensed under the [Apache License 2.0](./LICENSE).

## Develop

You need Go 1.26.6, Docker with Docker Compose, and Helm 3. The complete test suite
needs a reachable Docker daemon for PostgreSQL test containers and deployment validation.

```bash
make tools       # install pinned development tools
make test-short  # unit tests without Docker
make verify      # the complete local CI gate
```

Composed-process integration tests live in `test/controlplane`, independently authored
evaluation scenarios live in `test/eval/cases`, and the real Relay protocol proof lives
in the separate `test/e2e` module.

## Run locally

Start PostgreSQL, write its DSN to a file, and run the control plane:

```bash
printf '%s' 'postgres://user:password@localhost:5432/opencluster?sslmode=disable' \
  > /tmp/opencluster.dsn

cat > opencluster.yaml <<'YAML'
server:
  address: "127.0.0.1:8080"
database:
  dsn_file: "/tmp/opencluster.dsn"
YAML

go run ./cmd/controlplane --config opencluster.yaml
```

This minimal configuration exposes health, readiness, and metrics. See the
[self-hosted configuration](./docs/self-hosted/configuration.mdx) to enable operator
access, alert intake, Relays, integrations, and investigations.

## Deployment quick starts

Docker Compose starts the control plane beside PostgreSQL. Place the PostgreSQL password
and matching DSN in separate local files and expose their file paths. The Apache-licensed
Relay protocol contract is bundled, so no private repository access is required:

```bash
export OPENCLUSTER_POSTGRES_PASSWORD_FILE=/absolute/path/postgres-password
export OPENCLUSTER_DATABASE_DSN_FILE=/absolute/path/postgres-dsn
export OPENCLUSTER_OPERATOR_TOKEN_FILE=/absolute/path/admin-bootstrap-token
export OPENCLUSTER_SEALING_KEY_FILE=/absolute/path/32-byte-sealing-key
export OPENCLUSTER_MODEL_KEY_FILE=/absolute/path/model-api-key
export OPENCLUSTER_MODEL_NAME=claude-sonnet-5
export OPENCLUSTER_MODEL_CONSENTED_PROVIDERS=anthropic
export OPENCLUSTER_RUNTIME_UID=$(id -u)
export OPENCLUSTER_RUNTIME_GID=$(id -g)
docker compose -f deploy/compose/compose.yaml up --build
```

The DSN must use host `postgres`, database `opencluster`, user `opencluster`, and the
password from the first file. The administrator token must contain at least 32 characters;
its Organization defaults to `local` and can be set through `OPENCLUSTER_ORGANIZATION`.
The container runs as the invoking non-root UID/GID so it can read securely permissioned
bind-mounted files; no access token or database credential belongs in an image layer or
environment value.

For Kubernetes, create a Secret containing the `postgres-dsn` file and make your locally
built `opencluster-control-plane:dev` image available to the cluster:

```bash
kubectl create secret generic opencluster-database \
  --from-file=postgres-dsn=/absolute/path/postgres-dsn
kubectl create secret generic opencluster-credentials \
  --from-file=operator-token=/absolute/path/admin-bootstrap-token \
  --from-file=sealing-key=/absolute/path/32-byte-sealing-key \
  --from-file=model-key=/absolute/path/model-api-key
helm upgrade --install opencluster ./deploy/helm/opencluster \
  --set-json 'model.consentedProviders=["anthropic"]'
```

Both deployment examples enable the same-origin browser console, organization-scoped
operator bootstrap, webhook intake, Conversations, and a consented model provider. Open
the shared HTTP address to sign in, bootstrap the first administrator, start an
Investigation, inspect its Activity and Sources, or cancel active work.

The chart mounts both existing Secrets as files, disables service-account token mounting,
and runs the control plane as a non-root user with a read-only filesystem. Configure image
repository, pull policy, and the external PostgreSQL endpoint for your own environment.

To connect Relays, calculate the certificate's public-key pin and enable Compose's
optional non-root gRPC/TLS terminator:

```bash
export OPENCLUSTER_RELAY_TLS_CERT_FILE=/absolute/path/relay-cert.pem
export OPENCLUSTER_RELAY_TLS_KEY_FILE=/absolute/path/relay-key.pem
export OPENCLUSTER_RELAY_ADDRESS=:8444
export OPENCLUSTER_RELAY_SPKI_PINS="$(openssl x509 -in "$OPENCLUSTER_RELAY_TLS_CERT_FILE" \
  -pubkey -noout | openssl pkey -pubin -outform DER \
  | openssl dgst -sha256 -binary | openssl base64 -A)"
docker compose -f deploy/compose/compose.yaml --profile relay up --build
```

The TLS endpoint listens on `127.0.0.1:8443`; set
`OPENCLUSTER_RELAY_BIND_ADDRESS` when publishing it through your network edge.
For Kubernetes, create a TLS Secret and enable the chart's pinned gRPC/TLS sidecar:

```bash
kubectl create secret tls opencluster-relay-tls \
  --cert=/absolute/path/relay-cert.pem --key=/absolute/path/relay-key.pem
helm upgrade --install opencluster ./deploy/helm/opencluster \
  --set-json 'model.consentedProviders=["anthropic"]' \
  --set relay.enabled=true \
  --set relay.tls.existingSecret=opencluster-relay-tls \
  --set-json "relay.spkiPins=[\"$OPENCLUSTER_RELAY_SPKI_PINS\"]"
```

The Relay listener remains disabled unless explicitly enabled together with a real TLS
certificate and its matching SHA-256 SPKI pin.

## Documentation

- [Product documentation](./docs/index.mdx) — setup, integrations, investigations,
  security, and self-hosted operations
- [CONTEXT.md](./CONTEXT.md) — domain vocabulary
- [AGENTS.md](./AGENTS.md) — repository boundaries and required verification
- [ARCHITECTURE.md](./ARCHITECTURE.md) — service and customer trust boundaries
- [SECURITY.md](./SECURITY.md) — vulnerability reporting and security model

The Mintlify site is authored in `docs/`. Every shipped integration must have a
navigable product page.

## Contributing

Contributors should start with [CONTRIBUTING.md](./CONTRIBUTING.md).
[SUPPORT.md](./SUPPORT.md), [GOVERNANCE.md](./GOVERNANCE.md),
[MAINTAINERS.md](./MAINTAINERS.md), and [ROADMAP.md](./ROADMAP.md) describe support,
decision-making, ownership, and current direction. Contributions are licensed under the
[Apache License 2.0](./LICENSE).
