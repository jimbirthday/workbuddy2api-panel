package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestBenefitsBatchResultEndpoint 批量动作启动后必须在福利页返回可轮询的
// 最近一轮状态；空池时动作很快结束，状态应为 done 且不报错误。
func TestBenefitsBatchResultEndpoint(t *testing.T) {
	pl := pool.New("")
	up := upstream.New()
	s := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up, Scheduler: s})

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/panel/api/benefits/batches", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("batches unauth code=%d want 401", rr.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/streak_all", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer k")
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("start batch code=%d body=%s", rr.Code, rr.Body.String())
	}
	// The batch records completion asynchronously after its own scheduler done
	// signal, so poll the panel endpoint rather than reaching into the scheduler.
	deadline := time.Now().Add(2 * time.Second)
	for {
		req = httptest.NewRequest(http.MethodGet, "/panel/api/benefits/batches", nil)
		req.Header.Set("Authorization", "Bearer k")
		rr = httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("batch result code=%d body=%s", rr.Code, rr.Body.String())
		}
		var out struct {
			Runs []struct {
				Kind string `json:"kind"`
				Done bool   `json:"done"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode batch result: %v", err)
		}
		for _, run := range out.Runs {
			if run.Kind == "streak" && run.Done {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("streak batch did not finish: %s", rr.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
