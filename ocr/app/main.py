"""FastAPI OCR 服务。

对接 ps2api 网关（internal/provider/vision.go 的 callOCR）契约：
  请求  POST <ocr_api_base>  JSON: {"image": "<data URL / http(s) 直链 / base64>", "lang": "chi_sim+eng"}
        配置了 key 时带 Authorization: Bearer <OCR_API_KEY>
  响应  2xx，JSON 顶层含 "text" 字段（网关据此提取识别文本）

额外提供 /health 健康检查（docker-compose healthcheck 使用）。
"""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager

import anyio
from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from . import __version__
from .config import settings
from .image_utils import ImageError, load_image_bytes
from . import ocr_engine

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
logger = logging.getLogger("ocr.api")


@asynccontextmanager
async def lifespan(_: FastAPI):
    # 后台线程预热模型，不阻塞服务起来（/health 立即可用）。
    async def _warm():
        try:
            await anyio.to_thread.run_sync(ocr_engine.warmup)
            logger.info("模型预热完成")
        except Exception as exc:  # 预热失败不致命，首个请求会重试加载
            logger.warning("模型预热失败（首个请求将重试）: %s", exc)

    async with anyio.create_task_group() as tg:
        tg.start_soon(_warm)
        yield


app = FastAPI(title="ps2api OCR", version=__version__, lifespan=lifespan)


def require_auth(authorization: str | None = Header(default=None)) -> None:
    """可选 Bearer 鉴权：未配置 OCR_API_KEY 时放行；配置了则校验。"""
    if not settings.api_key:
        return
    expected = f"Bearer {settings.api_key}"
    if authorization != expected:
        raise HTTPException(status_code=401, detail="无效或缺失的 Authorization Bearer Token")


class OCRRequest(BaseModel):
    image: str = Field(..., description="data URL / http(s) 直链 / 纯 base64 的图片")
    # lang 由网关传入（默认 chi_sim+eng）。RapidOCR 中英一体模型无需切换，仅作记录/透传。
    lang: str | None = Field(default=None, description="语言提示（RapidOCR 中英一体，仅记录）")


class OCRLine(BaseModel):
    text: str
    score: float | None = None
    box: list | None = None


class OCRResponse(BaseModel):
    text: str
    lines: list[OCRLine] = []
    lang: str | None = None
    elapse_ms: int = 0
    # 诊断字段（网关只读 text，这些不影响契约）：原始图尺寸、检出行数、是否放大过。
    width: int = 0
    height: int = 0
    line_count: int = 0
    upscaled: bool = False


@app.get("/health")
async def health():
    return {"status": "ok", "service": "ps2api-ocr", "version": __version__}


@app.post("/ocr", response_model=OCRResponse, dependencies=[Depends(require_auth)])
async def ocr(req: OCRRequest):
    # 1) 解析图片输入 -> 原始字节
    try:
        raw = load_image_bytes(req.image)
    except ImageError as exc:
        logger.info("图片解析失败: %s", exc)
        raise HTTPException(status_code=400, detail=str(exc))

    # 2) 线程池执行 CPU 密集的 OCR，避免阻塞事件循环
    try:
        result = await anyio.to_thread.run_sync(ocr_engine.recognize, raw)
    except ImageError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    except Exception as exc:
        logger.exception("OCR 识别异常")
        raise HTTPException(status_code=500, detail=str(exc))

    text = result["text"]
    if settings.max_result_chars and len(text) > settings.max_result_chars:
        text = text[: settings.max_result_chars]

    img_info = result.get("image", {})
    return OCRResponse(
        text=text,
        lines=[OCRLine(**line) for line in result["lines"]],
        lang=req.lang,
        elapse_ms=result["elapse_ms"],
        width=img_info.get("width", 0),
        height=img_info.get("height", 0),
        line_count=len(result["lines"]),
        upscaled=img_info.get("upscaled", False),
    )


# 兼容：网关的 ocr_api_base 可能直接配到根路径 "/"。让根路径也能识别。
@app.post("/", response_model=OCRResponse, dependencies=[Depends(require_auth)])
async def ocr_root(req: OCRRequest):
    return await ocr(req)


@app.exception_handler(Exception)
async def unhandled(_, exc: Exception):
    logger.exception("未处理异常")
    return JSONResponse(status_code=500, content={"error": str(exc)})
