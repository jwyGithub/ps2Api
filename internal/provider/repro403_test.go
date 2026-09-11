package provider

// 403 最小复现实验（内容签名 vs 长度）。
//
// 目的：验证 Cloudflare 403 是被「出站 body 里的标记/脚本特征」触发（内容签名），
// 而非单纯的请求体过大（长度）。做法是构造若干 **字节数尽量相等** 的请求体，
// 只改变「标记密度」这一个变量，对每种变体重复发起真实上游请求，统计各自的
// 分类结果（成功 / 网关拦截 GatewayBlocked / 内容被拒 RequestRejected / 其他）
// 与 Cloudflare Ray ID。若「含 <script>/<template> 的变体」显著更容易被拦、
// 而「等长的纯文本变体」几乎不被拦，则证明是内容签名而非长度。
//
// 这是一个会 **真实消耗少量额度、并打到线上 Cloudflare** 的实验，因此默认跳过，
// 只有显式设置 REPRO_403=1 才运行：
//
//	REPRO_403=1 \
//	DATABASE_PATH=./data/gateway.db \
//	REPRO_403_REPS=3 \
//	REPRO_403_SIZE=4000 \
//	REPRO_403_MODEL=claude-haiku-4-5 \
//	REPRO_403_PROXY=0 \
//	go test ./internal/provider -run TestRepro403ContentSignature -v -count=1 -timeout 20m
//
// 说明：
//   - 403 与出口 IP/评分有关，从本机直连的结果不一定等同线上。设 REPRO_403_PROXY=1
//     可让实验走 proxy_urls 设置里的出口（与线上一致），更贴近真实诱因。
//   - 每个变体每次都重置会话并带唯一 nonce，保证是「全新对话、无历史复用」，
//     从而把变量隔离在「本轮 body 的内容」上。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ps2api/internal/store"
)

