package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"ps2api/internal/store"
)

// ─── 图片识别（vision）桥接 ─────────────────────────────────────────
//
// 上游 Postman /chat 只接受纯文本 query，没有任何图片通道（见 content.go 顶部注释与
// imgprobe_test.go 的探针结论）。MediaResolver 在入站层把图片内容块交给一个 OpenAI 兼容
// 的外部视觉模型（默认 xAI Grok）识别成文字，再把图片块「原地」替换成文本块，之后请求就
// 是纯文本、可正常走既有链路发上游。
//
// 设计原则：
//   - 配置全部实时读 SQLite settings 表（与 router/settings.go 同风格），面板改完即时生效、
//     无需重启。vision_enabled=false 或未配置 api_key 时完全不介入，行为与改造前一致。
//   - 只处理 image；document 仍由 UnsupportedMediaContent 拒绝。
//   - 识别失败 / 图片超数量 / 超体积一律返回 error，由 HTTP 层回退 400——绝不静默丢图
//     （content.go 已论证静默丢图比拒绝更糟）。
//   - 识别结果按「图片指纹 + 模型」缓存（配置 Redis 则跨实例共享，否则进程内），同一张图在
//     多轮对话里只识别一次。

// defaultOCRAPIBase 是随 ps2api 镜像内置的 OCR 服务地址：容器内 uvicorn 监听 127.0.0.1:8000，
// 接口为 /ocr。ocr_api_base 留空时默认用它——ocr / ocr_then_vision 模式开箱即用，无需再单独配置地址。
const defaultOCRAPIBase = "http://127.0.0.1:8000/ocr"

// imageBlockTypes 是三种入站协议里的图片内容块类型（与 unsupportedMediaKinds 中 image 类对齐）。
var imageBlockTypes = map[string]bool{
	"image":       true, // Anthropic /v1/messages
	"image_url":   true, // OpenAI /v1/chat/completions
	"input_image": true, // OpenAI /v1/responses
}

// MediaResolver 承载图片识别桥接。零值不可用，必须经 NewMediaResolver 构造。
type MediaResolver struct {
	store  *store.Store
	client *http.Client
	cache  visionCache

	clientsMu sync.Mutex
	clients   map[string]*http.Client // 按出口代理 URL 缓存，复用连接池；"" = 直连
}

// NewMediaResolver 构造一个 resolver。cache 依 REDIS_URL/REDIS_ADDR 选择 Redis 或进程内实现。
func NewMediaResolver(s *store.Store) *MediaResolver {
	return &MediaResolver{
		store:  s,
		client: &http.Client{}, // 超时由每次调用的 ctx 控制
		cache:  newVisionCache(),
	}
}

type visionConfig struct {
	apiBase        string
	apiKey         string
	model          string
	prompt         string
	maxImages      int
	maxImageBytes  int64
	maxResultChars int
	timeout        time.Duration
	proxyURL       string // 出口代理，复用上游同一份 proxy_urls 设置；空表示直连

	// 识别失败时的重试次数（不含首次）。0 表示不重试；每次重试前退避 retryBackoff。
	maxRetries   int
	retryBackoff time.Duration

	// 识别引擎选择：vision（仅视觉模型，默认）| ocr（仅外部 OCR 服务）| ocr_then_vision（OCR 优先，回退视觉）。
	mode       string
	ocrAPIBase string // 外部 OCR HTTP 服务完整接口 URL；空表示未配置
	ocrAPIKey  string
	ocrLang    string
	ocrTimeout time.Duration
}

