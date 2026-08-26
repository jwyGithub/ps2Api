#!/bin/sh
# ps2api 容器联合入口：先在后台拉起内置 OCR 服务（仅监听回环，供网关内部调用），
# 再把 ps2api 作为前台主进程运行。图片识别选 ocr / ocr_then_vision 引擎时开箱即用。
set -e

OCR_HOST="${OCR_HOST:-127.0.0.1}"
OCR_PORT="${OCR_PORT:-8000}"

# 后台启动 OCR（uvicorn）。识别失败不影响网关启动：ocr 模式的调用会返回错误、
# ocr_then_vision 模式会回退视觉模型。
(
  cd /app/ocr
  exec uv run uvicorn app.main:app --host "$OCR_HOST" --port "$OCR_PORT"
) &

# 前台运行网关（PID 1 语义交给 ps2api，保证信号/退出码正确传递）。
cd /app
exec /app/ps2api "$@"
