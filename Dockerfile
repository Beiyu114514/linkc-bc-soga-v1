# syntax=docker/dockerfile:1

# The build stage runs on the builder's native platform and cross-compiles, so
# multi-arch builds do not need QEMU for the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Version=${VERSION} \
      -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Commit=${COMMIT} \
      -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Date=${BUILD_DATE}" \
    -o /out/linkc-node ./cmd/linkc-node

FROM debian:bookworm-slim
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="linkc-node" \
      org.opencontainers.image.description="soga-v1 VLESS node agent with embedded Xray-core" \
      org.opencontainers.image.source="https://github.com/Beiyu114514/linkc-bc-soga-v1" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/linkc-node /var/lib/linkc-node /run/linkc-node \
    && chown -R 10001:10001 /var/lib/linkc-node /run/linkc-node
COPY --from=build /out/linkc-node /usr/local/bin/linkc-node
COPY --chmod=0644 configs/linkc-node.example.yaml /etc/linkc-node/config.yaml.example
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/linkc-node"]
CMD ["run", "-c", "/etc/linkc-node/config.yaml"]
