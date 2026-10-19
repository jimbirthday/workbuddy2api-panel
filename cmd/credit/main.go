// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// 接口与聚合逻辑：POST codebuddy.cn/v2/billing/meter/get-user-resource，聚合所有 package 的
// Cycle* 字段，TotalDosage 作 size 下限。
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal 国际版计费域。global 账号打 CN 域会得到 401：
// www.codebuddy.cn 不认 workbuddy.ai 的 token（实测 401，workbuddy.ai 同 token 为 code=0）。
var billingBaseGlobal = "https://www.workbuddy.ai"

// billingBaseFor 按账号 realm 选择计费域。
//
// 判定口径与 internal/auth.Realm() 一致：显式 realm=global 或 domain 落在
// workbuddy.ai 家族，都按国际版处理。cmd/credit 早先对所有账号硬编码 CN 域，
// 导致 global 账号余额查询恒返回 401（面板显示的是池内缓存值，不是实时的）。
func billingBaseFor(af *authFile) string {
	if isGlobalAuth(af) {
		return billingBaseGlobal
	}
	return billingBaseCN
}

// isGlobalAuth 按 realm/domain 判 global。路径前缀必须与返回的 base 绑定同一
// 判定，不能再用 `base == billingBaseGlobal` 比较 URL：测试或私有部署把两个
// base 配成同一地址时会误判为 global，企业端点会丢掉 /v2 前缀。
func isGlobalAuth(af *authFile) bool {
	if strings.EqualFold(strings.TrimSpace(af.Auth.Realm), "global") {
		return true
	}
	d := strings.ToLower(strings.TrimSpace(af.Auth.Domain))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

type authFile struct {
	Auth struct {
		AccessToken      string `json:"accessToken"`
		AccessTokenSnake string `json:"access_token"`
		Domain           string `json:"domain"`
		Realm            string `json:"realm"`
	} `json:"auth"`
	Account struct {
		UID               string `json:"uid"`
		EnterpriseID      string `json:"enterpriseId"`
		EnterpriseIDSnake string `json:"enterprise_id"`
		Nickname          string `json:"nickname"`
	} `json:"account"`
}

// accessToken 兼容插件嵌套形（accessToken）与 cockpit/扁平导入（access_token）。
// 两种命名在 internal/auth 与面板导入里都已支持；脚本若只看驼峰，会把可用的
// snake_case 凭据误报成 no accessToken，并漏掉企业额度分流。
func (af *authFile) accessToken() string {
	if af == nil {
		return ""
	}
	return firstNonBlank(af.Auth.AccessToken, af.Auth.AccessTokenSnake)
}

// firstNonBlank 返回第一个去空白后非空的字符串。
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// jwtEnterpriseID 从 accessToken 的 JWT payload 读取企业 ID。cmd/credit 历史上
// 只读文件里的 account.enterpriseId；旧版凭据文件该字段常为空，导致企业号被
// 当成个人号去打 get-user-resource。只解码不验签：这里只用于选对额度端点，
// 鉴权仍由上游校验签名。
func jwtEnterpriseID(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims struct {
		EnterpriseID      string `json:"enterprise_id"`
		EnterpriseIDCamel string `json:"enterpriseId"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if id := strings.TrimSpace(claims.EnterpriseID); id != "" {
		return id
	}
	return strings.TrimSpace(claims.EnterpriseIDCamel)
}

// enterpriseIDFor 返回该凭据当前的企业身份：JWT 有 claim 时以 claim 为准（文件里
// 的显式值可能是旧租户），无 claim 时回退显式字段（旧版导出只带 enterpriseId）。
func enterpriseIDFor(af *authFile) string {
	if claim := jwtEnterpriseID(af.accessToken()); claim != "" {
		return claim
	}
	return firstNonBlank(af.Account.EnterpriseID, af.Account.EnterpriseIDSnake)
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type resourcePackage struct {
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// packageRemainUsed 与 billing.go:203-258 一致
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func fetchUserResource(af *authFile) (remain, used, size int64, packs int, err error) {
	// 企业版额度不在个人资源包体系内：get-user-resource 对企业号恒返回空 Accounts，
	// 会把「有额度」显示成 0。与企业网关同口径分流到 get-enterprise-user-usage。
	if enterpriseIDFor(af) != "" {
		return fetchEnterpriseResource(af)
	}
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	base := billingBaseFor(af)
	// global 域无 /v2 前缀（与 internal/upstream 的 billingMeterPaths 同口径）。
	path := "/v2/billing/meter/get-user-resource"
	if isGlobalAuth(af) {
		path = "/billing/meter/get-user-resource"
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+af.accessToken())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if af.Account.UID != "" {
		req.Header.Set("X-User-Id", af.Account.UID)
	}
	// 个人端点也不能使用文件里过期的租户值：JWT 已判为个人时应无企业头，
	// 企业号则早已在上方分流到 fetchEnterpriseResource。
	if eid := enterpriseIDFor(af); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if af.Auth.Domain != "" {
		req.Header.Set("X-Domain", af.Auth.Domain)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	for _, a := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(a)
		remain += r
		used += u
		size += s
	}
	packs = len(env.Data.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

// fetchEnterpriseResource 查询企业账号的分配额度。上游口径与
// internal/upstream.enterpriseResource 一致：credit = 本周期已用，limitNum =
// 分配给本账号的额度；因此 remain = limitNum - credit（四舍五入）。
func fetchEnterpriseResource(af *authFile) (remain, used, size int64, packs int, err error) {
	base := billingBaseFor(af)
	path := "/v2/billing/meter/get-enterprise-user-usage"
	if isGlobalAuth(af) {
		path = "/billing/meter/get-enterprise-user-usage"
	}
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+af.accessToken())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if af.Account.UID != "" {
		req.Header.Set("X-User-Id", af.Account.UID)
	}
	if eid := enterpriseIDFor(af); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if af.Auth.Domain != "" {
		req.Header.Set("X-Domain", af.Auth.Domain)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit   *float64 `json:"credit"`
			UsedNum  *float64 `json:"used_num"`
			LimitNum *int64   `json:"limitNum"`
			LimitSn  *int64   `json:"limit_num"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	limit, consumed := env.Data.LimitNum, env.Data.Credit
	if limit == nil {
		limit = env.Data.LimitSn
	}
	if consumed == nil {
		consumed = env.Data.UsedNum
	}
	if limit == nil || consumed == nil {
		return 0, 0, 0, 0, fmt.Errorf("enterprise usage fields missing")
	}
	if *limit < 0 {
		// 不限量：与 internal/upstream 同口径，用大剩余量表示，total=-1。
		return 1 << 40, 0, -1, 1, nil
	}
	used = int64(*consumed + 0.5)
	size = *limit
	remain = size - used
	if remain < 0 {
		remain = 0
	}
	if remain > size {
		remain = size
	}
	return remain, used, size, 1, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s (set WB2A_AUTH_DIR or place workbuddy-*.json)\n", authDir)
		os.Exit(1)
	}

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.accessToken() == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, packs, err := fetchUserResource(&af)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.Packages = packs
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
	} else {
		raw, _ := json.Marshal(out)
		fmt.Println(string(raw))
	}
	// 存在失败账号时以非零退出码结束，供 cron/巡检判定（此前恒 0，全失败与全成功
	// 对调用方不可区分，静默失去告警能力）。os.Exit 前无 defer 清理需求。
	if failed := len(accounts) - okCount; failed > 0 {
		os.Exit(1)
	}
}

// printPretty 人类可读日报：四行汇总，无账号明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