// getSetting 读单个设置项，缺失或出错时回退 def。
func (m *MediaResolver) getSetting(key, def string) string {
	v, err := m.store.GetSetting(key)
	if err != nil || strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// Enabled 报告图片识别是否可用：开关打开且已配置 api_key。
func (m *MediaResolver) Enabled() bool {
	if m == nil || m.store == nil {
		return false
	}
	if m.getSetting("vision_enabled", "false") != "true" {
		return false
	}
	// 纯 OCR 模式不需要视觉模型 Key，也不需要单独配置 OCR 地址：ps2api 镜像已内置 OCR 服务
	// （ocr_api_base 留空即用 defaultOCRAPIBase）。其余模式（含 ocr_then_vision，视觉模型作兜底）
	// 仍以已配置 vision_api_key 为启用前提。
	if m.getSetting("vision_recognize_mode", "vision") == "ocr" {
		return true
	}
	return m.getSetting("vision_api_key", "") != ""
}

func (m *MediaResolver) config() visionConfig {
	atoiDef := func(key string, def int) int {
		if n, err := strconv.Atoi(m.getSetting(key, "")); err == nil && n > 0 {
			return n
		}
		return def
	}
	cfg := visionConfig{
		apiBase:        strings.TrimRight(m.getSetting("vision_api_base", "http://125.122.23.233:9080/v1"), "/"),
		apiKey:         m.getSetting("vision_api_key", ""),
		model:          m.getSetting("vision_model", "grok-4.6"),
		prompt:         m.getSetting("vision_prompt", "请把这张图片的内容尽量完整、结构化地转述成文字，包含其中所有可读文本（OCR）、图表与关键视觉信息。只输出转述内容本身，不要加任何前后缀说明。"),
		maxImages:      atoiDef("vision_max_images", 4),
		maxResultChars: atoiDef("vision_max_result_chars", 2000),
		timeout:        time.Duration(atoiDef("vision_timeout_seconds", 60)) * time.Second,
	}
	cfg.maxImageBytes = int64(atoiDef("vision_max_image_mb", 20)) * 1024 * 1024
	cfg.proxyURL = m.proxySetting()
	// 识别失败重试：vision_max_retries 允许为 0（不重试），故不用 atoiDef（其只接受 >0）。
	if n, err := strconv.Atoi(m.getSetting("vision_max_retries", "")); err == nil && n >= 0 {
		cfg.maxRetries = n
	}
	cfg.retryBackoff = time.Duration(atoiDef("vision_retry_backoff_ms", 500)) * time.Millisecond
	cfg.mode = m.getSetting("vision_recognize_mode", "vision")
	cfg.ocrAPIBase = strings.TrimRight(m.getSetting("ocr_api_base", defaultOCRAPIBase), "/")
	cfg.ocrAPIKey = m.getSetting("ocr_api_key", "")
	cfg.ocrLang = m.getSetting("ocr_lang", "chi_sim+eng")
	cfg.ocrTimeout = time.Duration(atoiDef("ocr_timeout_seconds", 30)) * time.Second
	return cfg
}

// proxySetting 由独立开关 vision_use_proxy 决定视觉/OCR 调用是否走代理，与上游出口的
// proxy_enabled 解耦（可单独为视觉服务开关代理）：开启时从代理池 proxy_urls 中随机取一个
// 代理 URL（复用上游同一份代理配置，随机分散出口负载）；关闭或代理池为空时返回 ""（直连）。
// 视觉服务(如 api.x.ai)在部分区域网络需经代理出口，否则本机直连超时。
func (m *MediaResolver) proxySetting() string {
	if m.getSetting("vision_use_proxy", "false") != "true" {
		return ""
	}
	raw := m.getSetting("proxy_urls", "")
	// proxy_urls 可含逗号/换行分隔的多个 URL，收集全部非空项作为代理池。
	var pool []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if p := strings.TrimSpace(part); p != "" {
			pool = append(pool, p)
		}
	}
	if len(pool) == 0 {
		return ""
	}
	return pool[rand.Intn(len(pool))]
}

// httpClientFor 返回按出口代理 URL 缓存的 http.Client（复用连接池），proxyURL 为空表示直连。
// 解析失败时回退直连并告警。超时由每次调用的 ctx 控制，故 client 不设 Timeout。
func (m *MediaResolver) httpClientFor(proxyURL string) *http.Client {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	if m.clients == nil {
		m.clients = map[string]*http.Client{}
	}
	if c, ok := m.clients[proxyURL]; ok {
		return c
	}
	c := &http.Client{}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			c.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		} else {
			log.Printf("WARN: 图片识别代理 URL 解析失败，回退直连: %v", err)
		}
	}
	m.clients[proxyURL] = c
	return c
}

