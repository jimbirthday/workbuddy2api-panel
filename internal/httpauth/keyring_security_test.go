package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func bearerReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestVerifyAnyEmptyKeyringAllows(t *testing.T) {
	if _, ok := VerifyAny(bearerReq(""), nil); !ok {
		t.Fatal("empty keyring must preserve no-auth legacy semantics")
	}
}

func TestVerifyAnyAllDisabledRejects(t *testing.T) {
	keys := []Key{{ID: "client", Key: "client-secret", Enabled: false}}
	if _, ok := VerifyAny(bearerReq("client-secret"), keys); ok {
		t.Fatal("all-disabled keyring must reject even the correct key")
	}
	if _, ok := VerifyAny(bearerReq(""), keys); ok {
		t.Fatal("all-disabled keyring must reject anonymous requests")
	}
}

func TestVerifyAnyEnabledMatchAndDisabledNoMatch(t *testing.T) {
	keys := []Key{
		{ID: "disabled", Key: "same-secret", Enabled: false},
		{ID: "enabled", Key: "enabled-secret", Enabled: true},
	}
	if _, ok := VerifyAny(bearerReq("same-secret"), keys); ok {
		t.Fatal("disabled entry must never authenticate")
	}
	id, ok := VerifyAny(bearerReq("enabled-secret"), keys)
	if !ok || id.ID != "enabled" {
		t.Fatalf("enabled entry should match, id=%+v ok=%v", id, ok)
	}
}

func TestAllowedModelGlob(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek/chat", true},
		{"deepseek/v4", true},
		{"glm-5.3-flash", true},
		{"glm-5.3", false},
	}
	for _, tc := range cases {
		if got := AllowedModel(tc.model, []string{"deepseek/*", "*-flash"}); got != tc.want {
			t.Errorf("model %q want %v got %v", tc.model, tc.want, got)
		}
	}
	if !AllowedModel("anything", nil) {
		t.Fatal("empty patterns must allow all")
	}
}
