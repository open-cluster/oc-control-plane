STATICCHECK_VERSION := 2025.1.1
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.1.4
GOLICENSES_VERSION := v2.0.1

.PHONY: tools lint openapi docs build test test-short vuln licenses deploy-verify verify

HARNESS_MODULE := test/e2e
TEST_TIMEOUT ?= 30m
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
LICENSE_FLAGS := --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC,MPL-2.0 \
	--ignore github.com/open-cluster/oc-control-plane

tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	go install github.com/google/go-licenses/v2@$(GOLICENSES_VERSION)

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	cd $(HARNESS_MODULE) && go vet ./...
	staticcheck ./...
	cd $(HARNESS_MODULE) && staticcheck ./...
	golangci-lint run
	cd $(HARNESS_MODULE) && golangci-lint run

openapi:
	sh scripts/openapi.sh

docs:
	node scripts/validate-docs.mjs

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" ./...

test:
	CGO_ENABLED=1 go test -race -count=1 -timeout $(TEST_TIMEOUT) ./...
	cd $(HARNESS_MODULE) && CGO_ENABLED=1 go test -race -count=1 -timeout $(TEST_TIMEOUT) ./...

test-short:
	CGO_ENABLED=1 go test -race -short -count=1 -timeout $(TEST_TIMEOUT) ./...

vuln:
	govulncheck ./...
	cd $(HARNESS_MODULE) && govulncheck ./...

licenses:
	go-licenses check ./... $(LICENSE_FLAGS)
	cd $(HARNESS_MODULE) && go-licenses check ./... $(LICENSE_FLAGS)

deploy-verify:
	docker compose -f deploy/compose/compose.yaml config --no-interpolate > /dev/null
	helm lint ./deploy/helm/opencluster --set-json 'model.consentedProviders=["anthropic"]'
	helm template opencluster ./deploy/helm/opencluster \
		--set-json 'model.consentedProviders=["anthropic"]' \
		--set relay.enabled=true \
		--set relay.tls.existingSecret=opencluster-relay-tls \
		--set-json 'relay.spkiPins=["AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="]' \
		> /dev/null
	docker build --file deploy/compose/Dockerfile --tag opencluster-control-plane:ci .
	docker build --file deploy/compose/Frontend.Dockerfile --tag opencluster-frontend:ci .
	sh scripts/verify-compose-routing.sh

verify: openapi docs lint build test vuln licenses deploy-verify
