package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockVisionServer 返回一个固定识别文本的 OpenAI Responses 兼容 /responses mock，并统计被调次数。
func mockVisionServer(t *testing.T, reply string, calls *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头错误: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":` + strconvQuote(reply) + `}]}]}`))
	})
	return httptest.NewServer(mux)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func testResolver(srv *httptest.Server) *MediaResolver {
	return &MediaResolver{
		client: srv.Client(),
		cache:  newMemoryVisionCache(),
	}
}

func testCfg(srv *httptest.Server) visionConfig {
	return visionConfig{
		apiBase:        srv.URL,
		apiKey:         "test-key",
		model:          "grok-4.6",
		prompt:         "描述这张图",
		maxImages:      4,
		maxImageBytes:  20 * 1024 * 1024,
		maxResultChars: 2000,
		timeout:        10 * time.Second,
	}
}

func TestTransformReplacesImageBlocks(t *testing.T) {
	var calls int32
	srv := mockVisionServer(t, "一只猫坐在键盘上", &calls)
	defer srv.Close()
	m := testResolver(srv)
	cfg := testCfg(srv)

	cases := map[string]string{
		"anthropic":  `[{"type":"text","text":"看这张图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aVZCT1J3MEtHZ28="}}]`,
		"openai":     `[{"type":"image_url","image_url":{"url":"data:image/png;base64,aVZCT1J3MEtHZ28="}},{"type":"text","text":"看这张图"}]`,
		"responses":  `[{"type":"input_image","image_url":"data:image/png;base64,aVZCT1J3MEtHZ28="}]`,
		"remote_url": `[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var v interface{}
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := m.transform(context.Background(), v, cfg)
			if err != nil {
				t.Fatalf("transform 出错: %v", err)
			}
			b, _ := json.Marshal(out)
			s := string(b)
			if !strings.Contains(s, "[图片识别内容]") || !strings.Contains(s, "一只猫坐在键盘上") {
				t.Errorf("图片块未被替换为识别文本: %s", s)
			}
			if strings.Contains(s, "\"image\"") || strings.Contains(s, "input_image") || strings.Contains(s, "image_url") {
				t.Errorf("替换后仍残留图片块: %s", s)
			}
			// 替换后不应再有不支持的媒体（document 除外，这里无 document）。
			if kind, bad := unsupportedMediaInValue(out); bad {
				t.Errorf("替换后仍被判定为不支持媒体: %s", kind)
			}
		})
	}
}

func TestRecognizeCachesByImage(t *testing.T) {
	var calls int32
	srv := mockVisionServer(t, "缓存命中测试", &calls)
	defer srv.Close()
	m := testResolver(srv)
	cfg := testCfg(srv)

	url := "data:image/png;base64,aVZCT1J3MEtHZ28="
	for i := 0; i < 3; i++ {
		if _, err := m.recognize(context.Background(), cfg, url); err != nil {
			t.Fatalf("recognize: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("相同图片应只调用一次视觉模型，实际调用 %d 次", got)
	}
}

func TestResultTruncation(t *testing.T) {
	var calls int32
	long := strings.Repeat("字", 5000)
	srv := mockVisionServer(t, long, &calls)
	defer srv.Close()
	m := testResolver(srv)
	cfg := testCfg(srv)
	cfg.maxResultChars = 100

	out, err := m.recognize(context.Background(), cfg, "data:image/png;base64,aa==")
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if runes := []rune(out); len(runes) <= 100 || len(runes) > 120 {
		t.Errorf("识别文本未按上限截断，长度=%d", len(runes))
	}
}

func TestVisionUpstreamErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	m := testResolver(srv)
	cfg := testCfg(srv)

	if _, err := m.recognize(context.Background(), cfg, "data:image/png;base64,aa=="); err == nil {
		t.Fatal("视觉模型 5xx 应返回错误，实际无错误（会导致静默丢图）")
	}
}

func TestImageSizeLimit(t *testing.T) {
	big := strings.Repeat("A", 4*1024*1024) // ~3MB 解码后
	node := map[string]interface{}{
		"type":   "image",
		"source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": big},
	}
	if _, err := imageToDataURL(node, 1*1024*1024); err == nil {
		t.Fatal("超过体积上限应返回错误")
	}
	if _, err := imageToDataURL(node, 20*1024*1024); err != nil {
		t.Fatalf("未超上限不应报错: %v", err)
	}
}

func TestCountImages(t *testing.T) {
	raw := `[{"type":"text","text":"x"},{"type":"image","source":{}},{"type":"tool_result","content":[{"type":"image_url","image_url":{"url":"u"}}]}]`
	var v interface{}
	_ = json.Unmarshal([]byte(raw), &v)
	if n := countImages(v); n != 2 {
		t.Errorf("应统计出 2 张图片（含嵌套），实际 %d", n)
	}
}
