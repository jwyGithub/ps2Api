package tlsfp

// 最小 HTTP/2 客户端：复用官方 golang.org/x/net/http2 的 Framer（帧编解码）与
// hpack（HPACK 编解码）原语，但连接建立顺序、SETTINGS 内容与顺序、连接级窗口增量、
// 伪头顺序全部自己掌控，从而注入目标客户端（当前为 Chromium/Edge）的 h2 指纹。
//
// 之所以不用官方 http2.Transport：它会以固定的库内顺序发送 SETTINGS 并自行管理连接，
// 无法把 SETTINGS/窗口增量/伪头顺序改成 Chromium 的形状——而这些正是 Cloudflare Bot
// Management 关联比对的 h2 指纹面。
//
// 能力范围（够用即可，不追求完整 RFC 7540 实现）：
//   - 单连接多路复用，读循环分发 HEADERS/DATA/RST/SETTINGS/PING/WINDOW_UPDATE/GOAWAY
//   - 发送与接收方向的流量控制（连接级 + 流级）
//   - 响应体经 io.Pipe 流式下发，天然支持 SSE
//   - 请求体流式上送并遵守对端流控窗口

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	// h2MaxFrameSize 单个 DATA 帧的最大负载，取默认 SETTINGS_MAX_FRAME_SIZE(16KiB)。
	h2MaxFrameSize = 1 << 14
	// h2InitialWindow 是 HTTP/2 协议规定的初始流控窗口默认值（连接级与流级）。
	h2InitialWindow = 65535
)

// errConnUnusable 表示连接已不可用于新请求（已关闭 / 收到 GOAWAY），
// 应由上层从连接池剔除并新建连接重试。
var errConnUnusable = errors.New("tlsfp: h2 连接不可用于新请求")

// h2Conn 封装一条已协商为 h2 的（uTLS）连接，管理其上的多路复用流。
type h2Conn struct {
	authority string
	conn      net.Conn
	fr        *http2.Framer

	// wmu 串行化所有出站帧写入以及 HPACK 编码（HPACK 编码器有连接级动态表状态，
	// 且 HEADERS 帧必须连续写出，不能与其它帧交错）。
	wmu  sync.Mutex
	henc *hpack.Encoder
	hbuf *bytes.Buffer
	fp   H2Fingerprint

	mu            sync.Mutex // 保护以下所有字段
	cond          *sync.Cond // 发送窗口变化时唤醒等待中的 writeData
	streams       map[uint32]*h2Stream
	nextID        uint32
	connSendWin   int64 // 连接级发送窗口（对端允许我方发送的字节数）
	initStreamWin int64 // 对端 SETTINGS_INITIAL_WINDOW_SIZE：新建流的初始发送窗口
	goAway        bool
	closed        bool
	closeErr      error

	settingsOnce sync.Once
	settingsCh   chan struct{} // 收到对端首个 SETTINGS 后关闭

	onClose func() // 连接失效时回调，用于从连接池剔除
}

// h2Stream 表示一个 HTTP/2 流（一次请求/响应）。
type h2Stream struct {
	id   uint32
	conn *h2Conn

	pr *io.PipeReader // 交给调用方作为 resp.Body
	pw *io.PipeWriter // 读循环把 DATA 写入此端

	sendWin int64 // 本流发送窗口

	hdrCh    chan *http2.MetaHeadersFrame // 响应头就绪信号（容量 1）
	gotHdr   bool                         // 是否已交付响应头（用于区分尾部 trailer）
	done     chan struct{}                // 流结束（END_STREAM 或 RST）时关闭
	doneOnce sync.Once
	closed   bool // 流已终结（受 h2Conn.mu 保护），用于唤醒阻塞中的 writeData
	resetErr error
}

