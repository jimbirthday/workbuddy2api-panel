// 面板读路径的轻量账号快照。这里的 API 刻意绕开 Pool.List：
// List 会为每个账号构建完整 Status（限流台账、冷却剩余秒、成本行等），
// 面板的剩余额度与接入页只需要身份/模型成本等少量字段，470 账号时放大明显。
package pool

import (
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// OverviewSnapshot 是 /overview 的一次性池快照：计数、账号状态与模型锁视图
// 共用同一把读锁、同一个 now，既避免三次全池扫描，也避免跨快照的计数/列表
// 不一致。字段仍复用既有 JSON 形状，前端无需改动。
type OverviewSnapshot struct {
	Total        int            `json:"total"`
	Healthy      int            `json:"healthy"`
	Cooling      int            `json:"cooling"`
	Disabled     int            `json:"disabled"`
	Paused       int            `json:"paused"`
	InFlightFull int            `json:"in_flight_full"`
	Accounts     []Status       `json:"accounts"`
	ModelLocks   []ModelLockRow `json:"model_locks"`
}

// StatusSnapshot 是 /status 一次请求所需的池视图：全局计数、cn/global 分组计数、
// 模型锁与（管理面才需要的）完整账号列表共用同一把读锁和同一个 now，避免
// CountsDetailed + 两次 CountsDetailedForRealm + ModelLockView + List 最多五次
// 全池扫描，以及跨快照的冷却边界不一致。
type StatusSnapshot struct {
	Total        int
	Healthy      int
	Cooling      int
	Disabled     int
	InFlightFull int
	Realms       map[string]RealmCounts
	ModelLocks   []ModelLockRow
	Accounts     []Status
}

// RealmCounts 是单个 realm 的五元组计数，字段口径同 CountsDetailed。
type RealmCounts struct {
	Total        int
	Healthy      int
	Cooling      int
	Disabled     int
	InFlightFull int
}

// Status 汇总 /status 所需的全部池状态。includeAccounts=false 时只做计数与模型锁
// 投影，不构建完整 Status（业务 Key 路径不能看到账号池详情）。调用方必须已确认
// 该请求有权限读取账号列表。
func (p *Pool) StatusSnapshot(includeAccounts bool) StatusSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	agg := modelLockAggregateFor(p.inFlightFull)
	snap := StatusSnapshot{Realms: map[string]RealmCounts{
		"cn":     {},
		"global": {},
	}}
	if includeAccounts {
		snap.Accounts = make([]Status, 0, len(p.byUID))
	}
	// 账号列表必须稳定排序；ModelLockView 的排序在 view() 内完成，计数无需排序。
	var uids []string
	if includeAccounts {
		uids = make([]string, 0, len(p.byUID))
		for uid := range p.byUID {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
	}
	for _, e := range p.byUID {
		snap.Total++
		realm := e.a.Realm()
		rc := snap.Realms[realm]
		rc.Total++
		switch {
		case e.disabled:
			snap.Disabled++
			rc.Disabled++
		case e.paused:
			// /status 的对外口径把暂停并入 disabled：两者都不参与选号。
			snap.Disabled++
			rc.Disabled++
		case !e.healthy(now):
			snap.Cooling++
			rc.Cooling++
		default:
			snap.Healthy++
			rc.Healthy++
			if p.inFlightFull(e) {
				snap.InFlightFull++
				rc.InFlightFull++
			}
		}
		snap.Realms[realm] = rc
		agg.addEntry(e, now)
	}
	snap.ModelLocks = agg.view()
	for _, uid := range uids {
		snap.Accounts = append(snap.Accounts, p.statusOf(uid, p.byUID[uid], now))
	}
	return snap
}

// Overview 汇总面板概况所需的全部池状态。计数口径与 CountsDetailedWithPaused
// 一致（paused 与 disabled 分开）；账号行与 ModelLockView 同口径跳过失效条目。
// 只做一次 byUID 遍历 + 一次 statusOf 投影，成本从三次扫描降为一次。
func (p *Pool) Overview() OverviewSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	snap := OverviewSnapshot{Accounts: make([]Status, 0, len(p.byUID))}
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	agg := modelLockAggregateFor(p.inFlightFull)
	for _, uid := range uids {
		e := p.byUID[uid]
		snap.Total++
		switch {
		case e.disabled:
			snap.Disabled++
		case e.paused:
			snap.Paused++
		case !e.healthy(now):
			snap.Cooling++
		default:
			snap.Healthy++
			if p.inFlightFull(e) {
				snap.InFlightFull++
			}
		}
		snap.Accounts = append(snap.Accounts, p.statusOf(uid, e, now))
		agg.addEntry(e, now)
	}
	snap.ModelLocks = agg.view()
	return snap
}

