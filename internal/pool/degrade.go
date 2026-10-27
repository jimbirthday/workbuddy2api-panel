// 连败降权（issue #114）：NoteFailures 计数 + 达阈临时出池 + NoteSuccess 清零。
// 与冷却/熔断「并存取更长者不叠加」由 entry.healthy / entry.healthyForModel 的
// 并列或门天然保证（任一截止未到期即不可选，生效的一定是最远者）。
//
// 账号级与模型级两套计数并存、互不污染：
//   - model == "" → 账号级（consecutiveFails / degradeUntil）：整号临时出池；
//   - model != "" → 模型级（modelDegrades[model]）：只对这一个模型临时出池，
//     该账号对其他模型仍可选（与 modelCooldowns 的模型豁免同哲学）。
package pool

import "time"

// NoteFailures 记录一次「不罚号的失败」（issue #114 连败兜底的喂入口）。
//
// model 非空时只累加该模型的连续失败，达阈只写该模型的降权截止，**绝不**写账号级
// degradeUntil / consecutiveFails（单模型的失败不得冻结整个账号的调度）。
// model 为空时退回账号级语义（兼容无模型上下文的调用点）。
//
// 达阈触发降权（Until = now + 降权时长），计数清零供下一轮重新累计。
// **已在降权期内**（Until 未到期）再次达阈：不延长、不翻倍（防用户重试把降权
// 越堆越厚，与 CooldownSoftRate 兜底探测同哲学）——计数清零，到期放行后需重新
// 连败满阈值才再降。成功（NoteSuccess/ClearModelDegrade）清零计数并清降权截止。
//
// 谁该喂：ErrClient（未知 4xx，applyErrorPolicy default 只换号不罚）与传输层
// 失败（handler 网络抖动分支，同样只换号不罚）——两者都是「不知道原因的失败」，
// 正是连败兜底的目标形态。带权威分类的错误（429/WAF/402/5xx/12153/11140/11102）
// 各有「知道原因的惩罚」（冷却/熔断/禁用/负缓存），不喂本计数器——惩罚已存在，
// 再喂连败是重复计罚（任务书纪律：两机制并存取更长者，不叠加）。
func (p *Pool) NoteFailures(uid, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if model != "" {
		p.noteModelFailuresLocked(e, model)
		return
	}
	e.consecutiveFails++
	if e.consecutiveFails < p.degradeThreshold {
		p.dirty.Store(true)
		return
	}
	e.consecutiveFails = 0
	now := time.Now()
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		// 降权期内的连败达阈：不延长（取更长者语义由 healthy 或门保证）。计数
		// 已清零——到期放行后需重新连败满阈值才再降。
		p.dirty.Store(true)
		return
	}
	degradeUntil := now.Add(p.degradeDurationLocked())
	e.degradeUntil = degradeUntil
	// 与冷却域共用同一个 e.reason 字段（不新增台账字段）：运维在 /status 的
	// 账号 reason 能看到「为什么被降权/冷却」。但 reason 与生效截止同源：既有账号级
	// 冷却（until）仍生效且不早于本次降权截止时，本次降权不是最远的生效截止，不得
	// 用降权原因掩盖真实冷却原因（statusOf 的 reason 必须描述仍在生效的那个惩罚）。
	if !now.Before(e.until) || !degradeUntil.Before(e.until) {
		e.reason = degradeReason
	}
	p.dirty.Store(true)
}

// noteModelFailuresLocked 模型级连败计数的达阈判定，与账号级同语义但作用域限定
// 在单个模型。调用方必须已持有 p.mu。
func (p *Pool) noteModelFailuresLocked(e *entry, model string) {
	if e.modelDegrades == nil {
		e.modelDegrades = map[string]modelDegrade{}
	}
	md := e.modelDegrades[model]
	md.Fails++
	if md.Fails < p.degradeThreshold {
		e.modelDegrades[model] = md
		p.dirty.Store(true)
		return
	}
	now := time.Now()
	if !md.Until.IsZero() && now.Before(md.Until) {
		// 该模型已在降权期内再次达阈：不延长、计数清零（同账号级语义）。
		md.Fails = 0
		e.modelDegrades[model] = md
		p.dirty.Store(true)
		return
	}
	md.Fails = 0
	md.Until = now.Add(p.degradeDurationLocked())
	e.modelDegrades[model] = md
	p.dirty.Store(true)
}

// ClearModelDegrade 清除指定模型的连败计数与降权截止（该模型实测又通了）。
// 只清该模型，其他模型的降权状态与账号级降权互不影响。
func (p *Pool) ClearModelDegrade(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelDegrades) == 0 {
		return
	}
	if _, exists := e.modelDegrades[model]; !exists {
		return
	}
	delete(e.modelDegrades, model)
	if len(e.modelDegrades) == 0 {
		e.modelDegrades = nil
	}
	p.dirty.Store(true)
}

// clearModelDegradesLocked 清空全部模型级连败降权（Revive 等「清除全部惩罚状态」
// 的迁移原语使用）。调用方必须已持有 p.mu。
func (p *Pool) clearModelDegradesLocked(e *entry) {
	if len(e.modelDegrades) == 0 {
		return
	}
	e.modelDegrades = nil
}

// degradeDurationLocked 返回降权时长（当前实现为固定 degradeCooldown，钳到
// [defaultDegradeCooldown, degradeCooldownMax]）。不做指数升级：触发计数在达阈时
// 清零、不持久化历史触发次数，档位不可推导；固定时长已满足「临时出池一段时间」
// 的语义（真持续坏号会在下一轮 5 连败里再降一次）。调用方必须已持有 p.mu。
func (p *Pool) degradeDurationLocked() time.Duration {
	d := p.degradeCooldown
	if d <= 0 {
		d = defaultDegradeCooldown
	}
	max := p.degradeCooldownMax
	if max <= 0 {
		max = defaultDegradeCooldownMax
	}
	if d > max {
		d = max
	}
	return d
}
