package provider

// 图片输入探针：探明 Postman /chat 有没有可用的图片/附件通道。
//
// 背景：上游 Postman 背后跑的是官方模型（devModeOptions.selectedModel 形如
// CLAUDE_OPUS_48_BEDROCK，抓包里 metadata.model 是 global.anthropic.claude-opus-4-8），
// 所以官方的 content blocks 格式**有可能**被透传下去。但当前抓包对齐的出站形态里
// input.query 是**单个字符串**，没有任何图片/附件通道 —— 把 base64 内联进 query
// 只会被当作普通文本，服务端不会将其识别为图片。
//
// 第一轮已排除 input.query：把它换成 Anthropic content blocks 数组，服务端返回
// {"errorType":"INPUT_VALIDATION_ERROR","message":"Forbidden"}，即该字段被强校验为
// string。本轮转而在 query 之外找独立字段（见 imgProbeVariants）。
//
// 实验设计（单变量）：body 由产线的 buildBody 生成，每个变体**只**改一个键，其余字段
// （版本三元组、clientTools、backgroundContext、devModeOptions…）与线上完全一致。
// 手抄抓包 body 容易漏字段，会把「漏字段导致的失败」误读成「图片格式不支持」。
//
// 变体 A 是对照组（body 完全不改）。A 必须成功，否则说明凭据/出口/版本有问题，
// 其余变体的结果不可归因 —— 没有对照组的探针会给出假阴性。
//
// 判定：图片是 32x32、左半纯红右半纯蓝。提问要求模型分别说出左右两半的颜色。
// 模型同时猜对两侧的概率很低，所以「答对即证明上游真的看到了图」，而不是在敷衍。
// 结果分三类，区分它们是实验能继续推进的前提：
//   - 命中：模型答出左红右蓝 → 该字段可用。
//   - 被拒：服务端报 INPUT_VALIDATION_ERROR → 字段被识别但格式不对，**有信息**，
//     下一步只需针对该字段换内容格式。
//   - 忽略：请求通过但模型看不到图 → 字段名不存在、被静默丢弃，**无信息**。
//
// 会真实消耗额度并打到线上 Cloudflare，因此默认跳过，显式开启才运行：
//
//	IMG_PROBE=1 \
//	DATABASE_PATH=$(pwd)/data/gateway.db \
//	IMG_PROBE_MODEL=claude-opus-4-8 \
//	IMG_PROBE_PROXY=1 \
//	go test ./internal/provider -run TestImageInputProbe -v -count=1 -timeout 10m
//
// 说明：
//   - IMG_PROBE_PROXY=1 走 proxy_urls 设置的出口（与线上一致）；默认本机直连，
//     直连被 Cloudflare 拦的概率更高，A 组失败时先试代理再下结论。
//   - 模型默认 claude-opus-4-8（Bedrock 上的 Claude，原生支持图片）。换 GPT 系
//     模型探测意义不大 —— 若上游真有图片通道，Claude 这条路最可能先通。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"ps2api/internal/store"
)

// redBluePNG 生成 32x32、左半纯红右半纯蓝的 PNG。约 200 字节，base64 后仍远小于
// 任何体积阈值，不会引入「body 过大触发 WAF」这个额外变量。
func redBluePNG() []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if x < size/2 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // 纯内存编码，不会失败
	}
	return buf.Bytes()
}

const imgProbeQuestion = "This image is split into a left half and a right half. " +
	"Answer with exactly two words: the color of the left half, then the color of the right half."

// imgProbeAskCapability 不带图，直接问上游自己的附件机制。上游模型跑在 Postman 的
// agent 环境里，其系统提示可能描述了可用的输入通道；A 组回答里出现的 "no image was
// attached" 措辞说明它至少知道「附件」这个概念。零额外成本的情报收集，可能直接
// 给出字段名或上传流程，比继续盲猜快。回答可能是幻觉，只作线索不作结论。
const imgProbeAskCapability = "Can you receive images? If yes, describe exactly how a user " +
	"attaches an image in this Postman agent mode: what the request payload field is called, " +
	"or whether the file must be uploaded first. If you cannot receive images at all, say so plainly."

