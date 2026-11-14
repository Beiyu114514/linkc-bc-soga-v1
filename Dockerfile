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
    && apt-get install -y --no-install-recommends ca-certificates libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/linkc-node /var/lib/linkc-node /run/linkc-node \
    && chown -R 10001:10001 /var/lib/linkc-node /run/linkc-node
COPY --from=build /out/linkc-node /usr/local/bin/linkc-node
COPY --chmod=0644 configs/linkc-node.example.yaml /etc/linkc-node/config.yaml.example
# The panel may assign a port below 1024 (the target panel uses 440). The
# container runs as UID 10001, so grant only the low-port bind capability to
# the binary instead of running the whole container as root. Docker cannot set
# ambient capabilities; file capabilities are the non-root equivalent, and
# they require the container to keep no-new-privileges disabled. libcap2-bin is
# kept in the image so `getcap` can verify the capability.
RUN setcap cap_net_bind_service=+ep /usr/local/bin/linkc-node
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/linkc-node"]
CMD ["run", "-c", "/etc/linkc-node/config.yaml"]
