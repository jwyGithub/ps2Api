# ps2api OCR 服务

精准 OCR 识别服务，作为 ps2api 网关图片识别（vision）功能的**外部 OCR 后端**。

- 引擎：**RapidOCR + ONNXRuntime**（PaddleOCR PP-OCRv4/v5 模型），中英一体、CPU 友好、无重系统依赖。
- 框架：FastAPI + Uvicorn。
- 环境管理：**uv**。

## 与网关的契约

网关 `internal/provider/vision.go` 的 `callOCR` 按如下方式调用本服务：

- **请求**：`POST <ocr_api_base>`，JSON：
  ```json
  { "image": "<data URL / http(s) 直链 / 纯 base64>", "lang": "chi_sim+eng" }
  ```
  若网关配置了 `ocr_api_key`，则带 `Authorization: Bearer <key>`。
- **响应**：`2xx`，JSON 顶层含 `text` 字段（网关据此提取识别文本）。

> `lang` 由网关传入（默认 `chi_sim+eng`）。RapidOCR 为中英一体模型，无需按语言切换，该字段仅作记录/透传。

### 接口

| 方法 | 路径      | 说明 |
| ---- | --------- | ---- |
| GET  | `/health` | 健康检查（docker healthcheck 使用） |
| POST | `/ocr`    | OCR 识别（推荐把网关 `ocr_api_base` 指向这里） |
| POST | `/`       | 与 `/ocr` 等价（兼容 base 直接配到根路径） |

响应示例：
```json
{
  "text": "Hello OCR 2026",
  "lines": [{ "text": "Hello OCR 2026", "score": 0.9683, "box": [[11,38],[310,37],[310,79],[11,79]] }],
  "lang": "chi_sim+eng",
  "elapse_ms": 389
}
```

## 环境变量

| 变量 | 默认 | 说明 |
| ---- | ---- | ---- |
| `OCR_HOST` | `0.0.0.0` | 监听地址 |
| `OCR_PORT` | `8000` | 监听端口 |
| `OCR_API_KEY` | 空 | 可选 Bearer 鉴权。**留空 = 不校验**（内网放行）；设置后要求正确 Bearer Token |
| `OCR_MAX_IMAGE_MB` | `20` | 单张图片大小上限（MB） |
| `OCR_DOWNLOAD_TIMEOUT` | `20` | http(s) 直链下载超时（秒） |
| `OCR_ALLOW_REMOTE_FETCH` | `true` | 是否允许下载 http(s) 直链（设 `false` 仅收 data URL / base64，杜绝 SSRF） |
| `OCR_MAX_RESULT_CHARS` | `0` | 返回文本上限，`0` 不截断（网关侧也会再截断一次） |

## 本地开发

```bash
cd ocr
uv sync                      # 安装依赖（含首次拉取 RapidOCR 模型）
uv run uvicorn app.main:app --host 127.0.0.1 --port 8000

# 冒烟测试
curl http://127.0.0.1:8000/health
curl -X POST http://127.0.0.1:8000/ocr \
  -H 'Content-Type: application/json' \
  -d '{"image":"data:image/png;base64,<...>","lang":"chi_sim+eng"}'
```

## Docker / Compose 联调

本服务已接入根目录 `docker-compose.yml`，`ps2api` 通过 `depends_on: ocr (service_healthy)` 等待其就绪。

```bash
docker compose up -d --build
```

启动后在网关面板 **系统设置 → 图片识别** 配置：

1. 识别引擎（`vision_recognize_mode`）：`ocr`（仅 OCR）或 `ocr_then_vision`（OCR 优先，失败回退视觉模型）。
2. `ocr_api_base` = `http://ocr:8000/ocr`（容器间用服务名 `ocr` 互访）。
3. 若设置了 `OCR_API_KEY`，把面板的 `ocr_api_key` 填成相同值。

> 镜像构建阶段已预下载 RapidOCR 模型，首次运行无需联网、无冷启动等待。
