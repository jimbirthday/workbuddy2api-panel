package usage

import (
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// CreditKind 积分历史方向。现有 usage 桶是逐请求 credit 的去向汇总，只能
// 可靠推导「扣减」；正向 credit 为 spend、负向 credit 只表示上游返回了冲正/返还
// 观测，不足以还原「谁在何时发放了多少」的领取流水，因此不提供 grant 记录。
type CreditKind string

const (
	CreditKindSpend CreditKind = "spend"
	CreditKindAll   CreditKind = "all"

	// CreditKindGrant 仅为兼容调用方保留；历史接口对该值返回 501，不做推断。
	CreditKindGrant CreditKind = "grant"
)

// 积分历史返回给面板的默认/最大条数。历史接口按时间倒序取最近 N 条，
// 上限与导出接口保持一致，避免一次面板请求把长期桶表全部序列化。
const (
	CreditHistoryDefaultLimit = 200
	CreditHistoryMaxLimit     = 1000
)

// CreditHistoryQuery 是积分历史查询条件。Hours/From/To 与 SnapshotWindow 同义：
// From/To 任一非零优先；否则 Hours>0 取滚动窗口；Hours<=0 表示全部历史。
type CreditHistoryQuery struct {
	Window Window
	Kind   CreditKind
	Limit  int
}

// CreditHistoryRecord 一条聚合扣减记录。Time 是桶起点（小时桶按整点、日桶按
// 当天零点），Granularity 明确告诉调用方这是小时/日聚合，不是逐请求明细。
// Account 是脱敏展示标签（昵称(uid8)），不含原始 UID 或任何凭证。
type CreditHistoryRecord struct {
	Time        string     `json:"time"`
	Scope       string     `json:"scope"`       // hour | day
	Granularity string     `json:"granularity"` // hourly | daily
	Realm       string     `json:"realm,omitempty"`
	Account     string     `json:"account"`
	Model       string     `json:"model"`
	Kind        CreditKind `json:"kind"` // 恒为 spend
	Credit      float64    `json:"credit"`
	Tokens      int64      `json:"tokens"`
	Requests    int64      `json:"requests"`

	// uid 仅用于在内存中补昵称，绝不序列化到接口响应。
	uid string
}

// CreditHistoryResult 历史接口响应。汇总按当前窗口实际返回的聚合记录计算；
// Records 为空时仍返回 [] 而非 null，前端可区分「成功但无记录」与读取失败。
// GrantAvailable 恒为 false：当前没有可靠的领取数据源，不伪造 grant 空记录。
type CreditHistoryResult struct {
	Records        []CreditHistoryRecord `json:"records"`
	Total          int                   `json:"total"`
	Limit          int                   `json:"limit"`
	Kind           CreditKind            `json:"kind"`
	Credits        float64               `json:"credits"`
	Samples        int64                 `json:"samples"`
	Tokens         int64                 `json:"tokens"`
	GrantAvailable bool                  `json:"grant_available"`
	Aggregated     bool                  `json:"aggregated"`
	// Truncated 为 true 表示匹配记录超过 limit，Records 只包含最近 limit 条。
	Truncated  bool   `json:"truncated"`
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo   string `json:"window_to,omitempty"`
	Generated  string `json:"generated"`
}

// CreditHistory 从现有用量桶读取按时间聚合的积分扣减记录，不修改任何桶、
// 不落盘、不推断逐请求或领取明细。
//
// 每个桶是一条聚合记录（同一小时/日、同一 realm、账号、模型、倍率、请求的
// credit 累计）。桶内 CR <= 0 或没有 CRN 样本时跳过：旧版本只有 token 的历史
// 不应显示为 0 积分，负向 credit 也不能被当作领取记录。若调用方请求
// CreditKindGrant，返回 GrantAvailable=false 的空结果（HTTP 层负责 501）。
func (r *Recorder) CreditHistory(q CreditHistoryQuery) CreditHistoryResult {
	kind := q.Kind
	if kind != CreditKindSpend && kind != CreditKindGrant {
		kind = CreditKindAll
	}
	limit := q.Limit
	if limit <= 0 {
		limit = CreditHistoryDefaultLimit
	}
	if limit > CreditHistoryMaxLimit {
		limit = CreditHistoryMaxLimit
	}

	out := CreditHistoryResult{
		Records:        make([]CreditHistoryRecord, 0),
		Limit:          limit,
		Kind:           kind,
		GrantAvailable: false,
		Aggregated:     true,
		Generated:      time.Now().Format(time.RFC3339),
	}
	if r == nil || kind == CreditKindGrant {
		return out
	}

	from, to := q.Window.bounds()
	explicit := !q.Window.From.IsZero() || !q.Window.To.IsZero()
	if explicit {
		if !from.IsZero() {
			out.WindowFrom = from.Format(time.RFC3339)
		}
		if !to.IsZero() {
			out.WindowTo = to.Format(time.RFC3339)
		}
	}

	parsed := make(map[string]bucketTimeAt, 96)
	// 先按桶时间选「最新 limit 条」，再为窗口内记录格式化时间/账号标签。
	// 时间格式化和脱敏标签对全历史每桶都做会放大临时分配；汇总仍逐桶累加，
	// 保证 Total/Credits/Samples/Tokens 覆盖全部命中行。
	type creditCandidate struct {
		at       time.Time
		realm    string
		uid      string
		model    string
		scope    string
		credit   float64
		tokens   int64
		requests int64
	}
	candidates := make([]creditCandidate, 0, 2*limit)
	newer := func(a, b creditCandidate) bool {
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		if a.uid != b.uid {
			return a.uid < b.uid
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.realm < b.realm
	}
	trim := func() {
		if len(candidates) < 2*limit {
			return
		}
		sort.Slice(candidates, func(i, j int) bool { return newer(candidates[i], candidates[j]) })
		candidates = candidates[:limit]
	}
	r.rollupMu.RLock()
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.RLock()
		for _, b := range sh.buckets {
			if b.CRN <= 0 || b.CR <= 0 {
				continue
			}
			ts, cached := parsed[b.Scope]
			if !cached {
				at, ok := bucketTime(b.Scope)
				ts = bucketTimeAt{ts: at, ok: ok}
				parsed[b.Scope] = ts
			}
			if !ts.ok {
				continue
			}
			if !from.IsZero() && ts.ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.ts.After(to) {
				continue
			}
			out.Total++
			out.Credits += b.CR
			out.Samples++
			out.Tokens += b.CRT
			candidates = append(candidates, creditCandidate{
				at:       ts.ts,
				realm:    b.Realm,
				uid:      b.UID,
				model:    b.Model,
				scope:    b.Scope,
				credit:   b.CR,
				tokens:   b.CRT,
				requests: b.Req,
			})
			trim()
		}
		sh.mu.RUnlock()
	}
	r.rollupMu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool { return newer(candidates[i], candidates[j]) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out.Records = make([]CreditHistoryRecord, len(candidates))
	for i, c := range candidates {
		out.Records[i] = CreditHistoryRecord{
			Time:        c.at.Format(time.RFC3339),
			Scope:       scopeKind(c.scope),
			Granularity: granularity(c.scope),
			Realm:       c.realm,
			Account:     logfmt.Label(c.uid, ""),
			Model:       canonicalUsageModel(c.model),
			Kind:        CreditKindSpend,
			Credit:      c.credit,
			Tokens:      c.tokens,
			Requests:    c.requests,
			uid:         c.uid,
		}
	}
	out.Truncated = out.Total > limit
	return out
}

// CreditHistoryWithNicks 在记录里补上池内昵称。历史接口通过 Pool.Nicknames
// 传入 uid→昵称映射；这不改变桶内容，只影响展示。账号标签仍只含 uid 前 8 位。
func (r *Recorder) CreditHistoryWithNicks(q CreditHistoryQuery, nicks map[string]string) CreditHistoryResult {
	res := r.CreditHistory(q)
	for i := range res.Records {
		rec := &res.Records[i]
		rec.Account = logfmt.Label(rec.uid, nicks[rec.uid])
	}
	return res
}

func scopeKind(scope string) string {
	if strings.HasPrefix(scope, "h:") {
		return "hour"
	}
	return "day"
}

func granularity(scope string) string {
	if strings.HasPrefix(scope, "h:") {
		return "hourly"
	}
	return "daily"
}