// newH2Conn 在一条已完成 h2 ALPN 协商的连接上初始化 h2 客户端：
// 写出客户端前导 → SETTINGS（按指纹顺序）→ 连接级 WINDOW_UPDATE，然后启动读循环，
// 并等待对端首个 SETTINGS 到达（以确定新流的初始发送窗口）。
func newH2Conn(ctx context.Context, conn net.Conn, authority string, fp H2Fingerprint) (*h2Conn, error) {
	if len(fp.Settings) == 0 {
		fp = chromiumH2() // 兜底：指纹未配置时退回 Chromium 默认，避免发出空 SETTINGS
	}
	hbuf := new(bytes.Buffer)
	c := &h2Conn{
		authority:     authority,
		conn:          conn,
		fr:            http2.NewFramer(conn, conn),
		henc:          hpack.NewEncoder(hbuf),
		hbuf:          hbuf,
		fp:            fp,
		streams:       make(map[uint32]*h2Stream),
		nextID:        1,
		connSendWin:   h2InitialWindow,
		initStreamWin: h2InitialWindow,
		settingsCh:    make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)
	// 读到的响应头由 Framer 自动做 HPACK 解码并以 MetaHeadersFrame 形式返回。
	// 解码器动态表上限 = 我方 SETTINGS 通告的 HEADER_TABLE_SIZE。
	c.fr.ReadMetaHeaders = hpack.NewDecoder(headerTableSize(fp), nil)
	if maxList := maxHeaderListSize(fp); maxList > 0 {
		c.fr.MaxHeaderListSize = maxList
	}

	// 1) 客户端连接前导。
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		return nil, fmt.Errorf("tlsfp: 写 h2 前导失败: %w", err)
	}
	// 2) SETTINGS（严格按指纹给定的顺序逐条写出）。
	settings := make([]http2.Setting, 0, len(fp.Settings))
	for _, s := range fp.Settings {
		settings = append(settings, http2.Setting{ID: http2.SettingID(s.ID), Val: s.Val})
	}
	if err := c.fr.WriteSettings(settings...); err != nil {
		return nil, fmt.Errorf("tlsfp: 写 h2 SETTINGS 失败: %w", err)
	}
	// 3) 连接级 WINDOW_UPDATE（Chromium 紧跟 SETTINGS 发送 15663105）。
	if fp.ConnWindowUpdate > 0 {
		if err := c.fr.WriteWindowUpdate(0, fp.ConnWindowUpdate); err != nil {
			return nil, fmt.Errorf("tlsfp: 写 h2 连接窗口增量失败: %w", err)
		}
	}

	go c.readLoop()

	// 等待对端首个 SETTINGS：据此确定新流初始发送窗口，避免首个请求体超发。
	select {
	case <-c.settingsCh:
	case <-ctx.Done():
		c.closeWithErr(ctx.Err())
		return nil, ctx.Err()
	}
	c.mu.Lock()
	bad := c.closed
	err := c.closeErr
	c.mu.Unlock()
	if bad {
		if err == nil {
			err = errConnUnusable
		}
		return nil, err
	}
	return c, nil
}

func headerTableSize(fp H2Fingerprint) uint32 {
	if fp.HeaderTableSize > 0 {
		return fp.HeaderTableSize
	}
	for _, s := range fp.Settings {
		if s.ID == 0x1 {
			return s.Val
		}
	}
	return 4096
}

func maxHeaderListSize(fp H2Fingerprint) uint32 {
	for _, s := range fp.Settings {
		if s.ID == 0x6 {
			return s.Val
		}
	}
	return 0
}

// roundTrip 在本连接上发起一次请求并返回响应（响应体流式）。
func (c *h2Conn) roundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if c.closed || c.goAway {
		c.mu.Unlock()
		return nil, errConnUnusable
	}
	id := c.nextID
	c.nextID += 2
	st := &h2Stream{
		id:      id,
		conn:    c,
		hdrCh:   make(chan *http2.MetaHeadersFrame, 1),
		done:    make(chan struct{}),
		sendWin: c.initStreamWin,
	}
	st.pr, st.pw = io.Pipe()
	c.streams[id] = st
	c.mu.Unlock()

	hasBody := req.Body != nil && req.ContentLength != 0
	if err := c.writeHeaders(st, req, !hasBody); err != nil {
		c.removeStream(id)
		return nil, err
	}

	if hasBody {
		go c.writeRequestBody(st, req)
	}

	select {
	case <-req.Context().Done():
		c.resetStream(st, http2.ErrCodeCancel)
		return nil, req.Context().Err()
	case <-st.done:
		if st.resetErr != nil {
			return nil, st.resetErr
		}
		// 无头即结束属异常。
		return nil, errors.New("tlsfp: h2 流在返回响应头前结束")
	case mh := <-st.hdrCh:
		return c.buildResponse(req, st, mh)
	}
}

