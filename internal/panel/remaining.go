package panel

import (
	"net/http"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// remainingUsageModel 是「账号 × 模型」的当日用量行。Used 用指针是为了区分
// 「今日确实是 0」与「用量记录器不可用/无快照」：后者必须是 null，不能伪装成
// 0 让面板显示成额度充足。
type remainingUsageModel struct {
	Model        string `json:"model"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Realm        string `json:"realm,omitempty"`
	Used         *int64 `json:"used"`
	Limit        int64  `json:"limit"`
	LimitEnabled bool   `json:"limit_enabled"`
	Remaining    *int64 `json:"remaining"`
	Limited      bool   `json:"limited"`
	UpdatedAt    string `json:"updated_at"`
}

// remainingUsageAccount 是账号级汇总。Models 只包含今日实际出现过的模型；
// 没有调用记录的模型不凭空补 0 行，避免把「未观测」误读成「可用」。
type remainingUsageAccount struct {
	UID          string                `json:"uid"`
	Nickname     string                `json:"nickname,omitempty"`
	Realm        string                `json:"realm,omitempty"`
	Used         *int64                `json:"used"`
	Limit        int64                 `json:"limit"`
	LimitEnabled bool                  `json:"limit_enabled"`
	Remaining    *int64                `json:"remaining"`
	Limited      bool                  `json:"limited"`
	ModelLimited bool                  `json:"model_limited"`
	UpdatedAt    string                `json:"updated_at"`
	Models       []remainingUsageModel `json:"models"`
}

// usageRemaining 返回「本地自然日零点起」的账号/模型 Token 额度视图。
//
// 数据源刻意复用 usage.Recorder.DailyTokenUsageAt：它已经是选号护栏使用的
// 增量快照（有版本缓存），这里只做一次 O(账号数) 投影，不重新扫用量分片。
func (p *Panel) usageRemaining(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.remainingUsageSnapshot(time.Now()))
}

// remainingUsageSnapshot 是 usageRemaining 的可测试主体。now 由调用方传入，
// 测试可以固定到任意本地日期验证「零点重置」；生产始终使用 time.Now()。
func (p *Panel) remainingUsageSnapshot(now time.Time) map[string]any {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// 470 账号 × 多模型时每行都调用 Format 会产生上千次重复字符串分配；
	// 同一份快照的时间必须完全一致，循环外只格式化一次。
	updatedAt := now.Format(time.RFC3339)
	out := map[string]any{
		"ok":             true,
		"day":            now.Format("2006-01-02"),
		"window_from":    start.Format(time.RFC3339),
		"window_to":      now.Format(time.RFC3339),
		"updated_at":     updatedAt,
		"timezone":       now.Location().String(),
		"data_available": false,
		"reason":         "",
		"limits": map[string]any{
			"account":         int64(0),
			"model":           int64(0),
			"account_enabled": false,
			"model_enabled":   false,
		},
		"accounts": []remainingUsageAccount{},
		"models":   []remainingUsageModel{},
		"summary": map[string]any{
			"accounts":         0,
			"limited_accounts": 0,
			"model_rows":       0,
			"limited_models":   0,
		},
	}

	if p.cfg.Pool == nil {
		out["reason"] = "account pool not available"
		return out
	}

	dailyLimit, modelLimit := p.cfg.Pool.DailyLimits()
	out["limits"] = map[string]any{
		"account":         dailyLimit,
		"model":           modelLimit,
		"account_enabled": dailyLimit > 0,
		"model_enabled":   modelLimit > 0,
	}

	var daily pool.DailyTokenUsageByUID
	dataAvailable := false
	if p.cfg.Usage != nil {
		snapshot, ok := p.cfg.Usage.DailyTokenUsageAt(now)
		if ok {
			// DailyTokenUsageAt 对活跃记录器即使无任何桶也返回 ok=true（空快照）。
			// 全新进程尚未 load/写入过用量时，不能把“空”当成“今日 0、额度充足”；
			// 只有已经承载过桶（历史数据或本次运行）才认为剩余额度可信。
			if p.cfg.Usage.HasRecordedUsage() {
				daily = snapshot
				dataAvailable = true
			}
		}
	}
	out["data_available"] = dataAvailable
	if !dataAvailable {
		out["reason"] = "usage snapshot unavailable"
	}

	rows := p.cfg.Pool.UsageRows()
	accounts := make([]remainingUsageAccount, 0, len(rows))
	models := make([]remainingUsageModel, 0)
	limitedAccounts, limitedModels := 0, 0

	for _, st := range rows {
		acct := remainingUsageAccount{
			UID:          st.UID,
			Nickname:     st.Nickname,
			Realm:        st.Realm,
			Limit:        dailyLimit,
			LimitEnabled: dailyLimit > 0,
			UpdatedAt:    updatedAt,
			Models:       []remainingUsageModel{},
		}
		var used int64
		u, hasUsage := daily[st.UID]
		if dataAvailable {
			if hasUsage {
				used = u.Tokens
			}
			usedCopy := used
			acct.Used = &usedCopy
			acct.Remaining = remainingAfter(used, dailyLimit)
			acct.Limited = dailyLimit > 0 && used >= dailyLimit
		}
		if acct.Limited {
			limitedAccounts++
		}

		modelNames := make([]string, 0, len(u.Models))
		for model := range u.Models {
			if model != "" {
				modelNames = append(modelNames, model)
			}
		}
		sort.Strings(modelNames)
		for _, model := range modelNames {
			modelUsed := u.Models[model]
			row := remainingUsageModel{
				Model:        model,
				UID:          st.UID,
				Nickname:     st.Nickname,
				Realm:        st.Realm,
				Limit:        modelLimit,
				LimitEnabled: modelLimit > 0,
				UpdatedAt:    updatedAt,
			}
			if dataAvailable {
				usedCopy := modelUsed
				row.Used = &usedCopy
				row.Remaining = remainingAfter(modelUsed, modelLimit)
				row.Limited = modelLimit > 0 && modelUsed >= modelLimit
			}
			if row.Limited {
				acct.ModelLimited = true
				limitedModels++
			}
			acct.Models = append(acct.Models, row)
			models = append(models, row)
		}
		accounts = append(accounts, acct)
	}

	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Model != models[j].Model {
			return models[i].Model < models[j].Model
		}
		if models[i].Realm != models[j].Realm {
			return models[i].Realm < models[j].Realm
		}
		if models[i].Nickname != models[j].Nickname {
			return models[i].Nickname < models[j].Nickname
		}
		return models[i].UID < models[j].UID
	})

	out["accounts"] = accounts
	out["models"] = models
	out["summary"] = map[string]any{
		"accounts":         len(accounts),
		"limited_accounts": limitedAccounts,
		"model_rows":       len(models),
		"limited_models":   limitedModels,
	}
	return out
}

// remainingAfter 计算剩余额度。limit<=0 表示该维度未启用限额：返回 nil，
// 让前端显示「未启用」而不是把 0 当成「已用尽」。达到/超过上限时钳到 0。
func remainingAfter(used, limit int64) *int64 {
	if limit <= 0 {
		return nil
	}
	left := limit - used
	if left < 0 {
		left = 0
	}
	return &left
}
