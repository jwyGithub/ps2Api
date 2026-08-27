"""RapidOCR 引擎封装：进程内单例 + 串行推理锁，暴露 recognize(raw_bytes) -> 结构化结果。"""

from __future__ import annotations

import logging
import threading
import time
from typing import Any

from .image_utils import ImageError, decode_image

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
    # 统一用 Pillow 解码（含超小图放大、RGB->BGR），规避格式/尺寸导致的空结果与精度损失。
    img, img_info = decode_image(raw)

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

    if not lines:
        # 空结果最常见的原因是图太小/文字太小导致检测阶段一行都没框出来。
        # 记一条带尺寸的 warning，便于线上区分「图没文字」和「图太小」。
        logger.warning(
            "OCR 未检出文本（可能图片过小/无文字）: width=%s height=%s final=%sx%s upscaled=%s",
            img_info["width"],
            img_info["height"],
            img_info["final_width"],
            img_info["final_height"],
            img_info["upscaled"],
        )

    return {"text": text, "lines": lines, "elapse_ms": elapse_ms, "image": img_info}