// ResolveMedia 扫描任意入站 JSON（一条消息的 content，或 Responses 的 input）。若含图片块，
// 逐张识别并把图片块原地替换为文本块，返回新的 JSON。无图则原样返回 changed=false。
// 任一图片识别失败 / 超数量 / 超体积返回 error（调用方回退 400）。
func (m *MediaResolver) ResolveMedia(ctx context.Context, raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		// 非 JSON（如纯字符串 content）不可能含结构化图片块，原样返回。
		return raw, false, nil
	}
	n := countImages(v)
	if n == 0 {
		return raw, false, nil
	}
	cfg := m.config()
	// 链路起点：本次入站检出的图片数量与将要走的识别引擎/上限，便于把后续每张图的
	// cache/recognize/HTTP 事件归到同一次 ResolveMedia 调用下。
	Trace(ctx, "vision.resolve.start", map[string]interface{}{
		"images": n, "mode": cfg.mode, "max_images": cfg.maxImages, "model": cfg.model,
	})
	start := time.Now()
	if n > cfg.maxImages {
		err := fmt.Errorf("图片数量 %d 超过上限 %d", n, cfg.maxImages)
		Trace(ctx, "vision.resolve.error", map[string]interface{}{
			"images": n, "max_images": cfg.maxImages, "error": err.Error(),
		})
		return raw, false, err
	}
	transformed, err := m.transform(ctx, v, cfg)
	if err != nil {
		Trace(ctx, "vision.resolve.error", map[string]interface{}{
			"images": n, "mode": cfg.mode, "error": err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return raw, false, err
	}
	out, err := json.Marshal(transformed)
	if err != nil {
		return raw, false, err
	}
	// 链路终点：全部图片识别并原地替换完成，请求已回落为纯文本可走既有上游链路。
	Trace(ctx, "vision.resolve.done", map[string]interface{}{
		"images": n, "mode": cfg.mode, "latency_ms": time.Since(start).Milliseconds(),
	})
	return out, true, nil
}

func countImages(v interface{}) int {
	switch node := v.(type) {
	case map[string]interface{}:
		if typ, _ := node["type"].(string); imageBlockTypes[typ] {
			return 1
		}
		total := 0
		for _, child := range node {
			total += countImages(child)
		}
		return total
	case []interface{}:
		total := 0
		for _, child := range node {
			total += countImages(child)
		}
		return total
	}
	return 0
}

func (m *MediaResolver) transform(ctx context.Context, v interface{}, cfg visionConfig) (interface{}, error) {
	switch node := v.(type) {
	case map[string]interface{}:
		if typ, _ := node["type"].(string); imageBlockTypes[typ] {
			url, err := imageToDataURL(node, cfg.maxImageBytes)
			if err != nil {
				// 图片块无法归一成 URL（缺 data、超体积等）：记下块类型与原因再上抛。
				Trace(ctx, "vision.image.error", map[string]interface{}{
					"block": typ, "stage": "extract", "error": err.Error(),
				})
				return nil, err
			}
			// 每张图的识别起点：块类型 + 紧凑图片标识，串起后续 cache/recognize/HTTP 事件。
			Trace(ctx, "vision.image.start", map[string]interface{}{
				"block": typ, "image": imageRef(url),
			})
			// GATEWAY_TRACE_IMG=1 时把入站图片原样落盘，便于回看这次请求收到的图片。
			saveInboundImage(ctx, url)
			text, err := m.recognize(ctx, cfg, url)
			if err != nil {
				Trace(ctx, "vision.image.error", map[string]interface{}{
					"block": typ, "stage": "recognize", "image": imageRef(url), "error": err.Error(),
				})
				return nil, err
			}
			return map[string]interface{}{"type": "text", "text": wrapRecognized(text)}, nil
		}
		out := make(map[string]interface{}, len(node))
		for k, child := range node {
			nc, err := m.transform(ctx, child, cfg)
			if err != nil {
				return nil, err
			}
			out[k] = nc
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(node))
		for i, child := range node {
			nc, err := m.transform(ctx, child, cfg)
			if err != nil {
				return nil, err
			}
			out[i] = nc
		}
		return out, nil
	}
	return v, nil
}

func wrapRecognized(text string) string {
	return "[图片识别内容]\n" + text + "\n[/图片识别内容]"
}

// imageToDataURL 把三种协议的图片块归一成视觉模型能吃的 URL（http(s) 直链或 data URL）。
//   - Anthropic：{"type":"image","source":{"type":"base64","media_type":..,"data":..}} 或 source.type=="url"
//   - OpenAI   ：{"type":"image_url","image_url":{"url":..}}
//   - Responses：{"type":"input_image","image_url":".."}（字符串）
func imageToDataURL(node map[string]interface{}, maxBytes int64) (string, error) {
	// Anthropic：source 对象
	if src, ok := node["source"].(map[string]interface{}); ok {
		styp, _ := src["type"].(string)
		switch styp {
		case "base64":
			data, _ := src["data"].(string)
			if data == "" {
				return "", fmt.Errorf("图片 source.data 为空")
			}
			if err := checkBase64Size(data, maxBytes); err != nil {
				return "", err
			}
			mediaType, _ := src["media_type"].(string)
			if mediaType == "" {
				mediaType = "image/png"
			}
			return "data:" + mediaType + ";base64," + data, nil
		case "url":
			if u, _ := src["url"].(string); u != "" {
				return u, nil
			}
			return "", fmt.Errorf("图片 source.url 为空")
		}
	}
	// OpenAI（对象）/ Responses（字符串）
	switch iu := node["image_url"].(type) {
	case map[string]interface{}:
		if u, _ := iu["url"].(string); u != "" {
			if err := checkDataURLSize(u, maxBytes); err != nil {
				return "", err
			}
			return u, nil
		}
	case string:
		if iu != "" {
			if err := checkDataURLSize(iu, maxBytes); err != nil {
				return "", err
			}
			return iu, nil
		}
	}
	return "", fmt.Errorf("无法从图片块提取内容")
}

// checkBase64Size 估算 base64 解码后字节数并校验上限。
func checkBase64Size(b64 string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	// 解码后长度 ≈ len*3/4（忽略换行/填充的细微误差，足够做上限保护）。
	size := int64(len(b64)) * 3 / 4
	if size > maxBytes {
		return fmt.Errorf("图片体积约 %d 字节超过上限 %d 字节", size, maxBytes)
	}
	return nil
}

// checkDataURLSize 仅当 URL 是 data:...;base64,... 时校验体积；http(s) 直链无法预知大小，放行。
func checkDataURLSize(u string, maxBytes int64) error {
	const marker = ";base64,"
	if !strings.HasPrefix(u, "data:") {
		return nil
	}
	if idx := strings.Index(u, marker); idx >= 0 {
		return checkBase64Size(u[idx+len(marker):], maxBytes)
	}
	return nil
}

// imageRef 生成图片的紧凑标识，供链路日志关联同一张图而不泄露完整 data URL：
// 短指纹 id + 类型（data/url）+ 估算字节数（data URL 取 base64 解码后大小，直链取字符串长度）。
func imageRef(imageURL string) map[string]interface{} {
	kind, size := "url", len(imageURL)
	if strings.HasPrefix(imageURL, "data:") {
		kind = "data"
		if idx := strings.Index(imageURL, ";base64,"); idx >= 0 {
			size = len(imageURL[idx+len(";base64,"):]) * 3 / 4
		}
	}
	return map[string]interface{}{"id": fingerprint(imageURL)[:12], "kind": kind, "bytes": size}
}

// saveInboundImage 在 GATEWAY_TRACE_IMG=1 时，把入站图片原样落盘到追踪目录，便于回看请求里到底收到了什么图。
//   - data URL：按 ";base64," 解码后依 media type 存成真正的图片文件（.png/.jpg/…）。
//   - http(s) 直链：网关本身未接收图片字节，仅落一个 .url.txt 引用文件记录链接。
//
// 文件名带 trace_id 与图片短指纹（= imageRef 的 id），与链路日志一一对应。失败仅告警，绝不影响识别主流程。
func saveInboundImage(ctx context.Context, imageURL string) {
	if !TraceImageEnabled() {
		return
	}
	imgID := fingerprint(imageURL)[:12]
	traceID, _ := ctx.Value(traceContextKey{}).(string)
	name := imgID
	if traceID != "" {
		name = traceID + "_" + imgID
	}

	// 目录：<traceBaseDir>/images[/endpoint]/<date>，与 jsonl 深追踪同根、按日期/调用方式归档。
	dir := filepath.Join(traceBaseDir(), "images")
	if ep, _ := ctx.Value(traceEndpointKey{}).(string); ep != "" {
		dir = filepath.Join(dir, filepath.Base(ep))
	}
	dir = filepath.Join(dir, time.Now().Format("2006-01-02"))

	if strings.HasPrefix(imageURL, "data:") {
		if idx := strings.Index(imageURL, ";base64,"); idx >= 0 {
			mediaType := strings.TrimPrefix(imageURL[:idx], "data:")
			b64 := strings.TrimSpace(imageURL[idx+len(";base64,"):])
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				Trace(ctx, "vision.image.save.error", map[string]interface{}{"id": imgID, "error": err.Error()})
				return
			}
			writeTraceImage(ctx, filepath.Join(dir, name+imageExt(mediaType)), data, imgID)
			return
		}
		// 非 base64 的 data URL（少见），存原文引用。
		writeTraceImage(ctx, filepath.Join(dir, name+".dataurl.txt"), []byte(imageURL), imgID)
		return
	}
	// http(s) 直链：只存链接引用。
	writeTraceImage(ctx, filepath.Join(dir, name+".url.txt"), []byte(imageURL), imgID)
}

