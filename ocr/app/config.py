"""运行时配置：全部经环境变量读取，便于 docker-compose 注入。"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _str(name: str, default: str) -> str:
    v = os.getenv(name)
    return v.strip() if v and v.strip() else default


def _int(name: str, default: int) -> int:
    try:
        return int(os.getenv(name, "").strip())
    except (TypeError, ValueError):
        return default


def _float(name: str, default: float) -> float:
    try:
        return float(os.getenv(name, "").strip())
    except (TypeError, ValueError):
        return default


@dataclass(frozen=True)
class Settings:
    # 监听地址 / 端口
    host: str = _str("OCR_HOST", "0.0.0.0")
    port: int = _int("OCR_PORT", 8000)

    # 可选 Bearer 鉴权：与网关的 ocr_api_key 对应。
    # 留空 = 不校验（内网 depends_on 场景默认放行）；设置了才要求正确的 Bearer Token。
    api_key: str = _str("OCR_API_KEY", "")

    # 图片大小 / 下载超时上限（http(s) 直链）
    max_image_bytes: int = _int("OCR_MAX_IMAGE_MB", 20) * 1024 * 1024
    download_timeout: float = _float("OCR_DOWNLOAD_TIMEOUT", 20.0)

    # 允许下载 http(s) 直链图片（关闭后仅接受 data URL / base64，杜绝 SSRF）
    allow_remote_fetch: bool = _str("OCR_ALLOW_REMOTE_FETCH", "true").lower() == "true"

    # 单张图识别的返回文本上限（0 = 不截断）；网关侧也会再截断一次。
    max_result_chars: int = _int("OCR_MAX_RESULT_CHARS", 0)

    # 超小图在送入 OCR 前放大到最短边 >= 该像素（0 = 关闭）。
    # 提升缩略图/低分辨率截图的召回：太小的图 RapidOCR 检测阶段常一行都框不出来。
    min_upscale_side: int = _int("OCR_MIN_UPSCALE_SIDE", 640)
    # 放大倍数上限，避免极小图被放到过大导致内存/耗时暴涨。
    max_upscale_factor: float = _float("OCR_MAX_UPSCALE_FACTOR", 8.0)


settings = Settings()
