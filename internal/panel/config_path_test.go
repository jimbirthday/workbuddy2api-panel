package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 配置页 GET 的 path 只应显示文件名；完整宿主路径（/app/config.json 或用户
// home）不进入浏览器。同时确认可编辑的 prompt.file 不被 basename 化，否则
// 用户只改别的字段保存时会把绝对路径写坏。
func TestGetConfigHidesHostPaths(t *testing.T) {
	cfg := map[string]any{
		"auth_dir":   "/srv/workbuddy/auths",
		"state_file": "/srv/workbuddy/data/state.json",
		"prompt":     map[string]any{"file": "/srv/workbuddy/prompt.md"},
		"api_key":    "secret",
	}
	p := New(Config{
		Version: "test", APIKey: "k", ConfigPath: "/srv/workbuddy/config.json",
		LoadConfig: func() (any, error) { return cfg, nil },
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	for _, leak := range []string{`"auth_dir":"/srv/workbuddy`, `"state_file":"/srv/workbuddy`, "secret"} {
		if strings.Contains(body, leak) {
			t.Fatalf("config response leaked %q: %s", leak, body)
		}
	}
	var out struct {
		Path   string         `json:"path"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != "config.json" || out.Config["auth_dir"] != "auths" || out.Config["state_file"] != "state.json" {
		t.Fatalf("redacted config=%+v", out)
	}
	if out.Config["prompt"].(map[string]any)["file"] != "/srv/workbuddy/prompt.md" {
		t.Fatalf("editable prompt.file must stay intact: %+v", out.Config["prompt"])
	}
}

// LoadConfig 的错误文本可能带完整宿主路径（Docker bind mount 误挂成目录
// 时尤其明显）。面板只回稳定文案，原文留给服务端日志。
func TestGetConfigLoadErrorHidesHostPath(t *testing.T) {
	p := New(Config{
		Version: "test", APIKey: "k",
		LoadConfig: func() (any, error) {
			return nil, errors.New("read config: open /srv/workbuddy/config.json: permission denied")
		},
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "/srv/workbuddy") {
		t.Fatalf("host path leaked: %s", rr.Body)
	}
}

// 空配置路径不能被 filepath.Base("") 显示成 "."：这是前端“配置文件”提示，
// "." 会让用户以为服务正在读取当前目录。
func TestDisplayBaseNameKeepsEmptyPathEmpty(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if got := displayBaseName(in); got != "" {
			t.Fatalf("displayBaseName(%q)=%q want empty", in, got)
		}
	}
	if got := displayBaseName("/srv/workbuddy/config.json"); got != "config.json" {
		t.Fatalf("displayBaseName(path)=%q", got)
	}
}
