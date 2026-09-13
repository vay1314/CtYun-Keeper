# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.24
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS TARGETARCH APP_VERSION=dev APP_COMMIT=unknown APP_BUILD_TIME=unknown UPDATE_PUBLIC_KEY="" LAUNCHER_VERSION=1.0.0
WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION} -X main.commit=${APP_COMMIT} -X main.buildTime=${APP_BUILD_TIME} -X main.updatePublicKey=${UPDATE_PUBLIC_KEY}" -o /out/ctyun-keeper ./cmd/ctyun-keeper
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.launcherVersion=${LAUNCHER_VERSION} -X main.updatePublicKey=${UPDATE_PUBLIC_KEY}" -o /out/launcher ./cmd/ctyun-keeper-launcher

FROM alpine:3.22
ARG APP_VERSION=dev LAUNCHER_VERSION=1.0.0
LABEL org.opencontainers.image.title="CtYunKeeper" \
      org.opencontainers.image.description="CtYunKeeper cloud desktop keep-alive, points tasks and management panel" \
      org.opencontainers.image.version="${APP_VERSION}" \
      org.opencontainers.image.source="https://github.com/vay1314/CtYun-Keeper"
RUN apk add --no-cache ca-certificates tzdata su-exec \
    && addgroup -S -g 10001 ctyun \
    && adduser -S -D -H -u 10001 -G ctyun ctyun \
    && mkdir -p /app/data /app/builtin/static \
    && chown -R ctyun:ctyun /app
WORKDIR /app
COPY --from=builder /out/launcher /app/launcher
COPY --from=builder /out/ctyun-keeper /app/builtin/ctyun-keeper
COPY --chown=ctyun:ctyun app/web/static /app/builtin/static
RUN printf '{"version":"%s","minimumLauncherVersion":"%s"}\n' "$APP_VERSION" "$LAUNCHER_VERSION" > /app/builtin/version.json \
    && chown -R ctyun:ctyun /app/builtin /app/launcher \
    && chmod 0755 /app/launcher /app/builtin/ctyun-keeper
COPY app/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh
ENV TZ=Asia/Shanghai CTYUN_CONTAINER=true CTYUN_DATA_DIR=/app/data BUILTIN_DIR=/app/builtin APP_PORT=9845
USER root
VOLUME ["/app/data"]
EXPOSE 9845
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -q -T 3 -O /dev/null http://127.0.0.1:9845/health || exit 1
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