// resolveExistingDB 把 DATABASE_PATH 解析成一个「确实已存在」的库文件路径。
// 关键：`go test ./internal/provider` 的工作目录是包目录(internal/provider)，
// 不是仓库根目录。若直接把相对路径 ./data/gateway.db 交给 store.Open，SQLite 会
// 在包目录下**新建一个空库**（有 schema、零账号），于是 ActiveAccounts 返回空，
// 误报「没有活跃账号」。这里对相对路径逐级向上查找真实库，找不到就报错而非新建。
func resolveExistingDB(t *testing.T, dbPath string) string {
	t.Helper()
	if filepath.IsAbs(dbPath) {
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("DATABASE_PATH 指向的库不存在: %s", dbPath)
		}
		return dbPath
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	for {
		cand := filepath.Join(dir, dbPath)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("从工作目录逐级向上都找不到已存在的库文件 %q（拒绝新建空库以免误报无账号）。"+
		"请用绝对路径设置 DATABASE_PATH，例如 DATABASE_PATH=$(pwd)/data/gateway.db", dbPath)
	return ""
}

// repro403Variant 是一个实验变体：name 用于报告，build(nonce,targetBytes) 产出该轮的用户消息文本。
type repro403Variant struct {
	name  string
	build func(nonce string, targetBytes int) string
}

// padTo 用一段良性注释把 s 填充到约 targetBytes 字节（UTF-8 近似按字节），
// 保证各变体长度可比。填充内容是纯散文，不含任何标记特征。
func padTo(s string, targetBytes int) string {
	const filler = " The quick brown fox jumps over the lazy dog while nearby a calm river flows past green fields."
	var b strings.Builder
	b.WriteString(s)
	for b.Len() < targetBytes {
		b.WriteString(filler)
	}
	out := b.String()
	if len(out) > targetBytes {
		out = out[:targetBytes]
	}
	return out
}

func repro403Variants() []repro403Variant {
	return []repro403Variant{
		{
			name: "A_plain_prose", // 对照组：等长纯文本，无任何标记
			build: func(nonce string, n int) string {
				return padTo("Please summarize this note in one short sentence. nonce="+nonce+". ", n)
			},
		},
		{
			name: "B_script_tag", // 裸 <script> 标签 + JS 事件/DOM（WAF XSS 规则最敏感）
			build: func(nonce string, n int) string {
				body := "Review this snippet. nonce=" + nonce + ". " +
					"<script>document.addEventListener('keydown',function(e){if(e.key==='Enter'){ws.close();}});" +
					"var x=document.querySelector('#app');x.innerHTML='<b>hi</b>';eval('1+1');</script> "
				return padTo(body, n)
			},
		},
		{
			name: "C_vue_sfc", // Vue 单文件组件：<template>/<style>/<script setup>
			build: func(nonce string, n int) string {
				body := "Explain this component. nonce=" + nonce + ". " +
					"<template><div class=\"url-input\" @keydown=\"onKeydown\"><el-input v-model=\"url\"/></div></template>" +
					"<script setup lang=\"ts\">import {ref} from 'vue';const url=ref('');function onKeydown(e){}</script>" +
					"<style scoped>.url-input :deep(.el-input__wrapper){box-shadow:none !important;}</style> "
				return padTo(body, n)
			},
		},
		{
			name: "D_json_script_source", // 携带「可执行脚本源码」字段的 JSON（易命中注入/RCE 签名）
			build: func(nonce string, n int) string {
				body := "Validate this request config. nonce=" + nonce + ". " +
					"{\"authType\":\"bearer\",\"authConfig\":{\"token\":\"abc\"}," +
					"\"script\":{\"enabled\":true,\"source\":\"pm.test('ok',()=>{const r=pm.response.json();eval(r.code);});\"}} "
				return padTo(body, n)
			},
		},
		// ── 2026-09-11 二分变体：当天 14:15 线上 403 的出站体 0 签名仍被拦，   ──
		// 与 14 秒前放行体逐叶 diff 后唯一新增内容是一段 CatPaw2API README tool
		// result（4136B）。以下变体按嫌疑串二分定位真正的触发特征。
		{
			name: "E_curl_bearer_auth", // curl -H "Authorization: Bearer $KEY"（凭证头样式）
			build: func(nonce string, n int) string {
				body := "Check this snippet. nonce=" + nonce + ". " +
					"```bash\ncurl http://127.0.0.1:7865/v1/models -H \"Authorization: Bearer $CP2A_API_KEY\"\n" +
					"curl -X POST http://127.0.0.1:7865/v1/chat/completions \\\n" +
					"  -H \"Authorization: Bearer $CP2A_API_KEY\" -H \"Content-Type: application/json\" \\\n" +
					"  -d '{\"model\":\"glm-5.2\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n``` "
				return padTo(body, n)
			},
		},
		{
			name: "F_apikey_env", // export XXX_API_KEY=... 赋值样式
			build: func(nonce string, n int) string {
				body := "Check this snippet. nonce=" + nonce + ". " +
					"```bash\ncp config.example.json config.json\nexport CP2A_API_KEY=your-random-key\n" +
					"./bin/catpaw2api -config config.json\n``` "
				return padTo(body, n)
			},
		},
		{
			name: "G_sudo_systemctl", // sudo/systemctl/docker 命令序列（RCE 签名嫌疑）
			build: func(nonce string, n int) string {
				body := "Check this snippet. nonce=" + nonce + ". " +
					"```bash\nsudo mkdir -p /opt/catpaw2api && sudo cp -r bin config.example.json auths /opt/catpaw2api/\n" +
					"sudo cp deploy/catpaw2api.service /etc/systemd/system/\n" +
					"sudo systemctl daemon-reload && sudo systemctl enable --now catpaw2api\n" +
					"sudo journalctl -u catpaw2api -f\nmkdir -p auths data\ndocker compose up -d --build\n``` "
				return padTo(body, n)
			},
		},
		{
			name: "H_xheader_angle", // `X-Catpaw-Conversation-Id: <conversationId>`（头注入/尖括号嫌疑）
			build: func(nonce string, n int) string {
				body := "Check this snippet. nonce=" + nonce + ". " +
					"多轮对话默认按账号自动续接上下文；也可用请求头\n" +
					"`X-Catpaw-Conversation-Id: <conversationId>` 或 body 里 `conversation_id` 显式指定会话。 "
				return padTo(body, n)
			},
		},
		{
			name: "I_full_readme", // 线上 403 的 tool result 原文（4136B）逐字复刻
			build: func(nonce string, n int) string {
				return padTo("nonce="+nonce+". "+repro403FailingToolMessage, n)
			},
		},
		// ── 二分第二轮：F_apikey_env 已 2/2 确定拦截，收窄到具体子串。     ──
		{
			name: "J_export_plain", // export FOO=bar（无关键字的 env 赋值）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nexport FOO=bar\n``` ", n)
			},
		},
		{
			name: "K_apikey_noexport", // CP2A_API_KEY=xxx（无 export 前缀）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nCP2A_API_KEY=your-random-key\n``` ", n)
			},
		},
		{
			name: "L_apikey_lower", // apikey=xxx（小写无下划线）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\napikey=your-random-key\n``` ", n)
			},
		},
		{
			name: "M_secret_assign", // secret=xxx / token=xxx / password=xxx
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nsecret=s1\ntoken=t2\npassword=p3\n``` ", n)
			},
		},
		{
			name: "N_key_suffix", // 非敏感名 _KEY=（MY_KEY=）区分「_KEY=」与「API_KEY=」
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nMY_KEY=abc\n``` ", n)
			},
		},
		// ── 二分第三轮：J/K/L/M/N 全放行，F 与 K 的剩余差异 = export 前缀 + cp/catpaw2api 上下文行。 ──
		{
			name: "O_export_apikey", // export CP2A_API_KEY=xxx 单行（export + API_KEY 组合）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nexport CP2A_API_KEY=your-random-key\n``` ", n)
			},
		},
		{
			name: "P_cp_binary_ctx", // F 去掉 export 行，只留 cp 与 ./bin/catpaw2api -config 行
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\ncp config.example.json config.json\n./bin/catpaw2api -config config.json\n``` ", n)
			},
		},
		{
			name: "Q_export_mykey", // export MY_KEY=abc（export + 普通名）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\nexport MY_KEY=abc\n``` ", n)
			},
		},
		// ── 二分第四轮：P 两行（cp config / ./bin/catpaw2api -config）确定触发，逐行拆。 ──
		{
			name: "R_cp_only", // cp config.example.json config.json 单行
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\ncp config.example.json config.json\n``` ", n)
			},
		},
		{
			name: "S_bin_only", // ./bin/catpaw2api -config config.json 单行
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/catpaw2api -config config.json\n``` ", n)
			},
		},
		{
			name: "T_bin_generic", // ./bin/myapp -config config.json（普通名二进制）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/myapp -config config.json\n``` ", n)
			},
		},
		{
			name: "U_no_dotslash", // catpaw2api -config config.json（无 ./bin/ 前缀）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\ncatpaw2api -config config.json\n``` ", n)
			},
		},
		// ── 二分第五轮：S=./bin/catpaw2api 拦、T=./bin/myapp 放、U=无./bin/ 放。       ──
		// 假设：bin/ 路径 + 「cat 开头的词」被当作 cat 命令执行路径匹配。
		{
			name: "V1_cat_cmd", // ./bin/cat -config config.json（真 cat 命令名）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/cat -config config.json\n``` ", n)
			},
		},
		{
			name: "V2_cat_noflag", // ./bin/catpaw2api（无 -config，锁定是否需要 ./bin/+词）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/catpaw2api\n``` ", n)
			},
		},
		{
			name: "V3_dogpaw", // ./bin/dogpaw2api -config config.json（同形状、非 cat 开头）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/dogpaw2api -config config.json\n``` ", n)
			},
		},
		{
			name: "V4_usb_bin", // /bin/cat config.json（绝对路径 cat）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n/bin/cat config.json\n``` ", n)
			},
		},
		// ── 二分第六轮（修复定向）：确认 ZWSP 破坏有效性与特征作用范围。 ──
		{
			name: "W1_zwsp_break", // ./bin/​catpaw2api（bin/ 与 cat 之间插零宽空格）→ 预期放行
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/​catpaw2api -config config.json\n``` ", n)
			},
		},
		{
			name: "W2_dotslash_cat", // ./cat config.json（无 bin/，./ 直接接 cat）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./cat config.json\n``` ", n)
			},
		},
		{
			name: "W3_bin_ls", // ./bin/ls -la（cat 之外的命令名，探测命令表范围）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/ls -la /tmp\n``` ", n)
			},
		},
		{
			name: "W4_bin_curl", // ./bin/curl http://...（curl 放在 bin/ 路径下）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/curl http://127.0.0.1:7865/healthz\n``` ", n)
			},
		},
		// ── 二分第七轮（命令表范围）：bin/cat 确认拦截、bin/ls 放行，探测命令表边界。 ──
		{
			name: "X1_bin_sh", // ./bin/sh -c echo hi
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/sh -c echo hi\n``` ", n)
			},
		},
		{
			name: "X2_bin_rm", // ./bin/rm -rf /tmp/x
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/rm -rf /tmp/x\n``` ", n)
			},
		},
		{
			name: "X3_bin_python", // ./bin/python script.py
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/python script.py\n``` ", n)
			},
		},
		{
			name: "X5_cat_upper", // ./bin/Catpaw2api（大写 Cat，探测大小写敏感性）
			build: func(nonce string, n int) string {
				return padTo("Check this snippet. nonce="+nonce+". ```bash\n./bin/Catpaw2api -config config.json\n``` ", n)
			},
		},
	}
}

// repro403FailingToolMessage 是 2026-09-11 14:15 线上触发 403 的出站
// toolResponses[0].content.message 原文（traces/anthropic/2026-09-11/b577d7cb*）。
// 逐字保留（含中文与 shell 示例），作为二分定位的「已知会拦」基准样本。
const repro403FailingToolMessage = "===== README.md =====\n# CatPaw2API\n\n> CatPaw（美团 CatDesk）免费额度的 OpenAI 兼容代理。**无需安装/运行 CatPaw 客户端**，\n> 纯 Go 直连云端 API，多账号轮转。\n\n## 参考项目\n\n本项目是 [Sliverkiss](https://github.com/Sliverkiss) 同系列开源项目的延伸实现，架构与运维形态参考了以下仓库：\n\n- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy CN OpenAI 兼容反代（账号池 / 轮转 / 签到架构）\n- [traework2api](https://github.com/Sliverkiss/traework2api) — TRAE Work OpenAI 兼容反代（零依赖 Go 骨架）\n- [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) — QoderWork CN OpenAI 兼容反代（OAuth 授权流程）\n\n感谢原作者的开源与优秀设计。\n\n## 快速开始（Ubuntu / Linux）\n\n### 1. 编译（Linux 二进制）\n\n```bash\nmake linux        # 产物在 bin/（纯静态，无 CGO 依赖）\nmake test         # 跑单测\n```\n\n### 2. 登录账号（浏览器登录，服务器上无浏览器也行）\n\n```bash\n# 服务器（无浏览器）：打印登录链接，轮询等待\n./login.sh -print-only\n#   ① 在任意机器浏览器打开打印的链接完成登录\n#   ② 浏览器跳到打不开的 127.0.0.1 属正常，token 会自动轮询下发\n\n# 本机有浏览器：直接打开浏览器登录\n./login.sh\n\n# 凭证落盘 auths/catpaw-{uid}.json（也可手动放同名文件）\n```\n\n### 3. 配置 & 启动\n\n直接运行（systemd 见下）：\n\n```bash\ncp config.example.json config.json\nexport CP2A_API_KEY=你的随机密钥\n./bin/catpaw2api -config config.json\n```\n\n### 4. 验证 + WebUI\n\n```bash\ncurl http://127.0.0.1:7865/healthz\ncurl http://127.0.0.1:7865/v1/models -H \"Authorization: Bearer $CP2A_API_KEY\"\ncurl -X POST http://127.0.0.1:7865/v1/chat/completions \\\n  -H \"Authorization: Bearer $CP2A_API_KEY\" -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"glm-5.2\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'\n```\n\n浏览器打开 **http://127.0.0.1:7865/** 即 WebUI 控制台：\n账号余额/状态、对话测试（流式/非流式）。\n首次使用输入 `CP2A_API_KEY` 即可（只存在当前浏览器会话）。\nWebUI 是纯静态单页，无外部 CDN 依赖，可直接 iframe 嵌入网站。\n\n多轮对话默认按账号自动续接上下文；也可用请求头\n`X-Catpaw-Conversation-Id: <conversationId>` 或 body 里 `conversation_id` 显式指定会话。\n\n## 部署（systemd / Docker）\n\nsystemd（Ubuntu）：\n\n```bash\nsudo mkdir -p /opt/catpaw2api && sudo cp -r bin config.example.json auths /opt/catpaw2api/\nsudo cp deploy/catpaw2api.service /etc/systemd/system/\n# 编辑 /opt/catpaw2api/.env 写入 CP2A_API_KEY，改好 config.json\nsudo systemctl daemon-reload && sudo systemctl enable --now catpaw2api\nsudo journalctl -u catpaw2api -f\n```\n\nDocker：\n\n```bash\nexport CP2A_API_KEY=你的随机密钥\nmkdir -p auths data\ndocker compose up -d --build\n```\n\n## 配置\n\n`config.json` 全部项可用 `CP2A_*` env 覆盖（`CP2A_API_KEY` 只能走 env）：\n`CP2A_LISTEN` / `CP2A_AUTH_DIR` / `CP2A_STATE_FILE` / `CP2A_DEFAULT_MODEL`。\n详见 `.env.example`。\n\n## 目录结构\n\n```\ncmd/server/        HTTP 服务（config + main）\ncmd/login/         浏览器登录 → auths/catpaw-{uid}.json\ncmd/credit/        余额查询 + 手动申请额度\ncmd/apply/         批量自动申请额度（对应 signin）\ninternal/auth/     auth 文件读写\ninternal/upstream/ 云端客户端（网关/直连/聊天/SSE）+ 常量\ninternal/pool/     账号池（token 校验/冷却/禁用）\ninternal/scheduler/余额看门狗 + 自动申请额度\ninternal/server/   OpenAI 兼容路由\ninternal/webui/    内嵌 WebUI 控制台（/ 根路径）\ndeploy/            systemd unit 样例\ndocs/              逆向过程与接口清单\n```\n\n## 免责声明\n\n仅供学习和研究使用。使用者需遵守 CatPaw 服务条款，自行承担使用风险。\n\n## License\n\nMIT\n\n===== 目录结构 =====\ntotal 112\ndrwxrwxr-x@  21 jiangweiye  staff   672 Sep 10 18:11 .\ndrwx------@ 234 jiangweiye  staff  7488 Sep 10 18:10 ..\n-rw-rw-r--@   1 jiangweiye  staff    36 Aug 25 18:19 .dockerignore\n-rw-rw-r--@   1 jiangweiye  staff   381 Aug 25 18:19 .env.example\n-rw-rw-r--@   1 jiangweiye  staff   115 Aug 25 18:19 .gitignore\n-rw-rw-r--@   1 jiangweiye  staff   659 Aug 25 18:19 Dockerfile\n-rw-rw-r--@   1 jiangweiye  staff  1066 Aug 25 18:19 LICENSE\n-rw-rw-r--@   1 jiangweiye  staff  1333 Aug 25 18:19 Makefile\n-rw-rw-r--@   1 jiangweiye  staff  3918 Aug 25 18:19 README.md\n-rwxr-xr-x@   1 jiangweiye  staff   553 Aug 25 18:19 apply.sh\ndrwxr-xr-x    6 jiangweiye  staff   192 Sep 10 18:11 bin\ndrwxrwxr-x@   6 jiangweiye  staff   192 Aug 25 18:19 cmd\n-rw-rw-r--@   1 jiangweiye  staff   442 Aug 25 18:19 config.example.json\n-rw-r--r--@   1 jiangweiye  staff   442 Sep 10 18:11 config.json\n-rwxr-xr-x@   1 jiangweiye  staff   467 Aug 25 18:19 credit.sh\ndrwxrwxr-x@   3 jiangweiye  staff    96 Aug 25 18:19 deploy\n-rw-rw-r--@   1 jiangweiye  staff   322 Aug 25 18:19 docker-compose.yml\ndrwxrwxr-x@   3 jiangweiye  staff    96 Aug 25 18:19 docs\n-rw-rw-r--@   1 jiangweiye  staff    27 Aug 25 18:19 go.mod\ndrwxrwxr-x@   7 jiangweiye  staff   224 Aug 25 18:19 internal\n-rwxr-xr-x@   1 jiangweiye  staff  1425 Aug 25 18:19 login.sh"

// classify 把一次 Chat 结果归类成一个短标签，并抽出 Cf-Ray（若有）。
func classify(res *Result) (label, ray string) {
	switch {
	case res == nil:
		label = "nil"
	case res.Success:
		label = "success"
	case res.GatewayBlocked:
		label = "GATEWAY_BLOCKED(403)"
	case res.RequestRejected:
		label = "request_rejected"
	case res.QuotaExhausted:
		label = "quota_exhausted"
	case res.RateLimited:
		label = "rate_limited"
	case res.AuthFailed:
		label = "auth_failed"
	default:
		label = "other_error"
	}
	if res != nil && res.RejectionDetail != "" {
		for _, ln := range strings.Split(res.RejectionDetail, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "Cf-Ray:") {
				ray = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "Cf-Ray:"))
			}
		}
	}
	return
}

