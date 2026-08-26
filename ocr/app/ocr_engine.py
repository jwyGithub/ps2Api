"""RapidOCR 引擎封装：进程内单例 + 串行推理锁，暴露 recognize(raw_bytes) -> 结构化结果。"""

from __future__ import annotations

import logging
import threading
import time
from typing import Any

from .image_utils import ImageError, to_rgb_ndarray

logger = logging.getLogger("ocr.engine")

# onnxruntime 会话不保证并发安全，且 CPU 推理本身吃满核心，串行化避免相互拖慢与潜在崩溃。
_engine_lock = threading.Lock()
_engine: Any = None


def _get_engine():
    global _engine
    if _engine is None:
        with _engine_lock:
            if _engine is None:
                from rapidocr_onnxruntime import RapidOCR

                logger.info("正在初始化 RapidOCR 引擎（首次加载模型）…")
                _engine = RapidOCR()
                logger.info("RapidOCR 引擎就绪")
    return _engine


def warmup() -> None:
    """启动时预加载模型，避免首个请求承担冷启动延迟。"""
    _get_engine()


def recognize(raw: bytes) -> dict[str, Any]:
    """对单张图片做 OCR。返回 {text, lines:[{text,score,box}], elapse_ms}。"""
    engine = _get_engine()
    # 统一用 Pillow 解码成 RGB ndarray，规避 CMYK/透明通道/异常格式导致的引擎内部报错。
    img = to_rgb_ndarray(raw)

    start = time.time()
    with _engine_lock:
        try:
            result, _ = engine(img)
        except ImageError:
            raise
        except Exception as exc:  # 引擎内部异常统一上抛，由路由转 500
            raise RuntimeError(f"OCR 引擎识别失败: {exc}") from exc
    elapse_ms = int((time.time() - start) * 1000)

    lines: list[dict[str, Any]] = []
    if result:
        for item in result:
            # RapidOCR 返回 [box, text, score]
            try:
                box, text, score = item[0], item[1], item[2]
            except (IndexError, TypeError):
                continue
            if text is None or str(text).strip() == "":
                continue
            lines.append(
                {
                    "text": str(text),
                    "score": round(float(score), 4) if score is not None else None,
                    "box": box,
                }
            )

    text = "\n".join(line["text"] for line in lines)
    return {"text": text, "lines": lines, "elapse_ms": elapse_ms}