// writeHeaders 编码并写出请求头。伪头按指纹顺序在前，随后普通头（h2 要求头名小写）。
func (c *h2Conn) writeHeaders(st *h2Stream, req *http.Request, endStream bool) error {
	authority := req.Host
	if authority == "" {
		authority = req.URL.Host
	}
	path := req.URL.RequestURI()

	c.wmu.Lock()
	defer c.wmu.Unlock()

	c.hbuf.Reset()
	// 伪头：严格按指纹给定顺序写出（Chromium 为 m,a,s,p）。
	pseudo := c.fp.PseudoHeaderOrder
	if len(pseudo) == 0 {
		pseudo = []string{":method", ":authority", ":scheme", ":path"}
	}
	for _, name := range pseudo {
		var val string
		switch name {
		case ":method":
			val = req.Method
		case ":authority":
			val = authority
		case ":scheme":
			val = "https"
		case ":path":
			val = path
		default:
			continue
		}
		c.henc.WriteField(hpack.HeaderField{Name: name, Value: val})
	}
	// content-length：浏览器对带 body 的请求会显式发送，且位于普通头首位。Go 只把长度
	// 记在 req.ContentLength、并不写进 req.Header（本应由标准库 Transport 补齐），这里按需
	// 补上，与真实 Chrome fetch 一致（缺失是"非浏览器"信号）。
	if !endStream && req.ContentLength > 0 {
		c.henc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.FormatInt(req.ContentLength, 10)})
	}
	// 普通头：按 Chromium fetch 的线上顺序输出（h2 头名一律小写；跳过 h1 专用/连接级头
	// 与已单独处理的 content-length）。绝不能按字母序——字母序是任何真实浏览器都不会有的
	// 头顺序，会在 Cloudflare 的 JA4H 头顺序指纹上直接露馅。
	for _, name := range orderedHeaderNames(req.Header) {
		lower := strings.ToLower(name)
		if isConnLevelHeader(lower) || lower == "content-length" {
			continue
		}
		for _, v := range req.Header[name] {
			c.henc.WriteField(hpack.HeaderField{Name: lower, Value: v})
		}
	}

	block := c.hbuf.Bytes()
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      st.id,
		BlockFragment: block,
		EndStream:     endStream,
		EndHeaders:    true,
	}); err != nil {
		return fmt.Errorf("tlsfp: 写 h2 HEADERS 失败: %w", err)
	}
	return nil
}

// writeRequestBody 按对端流控窗口分块上送请求体，读毕以 END_STREAM 收尾。
func (c *h2Conn) writeRequestBody(st *h2Stream, req *http.Request) {
	defer req.Body.Close()
	buf := make([]byte, h2MaxFrameSize)
	for {
		n, readErr := req.Body.Read(buf)
		if n > 0 {
			if err := c.writeData(st, buf[:n]); err != nil {
				c.resetStream(st, http2.ErrCodeCancel)
				return
			}
		}
		if readErr == io.EOF {
			// 读完：补发一个空 END_STREAM DATA 帧收尾。
			c.wmu.Lock()
			err := c.fr.WriteData(st.id, true, nil)
			c.wmu.Unlock()
			if err != nil {
				c.resetStream(st, http2.ErrCodeInternal)
			}
			return
		}
		if readErr != nil {
			c.resetStream(st, http2.ErrCodeInternal)
			return
		}
	}
}