// imgProbeVariant 是一个探针变体。mutate 拿到产线 buildBody 生成的 body（同时给出
// body 顶层与 input 两个 map，因为候选字段分布在两层），按需改写；nil 表示不改（对照组）。
// query 非空则覆盖提问文本。
type imgProbeVariant struct {
	name    string
	explain string
	query   string
	mutate  func(body, input map[string]interface{}, imgB64 string)
}

// anthropicImageBlock 是 Anthropic 官方的 image content block。上游 selectedModel 是
// CLAUDE_OPUS_48_BEDROCK（Bedrock 上的 Claude），若 Postman 透传官方格式，这是最可能
// 被接受的形状。若某个字段名报出 INPUT_VALIDATION_ERROR（= 字段被识别但格式不对），
// 再针对该字段单独试 OpenAI 的 image_url 形状。
func anthropicImageBlock(imgB64 string) map[string]interface{} {
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "image/png",
			"data":       imgB64,
		},
	}
}

func imgProbeVariants() []imgProbeVariant {
	return []imgProbeVariant{
		{
			name:    "A_control_plain_query",
			explain: "对照组：body 完全不改。必须成功，否则其余变体的结果不可归因。",
		},
		{
			name:    "C_input_attachments",
			explain: "input.attachments —— 最通用的附件字段命名。",
			mutate: func(_, input map[string]interface{}, img string) {
				input["attachments"] = []interface{}{anthropicImageBlock(img)}
			},
		},
		{
			name:    "D_input_images",
			explain: "input.images —— 直白命名。",
			mutate: func(_, input map[string]interface{}, img string) {
				input["images"] = []interface{}{anthropicImageBlock(img)}
			},
		},
		{
			name:    "E_input_files",
			explain: "input.files —— Postman 本身有文件/文件查看器概念（backgroundContext 里有 FILE_VIEWER_FOLDER）。",
			mutate: func(_, input map[string]interface{}, img string) {
				input["files"] = []interface{}{anthropicImageBlock(img)}
			},
		},
		{
			name: "F_selectedContext_image",
			explain: "body.selectedContext —— 真实客户端用来传「用户选中的上下文」的现成字段，" +
				"抓包里恒为空数组。按 backgroundContext 的 {type,value} 结构塞图，不靠猜新 key。",
			mutate: func(body, _ map[string]interface{}, img string) {
				body["selectedContext"] = []interface{}{
					map[string]interface{}{"type": "IMAGE", "value": anthropicImageBlock(img)},
				}
			},
		},
		{
			name:    "G_ask_model_capability",
			explain: "不带图，直接问上游模型它的附件机制。情报收集，回答可能是幻觉，只作线索。",
			query:   imgProbeAskCapability,
		},
	}
}

// imgProbeResult 汇总一次探针的可观测结果。
type imgProbeResult struct {
	status    int
	bodyBytes int
	text      string // 累积的 textChunk
	events    []string
	errLine   string
}