// writeTraceImage 建目录并写文件，成功后打一条 vision.image.saved 链路事件；任何失败仅告警。
func writeTraceImage(ctx context.Context, path string, data []byte, imgID string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("WARN: 创建图片追踪目录失败: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("WARN: 写入图片追踪文件失败: %v", err)
		return
	}
	Trace(ctx, "vision.image.saved", map[string]interface{}{"id": imgID, "path": path, "bytes": len(data)})
}

// imageExt 依 media type 推断图片文件后缀，未知类型回退 .bin。
func imageExt(mediaType string) string {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.Index(mediaType, ";"); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i])
	}
	switch mediaType {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".bin"
	}
}

// recognize 调用视觉模型识别单张图，带缓存。
func (m *MediaResolver) recognize(ctx context.Context, cfg visionConfig, imageURL string) (string, error) {
	img := imageRef(imageURL)
	// 缓存键纳入 mode 与 OCR 服务/语言：切换识别引擎后不复用另一引擎的旧结果。
	cacheKey := fingerprint("vision", cfg.mode, cfg.model, cfg.ocrAPIBase, cfg.ocrLang, imageURL)
	if cached, ok := m.cache.Get(ctx, cacheKey); ok {
		Trace(ctx, "vision.cache.hit", map[string]interface{}{
			"image": img, "mode": cfg.mode, "chars": len([]rune(cached)),
		})
		return cached, nil
	}
	Trace(ctx, "vision.cache.miss", map[string]interface{}{"image": img, "mode": cfg.mode})

	start := time.Now()
	text, err := m.recognizeWithRetry(ctx, cfg, imageURL, img)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("视觉模型返回空内容")
	}
	truncated := false
	if runes := []rune(text); cfg.maxResultChars > 0 && len(runes) > cfg.maxResultChars {
		text = string(runes[:cfg.maxResultChars]) + "…（识别内容已截断）"
		truncated = true
	}
	m.cache.Set(ctx, cacheKey, text)
	Trace(ctx, "vision.recognize.done", map[string]interface{}{
		"image": img, "mode": cfg.mode, "chars": len([]rune(text)),
		"truncated": truncated, "latency_ms": time.Since(start).Milliseconds(),
	})
	return text, nil
}

