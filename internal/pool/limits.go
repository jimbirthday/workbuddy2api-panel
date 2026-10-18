// 账号级日 Token 护栏：只做 pick 前过滤，不改变选号权重、成本分层、粘性、
// 租约或冷却状态。达到额度只会让账号在本轮候选集中被跳过；计数由外部
// provider 提供（通常来自 usage.Recorder 的只读聚合），读取失败时放行。
package pool

import (
	"strings"
	"time"
)

// DailyTokenUsage 是单个账号自本地零点以来的 token 计数。
// Models 仅包含有 token 消耗的模型；缺失表示该模型今日用量为 0。
type DailyTokenUsage struct {
	Tokens int64
	Models map[string]int64
}

// DailyTokenUsageByUID 是选号护栏共享的只读快照类型。定义在 pool 而不是
// usage，是为了让用量记录器直接返回同形视图，避免装配层每个请求把整张
// map 再复制一遍（470 账号约 17µs/41KiB）。调用方只读，不得修改。
type DailyTokenUsageByUID map[string]DailyTokenUsage

// DailyTokenUsageProvider 提供“今日（本地零点起）”的账号 token 用量快照。
//
// 契约：
//   - ok=false 表示读取失败/计数不可用；调用方必须 fail-open，不能因此让账号出池。
//   - 返回的 map 视为只读，pool 不会修改它。
//   - 实现必须便宜（建议短 TTL 缓存），因为每次 pick 至多调用一次。
//   - 实现不得反向调用 Pool 方法；本方法在选号路径释放 p.mu 后调用。
type DailyTokenUsageProvider interface {
	DailyTokenUsageAt(now time.Time) (DailyTokenUsageByUID, bool)
}

// DailyTokenUsageProviderFunc 让普通函数直接作为 provider 注入。
type DailyTokenUsageProviderFunc func(now time.Time) (DailyTokenUsageByUID, bool)

func (f DailyTokenUsageProviderFunc) DailyTokenUsageAt(now time.Time) (DailyTokenUsageByUID, bool) {
	if f == nil {
		return nil, false
	}
	return f(now)
}

// SetDailyLimits 热更新账号级日 Token 与按模型日 Token 上限。
// <=0 表示关闭对应护栏（与 config 的 0 = 关闭语义一致）。
func (p *Pool) SetDailyLimits(dailyTokenLimit, modelDailyTokenLimit int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if dailyTokenLimit < 0 {
		dailyTokenLimit = 0
	}
	if modelDailyTokenLimit < 0 {
		modelDailyTokenLimit = 0
	}
	p.dailyTokenLimit = dailyTokenLimit
	p.modelDailyTokenLimit = modelDailyTokenLimit
}

// DailyLimits 返回当前生效的两条日 Token 上限（0 = 关闭）。供状态/测试观测。
func (p *Pool) DailyLimits() (dailyTokenLimit, modelDailyTokenLimit int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dailyTokenLimit, p.modelDailyTokenLimit
}

// SetDailyTokenUsageProvider 注入今日 token 用量 provider（nil = 关闭计数读取，
// 护栏因无数据而 fail-open）。热更新可在运行期反复调用。
func (p *Pool) SetDailyTokenUsageProvider(provider DailyTokenUsageProvider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyUsage = provider
}

// dailyUsageSnapshot 读取一次今日用量快照。两条上限都关闭或未注入 provider
// 时不调用外部回调，避免无谓开销。
//
// 调用方必须在**释放 p.mu 后**调用：provider 可能做磁盘/缓存读取，持池锁
// 调用会把整池选号堵在外部实现上。先快照配置与 provider，再释放锁读取；
// 读到的计数用于本次候选过滤，天然允许极短的配置/计数不一致窗口。
func (p *Pool) dailyUsageSnapshot(now time.Time) (DailyTokenUsageByUID, bool, int64, int64) {
	p.mu.RLock()
	dailyLimit, modelLimit := p.dailyTokenLimit, p.modelDailyTokenLimit
	provider := p.dailyUsage
	p.mu.RUnlock()
	if dailyLimit <= 0 && modelLimit <= 0 || provider == nil {
		return nil, false, dailyLimit, modelLimit
	}
	usage, ok := provider.DailyTokenUsageAt(now)
	return usage, ok, dailyLimit, modelLimit
}

// dailyTokenBlocked 报告账号是否应因今日 Token 额度被本轮选号跳过。
// 只读判定，绝不修改账号状态（不冷却、不暂停、不写 reason）。
func dailyTokenBlocked(uid, model string, usage DailyTokenUsageByUID, ok bool, dailyLimit, modelLimit int64) bool {
	if !ok || usage == nil {
		return false
	}
	u, exists := usage[uid]
	if !exists {
		return false
	}
	if dailyLimit > 0 && u.Tokens >= dailyLimit {
		return true
	}
	if modelLimit > 0 && model != "" {
		if used, found := u.Models[canonicalDailyModel(model)]; found && used >= modelLimit {
			return true
		}
		return false
	}
	// 没有请求模型上下文（/healthz、会话可用集）时不能把「今天用过的模型」
	// 当成「全部已知模型」：只用了 m1 且 m1 触顶，不代表尚未使用的 m2 不可用。
	// 这里只检查账号级总额度；按模型额度的探活在调用方按具体模型查询时生效。
	// 账号级额度关闭时，缺少模型上下文一律放行（fail-open），与真实选号一致：
	// 未观测过的模型仍会被 pick 选中。
	return false
}

// canonicalDailyModel 把请求/目录侧可能带域前缀的模型名（cn:glm-5.3、
// global:glm-5.3）归一成用量侧记录的裸名。usage 快照本身已归一化；这里再兜
// 一层，避免调用方漏传 bareModel 时按模型额度被前缀绕过。
func canonicalDailyModel(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.IndexByte(model, ':'); i >= 0 {
		model = model[i+1:]
	}
	return strings.TrimSpace(model)
}
