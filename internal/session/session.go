// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
	// lastMirror 是最近一次镜像到 redisstore 的时刻。命中路径按 touchInterval
	// 节流续期，避免每个请求都抢全局写锁并把同一 key 的 SetBind 重复排进 Redis
	// 写队列；未到期的快速命中只读内存，不再产生写放大。
	lastMirror time.Time
}

const (
	// defaultMaxBindings 内存粘性绑定的默认上限。会话键可来自客户端任意输入，
	// 没有上限时长期运行会被无限增长的对话撑爆内存；达到上限后按最久未活跃
	// 的绑定批量淘汰（Redis 镜像同步删除）。
	defaultMaxBindings = 50000
	// maxTouchInterval 命中续期的上限。TTL 很短时按 TTL/10 收紧，确保活跃会话
	// 在过期前至少续期一次；TTL 较长时最多每分钟写一次 Redis。
	maxTouchInterval = time.Minute
)

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号（healthy 且未占满在途，
	// 且未被该模型限流/限额）。nil 时回落 Available（无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
	// CanRoute 是快路径的按账号判定（可选）：给定 (model, realm, uid) 回答绑定号
	// 现在是否仍可路由。生产装配注入 pool.UIDAvailableForModel 后，命中路径不再
	// 为校验而整池扫描/排序；nil 时回落 AvailableForModel 的线性 Contains。
	CanRoute func(model, realm, uid string) bool
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	// boundUIDs 只记录 uid -> 绑定数，用来回答「哪些账号当前空闲」。新会话分配
	// 时不应遍历最多 5 万条绑定构造 Set；命中副本/并发 re-check 都通过
	// adjustBoundLocked 维护计数，保证与 entries 同步。
	boundUIDs   map[string]int
	cfg         Config
	stop        chan struct{}
	touchEvery  time.Duration
	maxBindings int
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	touchEvery := cfg.TTL / 10
	if touchEvery > maxTouchInterval {
		touchEvery = maxTouchInterval
	}
	if touchEvery < time.Millisecond {
		touchEvery = time.Millisecond
	}
	return &Router{
		entries:     map[string]entry{},
		boundUIDs:   map[string]int{},
		cfg:         cfg,
		touchEvery:  touchEvery,
		maxBindings: defaultMaxBindings,
	}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil——既是数据竞争
