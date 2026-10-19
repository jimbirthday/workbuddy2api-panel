// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"

	_ "embed"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// staticETag 为嵌入资源生成强 ETag（内容哈希）：升级后内容变化即失效，无需
// 手工维护版本号；同一二进制内重复请求直接走 304，省掉整份 app.js 传输。
func staticETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

var (
	appETag = staticETag(appJS)
	// indexPage 是带 app.js 内容指纹的页面体。放在包初始化阶段计算，保证
	// 未压缩与预压缩两个表示内容一致；不能在 handler 里只替换原文却继续发送
	// 旧 indexGzip，否则 gzip 客户端仍会拿到不带版本号的脚本地址。
	indexPage = bytes.Replace(indexHTML, []byte(`<script src="app.js"></script>`),
		[]byte(`<script src="app.js?v=`+strings.Trim(appETag, `"`)+`"></script>`), 1)
	// ETag 必须对最终响应体取指纹，而不是对注入前的模板取。此前 indexETag 来自
	// indexHTML，浏览器若缓存了 ETag 相同的旧页面，服务端会回 304，客户端继续
	// 用旧 DOM 配新 app.js，直接触发 `null.innerHTML`。
	indexETag = staticETag(indexPage)
	// 预压缩一次、常驻进程：面板 JS 约 280 KB，压缩后通常 <70 KB。请求热路径
	// 只写内存字节，不做每请求压缩。压缩失败时回落原文（例如极小/不可压缩体）。
	indexGzip = gzipStatic(indexPage)
	appGzip   = gzipStatic(appJS)
)

// init 在启动期校验 app.js 版本注入确实命中：升级时若模板改写漏掉该锚点，
// 浏览器会继续复用旧脚本，与新的 DOM 组合成空引用异常。启动即失败比线上
// 错配可查。
func init() {
	if !bytes.Contains(indexPage, []byte(`app.js?v=`)) {
		panic(`panel: index.html must contain <script src="app.js"></script>`)
	}
}

// gzipStatic 预压缩嵌入资源；失败返回 nil，serveStatic 回落未压缩原文。
func gzipStatic(body []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(body); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// serveStatic 输出嵌入的静态资源并支持条件 GET。cacheControl 由调用方按
// 资源类型给出：页面必须每次回源校验，带内容指纹的脚本可以长期缓存。
func serveStatic(w http.ResponseWriter, r *http.Request, contentType, etag, cacheControl string, body, gzipped []byte) {
	h := w.Header()
	responseETag := etag
	if len(gzipped) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		responseETag = gzipETag(etag)
	}
	h.Set("ETag", responseETag)
	h.Set("Cache-Control", cacheControl)
	// 同一 ETag 对应两种编码。Vary 只在实际返回压缩体时写入（见下方），
	// 避免未压缩响应被 Vary 过宽导致中间缓存放弃存储。
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, responseETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	payload := body
	if len(gzipped) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		payload = gzipped
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	// Vary 按本次实际协商结果再收窄：Accept-Encoding: gzip 必然拿到压缩体，
	// 不应与未压缩表示共用缓存键；不支持压缩的客户端直接取原文，避免中间缓存
	// 因 Vary 过宽而放弃缓存。
	if _, compressed := h["Content-Encoding"]; compressed {
		h.Set("Vary", "Accept-Encoding")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// gzipETag 为压缩表示派生独立强 ETag（保留弱比较前缀 W/ 语义：输出始终是
// 裸引号标签，与未压缩版本不同）。fnv-1a 足以派生稳定短标签，不参与内容校验。
func gzipETag(etag string) string {
	hsh := fnv.New64a()
	_, _ = hsh.Write([]byte(etag))
	return `"` + hex.EncodeToString(hsh.Sum(nil)) + `-gzip"`
}

// acceptsGzip 判断 Accept-Encoding 是否允许 gzip。支持 q=0 拒绝语义。
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") {
				if q := strings.TrimSpace(strings.TrimPrefix(p, "q=")); q == "0" || q == "0.0" || q == "0.00" || q == "0.000" {
					return false
				}
			}
		}
		return true
	}
	return false
}

// etagMatches 按 RFC 7232 处理 If-None-Match：支持逗号分隔的多个候选与
// 弱比较前缀 W/；"*" 表示任意当前表示都匹配。
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	serveStatic(w, r, "text/html; charset=utf-8", indexETag, "no-cache", indexPage, indexGzip)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
//
// 页面引用的是 app.js?v=<内容指纹>。只有版本号与当前二进制一致的 URL 才能
// 长期缓存：升级后页面会换新 URL，反向代理/浏览器无需再猜测何时失效，也不可能
// 把旧脚本配到新 DOM 上（线上 app.js:2706 的 null.innerHTML 就是这种错配）。
// 无版本号的直连地址仍走 no-cache，避免旧页面引用裸 app.js 时被永久钉在旧内容。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	cacheControl := "no-cache"
	if v := strings.TrimSpace(r.URL.Query().Get("v")); v != "" && v == strings.Trim(appETag, `"`) {
		cacheControl = "public, max-age=31536000, immutable"
	}
	serveStatic(w, r, "text/javascript; charset=utf-8", appETag, cacheControl, appJS, appGzip)
}