// writeData 遵守连接级与流级发送窗口，把 data 分块写出（不含 END_STREAM）。
func (c *h2Conn) writeData(st *h2Stream, data []byte) error {
	for len(data) > 0 {
		c.mu.Lock()
		for !c.closed && !st.closed && (c.connSendWin <= 0 || st.sendWin <= 0) {
			c.cond.Wait()
		}
		if c.closed || st.closed {
			c.mu.Unlock()
			if st.resetErr != nil {
				return st.resetErr
			}
			if c.closeErr != nil {
				return c.closeErr
			}
			return errConnUnusable
		}
		chunk := int64(len(data))
		if chunk > h2MaxFrameSize {
			chunk = h2MaxFrameSize
		}
		if chunk > c.connSendWin {
			chunk = c.connSendWin
		}
		if chunk > st.sendWin {
			chunk = st.sendWin
		}
		c.connSendWin -= chunk
		st.sendWin -= chunk
		c.mu.Unlock()

		part := data[:chunk]
		data = data[chunk:]
		c.wmu.Lock()
		err := c.fr.WriteData(st.id, false, part)
		c.wmu.Unlock()
		if err != nil {
			return fmt.Errorf("tlsfp: 写 h2 DATA 失败: %w", err)
		}
	}
	return nil
}

// buildResponse 由响应头 MetaHeadersFrame 组装 *http.Response，响应体为流 st。
func (c *h2Conn) buildResponse(req *http.Request, st *h2Stream, mh *http2.MetaHeadersFrame) (*http.Response, error) {
	statusStr := mh.PseudoValue("status")
	code, err := strconv.Atoi(statusStr)
	if err != nil {
		c.resetStream(st, http2.ErrCodeProtocol)
		return nil, fmt.Errorf("tlsfp: h2 响应 :status 非法 %q", statusStr)
	}
	header := make(http.Header, len(mh.RegularFields()))
	for _, hf := range mh.RegularFields() {
		header.Add(hf.Name, hf.Value)
	}
	resp := &http.Response{
		Status:     strconv.Itoa(code) + " " + http.StatusText(code),
		StatusCode: code,
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		Header:     header,
		Body:       st, // st 实现 io.ReadCloser：Read 透传 pipe，Close 负责重置/清理
		Request:    req,
	}
	if cl := header.Get("Content-Length"); cl != "" {
		if n, e := strconv.ParseInt(cl, 10, 64); e == nil {
			resp.ContentLength = n
		}
	} else {
		resp.ContentLength = -1
	}
	return resp, nil
}

// ---- 读循环与帧分发 ----

func (c *h2Conn) readLoop() {
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			c.closeWithErr(err)
			return
		}
		switch fr := f.(type) {
		case *http2.MetaHeadersFrame:
			c.handleHeaders(fr)
		case *http2.DataFrame:
			c.handleData(fr)
		case *http2.RSTStreamFrame:
			c.handleReset(fr.StreamID, fmt.Errorf("tlsfp: h2 流被对端重置 (code=%v)", fr.ErrCode))
		case *http2.SettingsFrame:
			c.handleSettings(fr)
		case *http2.PingFrame:
			if !fr.IsAck() {
				c.wmu.Lock()
				_ = c.fr.WritePing(true, fr.Data)
				c.wmu.Unlock()
			}
		case *http2.WindowUpdateFrame:
			c.handleWindowUpdate(fr)
		case *http2.GoAwayFrame:
			c.mu.Lock()
			c.goAway = true
			c.mu.Unlock()
			if c.onClose != nil {
				c.onClose() // 不再接受新请求，但存量流继续到自然结束
			}
		}
	}
}

