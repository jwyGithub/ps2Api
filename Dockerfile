# 多阶段构建：
#   1) go-build —— 编译纯静态 ps2api 二进制（modernc sqlite 纯 Go，无需 cgo）。
#   2) 运行阶段 —— 基于 python:3.12-slim，镜像内同时内置 OCR 服务（RapidOCR + ONNXRuntime）与 ps2api：
#      入口脚本先在 127.0.0.1:8000 起 OCR，再运行网关。图片识别选 ocr / ocr_then_vision 引擎即开箱即用，
#      无需再单独部署 OCR 容器、也无需在面板配置 OCR 服务地址。
FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ps2api .

FROM python:3.12-slim
# RapidOCR 依赖 opencv：slim 镜像需补 libGL / glib 运行库；ca-certificates 供 ps2api 出网 TLS；
# wget 供 docker-compose 健康检查（探测 ps2api /health）。
RUN apt-get update && apt-get install -y --no-install-recommends \
        libgl1 \
        libglib2.0-0 \
        ca-certificates \
        wget \
    && rm -rf /var/lib/apt/lists/*

# 引入 uv（官方静态二进制）管理内置 OCR 服务的 Python 依赖。
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# ── 内置 OCR 服务 ──────────────────────────────────────────────
# 先装依赖（利用层缓存）：仅 ocr/pyproject.toml 变化才重装。
WORKDIR /app/ocr
COPY ocr/pyproject.toml ./
RUN uv sync --no-dev --no-install-project
COPY ocr/app ./app
# 预下载 RapidOCR 模型到镜像层，避免首个请求承担联网拉取（离线环境亦可用）。
RUN uv run python -c "from rapidocr_onnxruntime import RapidOCR; RapidOCR()"

# ── ps2api 网关 + 联合入口 ─────────────────────────────────────
WORKDIR /app
COPY --from=go-build /ps2api /app/ps2api
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

ENV DATABASE_PATH=/data/gateway.db \
    OCR_HOST=127.0.0.1 \
    OCR_PORT=8000
VOLUME /data
EXPOSE 1930
ENTRYPOINT ["/app/docker-entrypoint.sh"]
