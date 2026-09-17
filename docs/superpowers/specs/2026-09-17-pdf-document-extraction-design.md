# PDF document 块文本提取设计

日期：2026-09-17

## 背景

Claude Code 等客户端通过 `/v1/messages` 读 PDF 时发送 Anthropic 原生 `document` 块（base64 PDF 本体），代理当前一律 400 拒绝（上游 Postman 只有纯文本 query 通道）。而 codex 走 `/v1/responses` 之所以"能读 PDF"，是客户端在本地用 pypdf 提取文本后只发文本——服务端从未支持过 PDF。

目标：服务端把入站 PDF 块的文本提取出来、替换成文本块，使 `/v1/messages` 的 PDF 读取可用。纯 Go 提取，不走视觉模型。

## 方案

### 覆盖的入站块类型
- `/v1/messages`：`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":...}}`
- `/v1/responses`：`{"type":"input_file","file_data":"data:application/pdf;base64,..."}`（及 file_id 形式继续 400）
- `/v1/chat/completions`：`{"type":"file","file":{"file_data":"data:application/pdf;base64,..."}}`

### media_type 分流
- `application/pdf` → PDF 文本提取（ledongthuc/pdf）
- `text/plain`（及 data URL 的 `text/plain`）→ base64 解码直接作为文本（PDF 还需要解析库，txt 不需要，没有理由拒绝）
- 其他 media_type（二进制文档格式）→ 维持 400 拒绝，语义不变。

### 实现
- 新文件 `internal/provider/pdf.go`：`extractDocumentText(mediaType, b64 string) (string, error)`——text/plain 直接 base64 解码；application/pdf 用 `github.com/ledongthuc/pdf`（纯 Go、无 cgo、无外部依赖）逐页提取文本。
- 扩展 `MediaResolver.transform`：遇到 PDF 类 document/input_file/file 块时提取文本，原地替换为 `{"type":"text","text":"[PDF 文档内容]\n...\n[/PDF 文档内容]"}`；沿用 visionCache（键含 fingerprint），同一 PDF 不重复提取。
- 拦截顺序不变：`ResolveMedia`（图片+PDF）→ `UnsupportedMedia*` 兜底。PDF 被提取后不再是 document 块；提取失败/非 PDF 仍被兜底拦截。
- 独立设置 `pdf_extract_enabled`（默认开）。与视觉开关无关。
- 错误处理：提取失败（加密/损坏）→ 400 带明确错误；提取结果为空（纯扫描件）→ 400 提示改用图片方式提供。不静默降级。
- 不做预算控制（用户明确要求）；超长文本由出站 `capUpstreamQuery` 的 10000 字符兜底截断。

## 测试
- `extractPdfText` 单测：程序化构造最小合法 PDF，验证文本提取。
- 端到端：`/v1/messages` 带 PDF document 块不再 400 且块替换为文本；text/plain document 同样替换为文本；其他 media_type 的 document 仍 400。

## 取舍
- `ledongthuc/pdf` 而非 pdfcpu：前者专做文本提取、API 简单；pdfcpu 偏 PDF 操作，文本提取反而绕。
- 扫描件不支持（提取不到内嵌文本），属已知边界，错误信息里指路。
