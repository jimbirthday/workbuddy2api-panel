package httpauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func newReq(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

// 多 key 命中必须返回正确身份；禁用条目不能认证；重复 key 时按配置顺序取第一个。
func TestVerifyAnyIdentityAndDisabled(t *testing.T) {
	falseVal := false
	keys := []Key{
		{ID: "first", Key: "same-key", Enabled: true},
		{ID: "second", Key: "same-key", Enabled: true},
		{ID: "off", Key: "disabled-key", Enabled: false},
		{ID: "global", Key: "global-key", Enabled: true, Realm: "global", Models: []string{"kimi-*"}},
	}
	id, ok := VerifyAny(newReq("same-key"), keys)
	if !ok || id.ID != "first" {
		t.Fatalf("identity=%+v ok=%v, want first", id, ok)
	}
	if _, ok := VerifyAny(newReq("disabled-key"), keys); ok {
		t.Fatal("disabled key must not authenticate")
	}
	id, ok = VerifyAny(newReq("global-key"), keys)
	if !ok || id.Realm != "global" || !id.AllowsModel("kimi-k3") || id.AllowsModel("glm-5.3") {
		t.Fatalf("global identity=%+v allow=%v/%v", id, id.AllowsModel("kimi-k3"), id.AllowsModel("glm-5.3"))
	}
	_ = falseVal
}

// 空 keyring = 未启用鉴权；非空但全部禁用的 keyring 必须拒绝（不能退化成匿名）。
func TestVerifyAnyEmptyVsAllDisabled(t *testing.T) {
	if _, ok := VerifyAny(newReq(""), nil); !ok {
		t.Fatal("empty keyring must be open (legacy no-auth)")
	}
	allOff := []Key{{ID: "a", Key: "secret-a", Enabled: false}}
	if _, ok := VerifyAny(newReq("secret-a"), allOff); ok {
		t.Fatal("all-disabled keyring must reject")
	}
}

func TestVerifyAnyNoLeakOnQuery(t *testing.T) {
	// 即使 token 出现在 URL query，Authorization 头仍是唯一凭据来源。
	r := httptest.NewRequest("GET", "/?api_key=secret", nil)
	r.URL.RawQuery = url.Values{"api_key": []string{"secret"}}.Encode()
	if _, ok := VerifyAny(r, []Key{{ID: "a", Key: "secret", Enabled: true}}); ok {
		t.Fatal("query token must not authenticate")
	}
}

func TestVerifyAnyConcurrent(t *testing.T) {
	keys := []Key{
		{ID: "a", Key: "secret-a", Enabled: true},
		{ID: "b", Key: "secret-b", Enabled: true, Realm: "cn"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, want := "secret-a", "a"
			if i%2 == 1 {
				tok, want = "secret-b", "b"
			}
			id, ok := VerifyAny(newReq(tok), keys)
			if !ok || id.ID != want {
				t.Errorf("i=%d got=%+v ok=%v want=%s", i, id, ok, want)
			}
		}(i)
	}
	wg.Wait()
}

func TestKeyringCopiesModels(t *testing.T) {
	models := []string{"glm-*"}
	ring := Keyring("legacy-key", []KeySpec{{ID: "a", Key: "a-key", Models: models, Enabled: true}})
	if len(ring) != 2 || ring[0].ID != DefaultLegacyKeyID || ring[1].ID != "a" {
		t.Fatalf("ring=%+v", ring)
	}
	models[0] = "mutated"
	if ring[1].Models[0] != "glm-*" {
		t.Fatal("Keyring must copy models")
	}
}
