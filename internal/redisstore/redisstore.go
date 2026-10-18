// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（后台 goroutine + 失败仅 debug 日志），读操作只发生在启动时
// （加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
package redisstore

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

const (
	// writeConcurrencyLimit 固定 Redis 写 worker 数。旧实现每次写都先派生 goroutine
	// 再阻塞抢信号量，队列本身没有上限；高写入速率下会堆积任意多 goroutine。
	writeConcurrencyLimit = 8
	// writeQueueLimit 有界写队列容量。队列满时丢弃本次镜像（内存仍是权威状态，
	// Redis 只是恢复备份）并计数告警，绝不阻塞请求热路径。
	writeQueueLimit = 256
)

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
	// Close 关停 Store：Upstash 等待已提交的异步写全部执行完再关底层连接
	// （停机语义：最后一笔 Redis 镜像必须写完），之后新提交的写直接丢弃；幂等。
	// Noop 为空操作。进程退出前在 pool.Close() 之后调用。
	Close() error
}

const (
	bindPrefix      = "wb2api:bind:"
	stateKey        = "wb2api:state"
	readTimeout     = 3 * time.Second
	bindLoadTimeout = 30 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] 警告: redis 连接串解析失败 (%v)，降级 Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)
	u := &Upstash{client: client}
	u.ensure()
	return u
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 若 url 本身已含 scheme（rediss://、redis://、https://...upstash.io 等）：
//   - rediss:// 或 redis:// 原样返回（已是完整连接串）
//   - 其余（如 https://xxx.upstash.io）剥掉 "://" 前缀只取 host，再按
//     "rediss://default:<token>@<host>:6379" 组装
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash 真实现：redis.Client 封装。
//
// 写入模型：固定 worker + 有界队列。SetBind/DelBind 队列满时直接丢弃本次镜像，
// SaveState 只保留最新一份待写快照。Close 在提交锁内关闭队列，worker 会排空
// 已接受的写后再退出；此后新提交的写直接丢弃。
type Upstash struct {
	client *redis.Client

	queue chan writeTask
	done  chan struct{}

	initOnce  sync.Once
	closeOnce sync.Once
	// submitMu 同时保护提交/关停边界与队列关闭：goWrite 的 wg.Add + send 和 Close
	// 的 close(done)+close(queue) 互斥，避免向已关闭队列发送或 Wait 后才 Add。
	submitMu sync.Mutex
	wg       sync.WaitGroup
	dropped  atomic.Uint64

	stateMu      sync.Mutex
	pendingState []byte
	stateQueued  bool
}

type writeTask struct {
	fn func()
}

// ensure 初始化零值 Upstash（仅测试会直接构造）并启动固定写 worker。
func (u *Upstash) ensure() {
	u.initOnce.Do(func() {
		if u.queue == nil {
			u.queue = make(chan writeTask, writeQueueLimit)
		}
		if u.done == nil {
			u.done = make(chan struct{})
		}
		u.startWorkers(writeConcurrencyLimit)
	})
}

func (u *Upstash) startWorkers(n int) {
	for i := 0; i < n; i++ {
		go func() {
			for task := range u.queue {
				func() {
					defer u.wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[redisstore] WARN: 异步写 panic: %v", r)
						}
					}()
					task.fn()
				}()
			}
		}()
	}
}

// goWrite 非阻塞提交一次写；返回 false 表示队列已满或 Store 已关停。
// wg.Add 必须在 send 前且与 Close 互斥：worker 可能立即执行完任务，若先 send
// 后 Add 会让 Close 的 Wait 提前返回。
func (u *Upstash) goWrite(fn func()) bool {
	u.ensure()
	u.submitMu.Lock()
	defer u.submitMu.Unlock()
	select {
	case <-u.done:
		return false
	default:
	}
	u.wg.Add(1)
	select {
	case u.queue <- writeTask{fn: fn}:
		return true
	default:
		u.wg.Done()
		u.noteDrop()
		return false
	}
}

func (u *Upstash) noteDrop() {
	n := u.dropped.Add(1)
	if n == 1 || n%1024 == 0 {
		log.Printf("[redisstore] WARN: Redis 写队列已满，累计丢弃 %d 次镜像（内存状态不受影响）", n)
	}
}

// Close 等待已接受的异步写全部执行完毕，再关底层 redis 连接；幂等。
// 之后新提交的写直接丢弃。停机路径在 pool.Close() 后调用：
// pool 的最后一次 Flush→SaveState 已提交，本方法保证它写完才返回。
func (u *Upstash) Close() error {
	u.ensure()
	u.submitMu.Lock()
	u.closeOnce.Do(func() {
		close(u.done)
		close(u.queue)
	})
	u.submitMu.Unlock()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// 兜底超时（单写上限 5s，10s 富余）：卡死的写不应阻塞进程退出。
		log.Printf("[redisstore] WARN: Close 等待在途写超时，放弃（镜像可能未写完）")
	}
	return u.closeClient()
}

// closeClient 关底层 redis 连接（client 为 nil——测试构造——时跳过）。
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}
	return u.client.Close()
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState 异步写池状态 JSON 快照，并合并排队中的快照：只保留最后提交的
// 一份，避免 5 秒一次、每次数十 KB~数 MB 的全量状态在队列里叠多份。
func (u *Upstash) SaveState(data []byte) {
	u.ensure()
	u.stateMu.Lock()
	u.pendingState = data
	needTask := !u.stateQueued
	if needTask {
		u.stateQueued = true
	}
	u.stateMu.Unlock()
	if !needTask {
		return
	}
	if !u.goWrite(u.flushLatestState) {
		// 队列满/已关停：撤销占位并释放待写快照，下一次 SaveState 可重新提交。
		u.stateMu.Lock()
		u.stateQueued = false
		u.pendingState = nil
		u.stateMu.Unlock()
	}
}

// flushLatestState 在同一个 worker 任务里持续清空最新快照。期间新 SaveState
// 只替换 pendingState，不会新增任务，因此积压上限固定为 1 份快照。
func (u *Upstash) flushLatestState() {
	for {
		u.stateMu.Lock()
		data := u.pendingState
		u.pendingState = nil
		if data == nil {
			u.stateQueued = false
			u.stateMu.Unlock()
			return
		}
		u.stateMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
		cancel()
	}
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀）。
// 使用独立的冷启动预算：绑定多时 SCAN + 逐条 GET 共用 3 秒会中途超时，
// 导致恢复不完整且用户无感。单次 GET 仍受 readTimeout 约束，避免一条慢 key
// 拖住整个恢复；总预算放宽到 bindLoadTimeout。
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), bindLoadTimeout)
	defer cancel()
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		getCtx, getCancel := context.WithTimeout(ctx, readTimeout)
		v, err := u.client.Get(getCtx, key).Result()
		getCancel()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
func (Noop) Close() error                          { return nil }
