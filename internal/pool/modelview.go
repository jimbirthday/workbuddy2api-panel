package pool

import (
	"sort"
	"time"
)

// ModelLockRow 单个 (域, 模型) 的锁池画像：这个模型在这个域还能不能选、被锁了几个号、
// 最早/全部解锁是什么时候。
//
// 与 ModelBlocked 的分工：ModelBlocked 回答「**这一个**模型此刻是不是全池被挡」
// （供请求失败时回 model_unavailable），本结构是它的**全清单**版本——把当前所有
// 有未过期模型级冷却的 (域, 模型) 一次性列出来，供 /status 与面板直接展示，
// 运维不必先失败一次才知道哪个模型不能用、还要等多久。
type ModelLockRow struct {
	// Model 裸模型名（不含 cn:/global: 前缀；域由 Realm 字段表达）。
	Model string `json:"model"`
	// Realm 账号域（cn | global）。同名模型在两个域各自独立计数。
	Realm string `json:"realm"`
	// Total 该域**参与选号**的账号数（禁用/暂停号不计，与 ModelBlocked 同口径），
	// 作为「可选 / 总数」的分母。
	Total int `json:"total"`
	// Servable 此刻真正能服务该模型的账号数（healthyForModel 且未在途占满）。
	Servable int `json:"servable"`
	// Locked 因该模型自身冷却被挡的账号数。
	Locked int `json:"locked"`
	// State 聚合状态：
	//   - locked  ：全部参与选号的账号都被该模型冷却挡住（换模型或等解锁才有用，
	//               与 ModelBlocked 的 Blocked 同口径）；
	//   - starved ：此刻没有号能服务，但不是模型冷却造成的（账号级冷却/熔断/在途占满）
	//               ——等一下就会好，与模型无关；
	//   - partial ：仍有号能服务该模型，只是部分号被锁。
	State string `json:"state"`
	// UnlockAt 最早解锁时刻：第一个被锁账号恢复的时刻（partial 下「何时值得重试」）。
	UnlockAt time.Time `json:"unlock_at"`
	// FullyUnlockAt 全部解锁时刻：最后一个被锁账号恢复的时刻。
	FullyUnlockAt time.Time `json:"fully_unlock_at"`
	// Reason 该模型被锁的原因（取最早解锁账号的上游原文）。
	Reason string `json:"reason,omitempty"`
}

// modelLockAggregate 复用一次池遍历的结果。Overview 与 ModelLockView 都从
// 同一个聚合器输出，避免两份实现的口径漂移；调用方必须持有 p.mu 读锁。
type modelLockAggregate struct {
	rows         map[modelLockKey]*modelLockAcc
	routable     map[string]int
	base         map[string]int
	blocked      map[modelLockKey]int
	inFlightFull func(*entry) bool
}

// modelLockKey 是 (域, 模型) 的结构体键：热路径不再为每个账号的每个冷却模型
// 拼接 realm+"\x1f"+model 字符串（470 账号 × 10 模型约 1000 次分配），只在最终
// view() 为实际输出的少量行构造展示字符串。
type modelLockKey struct {
	realm string
	model string
}

type modelLockAcc struct {
	locked   int
	unlockAt time.Time
	fullyAt  time.Time
	reason   string
}

func modelLockAggregateFor(inFlightFull func(*entry) bool) *modelLockAggregate {
	return &modelLockAggregate{
		rows: make(map[modelLockKey]*modelLockAcc),
		// realm/被锁模型的数量远小于账号数，按账号数预分配会制造无谓的大 map
		//（470 账号实测从 ~11 KiB 放大到 ~96 KiB）。让 map 按实际键增长。
		routable:     make(map[string]int),
		base:         make(map[string]int),
		blocked:      make(map[modelLockKey]int),
		inFlightFull: inFlightFull,
	}
}

// addEntry 累计单账号在模型锁视图中的贡献。healthy 口径与选号一致：禁用/暂停
// 不进入分母，账号级冷却或占满只影响 Servable，不改变 Locked。
func (a *modelLockAggregate) addEntry(e *entry, now time.Time) {
	realm := e.a.Realm()
	if e.disabled || e.paused {
		return
	}
	a.routable[realm]++
	healthy := e.healthy(now) && !a.inFlightFull(e)
	if healthy {
		a.base[realm]++
	}
	for model, mc := range e.modelCooldowns {
		if mc.AuditOnly || mc.Until.IsZero() || !now.Before(mc.Until) {
			continue
		}
		key := modelLockKey{realm: realm, model: model}
		if healthy {
			a.blocked[key]++
		}
		row := a.rows[key]
		if row == nil {
			row = &modelLockAcc{}
			a.rows[key] = row
		}
		row.locked++
		if row.unlockAt.IsZero() || mc.Until.Before(row.unlockAt) {
			row.unlockAt = mc.Until
			row.reason = mc.Reason
		}
		if row.fullyAt.IsZero() || mc.Until.After(row.fullyAt) {
			row.fullyAt = mc.Until
		}
	}
}

func (a *modelLockAggregate) view() []ModelLockRow {
	if len(a.rows) == 0 {
		return nil
	}
	out := make([]ModelLockRow, 0, len(a.rows))
	for key, row := range a.rows {
		realm, model := key.realm, key.model
		servable := a.base[realm] - a.blocked[key]
		if servable < 0 {
			servable = 0
		}
		item := ModelLockRow{
			Model:         model,
			Realm:         realm,
			Total:         a.routable[realm],
			Locked:        row.locked,
			Servable:      servable,
			UnlockAt:      row.unlockAt,
			FullyUnlockAt: row.fullyAt,
			Reason:        row.reason,
		}
		switch {
		case item.Servable > 0:
			item.State = "partial"
		case item.Locked >= item.Total:
			item.State = "locked"
		default:
			item.State = "starved"
		}
		out = append(out, item)
	}

	rank := func(state string) int {
		switch state {
		case "locked":
			return 0
		case "starved":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i].State), rank(out[j].State); ri != rj {
			return ri < rj
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Realm < out[j].Realm
	})
	return out
}

// ModelLockView 汇总当前所有「有未过期模型级限流」的 (域, 模型)，按不可用优先排序。
//
// 为什么需要它：选号失败时上游只回 no_healthy_account（issue #102），ModelBlocked 虽
// 已把「号都在、只是都对这个模型关闭」从「池子真没号」里区分出来，但那是一次请求
// 失败后的单模型回答；运维仍看不到「此刻一共哪些模型不能用、分别还要锁多久」。
// 本视图把 (域, 模型) 维度聚合成一张清单补齐这一点。
//
// 口径与选号/ModelBlocked 严格一致，三处易错点：
//   - 只看**参与选号**的账号：disabled / paused 号跳过——它们的不可用与模型无关，
//     计进来会把「模型被锁」和「号被停了」混为一谈；
//   - 只看**真正拦路由**的冷却：AuditOnly 台账（无重置时间的 6004）不改变选号，
//     既不产生行也不计入 Locked；
//   - 已过期的冷却不算锁。
//
// 只读：持 RLock 遍历，不改任何状态。无锁时返回 nil（JSON 里是 null，面板按空态渲染）。
func (p *Pool) ModelLockView() []ModelLockRow {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	agg := modelLockAggregateFor(p.inFlightFull)
	for _, e := range p.byUID {
		agg.addEntry(e, now)
	}
	return agg.view()
}
