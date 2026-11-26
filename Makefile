VERSION ?= dev
ARCH ?= amd64
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
SOAK_DURATION ?= 90s
SOAK_TIMEOUT ?= 2m
LDFLAGS := -s -w \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Version=$(VERSION) \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Commit=$(COMMIT) \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Date=$(DATE)

.PHONY: quickstart all build linux-amd64 linux-arm64 test race vet fmt clean clean-dist package-deb package-rpm release-artifacts soak bench docker-launch test-launcher test-quickstart

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o linkc-node ./cmd/linkc-node

linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/linkc-node-linux-amd64 ./cmd/linkc-node

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/linkc-node-linux-arm64 ./cmd/linkc-node

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -rf linkc-node dist

clean-dist:
	rm -rf dist

# Build every release artifact (binaries + deb/rpm + checksums). Pass an
# explicit DATE so the recursive make invocations embed the same build time.
release-artifacts: clean-dist
	mkdir -p dist
	$(MAKE) linux-amd64 linux-arm64 VERSION=$(VERSION) DATE=$(DATE)
	$(MAKE) package-deb package-rpm VERSION=$(VERSION) ARCH=amd64 DATE=$(DATE)
	$(MAKE) package-deb package-rpm VERSION=$(VERSION) ARCH=arm64 DATE=$(DATE)
	install -m 0644 LICENSE THIRD_PARTY_NOTICES.md dist/
	cd dist && find . -maxdepth 1 -type f ! -name 'SHA256SUMS*' -printf '%f\0' \
		| sort -z | xargs -0 sha256sum > SHA256SUMS.tmp
	mv dist/SHA256SUMS.tmp dist/SHA256SUMS

package-deb: linux-$(ARCH)
	mkdir -p dist
	ARCH=$(ARCH) VERSION=$(VERSION) nfpm package -f packaging/nfpm.yaml -p deb -t dist/

package-rpm: linux-$(ARCH)
	mkdir -p dist
	ARCH=$(ARCH) VERSION=$(VERSION) nfpm package -f packaging/nfpm.yaml -p rpm -t dist/

# Randomized fault-injection soak (panel 5xx/429, outages, port swaps, user
# churn, certificate rotation). Use e.g. SOAK_DURATION=24h SOAK_TIMEOUT=25h.
soak:
	SOAK_DURATION=$(SOAK_DURATION) go test ./internal/agent -run TestSoak -count=1 -timeout $(SOAK_TIMEOUT)

bench:
	go test ./internal/xray -run '^$$' -bench . -benchtime 20x

# Interactive CLI wizard for deploying linkc-node in Docker.
docker-launch:
	./scripts/docker-launch.sh wizard

# 一键启动脚本的语法与帮助输出检查（不需要 root / Docker）。
quickstart:
	bash -n scripts/quickstart.sh
	./scripts/quickstart.sh --help >/dev/null

# Unit tests for the Docker launcher (stdlib only; no Docker or root needed).
test-launcher:
	python3 -m unittest discover -s scripts -p 'test_*.py' -v

# Regression tests for installer path safety and quickstart argument handling.
test-quickstart:
	bash -n scripts/quickstart.sh scripts/install_paths.sh scripts/test_quickstart.sh
	sh -n scripts/install.sh scripts/install_paths.sh
	bash scripts/test_quickstart.sh
