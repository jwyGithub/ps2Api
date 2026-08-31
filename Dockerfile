# 多阶段构建：
#   1) go-build —— 编译纯静态 ps2api 二进制（modernc sqlite 纯 Go，无需 cgo）。
#   2) 运行阶段 —— 极简 alpine，仅放置 ps2api 二进制。图片识别统一走外部视觉模型（vision），
#      无需再内置任何识别服务或额外运行时。
FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ps2api .

FROM alpine:3.20
# ca-certificates 供 ps2api 出网 TLS；wget 由 busybox 内置，供 docker-compose 健康检查探测 /health。
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=go-build /ps2api /app/ps2api

ENV DATABASE_PATH=/data/gateway.db
VOLUME /data
EXPOSE 1930
ENTRYPOINT ["/app/ps2api"]