// runImgProbe 发一次真实请求。刻意不走 p.Chat/streamInternal —— 那条链的 buildBody →
// splitMessages → ExtractText 会把图片压成 "[image attachment]" 占位符，正是要绕开的东西。
// 但复用 p.Client（uTLS 指纹，普通 client 会被 Cloudflare 拦）、buildHeaders、chatURL、
// applyCookies，保证除该变体改动的那一个键之外，一切都与产线出站一致。
func runImgProbe(t *testing.T, p *Provider, acc *store.Account, tokens *Tokens, postmanModel, imgB64 string, v imgProbeVariant) imgProbeResult {
	t.Helper()
	out := imgProbeResult{}

	// 全新会话：避免历史复用把变量带偏。
	p.ResetConversation(acc.ID)

	ask := imgProbeQuestion
	if v.query != "" {
		ask = v.query
	}
	question, _ := json.Marshal(ask)
	req := &ChatRequest{
		Model:    postmanModel,
		Messages: []ChatMessage{{Role: "user", Content: question}},
	}
	body, _ := p.buildBody(req, tokens, postmanModel, acc.ID)
	if v.mutate != nil {
		input, ok := body["input"].(map[string]interface{})
		if !ok {
			t.Fatalf("buildBody 的 input 不是 map，探针需要更新: %T", body["input"])
		}
		v.mutate(body, input, imgB64)
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化 body 失败: %v", err)
	}
	out.bodyBytes = len(bodyBytes)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(tokens), bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	httpReq.Header = p.buildHeaders(tokens)
	client := p.Client
	egress := "direct"
	if c, e, viaProxy := p.proxies.selectFor(acc.ID, 0); viaProxy {
		client, egress = c, e
	}
	p.applyCookies(acc.ID, httpReq, egress)
	t.Logf("[%s] 出站 %d 字节 → %s (出口=%s)", v.name, len(bodyBytes), httpReq.URL, egress)

	resp, err := client.Do(httpReq)
	if err != nil {
		out.errLine = "transport: " + err.Error()
		return out
	}
	defer resp.Body.Close()
	out.status = resp.StatusCode

	// 非 2xx：整体读出来，错误正文本身就是最有价值的情报（尤其是字段类型校验的报错）。
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		out.errLine = string(raw)
		return out
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var text strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var evt struct {
			EventType string          `json:"eventType"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(payload), &evt) != nil {
			// 非 JSON 行（HTML 挑战页等）原样留证。
			out.events = append(out.events, "RAW: "+truncate(line, 300))
			continue
		}
		switch evt.EventType {
		case "textChunk":
			var d struct {
				TextContent string `json:"textContent"`
			}
			_ = json.Unmarshal(evt.Data, &d)
			text.WriteString(d.TextContent)
		case "thinkingChunk":
			// 思考内容不参与判定，跳过以免淹没日志。
		case "failure", "error":
			out.errLine = truncate(payload, 600)
			out.events = append(out.events, evt.EventType+": "+out.errLine)
		default:
			out.events = append(out.events, evt.EventType+": "+truncate(string(evt.Data), 200))
		}
	}
	if err := scanner.Err(); err != nil {
		out.errLine = "scan: " + err.Error()
	}
	out.text = text.String()
	return out
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestImageInputProbe(t *testing.T) {
	if os.Getenv("IMG_PROBE") != "1" {
		t.Skip("设置 IMG_PROBE=1 运行该探针（会真实消耗额度并打到线上 Cloudflare）")
	}

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/gateway.db"
	}
	dbPath = resolveExistingDB(t, dbPath)

	model := os.Getenv("IMG_PROBE_MODEL")
	if model == "" {
		model = "claude-opus-4-8"
	}
	postmanModel, ok := ResolvePostmanModel(model)
	if !ok {
		t.Fatalf("模型无法解析: %s", model)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开 store 失败 (%s): %v", dbPath, err)
	}
	defer s.Close()
	accs, err := s.ActiveAccounts()
	if err != nil {
		t.Fatalf("读取活跃账号失败: %v", err)
	}
	if len(accs) == 0 {
		t.Fatal("没有活跃账号，无法发起真实请求")
	}
	acc := accs[0]

	p := New()
	if os.Getenv("IMG_PROBE_PROXY") == "1" {
		p.SetProxyList(func() []string {
			raw, _ := s.GetSetting("proxy_urls")
			return []string{raw}
		})
		t.Log("出口：走 proxy_urls 设置的代理（与线上一致）")
	} else {
		t.Log("出口：本机直连。若对照组 A 就失败，先用 IMG_PROBE_PROXY=1 重跑再下结论")
	}
	tokens, err := p.GetTokens(acc)
	if err != nil {
		t.Fatalf("账号 %s 凭据无效: %v", acc.Email, err)
	}

	pngBytes := redBluePNG()
	imgB64 := base64.StdEncoding.EncodeToString(pngBytes)
	t.Logf("账号=%s 模型=%s(%s)  图片=32x32 左红右蓝, PNG %d 字节 → base64 %d 字符",
		acc.Email, model, postmanModel, len(pngBytes), len(imgB64))

	results := map[string]imgProbeResult{}
	for _, v := range imgProbeVariants() {
		t.Logf("──── 变体 %s ────  %s", v.name, v.explain)
		r := runImgProbe(t, p, acc, tokens, postmanModel, imgB64, v)
		results[v.name] = r
		t.Logf("[%s] status=%d 出站=%dB", v.name, r.status, r.bodyBytes)
		if r.errLine != "" {
			t.Logf("[%s] 错误/正文: %s", v.name, truncate(r.errLine, 900))
		}
		for _, e := range r.events {
			t.Logf("[%s] 事件 %s", v.name, e)
		}
		if r.text != "" {
			t.Logf("[%s] 模型回答: %q", v.name, truncate(r.text, 700))
		}
		time.Sleep(2 * time.Second) // 轻微退避，别把结果污染成速率限制
	}

	// ── 判定 ──────────────────────────────────────────────
	t.Log("================ 图片输入探针 汇总 ================")
	control := results["A_control_plain_query"]
	if control.status != 200 || control.text == "" {
		t.Fatalf("对照组 A 未成功(status=%d, 回答=%q, err=%s)——凭据/出口/版本有问题，"+
			"其余变体的结果不可归因。先让 A 通过再看其他。",
			control.status, control.text, truncate(control.errLine, 300))
	}
	t.Log("对照组 A 正常（status=200，有回答）→ 探针链路有效，其余变体结果可归因")

	var hits, rejected, ignored []string
	for _, v := range imgProbeVariants() {
		if v.name == "A_control_plain_query" || v.name == "G_ask_model_capability" {
			continue
		}
		r := results[v.name]
		switch classifyImgProbe(r) {
		case imgProbeSawImage:
			hits = append(hits, v.name)
			t.Logf("命中  %-26s 模型答出左红右蓝：%q", v.name, truncate(r.text, 200))
		case imgProbeRejected:
			// 最有价值的一类：字段被服务端识别并校验，说明「字段存在、格式不对」。
			rejected = append(rejected, v.name)
			t.Logf("被拒  %-26s 服务端校验失败（说明该字段被识别）：%s", v.name, truncate(r.errLine, 400))
		default:
			ignored = append(ignored, v.name)
			t.Logf("忽略  %-26s 请求通过但模型没看到图（字段很可能不存在、被静默丢弃）：%q",
				v.name, truncate(r.text, 160))
		}
	}

	if ask := results["G_ask_model_capability"]; ask.text != "" {
		t.Logf("──── 上游自述的附件机制（线索，可能是幻觉）────\n%s", truncate(ask.text, 1200))
	}

	switch {
	case len(hits) > 0:
		t.Logf("结论：命中字段 %v —— 可以按该形态实现产线图片透传。", hits)
	case len(rejected) > 0:
		t.Logf("结论：没有命中，但 %v 被服务端**识别并校验**了。", rejected)
		t.Log("       下一步：只针对这些字段换内容格式重试（OpenAI image_url 形状、" +
			"纯 base64 字符串、先上传文件拿 ID 再引用），不要再扩字段名。")
	default:
		t.Logf("结论：%v 全部被静默忽略，服务端不认这些字段名。", ignored)
		t.Log("       继续猜字段名的期望收益已经很低——下一步应去 Postman 网页版手动发一张图抓包，" +
			"拿到真实字段名和上传流程；若网页版 UI 根本没有传图入口，则上游确实不支持图片输入，" +
			"网关应改为对图片请求返回明确的 400 而不是静默降级成 [image attachment]。")
	}
}

// 探针结果分类。三类之间可区分是这个实验能继续推进的前提：
// 被拒 = 字段存在但格式不对（有信息）；忽略 = 字段名不存在（无信息）。
const (
	imgProbeSawImage = "saw_image"
	imgProbeRejected = "rejected"
	imgProbeIgnored  = "ignored"
)

func classifyImgProbe(r imgProbeResult) string {
	answer := strings.ToLower(r.text)
	sawRed := strings.Contains(answer, "red") || strings.Contains(answer, "红")
	sawBlue := strings.Contains(answer, "blue") || strings.Contains(answer, "蓝")
	switch {
	case sawRed && sawBlue:
		return imgProbeSawImage
	case r.status != 200 || strings.Contains(r.errLine, "INPUT_VALIDATION_ERROR") || strings.Contains(r.errLine, "failure"):
		return imgProbeRejected
	default:
		return imgProbeIgnored
	}
}
