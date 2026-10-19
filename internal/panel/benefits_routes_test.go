package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestBenefitsRoutesRequireAuthAndDispatch 钉住福利页新增的两个批量入口：
// 无鉴权 401；有鉴权时进入对应批量动作并返回 started。调度器在测试里只接
// 空池，因此动作本身不会打上游，这里验证的是路由与单飞入口都真实存在。
func TestBenefitsRoutesRequireAuthAndDispatch(t *testing.T) {
	pl := pool.New("")
	up := upstream.New()
	s := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up, Scheduler: s})

	for _, path := range []string{"/panel/api/daily_chat_all", "/panel/api/streak_all"} {
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauth code=%d want 401", path, rr.Code)
		}

		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer k")
		rr = httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"started":true`) {
			t.Fatalf("%s code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	s.WaitBatches()
}

// TestBenefitsAccountActionRef 单号动作必须只接受当前快照生成的短引用：
// 完整 UID 不能再作为路径参数；过期/伪造引用明确 400。
func TestBenefitsAccountActionRef(t *testing.T) {
	pl := pool.New("")
	a := &auth.Auth{UID: "13800138000", AccessToken: "at"}
	pl.Add(a)
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"ok","data":{}}`)), Header: http.Header{}}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	s := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p.cfg.Scheduler = s
	p.rememberBenefitRefs(pl.List())
	ref := benefitRefFor(a.UID)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/panel/api/benefits/13800138000/streak", http.StatusBadRequest},
		{"/panel/api/benefits/not-a-ref/streak", http.StatusBadRequest},
		{"/panel/api/benefits/" + ref + "/streak", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s code=%d want %d body=%s", tc.path, rr.Code, tc.want, rr.Body.String())
		}
	}
}
