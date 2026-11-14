VERSION ?= dev
ARCH ?= amd64
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Version=$(VERSION) \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Commit=$(COMMIT) \
  -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Date=$(DATE)

.PHONY: all build linux-amd64 linux-arm64 test race vet fmt clean package-deb package-rpm

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

package-deb: linux-$(ARCH)
	mkdir -p dist
	ARCH=$(ARCH) VERSION=$(VERSION) nfpm package -f packaging/nfpm.yaml -p deb -t dist/

package-rpm: linux-$(ARCH)
	mkdir -p dist
	ARCH=$(ARCH) VERSION=$(VERSION) nfpm package -f packaging/nfpm.yaml -p rpm -t dist/
