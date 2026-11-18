# 多阶段构建：编译期用 golang 镜像，运行期用 distroless static（含 CA 证书与 tzdata，
# 但没有 shell —— 少一个 shell 就少一整类「进容器 exec 一把」的运维路径）。
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
WORKDIR /src

# 先只拷 go.mod：本仓零第三方依赖，这一层几乎没有缓存价值，但它把「改代码」与
# 「拉依赖」彻底分开（将来加依赖时不必每次重拉）。
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/aivideomaker2api ./cmd/server \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/avm2api-probe ./cmd/probe

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/aivideomaker2api /usr/local/bin/aivideomaker2api
COPY --from=build /out/avm2api-probe /usr/local/bin/avm2api-probe

# 运行期数据与凭据都挂在这里：
#   /app/auths   账号（每个号一个 json，0700）
#   /app/data    任务表与账号状态
#   /app/config.json
VOLUME ["/app/auths", "/app/data"]

EXPOSE 8808

# 健康检查跟着 -listen / AVM2_LISTEN 走：写死端口的 HEALTHCHECK 在挪端口之后恒
# unhealthy，服务好端端的却让人去查错方向（原项目为这个白跑过一轮实验）。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/aivideomaker2api", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/aivideomaker2api"]
CMD ["-config", "/app/config.json"]
