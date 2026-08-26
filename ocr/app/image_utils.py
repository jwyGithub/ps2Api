"""图片输入解析：把网关传来的 image 字段（data URL / 纯 base64 / http(s) 直链）统一转成原始字节。"""

from __future__ import annotations

import base64
import binascii
import io
import re

import httpx
from PIL import Image

from .config import settings

# data:[<mediatype>][;base64],<data>
_DATA_URL_RE = re.compile(r"^data:(?P<mime>[^;,]+)?(?P<b64>;base64)?,(?P<data>.*)$", re.DOTALL)


class ImageError(ValueError):
    """图片解析失败（格式非法、超限、下载失败等），由上层转成 4xx。"""


def _decode_base64(data: str) -> bytes:
    # 去掉 URL 编码里可能残留的空白/换行
    cleaned = re.sub(r"\s+", "", data)
    try:
        return base64.b64decode(cleaned, validate=False)
    except (binascii.Error, ValueError) as exc:
        raise ImageError(f"base64 解码失败: {exc}") from exc


def _fetch_remote(url: str) -> bytes:
    if not settings.allow_remote_fetch:
        raise ImageError("服务未开启 http(s) 直链下载（OCR_ALLOW_REMOTE_FETCH=false）")
    limit = settings.max_image_bytes
    try:
        with httpx.Client(timeout=settings.download_timeout, follow_redirects=True) as client:
            with client.stream("GET", url) as resp:
                resp.raise_for_status()
                buf = io.BytesIO()
                for chunk in resp.iter_bytes():
                    buf.write(chunk)
                    if limit and buf.tell() > limit:
                        raise ImageError(f"图片超过大小上限 {limit} 字节")
                return buf.getvalue()
    except httpx.HTTPError as exc:
        raise ImageError(f"下载图片失败: {exc}") from exc


def load_image_bytes(image: str) -> bytes:
    """把 image 字段解析为原始字节。支持：
    - data URL：data:image/png;base64,....（也兼容非 base64 的百分号编码）
    - http(s) 直链：下载（受 OCR_ALLOW_REMOTE_FETCH / 大小 / 超时约束）
    - 纯 base64 字符串（无 data: 前缀）
    """
    if not image or not image.strip():
        raise ImageError("image 字段为空")
    image = image.strip()

    if image.startswith("http://") or image.startswith("https://"):
        raw = _fetch_remote(image)
    else:
        m = _DATA_URL_RE.match(image)
        if m:
            data = m.group("data") or ""
            raw = _decode_base64(data) if m.group("b64") else _decode_base64(data)
        else:
            # 兜底当作裸 base64
            raw = _decode_base64(image)

    if not raw:
        raise ImageError("解析后的图片为空")
    if settings.max_image_bytes and len(raw) > settings.max_image_bytes:
        raise ImageError(f"图片超过大小上限 {settings.max_image_bytes} 字节")
    return raw


def to_rgb_ndarray(raw: bytes):
    """用 Pillow 稳健解码为 RGB ndarray（统一 CMYK/调色板/带透明通道等图片），交给 RapidOCR。"""
    import numpy as np

    try:
        with Image.open(io.BytesIO(raw)) as im:
            im = im.convert("RGB")
            return np.asarray(im)
    except Exception as exc:  # PIL 抛出的异常类型繁杂，统一转成 ImageError
        raise ImageError(f"无法识别的图片格式: {exc}") from exc