func (c *h2Conn) handleSettings(f *http2.SettingsFrame) {
	if f.IsAck() {
		return
	}
	var newInitWin int64 = -1
	var tableSize uint32
	var hasTable bool
	_ = f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingInitialWindowSize:
			newInitWin = int64(s.Val)
		case http2.SettingHeaderTableSize:
			tableSize = s.Val
			hasTable = true
		}
		return nil
	})
	c.mu.Lock()
	if newInitWin >= 0 {
		// INITIAL_WINDOW_SIZE 变化需以差值同步作用到所有存量流的发送窗口。
		delta := newInitWin - c.initStreamWin
		c.initStreamWin = newInitWin
		for _, st := range c.streams {
			st.sendWin += delta
		}
	}
	c.mu.Unlock()
	c.cond.Broadcast()

	// 对端允许的 HPACK 动态表大小作用到我方编码器；该上限会在下次编码 header
	// 块时以「动态表大小更新」形式自动发出，无需手动写字段。
	if hasTable {
		c.wmu.Lock()
		c.henc.SetMaxDynamicTableSize(tableSize)
		c.wmu.Unlock()
	}

	// 回 SETTINGS ACK。
	c.wmu.Lock()
	_ = c.fr.WriteSettingsAck()
	c.wmu.Unlock()

	c.settingsOnce.Do(func() { close(c.settingsCh) })
}

func (c *h2Conn) handleWindowUpdate(f *http2.WindowUpdateFrame) {
	c.mu.Lock()
	if f.StreamID == 0 {
		c.connSendWin += int64(f.Increment)
	} else if st, ok := c.streams[f.StreamID]; ok {
		st.sendWin += int64(f.Increment)
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *h2Conn) handleHeaders(f *http2.MetaHeadersFrame) {
	c.mu.Lock()
	st, ok := c.streams[f.StreamID]
	c.mu.Unlock()
	if !ok {
		return
	}
	if !st.gotHdr {
		st.gotHdr = true
		select {
		case st.hdrCh <- f:
		default:
		}
		if f.StreamEnded() {
			// 无响应体：立即以 EOF 结束 body。
			st.pw.Close()
			c.finishStream(st, nil)
		}
		return
	}
	// 已交付过响应头：此为尾部 trailer，流随之结束。
	if f.StreamEnded() {
		st.pw.Close()
		c.finishStream(st, nil)
	}
}

func (c *h2Conn) handleData(f *http2.DataFrame) {
	c.mu.Lock()
	st, ok := c.streams[f.StreamID]
	c.mu.Unlock()
	if !ok {
		return
	}
	data := f.Data()
	if len(data) > 0 {
		if _, err := st.pw.Write(data); err != nil {
			// 调用方已放弃读取 body：重置该流。
			c.resetStream(st, http2.ErrCodeCancel)
			return
		}
		// 消费多少即补发多少接收窗口（连接级 + 流级），维持窗口不枯竭。
		c.wmu.Lock()
		_ = c.fr.WriteWindowUpdate(0, uint32(len(data)))
		_ = c.fr.WriteWindowUpdate(st.id, uint32(len(data)))
		c.wmu.Unlock()
	}
	if f.StreamEnded() {
		st.pw.Close()
		c.finishStream(st, nil)
	}
}

func (c *h2Conn) handleReset(streamID uint32, cause error) {
	c.mu.Lock()
	st, ok := c.streams[streamID]
	c.mu.Unlock()
	if !ok {
		return
	}
	st.resetErr = cause
	st.pw.CloseWithError(cause)
	c.finishStream(st, cause)
}

// ---- 流与连接生命周期 ----

// finishStream 标记流结束（幂等）：关闭 done、置 closed、从流表移除，并唤醒
// 可能阻塞在发送窗口上的 writeData（否则请求体上送 goroutine 会泄漏到连接关闭）。
func (c *h2Conn) finishStream(st *h2Stream, _ error) {
	st.doneOnce.Do(func() { close(st.done) })
	c.mu.Lock()
	st.closed = true
	delete(c.streams, st.id)
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *h2Conn) removeStream(id uint32) {
	c.mu.Lock()
	delete(c.streams, id)
	c.mu.Unlock()
}

// resetStream 向对端发送 RST_STREAM 并本地结束该流。
func (c *h2Conn) resetStream(st *h2Stream, code http2.ErrCode) {
	c.wmu.Lock()
	_ = c.fr.WriteRSTStream(st.id, code)
	c.wmu.Unlock()
	err := fmt.Errorf("tlsfp: 本地重置 h2 流 (code=%v)", code)
	st.pw.CloseWithError(err)
	if st.resetErr == nil {
		st.resetErr = err
	}
	c.finishStream(st, err)
}

// closeWithErr 关闭整条连接，唤醒所有等待者并让存量流以错误结束。
func (c *h2Conn) closeWithErr(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	streams := make([]*h2Stream, 0, len(c.streams))
	for _, st := range c.streams {
		streams = append(streams, st)
	}
	c.mu.Unlock()

	c.cond.Broadcast()
	for _, st := range streams {
		st.pw.CloseWithError(err)
		st.doneOnce.Do(func() { close(st.done) })
	}
	c.conn.Close()
	c.settingsOnce.Do(func() { close(c.settingsCh) }) // 唤醒仍在等待首个 SETTINGS 的初始化
	if c.onClose != nil {
		c.onClose()
	}
}

// usable 报告连接是否仍可承接新请求。
func (c *h2Conn) usable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.goAway
}

