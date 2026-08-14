# 多阶段构建：builder 编译纯静态二进制（modernc sqlite 纯 Go，无需 cgo），运行阶段 alpine。
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ps2api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /ps2api /app/ps2api
ENV DATABASE_PATH=/data/gateway.db
VOLUME /data
EXPOSE 1930
ENTRYPOINT ["/app/ps2api"]