// TaskAccount 是保号/余额类后台任务需要的轻量账号引用：UID 与凭证指针。
// 在一次读锁内拍快照，避免调度器为了筛选禁用号而构建整表 Status（含冷却、
// 限流台账、成本账本），也避免逐个 AuthByUID 造成 2N 次加锁。
type TaskAccount struct {
	UID    string
	Auth   *auth.Auth
	Paused bool
	// Disabled 仅在 includeDisabled=true 时用于调用方区分禁用号；默认列表里
	// 恒 false。加入后面板扫描无需构造包含冷却/限额/成本台账的完整 Status。
	Disabled bool
}

// TaskAccounts 返回参与保号/余额刷新的账号引用。默认跳过 disabled，与
// include_disabled_in_tasks 的语义一致；paused 账号仍会返回，因为它们只退出
// 选号、不退出签到/保活/余额刷新。顺序按 UID 稳定排序，便于测试与日志复现。
func (p *Pool) TaskAccounts(includeDisabled bool) []TaskAccount {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]TaskAccount, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e.a == nil || (e.disabled && !includeDisabled) {
			continue
		}
		out = append(out, TaskAccount{UID: uid, Auth: e.a, Paused: e.paused, Disabled: e.disabled})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// UsageRow 是剩余额度页需要的账号身份信息（不含任何运行态/凭证字段）。
type UsageRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
}

// ConnectModelRow 是接入页模型列表所需的每账号投影：
// 只带 realm、实测成本台账与最近使用模型，不构建完整 Status。
type ConnectModelRow struct {
	UID        string            `json:"uid"`
	Realm      string            `json:"realm,omitempty"`
	ModelCosts []ModelCostStatus `json:"model_costs,omitempty"`
	LastModel  string            `json:"last_model,omitempty"`
}

// UsageRows 返回按 UID 稳定排序的账号身份快照。与 List 不同，它不计算
// 冷却、限流模型、Token 用量或成本台账，是剩余额度页的 O(账号数) 投影。
func (p *Pool) UsageRows() []UsageRow {
	p.mu.RLock()
	defer p.mu.RUnlock()

	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]UsageRow, 0, len(uids))
	for _, uid := range uids {
		e := p.byUID[uid]
		out = append(out, UsageRow{
			UID:      uid,
			Nickname: e.a.Nickname,
			Realm:    e.a.Realm(),
		})
	}
	return out
}

// ConnectModelRows 返回接入页模型列表所需的每账号投影。成本行仍复用
// modelCostsStatusLocked 的有效期/排序口径，但不构建 Status，也不计算
// 冷却、限流与在途计数。now 由调用方传入，便于测试固定过期边界。
func (p *Pool) ConnectModelRows(now time.Time) []ConnectModelRow {
	p.mu.RLock()
	defer p.mu.RUnlock()

	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]ConnectModelRow, 0, len(uids))
	for _, uid := range uids {
		e := p.byUID[uid]
		out = append(out, ConnectModelRow{
			UID:        uid,
			Realm:      e.a.Realm(),
			ModelCosts: p.modelCostsStatusLocked(e, now),
			LastModel:  e.tokenUsage.LastModel,
		})
	}
	return out
}
