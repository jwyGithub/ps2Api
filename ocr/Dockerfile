# 精准 OCR 服务（RapidOCR + ONNXRuntime）。用 uv 管理依赖，多阶段构建保持镜像精简。
FROM python:3.12-slim AS base

# RapidOCR 依赖 opencv：slim 镜像需补 libGL / glib 运行库。
RUN apt-get update && apt-get install -y --no-install-recommends \
        libgl1 \
        libglib2.0-0 \
        curl \
    && rm -rf /var/lib/apt/lists/*

# 引入 uv（官方静态二进制）
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

# 先装依赖（利用层缓存）：仅 pyproject 变化才重装。
COPY pyproject.toml ./
RUN uv sync --no-dev --no-install-project

# 再拷贝应用代码
COPY app ./app

# 预下载 RapidOCR 模型到镜像层，避免首次运行时联网拉取造成冷启动/离线失败。
RUN uv run python -c "from rapidocr_onnxruntime import RapidOCR; RapidOCR()"

ENV OCR_HOST=0.0.0.0 \
    OCR_PORT=8000

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8000/health || exit 1

CMD ["uv", "run", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000"]