// recognizeWithRetry 包裹 doRecognize：识别失败（返回 error 或空文本）时按 cfg.maxRetries 重试，
// 每次重试前退避 cfg.retryBackoff（ctx 取消则立即放弃）。全部尝试失败返回最后一次错误。
func (m *MediaResolver) recognizeWithRetry(ctx context.Context, cfg visionConfig, imageURL string, img interface{}) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= cfg.maxRetries; attempt++ {
		if attempt > 0 {
			Trace(ctx, "vision.recognize.retry", map[string]interface{}{
				"image": img, "mode": cfg.mode, "attempt": attempt,
				"max_retries": cfg.maxRetries, "last_error": lastErr.Error(),
			})
			if cfg.retryBackoff > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(cfg.retryBackoff):
				}
			}
		}
		text, err := m.doRecognize(ctx, cfg, imageURL)
		if err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("视觉模型返回空内容")
		}
		// ctx 已取消（超时/断连）时重试也不会成功，直接放弃。
		if ctx.Err() != nil {
			return "", lastErr
		}
	}
	return "", lastErr
}

// doRecognize 按 cfg.mode 选择识别引擎：
//   - "ocr"             仅外部 OCR 服务；
//   - "ocr_then_vision" OCR 优先，OCR 失败或返回空文本时回退视觉模型；
//   - 其它（默认 "vision"）视觉模型。
func (m *MediaResolver) doRecognize(ctx context.Context, cfg visionConfig, imageURL string) (string, error) {
	switch cfg.mode {
	case "ocr":
		return m.callOCR(ctx, cfg, imageURL)
	case "ocr_then_vision":
		text, err := m.callOCR(ctx, cfg, imageURL)
		if err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
		reason := "OCR 返回空文本"
		if err != nil {
			reason = err.Error()
		}
		Trace(ctx, "ocr.fallback", map[string]interface{}{"reason": reason})
		return m.callVision(ctx, cfg, imageURL)
	default:
		return m.callVision(ctx, cfg, imageURL)
	}
}

