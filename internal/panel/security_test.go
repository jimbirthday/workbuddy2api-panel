package panel

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js?v=`) || !strings.Contains(body, `"></script>`) {
		t.Error("index.html must load versioned app.js externally (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// 静态脚本 URL 必须带内容指纹，且 gzip/plain 两个表示拿到同一版本号：
// 前者防止反代缓存旧 app.js，后者防止只替换了未压缩页面而漏掉 gzip 页面。
func TestIndexScriptVersionMatchesAppETag(t *testing.T) {
	p := newTestPanel()
	plain := httptest.NewRecorder()
	p.ServeHTTP(plain, httptest.NewRequest("GET", "/panel/", nil))
	gz := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	p.ServeHTTP(gz, req)
	if gz.Code != http.StatusOK || gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip page code=%d enc=%q", gz.Code, gz.Header().Get("Content-Encoding"))
	}
	want := "app.js?v=" + strings.Trim(appETag, `"`)
	if !strings.Contains(plain.Body.String(), want) {
		t.Fatalf("plain index missing %q", want)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Fatalf("gzip index missing %q", want)
	}
}

// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("app.js must carry a content ETag")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("unversioned app.js Cache-Control=%q want no-cache", got)
	}
}

// 页面引用的 app.js 自带内容指纹：该 URL 的内容永不变，反向代理与浏览器可以
// 长期缓存；无版本号或版本号与当前二进制不一致时仍必须回源校验。否则反代可能
// 把旧脚本配到新 DOM 上，重新触发 loadUsage 的 null.innerHTML 崩溃。
func TestVersionedAppScriptIsImmutable(t *testing.T) {
	p := newTestPanel()
	version := strings.Trim(appETag, `"`)
	cases := []struct {
		query string
		want  string
	}{
		{"?v=" + version, "public, max-age=31536000, immutable"},
		{"?v=stale-version", "no-cache"},
		{"", "no-cache"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js"+c.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: code=%d", c.query, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%q: Cache-Control=%q want %q", c.query, got, c.want)
		}
	}
	// 页面本身绝不能长期缓存：升级后必须拿到新的 app.js 版本号。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control=%q want no-cache", got)
	}
}

// 静态资源条件 GET：同版本重复访问必须 304 且不回正文；版本升级（ETag 变化）
// 必须重新下发，不能因为缓存让用户继续跑旧脚本。
func TestStaticAssetsConditionalGET(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"/panel/", "/panel/app.js"} {
		first := httptest.NewRecorder()
		p.ServeHTTP(first, httptest.NewRequest("GET", path, nil))
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: missing ETag", path)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("%s: code=%d want 304", path, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: 304 must not include body, got %d bytes", path, rec.Body.Len())
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: 304 must keep security headers", path)
		}
		// 弱比较与多候选都应命中。
		req2 := httptest.NewRequest("GET", path, nil)
		req2.Header.Set("If-None-Match", `"stale", W/`+etag)
		rec2 := httptest.NewRecorder()
		p.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("%s: weak/multi ETag mismatch code=%d", path, rec2.Code)
		}
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}
