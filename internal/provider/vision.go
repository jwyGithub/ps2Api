package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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
	// 已配置视觉模型 Key 才视为启用。
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
		apiBase:        strings.TrimRight(m.getSetting("vision_api_base", "http://192.168.10.103:8080/v1"), "/"),
		apiKey:         m.getSetting("vision_api_key", ""),
		model:          m.getSetting("vision_model", "grok-4.6"),
		prompt:         m.getSetting("vision_prompt", "Transcribe the contents of this image into text as completely and in as structured a way as possible, including all readable text, charts, and key visual information. Output only the transcription itself, without any prefix or suffix explanation."),
		maxImages:      atoiDef("vision_max_images", 4),
		maxResultChars: atoiDef("vision_max_result_chars", 2000),
		timeout:        time.Duration(atoiDef("vision_timeout_seconds", 60)) * time.Second,
	}
	cfg.maxImageBytes = int64(atoiDef("vision_max_image_mb", 20)) * 1024 * 1024
	cfg.proxyURL = m.proxySetting()
	return cfg
}

// proxySetting 复用上游出口的 proxy_enabled + proxy_urls 设置：开启且已配置时返回首个代理 URL，
// 否则返回 ""（直连）。视觉服务(如 api.x.ai)在部分区域网络需与上游走同一出口，否则本机直连超时。
func (m *MediaResolver) proxySetting() string {
	if m.getSetting("proxy_enabled", "false") != "true" {
		return ""
	}
	raw := m.getSetting("proxy_urls", "")
	// proxy_urls 可含逗号/换行分隔的多个 URL，取第一个非空项。
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if p := strings.TrimSpace(part); p != "" {
			return p
		}
	}
	return ""
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
	if n > cfg.maxImages {
		return raw, false, fmt.Errorf("图片数量 %d 超过上限 %d", n, cfg.maxImages)
	}
	transformed, err := m.transform(ctx, v, cfg)
	if err != nil {
		return raw, false, err
	}
	out, err := json.Marshal(transformed)
	if err != nil {
		return raw, false, err
	}
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
				return nil, err
			}
			text, err := m.recognize(ctx, cfg, url)
			if err != nil {
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

// recognize 调用视觉模型识别单张图，带缓存。
func (m *MediaResolver) recognize(ctx context.Context, cfg visionConfig, imageURL string) (string, error) {
	// 缓存键纳入模型：切换视觉模型后不复用旧结果。
	cacheKey := fingerprint("vision", cfg.model, imageURL)
	if cached, ok := m.cache.Get(ctx, cacheKey); ok {
		return cached, nil
	}
	text, err := m.callVision(ctx, cfg, imageURL)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("视觉模型返回空内容")
	}
	if runes := []rune(text); cfg.maxResultChars > 0 && len(runes) > cfg.maxResultChars {
		text = string(runes[:cfg.maxResultChars]) + "…（识别内容已截断）"
	}
	m.cache.Set(ctx, cacheKey, text)
	return text, nil
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