// callOCR 调外部 OCR HTTP 服务识别单张图。契约（与设置项说明一致）：
//   - 请求：POST 到 cfg.ocrAPIBase（完整 URL），JSON 体 {"image":"<data URL 或 http(s) 直链>","lang":"..."}，
//     配置了 ocrAPIKey 时带 Authorization: Bearer。
//   - 响应：2xx，从 text/result/transcription/stdout 等字段（含数组/嵌套结构）收集识别文本。
//
// 与 callVision 同构：复用出口代理、缓存（在 recognize 层）与 trace（ocr.request/response/error）。
func (m *MediaResolver) callOCR(ctx context.Context, cfg visionConfig, imageURL string) (string, error) {
	if cfg.ocrAPIBase == "" {
		return "", fmt.Errorf("未配置 OCR 服务地址(ocr_api_base)")
	}
	payload, err := json.Marshal(map[string]interface{}{"image": imageURL, "lang": cfg.ocrLang})
	if err != nil {
		return "", err
	}

	callCtx := ctx
	timeout := cfg.ocrTimeout
	if timeout <= 0 {
		timeout = cfg.timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	endpoint := cfg.ocrAPIBase
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.ocrAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.ocrAPIKey)
	}

	// 完整记录发往 OCR 服务的调用：控制台只显示 body 字节数，jsonl 文件落全量请求体。
	Trace(ctx, "ocr.request", map[string]interface{}{
		"url": endpoint, "lang": cfg.ocrLang,
		"proxy": cfg.proxyURL != "", "body": json.RawMessage(payload),
	})

	start := time.Now()
	resp, err := m.httpClientFor(cfg.proxyURL).Do(req)
	if err != nil {
		Trace(ctx, "ocr.error", map[string]interface{}{"url": endpoint, "error": err.Error()})
		return "", fmt.Errorf("调用 OCR 服务失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var logBody interface{} = string(body)
	if json.Valid(body) {
		logBody = json.RawMessage(body)
	}
	Trace(ctx, "ocr.response", map[string]interface{}{
		"url": endpoint, "status": resp.StatusCode,
		"latency_ms": time.Since(start).Milliseconds(), "body": logBody,
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OCR 服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	text := extractOCRText(body)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("OCR 服务未返回可识别文本")
	}
	return text, nil
}

// extractOCRText 从各家 OCR 服务响应里尽量鲁棒地提取识别文本：先取顶层 text/result 字符串，
// 否则递归收集所有名为 text/transcription/stdout 的字符串字段（覆盖 Paddle/Umi/Tesseract 等分行结构），按行拼接。
func extractOCRText(body []byte) string {
	var shaped struct {
		Text   string `json:"text"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &shaped); err == nil {
		if t := strings.TrimSpace(shaped.Text); t != "" {
			return t
		}
		if t := strings.TrimSpace(shaped.Result); t != "" {
			return t
		}
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	var parts []string
	collectOCRText(v, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

var ocrTextKeys = map[string]bool{"text": true, "transcription": true, "stdout": true}

func collectOCRText(v interface{}, out *[]string) {
	switch node := v.(type) {
	case map[string]interface{}:
		for key := range ocrTextKeys {
			if s, ok := node[key].(string); ok && strings.TrimSpace(s) != "" {
				*out = append(*out, s)
			}
		}
		for _, child := range node {
			collectOCRText(child, out)
		}
	case []interface{}:
		for _, child := range node {
			collectOCRText(child, out)
		}
	}
}

func (m *MediaResolver) callVision(ctx context.Context, cfg visionConfig, imageURL string) (string, error) {
	// 标准 OpenAI Responses 协议（/v1/responses）：input_image 直接吃字符串 image_url，
	// 提示走 input_text。
	reqBody := map[string]interface{}{
		"model": cfg.model,
		"input": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":      "input_image",
						"image_url": imageURL,
						"detail":    "high",
					},
					map[string]interface{}{"type": "input_text", "text": cfg.prompt},
				},
			},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	callCtx := ctx
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}
	endpoint := cfg.apiBase + "/responses"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)

	// 完整记录发往图片模型的调用：控制台只显示 body 字节数，jsonl 文件落全量请求体。
	Trace(ctx, "vision.request", map[string]interface{}{
		"url": endpoint, "model": cfg.model, "prompt": cfg.prompt,
		"proxy": cfg.proxyURL != "", "body": json.RawMessage(payload),
	})

	start := time.Now()
	resp, err := m.httpClientFor(cfg.proxyURL).Do(req)
	if err != nil {
		Trace(ctx, "vision.error", map[string]interface{}{"url": endpoint, "error": err.Error()})
		return "", fmt.Errorf("调用视觉模型失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var logBody interface{} = string(body)
	if json.Valid(body) {
		logBody = json.RawMessage(body)
	}
	Trace(ctx, "vision.response", map[string]interface{}{
		"url": endpoint, "status": resp.StatusCode,
		"latency_ms": time.Since(start).Milliseconds(), "body": logBody,
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("视觉模型返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Responses 协议返回体：优先取 SDK 便捷字段 output_text，否则从 output[].content[] 里
	// 收集 output_text 块（content 复用 ExtractText 兜底纯字符串/blocks 两种形态）。
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content json.RawMessage `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("解析视觉模型响应失败: %w", err)
	}
	if txt := strings.TrimSpace(parsed.OutputText); txt != "" {
		return txt, nil
	}
	if len(parsed.Output) == 0 {
		return "", fmt.Errorf("视觉模型响应无 output")
	}
	var sb strings.Builder
	for _, item := range parsed.Output {
		if t := ExtractText(item.Content); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String(), nil
}

// ─── 识别结果缓存 ───────────────────────────────────────────────

type visionCache interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, val string)
}

const visionCacheTTL = 24 * time.Hour

// newVisionCache：配置 Redis 则用 Redis（跨实例共享），否则进程内内存 + TTL。
func newVisionCache() visionCache {
	if rdb := dialRedis(); rdb != nil {
		return &redisVisionCache{rdb: rdb, prefix: redisKeyPrefix()}
	}
	return newMemoryVisionCache()
}

// dialRedis 复用 newConversationStore 同款配置（REDIS_URL/REDIS_ADDR），拨号成功返回 client，否则 nil。
func dialRedis() *redis.Client {
	raw := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if raw == "" {
		if addr := strings.TrimSpace(os.Getenv("REDIS_ADDR")); addr != "" {
			if strings.Contains(addr, "://") {
				raw = addr
			} else {
				raw = "redis://" + addr
			}
		}
	}
	if raw == "" {
		return nil
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		log.Printf("WARN: 图片识别缓存解析 Redis 连接串失败，回退进程内缓存: %v", err)
		return nil
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("WARN: 图片识别缓存连接 Redis 失败，回退进程内缓存: %v", err)
		_ = rdb.Close()
		return nil
	}
	return rdb
}

type redisVisionCache struct {
	rdb    *redis.Client
	prefix string
}

func (c *redisVisionCache) key(k string) string { return c.prefix + ":vision:" + k }

func (c *redisVisionCache) Get(ctx context.Context, key string) (string, bool) {
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := c.rdb.Get(opCtx, c.key(key)).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

func (c *redisVisionCache) Set(ctx context.Context, key, val string) {
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.rdb.Set(opCtx, c.key(key), val, visionCacheTTL).Err(); err != nil {
		log.Printf("WARN: 写图片识别缓存失败: %v", err)
	}
}

type memoryVisionCache struct {
	mu    sync.Mutex
	items map[string]memoryCacheItem
}

type memoryCacheItem struct {
	val    string
	expire time.Time
}

func newMemoryVisionCache() *memoryVisionCache {
	return &memoryVisionCache{items: map[string]memoryCacheItem{}}
}

func (c *memoryVisionCache) Get(_ context.Context, key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok {
		return "", false
	}
	if time.Now().After(it.expire) {
		delete(c.items, key)
		return "", false
	}
	return it.val, true
}

func (c *memoryVisionCache) Set(_ context.Context, key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = memoryCacheItem{val: val, expire: time.Now().Add(visionCacheTTL)}
}