// （-race 可复现），又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），
// 于是关停彻底失效：goroutine 再也不会退出，ticker 无限触发 gcOnce（goroutine
// 泄漏 + 关停后仍持续 GC）。捕获局部变量后，close(stop) 与 select 观测的是同一个
// channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	limit := r.bindLimitLocked()
	loaded, skipped := 0, 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		if len(r.entries) >= limit {
			skipped++
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now, lastMirror: now}
		r.adjustBoundLocked(uid, 1)
		loaded++
	}
	evicted := r.evictOverflowLocked(now)
	r.mu.Unlock()
	for _, key := range evicted {
		r.cfg.Store.DelBind(key)
	}
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
	if skipped > 0 {
		log.Printf("[session] WARN: Redis 粘性绑定超过内存上限 %d，已忽略 %d 条旧绑定", limit, skipped)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")），保留给不关心模型的调用方。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的 softRateModel 豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()

	// ── Fast path: RLock 快查 ──────────────────────────────
	// 已绑定的请求占绝大多数。旧实现无论是否命中都会先 availableSet(model)
	// 复制整池可用账号成 map：470 号时每次请求 ~21KB 分配、约 10µs，命中路径
	// 也被这层分配拖慢。现在先读绑定，再只在需要校验时线性扫描候选列表；
	// 未命中才构建集合（重新分配本来就要遍历候选）。
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if r.availableContains(model, e.uid) {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/占满 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if r.availableContains(model, e2.uid) {
			e2.lastActive = now
			r.entries[key] = e2
			r.mu.Unlock()
			return e2.uid, true
		}
		r.dropEntryLocked(key) // 失效：清掉再分配
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		r.mu.Unlock()
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	// boundUIDs 是增量维护的 uid 绑定数，避免每次新会话在写锁内 O(绑定数)
	// 复制整张表（5 万上限时是明显的长尾）。
	var idle []string
	for _, u := range uids {
		if r.boundUIDs[u] == 0 {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.adjustBoundLocked(prev.uid, -1)
	r.entries[key] = entry{uid: uid, lastActive: now, lastMirror: now}
	r.adjustBoundLocked(uid, 1)
	evicted := r.evictOverflowLocked(now)
	r.mu.Unlock()

	// Redis 写入是异步的，但不能在持内存写锁时提交一长串任务：淘汰一批绑定后
	// 这里可能连续 DelBind 多条，放到锁外可避免其他会话命中排队。
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	for _, stale := range evicted {
		r.cfg.Store.DelBind(stale)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像，但按 touchEvery 节流写路径。
// 高频命中（同一会话连续多轮）在窗口内只做 RLock 读，不抢全局写锁、不重复
// SetBind；窗口到期后写锁 re-check 再用内存中的最新值续期 Redis TTL。
func (r *Router) touch(key, uid string, now time.Time) {
	if r.touchEvery > 0 && now.Sub(r.entriesMirrorAt(key, uid)) < r.touchEvery {
		return
	}
	r.mu.Lock()
	e, ok := r.entries[key]
	if !ok || e.uid != uid {
		r.mu.Unlock()
		return
	}
	if r.touchEvery > 0 && now.Sub(e.lastMirror) < r.touchEvery {
		r.mu.Unlock()
		return
	}
	e.lastActive = now
	e.lastMirror = now
	r.entries[key] = e
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// entriesMirrorAt 是 touch 快路径的只读探针；返回零值表示未找到/不匹配，
// 调用方会自然进入写锁 re-check。它不返回 entry 本体，避免把内部结构复制出去。
func (r *Router) entriesMirrorAt(key, uid string) time.Time {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	if !ok || e.uid != uid {
		return time.Time{}
	}
	return e.lastMirror
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	prev, existed := r.entries[key]
	if existed && prev.uid == uid {
		// 成功路径每次请求都会 Bind 最终成功号。同一账号且镜像窗口未到时不重复写
		// Redis，只滚动内存活跃时间；账号变化或窗口到期才续期/覆盖。
		if r.touchEvery <= 0 || now.Sub(prev.lastMirror) >= r.touchEvery {
			prev.lastActive = now
			prev.lastMirror = now
			r.entries[key] = prev
			r.mu.Unlock()
			r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
			return
		}
		prev.lastActive = now
		r.entries[key] = prev
		r.mu.Unlock()
		return
	}
	if existed {
		r.adjustBoundLocked(prev.uid, -1)
	}
	r.entries[key] = entry{uid: uid, lastActive: now, lastMirror: now}
	r.adjustBoundLocked(uid, 1)
	evicted := r.evictOverflowLocked(now)
	r.mu.Unlock()
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	for _, stale := range evicted {
		r.cfg.Store.DelBind(stale)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		r.dropEntryLocked(key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// bindLimitLocked 返回当前内存绑定上限（测试可调小）。
func (r *Router) bindLimitLocked() int {
	if r.maxBindings <= 0 {
		return defaultMaxBindings
	}
	return r.maxBindings
}

// adjustBoundLocked 维护 uid 绑定计数。调用方必须持有 r.mu 写锁。
func (r *Router) adjustBoundLocked(uid string, delta int) {
	if uid == "" || delta == 0 {
		return
	}
	if r.boundUIDs == nil {
		r.boundUIDs = make(map[string]int)
	}
	n := r.boundUIDs[uid] + delta
	if n <= 0 {
		delete(r.boundUIDs, uid)
		return
	}
	r.boundUIDs[uid] = n
}

// dropEntryLocked 删除绑定并同步空闲索引。调用方必须持有 r.mu 写锁。
func (r *Router) dropEntryLocked(key string) {
	if e, ok := r.entries[key]; ok {
		r.adjustBoundLocked(e.uid, -1)
		delete(r.entries, key)
	}
}

// evictOverflowLocked 在超过上限时按 lastActive 从旧到新批量淘汰。
// 批量淘汰避免“每来一个新会话就全表扫一次找最旧”的 O(n) 抖动；返回的 key
// 必须在锁外由调用方镜像 DelBind（Redis 网络调用不能占内存写锁）。
func (r *Router) evictOverflowLocked(now time.Time) []string {
	limit := r.bindLimitLocked()
	if len(r.entries) <= limit {
		return nil
	}
	overflow := len(r.entries) - limit
	target := overflow
	if batch := limit / 20; batch > target {
		target = batch
	}
	if target > len(r.entries) {
		target = len(r.entries)
	}
	type keyedEntry struct {
		key string
		e   entry
	}
	items := make([]keyedEntry, 0, len(r.entries))
	for key, e := range r.entries {
		items = append(items, keyedEntry{key: key, e: e})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].e.lastActive.Before(items[j].e.lastActive)
	})
	evicted := make([]string, 0, target)
	for i := 0; i < target; i++ {
		r.dropEntryLocked(items[i].key)
		evicted = append(evicted, items[i].key)
	}
	return evicted
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		r.dropEntryLocked(key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableContains 报告 uid 是否在当前模型的可用候选列表里。
//
// 快路径只需回答“绑定号还能不能路由”，不需要为整池建 map。线性 Contains 的
// 开销随候选数增长，但命中请求不再分配 470 项的 map；重新分配（未命中/失效）
// 仍在下方的 slow path 按原双段策略处理。候选列表含虚拟实例重复项，Contains
// 的结果与集合语义一致。
func (r *Router) availableContains(model, uid string) bool {
	if r.cfg.CanRoute != nil {
		realm, bare := splitModelRealm(model)
		return r.cfg.CanRoute(bare, realm, uid)
	}
	for _, candidate := range r.availableSlice(model) {
		if candidate == uid {
			return true
		}
	}
	return false
}

// splitModelRealm 把网关模型名拆成 realm 与裸模型。CanRoute 需要 realm 才能与
// 分池选号严格同口径；裸名按现有协议归 cn（与 server.ResolveModel 一致）。
// 这里只做前缀判定，不引入 server/pool 依赖，保持 session 包可独立测试。
func splitModelRealm(model string) (realm, bare string) {
	// 热路径：只识别开头的 "cn:" / "global:"，避免为每次粘性命中构造
	// strings.IndexByte 之外的临时值。裸名与未知前缀保持归 cn 的现状语义。
	if strings.HasPrefix(model, "cn:") {
		return "cn", model[3:]
	}
	if strings.HasPrefix(model, "global:") {
		return "global", model[7:]
	}
	return "cn", model
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available（nil 视空池）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键，见下）
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退，
//     带 user_id 的请求不派生——见下方契约说明）
//
// 前 5 项均为 conversation 维度（对话级）。metadata.user_id 不再作为粘性键
// （P1-anti-monopoly 剔除）：user 维度粒度过粗——一个 user 的全部并行对话
// 会钉同一账号（粘性范围远大于上游 prompt cache 的对话级边界），且曾抢占顶层
// conversation_id 的优先级。剔除后发 user_id 的客户端回落加权轮换（与无标识
// 客户端同路径），旧 user_id 绑定靠 TTL 自然过期，键消失不产生脏绑定。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。纳入识别后，这类客户端
// 无需改配置即可命中粘性。置于显式会话键之后、派生键之前，绝不抢占 conversation
// 维度的优先级。
//
// 派生键的 user_id 抑制（P1-anti-monopoly 契约在 fallback 路径的延伸）：body 携带
// metadata.user_id 或顶层 user_id 时**不派生**。ExtractKey 有意剔除 user_id 作粘性键
// （user 维度粒度过粗——一个 user 的全部并行对话会被钉到同一账号），若派生路径不设
// 此闸，只发 user_id 的请求会借内容哈希重新获得粘性，使该契约在 fallback 路径失效
// （上游 #169 同款回归修正）。这类客户端回落加权轮换。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	return ExtractKeyFromObject(obj)
}

// ExtractKeyFromObject 是 ExtractKey 的已解析版本：handler 在请求入口已把 body
// 解码为 map 后直接复用，不再为会话键做第二次全量解码。obj 必须是成功
// json.Unmarshal 到 map[string]any 的结果（与 ExtractKey 内解析同构）；nil 返回空串。
func ExtractKeyFromObject(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI 系的会话级前缀缓存键，语义就是"同一会话复用同一
	//    前缀"，与粘性诉求同源。放显式会话键之后、派生键之前，不抢占优先级。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 派生回退闸：带 user 维度标识的请求不派生（契约见上）。
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// ConversationIDFields 是 ResolveConversationID 所需的显式会话 ID 字段。
type ConversationIDFields struct {
	Metadata                    map[string]any
	MetadataConversationID      string
	MetadataConversationIDCamel string
	ConversationID              string
	ConversationIDCamel         string
}

// ResolveConversationIDFromFields 是 ResolveConversationID 的已解码版本：
// metadata 优先、snake 优先于 camel，且绝不回落 user_id。
func ResolveConversationIDFromFields(f ConversationIDFields) string {
	metaConversation, metaConversationCamel := f.MetadataConversationID, f.MetadataConversationIDCamel
	if f.Metadata != nil {
		if v := strOrEmpty(f.Metadata["conversation_id"]); v != "" {
			metaConversation = v
		}
		if v := strOrEmpty(f.Metadata["conversationId"]); v != "" {
			metaConversationCamel = v
		}
	}
	if metaConversation != "" {
		return metaConversation
	}
	if metaConversationCamel != "" {
		return metaConversationCamel
	}
	if f.ConversationID != "" {
		return f.ConversationID
	}
	return f.ConversationIDCamel
}

// hasUserIDKey 报告已解析 body 是否携带 user 维度标识（metadata.user_id 或顶层
// user_id，非空字符串才算）。只用于派生回退闸（ExtractKey）；解析失败按无处理。
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 粘性完全失效；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话；
//   - 同一会话多轮请求 → 同一键 → 稳定粘住同一账号（上游 prompt 缓存命中）。
//
// 取不到用户文本（纯图片等）时返回空串：不粘性，退回普通轮换（安全降级）。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				// 首条 user 用内容签名（ids.go contentSignature）而非纯文本提取：
				// 纯图片轮（无 text part）在 messageText 下恒空串 → 派生键失效
				//（首图会话的粘性盲区）；签名口径下图片 part 以 [type:摘要] 入键。
				firstUserText = userContentSignature(msg["content"])
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature 把已解析的 content（any）转回 RawMessage 后取**会话级稳定**
// 签名，供 deriveKey（粘性键）专用。与 TurnKey 的 contentSignature（全文摘）不同：
// 文本 part 照常拼接；非文本 part 只入 "[type]" 占位**不入内容摘要**——同一逻辑
// 图片的 URL 逐轮变化（签名 URL/重编码 base64）是真实场景，内容摘要入键会让
// 派生键随轮漂移、粘性名存实亡（fork 既有契约：image url changes must not
// break derived key stability）。占位仍能区分"有无图片/图片数量"，纯图片首条
// 消息（无 text part）由此可派生非空键（首图会话粘性盲区修复，对齐上游 G1
// 的目标语义而保留 fork 的稳定性口径）。marshal 失败按无内容处理（不伪造）。
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// KeyMessage 是会话键派生所需的单条消息视图。Content 保留原始 JSON 字节，
// 多模态 part 的签名与 TurnKey 使用完全相同的字节口径。
type KeyMessage struct {
	Role    string
	Content json.RawMessage
}

// KeyFields 是 ExtractKey 所需的已解码字段。handler 入口解码一次请求后直接
// 构造它，避免会话键、轮级键和工具名单各自重新解析 JSON。
type KeyFields struct {
	// Metadata 是原始的 metadata 对象；handler 只解析一次请求后直接传入，由
	// ExtractKeyFromFields 在内存里读取会话 ID/user_id。nil 表示无 metadata。
	Metadata                    map[string]any
	MetadataConversationID      string
	MetadataConversationIDCamel string
	ConversationID              string
	ConversationIDCamel         string
	PromptCacheKey              string
	MetadataUserID              string
	UserID                      string
	Messages                    []KeyMessage
}

// ExtractKeyFromFields 是 ExtractKey 的已解码版本，优先级与派生规则逐字一致：
// metadata 显式 ID → 顶层显式 ID → prompt_cache_key → user_id 抑制 → 内容派生。
func ExtractKeyFromFields(f KeyFields) string {
	metaConversation, metaConversationCamel, metaUserID := f.MetadataConversationID, f.MetadataConversationIDCamel, f.MetadataUserID
	if f.Metadata != nil {
		if v := strOrEmpty(f.Metadata["conversation_id"]); v != "" {
			metaConversation = v
		}
		if v := strOrEmpty(f.Metadata["conversationId"]); v != "" {
			metaConversationCamel = v
		}
		if v := strOrEmpty(f.Metadata["user_id"]); v != "" {
			metaUserID = v
		}
	}
	if metaConversation != "" {
		return metaConversation
	}
	if metaConversationCamel != "" {
		return metaConversationCamel
	}
	if f.ConversationID != "" {
		return f.ConversationID
	}
	if f.ConversationIDCamel != "" {
		return f.ConversationIDCamel
	}
	if f.PromptCacheKey != "" {
		return f.PromptCacheKey
	}
	if metaUserID != "" || f.UserID != "" {
		return ""
	}
	return deriveKeyFields(f.Messages)
}

// deriveKeyFields 与 deriveKey 同口径，但直接消费原始 content 字节：
// system 取首条文本，user 取首条内容签名，二者都齐后提前停止扫描。
func deriveKeyFields(msgs []KeyMessage) string {
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = messageTextRaw(m.Content)
			}
		case "user":
			if firstUserText == "" {
				firstUserText = userContentSignatureRaw(m.Content)
			}
		}
		if firstUserText != "" && systemText != "" {
			break
		}
	}
	if firstUserText == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// messageTextRaw 是 messageText 的原始字节版本：字符串原样返回；parts 数组
// 只拼接 text 字段（与 messageText 对已解析 map 的口径一致），其他形态为空。
func messageTextRaw(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return str
		}
		return ""
	}
	if s[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, pr := range parts {
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) == nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// userContentSignatureRaw 是 userContentSignature 的原始字节版本：文本 part
// 拼接原文，非文本 part 只入 [type] 占位（图片 URL 变化不破坏会话粘性）。
func userContentSignatureRaw(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if s[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