// ---- h2Stream 作为 resp.Body 的 io.ReadCloser ----

func (st *h2Stream) Read(p []byte) (int, error) {
	return st.pr.Read(p)
}

// Close 由调用方关闭响应体时触发：若流尚未自然结束，则发送 RST_STREAM 取消，
// 并释放本地资源。重复调用安全。
func (st *h2Stream) Close() error {
	select {
	case <-st.done:
		// 已结束，仅关闭读端。
		return st.pr.Close()
	default:
		st.conn.resetStream(st, http2.ErrCodeCancel)
		return st.pr.Close()
	}
}

// 确保 h2Stream 满足 io.ReadCloser。
var _ io.ReadCloser = (*h2Stream)(nil)

// chromeHeaderOrder 是 Chromium 系（Chrome / Edge / Electron）fetch/XHR 请求在
// HTTP/2 线上的普通头顺序（全小写）。
//
// 重要：Chrome DevTools 的「Headers」面板与「Copy as cURL」都把请求头按【字母序】
// 展示，并非真实线序，因此不能照抄它们的排列。此表依据 Chromium 网络栈实际发包顺序
// 整理：content-length 打头，随后是 UA-CH（sec-ch-ua*）、应用自定义头、user-agent、
// accept、origin、sec-fetch-*、referer、accept-encoding/language、priority，cookie 收尾。
// Cloudflare 的 JA4H 对头名顺序做指纹（且忽略 cookie/referer），故顺序必须贴合浏览器。
//
// 表中未出现的头会按稳定顺序追加到末尾（见 orderedHeaderNames），保证不丢头。
var chromeHeaderOrder = []string{
	"content-length",
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"newrelic",
	"traceparent",
	"tracestate",
	"content-type",
	"x-access-token",
	"x-app-version",
	"x-pstmn-req-service",
	"user-agent",
	"accept",
	"origin",
	"sec-fetch-site",
	"sec-fetch-mode",
	"sec-fetch-dest",
	"referer",
	"accept-encoding",
	"accept-language",
	"priority",
	"cookie",
}

// orderedHeaderNames 返回 req.Header 的键名，按 chromeHeaderOrder 给定的 Chromium
// 线序排列；表中未登记的头按字母序稳定追加到末尾（既贴合浏览器指纹又不丢头）。
func orderedHeaderNames(h http.Header) []string {
	// lower(头名) -> 实际 canonical key（http.Header 的键经 CanonicalMIMEHeaderKey 规范化）。
	byLower := make(map[string]string, len(h))
	for name := range h {
		byLower[strings.ToLower(name)] = name
	}
	out := make([]string, 0, len(h))
	seen := make(map[string]bool, len(h))
	for _, lower := range chromeHeaderOrder {
		if key, ok := byLower[lower]; ok {
			out = append(out, key)
			seen[key] = true
		}
	}
	rest := make([]string, 0)
	for name := range h {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// isConnLevelHeader 判断是否为 HTTP/1.1 连接级/逐跳头——这些在 h2 中被禁止携带。
func isConnLevelHeader(lower string) bool {
	switch lower {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding",
		"upgrade", "host", "te":
		// 注：Host 由 :authority 承载；TE 仅允许 "trailers"，此处一并剔除从简。
		return true
	}
	return false
}