func TestRepro403ContentSignature(t *testing.T) {
	if os.Getenv("REPRO_403") != "1" {
		t.Skip("设置 REPRO_403=1 运行该实验（会真实消耗额度并打到线上 Cloudflare）")
	}

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/gateway.db"
	}
	dbPath = resolveExistingDB(t, dbPath)
	reps := envInt("REPRO_403_REPS", 3)
	size := envInt("REPRO_403_SIZE", 4000)
	model := os.Getenv("REPRO_403_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
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
	// REPRO_403_ACCOUNT=id或邮箱片段 指定探针账号（默认取第一个；额度耗尽时换号用）。
	if want := os.Getenv("REPRO_403_ACCOUNT"); want != "" {
		found := false
		for _, a := range accs {
			if strings.Contains(a.Email, want) || strconv.FormatInt(a.ID, 10) == want {
				acc = a
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("REPRO_403_ACCOUNT=%q 在活跃账号中未找到", want)
		}
	}

	p := New()
	if os.Getenv("REPRO_403_PROXY") == "1" {
		p.SetProxyList(func() []string {
			raw, _ := s.GetSetting("proxy_urls")
			return []string{raw}
		})
		t.Logf("出口：走 proxy_urls 设置的代理（与线上一致）")
	} else {
		t.Logf("出口：本机直连（403 与出口 IP 相关，结果不一定等同线上；REPRO_403_PROXY=1 走代理）")
	}

	if _, ok := ResolvePostmanModel(model); !ok {
		t.Fatalf("模型无法解析: %s", model)
	}

	t.Logf("账号=%s  模型=%s  目标体积≈%d 字节/条  每变体重复=%d 次", acc.Email, model, size, reps)

	type agg struct {
		counts   map[string]int
		bytesMin int
		bytesMax int
		rays     []string
	}
	results := map[string]*agg{}

	for _, v := range repro403Variants() {
		// REPRO_403_ONLY=J_,K_ 只跑名字前缀命中的变体（二分迭代时省额度）。
		if only := os.Getenv("REPRO_403_ONLY"); only != "" && !containsAnyPrefix(v.name, strings.Split(only, ",")) {
			continue
		}
		a := &agg{counts: map[string]int{}, bytesMin: 1 << 30}
		for i := 0; i < reps; i++ {
			// 全新对话：重置会话 + 唯一 nonce，隔离变量到「本轮内容」。
			p.ResetConversation(acc.ID)
			nonce := v.name + "-" + strconv.Itoa(i) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			text := v.build(nonce, size)
			content, _ := json.Marshal(text) // 正确转义成 JSON 字符串
			req := &ChatRequest{
				Model:    model,
				Messages: []ChatMessage{{Role: "user", Content: content}},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			res := p.Chat(ctx, acc, req)
			cancel()

			label, ray := classify(res)
			a.counts[label]++
			if res.RequestBytes < a.bytesMin {
				a.bytesMin = res.RequestBytes
			}
			if res.RequestBytes > a.bytesMax {
				a.bytesMax = res.RequestBytes
			}
			if ray != "" {
				a.rays = append(a.rays, ray)
			}
			t.Logf("[%s #%d] -> %-20s bytes=%d ray=%s err=%.80s",
				v.name, i+1, label, res.RequestBytes, ray, oneLine(res.Error))

			time.Sleep(1500 * time.Millisecond) // 轻微退避，避免把结果污染成纯速率限制
		}
		results[v.name] = a
	}

	// ── 汇总报告 ──────────────────────────────────────────────
	t.Log("================ 403 内容签名实验 汇总 ================")
	for _, v := range repro403Variants() {
		a := results[v.name]
		if a == nil { // REPRO_403_ONLY 过滤掉的变体没有结果
			continue
		}
		blocked := a.counts["GATEWAY_BLOCKED(403)"]
		t.Logf("变体 %-22s 出站字节≈[%d..%d]  403拦截=%d/%d  明细=%v  rays=%v",
			v.name, a.bytesMin, a.bytesMax, blocked, reps, a.counts, a.rays)
	}
	t.Log("解读：若 B/C/D（含标记/脚本）的 403 明显多于等长的 A（纯文本），")
	t.Log("即证明触发因子是【内容签名】而非【请求体长度】。")
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// containsAnyPrefix 报告 s 是否以 prefixes 中任一前缀开头。
func containsAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, strings.TrimSpace(p)) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
