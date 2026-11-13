# syntax=docker/dockerfile:1
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Version=${VERSION} -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Commit=${COMMIT} -X github.com/Beiyu114514/linkc-bc-soga-v1/internal/version.Date=${BUILD_DATE}" \
    -o /out/linkc-node ./cmd/linkc-node

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/linkc-node /var/lib/linkc-node /run/linkc-node \
    && chown -R 10001:10001 /var/lib/linkc-node /run/linkc-node
COPY --from=build /out/linkc-node /usr/local/bin/linkc-node
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/linkc-node"]
CMD ["run", "-c", "/etc/linkc-node/config.yaml"]
