// Package activity 持久化账号每日活动的结构化历史（签到 / 活跃 / 旅行 / 保活）。
//
// 历史只记录运维元数据：时间、uid、昵称、区域、任务、触发来源、结果与一句
// 截断后的说明。token、cookie、API key、授权头等凭证不落盘。
package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Task 活动任务类型。值同时用于 JSONL 与查询过滤。
type Task string

const (
	TaskCheckin   Task = "checkin"
	TaskActivity  Task = "activity"
	TaskTravel    Task = "travel"
	TaskKeepalive Task = "keepalive"
	// TaskDailyChat 国际版每日活跃对话。与 TaskActivity 的国内成长连登上报
	// 语义不同：它真的发起一次轻量对话并计入官方每日活跃 Bonus Pack。
	TaskDailyChat Task = "daily_chat"
)

// Trigger 触发来源。
type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
	TriggerUnknown  Trigger = "unknown"
)

const (
	defaultMaxBytes      = 16 << 20 // 16 MiB：超过即整理，保证文件有硬上限
	defaultRetentionDays = 90
	defaultLimit         = 100
	maxLimit             = 1000
	maxMessageBytes      = 200
	compactInterval      = time.Hour
	// 活动历史含账号标识，文件只允许服务进程用户读写；目录同理收敛为 0700。
	defaultFilePerm     = 0o600
	defaultDirPerm      = 0o700
	activityHistoryName = "activity_history.jsonl"
	appendQueueSize     = 4096
	appendBatchMax      = 256
	appendFlushInterval = 2 * time.Second
)

var (
	// ErrInvalidFilter 查询过滤条件非法（任务/结果/时间范围）。
	ErrInvalidFilter = errors.New("invalid activity filter")
	// ErrQueueFull 表示异步写队列已满。调用方可直接忽略：活动历史是旁路，
	// 不得因观测数据积压影响任务本身。
	ErrQueueFull = errors.New("activity history queue full")

	bearerRE   = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	secretKVRE = regexp.MustCompile(`(?i)\b(access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|authorization|cookie|password|secret)\b\s*[:=]\s*[^\s,;"']+`)
)

// Record 一条真实任务尝试。字段是 JSONL 的稳定契约。
type Record struct {
	Time     time.Time `json:"ts"`
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Realm    string    `json:"realm,omitempty"`
	Task     Task      `json:"task"`
	Trigger  Trigger   `json:"trigger"`
	OK       bool      `json:"ok"`
	Message  string    `json:"message,omitempty"`
}

// Filter 读取过滤条件。Result 为空 / "ok" / "failed"；时间区间为闭区间。
type Filter struct {
	UID    string
	Task   Task
	Result string
	// Query 在账号昵称与消息做大小写不敏感的子串匹配。它只服务面板的本地
	// 活动历史文件，不返回 UID；空值表示不过滤。长度在上层限到 64 字符。
	Query string
	Since time.Time
	Until time.Time
	Limit int
}

// QueryResult 查询结果。Total 是应用过滤后、限制条数前的命中总数。
type QueryResult struct {
	Records []Record
	Total   int
	Limit   int
}

// Config 构造参数。Path 为空时 Store 为只读空实现（Append 直接返回 nil）。
type Config struct {
	Path     string
	MaxBytes int64
	// RetentionDays 保留的自然日数（0 = 默认 90）。按本地日历日 0 点切分，
	// 避免滚动 24 小时让「昨天同一时刻」时有时无地消失。
	RetentionDays int
	Now           func() time.Time
	// Async 为 true 时 Append 只入有界队列，由后台单写协程批量落盘并定期
	// fsync。调度任务不会被每账号一次磁盘同步拖慢；队列满时丢弃并计数，
	// 与 reqlog 的「日志绝不阻塞主流程」口径一致。Close 会排空队列。
	Async bool
}

// Store 是一个并发安全的 JSONL 活动历史存储。
//
// 写入只在末尾追加一行；保留策略由时间窗口与文件字节上限共同约束。整理采用
// 临时文件 + rename，读取侧不会看到半份文件。
type Store struct {
	// writeMu 串行化 Append/Compact（落盘、整理）；queryMu 只保护「读文件时不与
	// rename 交错」的短窗口。查询不等待 fsync，也不因整理阻塞在写锁上。
	writeMu      sync.Mutex
	queryMu      sync.RWMutex
	path         string
	maxBytes     int64
	retentionDay int
	now          func() time.Time
	lastCompact  time.Time
	async        bool

	appMu     sync.Mutex
	appClosed bool
	appQueue  chan []byte
	appClose  chan struct{}
	appDone   chan struct{}
	appStart  sync.Once
	closeOnce sync.Once
	appErr    error
	dropped   uint64

	// queryCache 缓存最近一次整文件扫描的结果。活动历史是只追加的 JSONL，
	// 面板连续切筛选/翻页时同样的 Query 会反复读整份文件；这里以「文件大小 +
	// mtime」作为快照签名，文件一变化（异步落盘/整理）就自动失效。同条件的
	// 并发查询共享一次扫描，避免多个标签页同时打开时重复读盘。
	queryCacheMu sync.Mutex
	queryCache   map[queryCacheKey]queryCacheEntry
	queryFlight  map[queryCacheKey]chan struct{}
}

const (
	queryCacheTTL = 2 * time.Second
	// queryCacheMax 限制按筛选条件缓存的查询结果数。活动页会随 UID/任务/结果/
	// 时间窗组合产生大量不同的 key；若不设上限，长时间切换筛选会让缓存连同
	// 每份最多 maxLimit 条记录持续增长。
	queryCacheMax = 32
)

type queryCacheKey struct {
	uid, task, result string
	query             string
	since, until      int64
}

type queryCacheEntry struct {
	result   QueryResult
	at       time.Time
	size     int64
	modUnix  int64
	modNanos int64
}

// New 构造 Store。默认保留 90 个自然日 / 16 MiB；Path 为空时所有读操作返回空结果。
func New(cfg Config) *Store {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	retentionDay := cfg.RetentionDays
	if retentionDay <= 0 {
		retentionDay = defaultRetentionDays
	}
	return &Store{
		path:         strings.TrimSpace(cfg.Path),
		maxBytes:     cfg.MaxBytes,
		retentionDay: retentionDay,
		now:          cfg.Now,
		async:        cfg.Async,
		appQueue:     make(chan []byte, appendQueueSize),
		appClose:     make(chan struct{}),
		appDone:      make(chan struct{}),
		appStart:     sync.Once{},
	}
}

// startAsync 惰性启动单写协程。只有 Async 且路径非空时需要。
func (s *Store) startAsync() {
	if s == nil || !s.async || s.path == "" {
		return
	}
	s.appStart.Do(func() { go s.appendLoop() })
}

// appendLoop 把队列中的行合并成有界批次，追加一次并 fsync 一次。队列满时
// Append 会直接返回 ErrQueueFull（调度器侧静默），不会阻塞任务。
func (s *Store) appendLoop() {
	defer close(s.appDone)
	t := time.NewTicker(appendFlushInterval)
	defer t.Stop()
	batch := make([]byte, 0, 16<<10)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.appendBatch(batch); err != nil {
			s.setErr(err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case line, ok := <-s.appQueue:
			if !ok {
				flush()
				return
			}
			// 每轮最多合并 256 行/256 KiB：单次 fsync 有限，且不会让一个
			// 已经堆积的大批数据无限推迟写入。
			batch = append(batch, line...)
			for n := 1; n < appendBatchMax && len(batch) < 256<<10; n++ {
				select {
				case line, ok = <-s.appQueue:
					if !ok {
						flush()
						return
					}
					batch = append(batch, line...)
				default:
					break
				}
			}
			flush()
		case <-t.C:
			flush()
		case <-s.appClose:
			for {
				select {
				case line, ok := <-s.appQueue:
					if !ok {
						flush()
						return
					}
					batch = append(batch, line...)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Path 返回实际写入路径；空 Store 返回空串。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Append 追加一条记录。失败只返回 error，不 panic，也不改变调用方状态。
func (s *Store) Append(rec Record) error {
	if s == nil || s.path == "" {
		return nil
	}
	rec = normalizeRecord(rec, s.now())
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if s.async {
		s.startAsync()
		// 关闭后不能再入队。这里只做快速判定；入队本身是非阻塞的，不需要把磁盘
		// I/O 或消费者锁带进来。
		s.appMu.Lock()
		closed := s.appClosed
		s.appMu.Unlock()
		if closed {
			atomic.AddUint64(&s.dropped, 1)
			return ErrQueueFull
		}
		// 关闭与入队的竞态由消费者排空协议兜底：Close 先置 appClosed，再关闭
		// appClose；appendLoop 收到关闭后会把队列中已经入队的行全部写完。因此
		// Append 在这里返回 nil 时，记录要么被消费者取走，要么在 Close 的排空
		// 阶段被写入；绝不会停留在无人消费的队列里。
		select {
		case s.appQueue <- line:
			return nil
		default:
			atomic.AddUint64(&s.dropped, 1)
			return ErrQueueFull
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), defaultDirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(line)
	if writeErr == nil && n != len(line) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}

	// 整理失败不能把已经成功落盘的一行当作写入失败。
	var size int64
	if fi, statErr := os.Stat(s.path); statErr == nil {
		size = fi.Size()
	}
	s.maybeCompactLocked(size)
	return nil
}

// appendBatch 在写锁内追加一批行并 fsync 一次；随后按需整理。
func (s *Store) appendBatch(batch []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), defaultDirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(batch)
	if writeErr == nil && n != len(batch) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	var size int64
	if fi, statErr := os.Stat(s.path); statErr == nil {
		size = fi.Size()
	}
	s.maybeCompactLocked(size)
	return nil
}

// Close 停止异步写协程并排空队列。同步模式无需调用；可重复调用。
// 返回最后一次写入错误（若有），但不会因错误阻塞退出。
func (s *Store) Close() error {
	if s == nil || !s.async || s.path == "" {
		return nil
	}
	s.startAsync()
	s.closeOnce.Do(func() {
		// 先置关闭标记，再让 appendLoop 排空。Append 读该标记后仍可能把一条
		// 记录送入队列；appendLoop 在收到 appClose 后会继续消费到空，所以
		// 这类在途入队也会落盘。标记只用极短的锁切换，不覆盖磁盘 I/O。
		s.appMu.Lock()
		s.appClosed = true
		s.appMu.Unlock()
		close(s.appClose)
	})
	<-s.appDone
	s.appMu.Lock()
	defer s.appMu.Unlock()
	return s.appErr
}

// Dropped 返回异步队列满时丢弃的记录数（仅观测用）。
func (s *Store) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return atomic.LoadUint64(&s.dropped)
}

func (s *Store) setErr(err error) {
	s.appMu.Lock()
	s.appErr = err
	s.appMu.Unlock()
}

// Compact 立即执行一次保留策略整理，返回保留与丢弃行数。
func (s *Store) Compact() (kept, dropped int, err error) {
	if s == nil || s.path == "" {
		return 0, 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.lastCompact = s.now()
	return s.compactLocked()
}

// Query 读取历史，最新写入的在前。坏行跳过；调用方只能拿到白名单字段。
func (s *Store) Query(f Filter) (QueryResult, error) {
	if s == nil || s.path == "" {
		limit := normalizeLimit(f.Limit)
		return QueryResult{Limit: limit}, nil
	}
	limit := normalizeLimit(f.Limit)
	resultMode, err := normalizeResult(f.Result)
	if err != nil {
		return QueryResult{}, err
	}
	if f.Task != "" && !validTask(f.Task) {
		return QueryResult{}, fmt.Errorf("%w: task=%q", ErrInvalidFilter, f.Task)
	}

	uid := strings.TrimSpace(f.UID)
	query := strings.ToLower(strings.TrimSpace(f.Query))
	key := queryCacheKey{
		uid: uid, task: string(f.Task), result: resultMode, query: query,
		since: f.Since.UnixNano(), until: f.Until.UnixNano(),
	}
	size, modTime := s.fileSignature()
	now := time.Now()
	for {
		s.queryCacheMu.Lock()
		if entry, ok := s.queryCache[key]; ok && cachedWindowCovers(entry, limit) && now.Sub(entry.at) < queryCacheTTL &&
			entry.size == size && entry.modUnix == modTime.Unix() && entry.modNanos == int64(modTime.Nanosecond()) {
			out := sliceQueryWindow(entry.result, limit)
			s.queryCacheMu.Unlock()
			return out, nil
		}
		if wait, ok := s.queryFlight[key]; ok {
			s.queryCacheMu.Unlock()
			<-wait
			size, modTime = s.fileSignature()
			now = time.Now()
			continue
		}
		if s.queryFlight == nil {
			s.queryFlight = make(map[queryCacheKey]chan struct{})
		}
		wait := make(chan struct{})
		s.queryFlight[key] = wait
		s.queryCacheMu.Unlock()
		defer func() {
			s.queryCacheMu.Lock()
			delete(s.queryFlight, key)
			close(wait)
			s.queryCacheMu.Unlock()
		}()
		break
	}

	s.queryMu.RLock()

	// 一次扫描保留本次 limit 的最新窗口；同一筛选条件的小 limit 可以直接
	// 从已缓存窗口截片，大 limit 则重新扩大窗口。文件追加序天然是时间序。
	windowSize := limit
	if windowSize > maxLimit {
		windowSize = maxLimit
	}
	matched := make([]Record, 0, minInt(windowSize, 16))
	start, count := 0, 0
	total := 0
	err = s.scanLocked(func(rec Record) {
		if uid != "" && rec.UID != uid {
			return
		}
		if f.Task != "" && rec.Task != f.Task {
			return
		}
		if query != "" && !strings.Contains(strings.ToLower(rec.Nickname+" "+rec.Message), query) {
			return
		}
		if resultMode != "" && rec.OK != (resultMode == "ok") {
			return
		}
		if !inTimeRange(rec.Time, f.Since, f.Until) {
			return
		}
		total++
		if count < windowSize {
			if len(matched) == cap(matched) {
				next := cap(matched) * 2
				if next == 0 {
					next = 1
				}
				if next > windowSize {
					next = windowSize
				}
				grown := make([]Record, len(matched), next)
				copy(grown, matched)
				matched = grown
			}
			matched = append(matched, rec)
			count++
			return
		}
		matched[start] = rec
		start = (start + 1) % windowSize
	})
	s.queryMu.RUnlock()
	if err != nil {
		return QueryResult{}, err
	}
	// 环形窗口的起点是最旧一条；从末尾向前展开即读取契约要求的「最新在前」。
	records := make([]Record, count)
	for i := 0; i < count; i++ {
		if count < windowSize {
			records[i] = matched[count-1-i]
		} else {
			records[i] = matched[(start+count-1-i)%windowSize]
		}
	}
	result := QueryResult{Records: records, Total: total, Limit: windowSize}
	// 扫描期间文件若发生变化（异步落盘/整理），不缓存这份混合快照；下次查询
	// 重新读取即可。签名一致时结果对应同一份文件内容，可直接复用。
	if endSize, endMod := s.fileSignature(); endSize == size && endMod.Equal(modTime) {
		s.queryCacheMu.Lock()
		if s.queryCache == nil {
			s.queryCache = make(map[queryCacheKey]queryCacheEntry)
		}
		s.pruneQueryCacheLocked(time.Now())
		// 只在缓存窗口足够大时命中。若小窗口缓存只剩 200 条，而本次请求
		// limit=600，sliceQueryWindow 会错误返回 200 条；必须重扫扩大窗口。
		if old, ok := s.queryCache[key]; !ok || !cachedWindowCovers(old, limit) {
			s.queryCache[key] = queryCacheEntry{result: result, at: time.Now(), size: size, modUnix: modTime.Unix(), modNanos: int64(modTime.Nanosecond())}
		}
		s.queryCacheMu.Unlock()
	}
	return sliceQueryWindow(result, limit), nil
}

// sliceQueryWindow 从已缓存的最新窗口取前 limit 条。缓存窗口可能小于
// 请求 limit（超过 maxLimit 时），但 API 的 limit 本身已钳到 maxLimit；
// Total 保持完整命中数，调用方据此判断 has_more。
func sliceQueryWindow(result QueryResult, limit int) QueryResult {
	out := result
	out.Limit = limit
	if len(out.Records) > limit {
		out.Records = append([]Record(nil), out.Records[:limit]...)
	} else {
		out.Records = append([]Record(nil), out.Records...)
	}
	return out
}

// cachedWindowCovers 判断缓存窗口是否足够回答 limit 条。命中总数不足 limit
// 时，缓存里已经是全部匹配记录，短窗口同样可复用，不能因 len < limit 再扫整文件。
// 读取侧仍调用 sliceQueryWindow 精确截断并返回请求的 Limit。
func cachedWindowCovers(entry queryCacheEntry, limit int) bool {
	return len(entry.result.Records) >= limit || len(entry.result.Records) >= entry.result.Total
}

func (s *Store) fileSignature() (int64, time.Time) {
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0, time.Time{}
	}
	return fi.Size(), fi.ModTime()
}

// Signature 返回当前历史文件的内容版本（大小 + mtime）。面板缓存派生索引
// 时用它判断底层活动记录是否变化；文件不存在返回零值。
func (s *Store) Signature() (int64, time.Time) {
	if s == nil || s.path == "" {
		return 0, time.Time{}
	}
	s.queryMu.RLock()
	defer s.queryMu.RUnlock()
	return s.fileSignature()
}

// pruneQueryCacheLocked 丢弃过期条目，并在仍超限时按最旧写入时间淘汰。
// 调用方必须持有 s.queryCacheMu。飞行中的 key 不会被淘汰，避免等待者醒来后
// 读不到本轮结果。
func (s *Store) pruneQueryCacheLocked(now time.Time) {
	for k, entry := range s.queryCache {
		if now.Sub(entry.at) >= queryCacheTTL {
			delete(s.queryCache, k)
		}
	}
	for len(s.queryCache) >= queryCacheMax {
		var oldestKey queryCacheKey
		var oldestAt time.Time
		first := true
		for k, entry := range s.queryCache {
			// 飞行中的条目可能被等待者引用，不能删；跳过它继续找其他过期项，
			// 否则最旧项恰好飞行中时会直接放弃淘汰，缓存可超过 queryCacheMax。
			if _, inFlight := s.queryFlight[k]; inFlight {
				continue
			}
			if first || entry.at.Before(oldestAt) {
				oldestKey, oldestAt, first = k, entry.at, false
			}
		}
		if first {
			return
		}
		delete(s.queryCache, oldestKey)
	}
}

func (s *Store) maybeCompactLocked(size int64) {
	now := s.now()
	due := s.lastCompact.IsZero() || now.Sub(s.lastCompact) >= compactInterval || (s.maxBytes > 0 && size > s.maxBytes)
	if !due {
		return
	}
	s.lastCompact = now
	if _, _, err := s.compactLocked(); err != nil {
		// 失败不能把 lastCompact 留在“刚刚成功”的位置，否则接下来一小时都不再
		// 重试；文件可能持续超过硬上限。下一次 Append 再试即可。
		s.lastCompact = time.Time{}
	}
}

func (s *Store) compactLocked() (kept, dropped int, err error) {
	rawBytes, statErr := s.rawSizeLocked()
	if statErr != nil {
		return 0, 0, statErr
	}
	now := s.now()
	cutoff := startOfLocalDay(now).AddDate(0, 0, -(s.retentionDay - 1))

	// 一次扫描把保留窗口内的记录写成临时文件，同时只记录每行的长度。这样不需要
	// 把整份历史解码到 []Record，也不需要保存全部行的副本；字节上限裁剪时按长度
	// 截掉最旧的若干行，再原子替换。
	tmp, err := s.compactTempLocked(cutoff)
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(tmp.name)
	if tmp.kept == 0 {
		// decoded==0 表示没有可解码记录（全是坏行）；只有文件超过字节上限时才需要
		// 重写，否则保留原文件，避免每次整理都无谓改写坏行文件。
		if tmp.decoded > 0 || (rawBytes > 0 && s.maxBytes > 0 && rawBytes > s.maxBytes) {
			if rewriteErr := s.rewriteLocked(nil); rewriteErr != nil {
				return 0, 0, rewriteErr
			}
			return 0, tmp.decoded, nil
		}
		return 0, 0, nil
	}

	start := 0
	var offset int64
	if s.maxBytes > 0 {
		for start < len(tmp.lines) && tmp.totalBytes > s.maxBytes {
			tmp.totalBytes -= tmp.lines[start].size
			offset += tmp.lines[start].size
			start++
		}
	}
	kept = len(tmp.lines) - start
	dropped = tmp.decoded - kept

	if err := s.commitTempLocked(tmp.file, tmp.name, offset); err != nil {
		return 0, 0, err
	}
	return kept, dropped, nil
}

type compactLine struct {
	size int64
}

type compactTemp struct {
	file       *os.File
	name       string
	lines      []compactLine
	totalBytes int64
	kept       int
	decoded    int
}

// compactTempLocked 把时间窗口内仍有效的记录写入同目录临时文件。坏行、过期行和
// 无法重新编码的行都不会进入临时文件；返回的 lines 只记录偏移与长度，内存与保留
// 条数同阶，但与文件字节数无关。
func (s *Store) compactTempLocked(cutoff time.Time) (*compactTemp, error) {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".activity-*.tmp")
	if err != nil {
		return nil, err
	}
	out := &compactTemp{file: tmp, name: tmp.Name()}
	cleanup := func(err error) (*compactTemp, error) {
		tmp.Close()
		os.Remove(out.name)
		return nil, err
	}
	if err := tmp.Chmod(defaultFilePerm); err != nil {
		return cleanup(err)
	}
	bw := bufio.NewWriterSize(tmp, 64<<10)
	var written countingWriter
	written.w = bw
	enc := json.NewEncoder(&written)
	err = s.scanLocked(func(rec Record) {
		out.decoded++
		if !rec.Time.IsZero() && rec.Time.Before(cutoff) {
			return
		}
		written.n = 0
		if encodeErr := enc.Encode(rec); encodeErr != nil {
			if err == nil {
				err = encodeErr
			}
			return
		}
		out.lines = append(out.lines, compactLine{size: written.n})
		out.totalBytes += written.n
		out.kept++
	})
	if err != nil {
		return cleanup(err)
	}
	if err := bw.Flush(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	return out, nil
}

// countingWriter 统计 json.Encoder 实际写出的字节数。整理时需要按行长度做
// 字节上限裁剪，但让 Encoder 直接写 bufio 可以复用其内部编码缓冲，避免每条
// 记录先生成一份 []byte 再复制进写缓冲。
type countingWriter struct {
	w *bufio.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// commitTempLocked 跳过临时文件开头 offset 字节，把剩余内容原子替换到正式文件。
// 只有临时文件，不存在读者看到半份正式文件的窗口；真正 rename 时才持查询读锁。
func (s *Store) commitTempLocked(src *os.File, srcName string, offset int64) error {
	if offset > 0 {
		// 保留区间在临时文件中本来就是连续后缀；直接 rename 后再用 ftruncate
		// 去掉前面的旧字节并不可行（会把后缀一起截掉），因此在同一文件内把
		// 后缀搬到开头。bufio 只占固定大小缓冲，不复制整份历史到第二份文件。
		if err := truncatePrefix(src, offset); err != nil {
			src.Close()
			return err
		}
	}
	if err := src.Close(); err != nil {
		return err
	}
	s.queryMu.Lock()
	defer s.queryMu.Unlock()
	return os.Rename(srcName, s.path)
}

// truncatePrefix 把文件中 [offset, EOF) 的字节搬到文件开头，并截断尾部。
// f 以读写方式打开，但这里必须用独立的只读句柄读取后缀：同一句柄的读写偏移
// 共享，Seek 到 0 后写的第一个字节会覆盖尚未读到的源数据；更糟的是从 offset
// 读完后文件偏移已在 EOF，再 Seek(0) 读同一句柄会把已覆盖内容当成后缀，最终
// 丢掉最新记录。独立句柄也避免 bufio.Reader 预读与写入交错。
func truncatePrefix(f *os.File, offset int64) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if offset <= 0 {
		return nil
	}
	if offset >= info.Size() {
		return f.Truncate(0)
	}
	br := bufio.NewReaderSize(io.NewSectionReader(f, offset, info.Size()-offset), 64<<10)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	if _, err := io.Copy(bw, br); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Truncate(info.Size() - offset); err != nil {
		return err
	}
	// 搬移后再次 fsync：掉电时临时文件若只元数据落盘，rename 后可能出现空洞/旧
	// 尾部。commitTempLocked 在成功后才 rename，这里的 Sync 必须早于它。
	return f.Sync()
}

func (s *Store) rewriteLocked(lines [][]byte) error {
	// 与查询的短读锁互斥：rename 前后都不能让读取看到半份文件（rename 本身原子，
	// 但把「读文件」与「替换文件」串行化后，读取语义更直观，也避免旧句柄读到
	// 被截断的中间态）。
	s.queryMu.Lock()
	defer s.queryMu.Unlock()
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".activity-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(defaultFilePerm); err != nil {
		tmp.Close()
		return err
	}
	for _, line := range lines {
		if _, err := tmp.Write(line); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *Store) rawSizeLocked() (int64, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, nil
	}
	return fi.Size(), nil
}

// scanLocked 按文件追加顺序（最旧 → 最新）逐行解码；坏行跳过。
// 调用方必须已持有 s.mu（读锁或写锁）。读取器不缓存整份文件。
func (s *Store) scanLocked(fn func(Record)) error {
	if fi, statErr := os.Stat(s.path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return statErr
	} else if !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	// ReadSlice 直接返回读缓冲中的切片，正常行不产生逐行分配；超长行才把分片
	// 拼进 scratch。decodeRecord 在回调内立即完成解析，不保留切片，因此无需复制。
	br := bufio.NewReaderSize(f, 64*1024)
	var scratch []byte
	for {
		line, readErr := br.ReadSlice('\n')
		if readErr == bufio.ErrBufferFull {
			if cap(scratch) < len(line) {
				scratch = make([]byte, 0, len(line)*2)
			} else {
				scratch = scratch[:0]
			}
			scratch = append(scratch, line...)
			for readErr == bufio.ErrBufferFull {
				line, readErr = br.ReadSlice('\n')
				scratch = append(scratch, line...)
			}
			line = scratch
		}
		if len(line) > 0 {
			if rec, ok := decodeRecord(line); ok {
				fn(rec)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	return nil
}

func decodeRecord(line []byte) (Record, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Record{}, false
	}
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Record{}, false
	}
	if strings.TrimSpace(rec.UID) == "" || !validTask(rec.Task) {
		return Record{}, false
	}
	return normalizeRecord(rec, rec.Time), true
}

func normalizeRecord(rec Record, fallbackTime time.Time) Record {
	if rec.Time.IsZero() {
		rec.Time = fallbackTime
	}
	// 历史只用于运维定位：URI 一律截断为 uid8，绝不落完整账号标识。
	rawUID := strings.TrimSpace(rec.UID)
	rec.UID = logfmt.UID8(rawUID)
	rec.Nickname = strings.TrimSpace(rec.Nickname)
	rec.Realm = strings.ToLower(strings.TrimSpace(rec.Realm))
	if rec.Realm == "" {
		rec.Realm = "cn"
	}
	rec.Task = Task(strings.ToLower(strings.TrimSpace(string(rec.Task))))
	switch strings.ToLower(strings.TrimSpace(string(rec.Trigger))) {
	case string(TriggerSchedule):
		rec.Trigger = TriggerSchedule
	case string(TriggerManual):
		rec.Trigger = TriggerManual
	default:
		rec.Trigger = TriggerUnknown
	}
	rec.Message = sanitizeMessage(rec.Message)
	// 动作消息里常带账号标识（上游错误、请求回显等）。只截 UID 字段不够：
	// 完整值可能已经复制进 Message，落盘/接口响应仍会泄露。先做敏感串清理，
	// 再把原文中出现的长 UID 收敛成 uid8；短 UID 本身没有隐私价值，替换它
	// 反而会把普通数字（如耗时、数量）误伤成半截标识。
	if len(rawUID) > 8 && rec.Message != "" {
		rec.Message = strings.ReplaceAll(rec.Message, rawUID, logfmt.UID8(rawUID))
	}
	return rec
}

func sanitizeMessage(msg string) string {
	msg = normalizeMessageSpace(msg)
	// 先脱敏再截断：否则长前缀会先把 key=value 的敏感尾段切掉前一半，
	// 留下半个凭证或让测试/审计无法确认敏感串已消失。
	// 大多数消息不含任何敏感关键字；先做一次大小写无关的廉价预检，避免两次
	// 正则替换在普通消息上产生临时字符串。
	if hasSensitiveHint(msg) {
		msg = bearerRE.ReplaceAllString(msg, "$1 [redacted]")
		msg = secretKVRE.ReplaceAllString(msg, "$1=[redacted]")
	}
	return logfmt.Truncate(msg, maxMessageBytes)
}

// normalizeMessageSpace 与 strings.Join(strings.Fields(msg), " ") 等价，但常见
// 的「已被 TrimSpace、且只有单个 ASCII 空格」消息直接返回原切片，避免 Fields
// 为每条记录分配一个 []string。
func normalizeMessageSpace(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return msg
	}
	prevSpace := false
	multibyte := false
	i := 0
	for i < len(msg) {
		c := msg[i]
		if c < utf8.RuneSelf {
			switch c {
			case ' ':
				if prevSpace || i == 0 || i == len(msg)-1 {
					multibyte = true
				}
				prevSpace = true
			case '\t', '\n', '\r', '\v', '\f':
				multibyte = true
			default:
				prevSpace = false
			}
			i++
		} else {
			r, size := utf8.DecodeRuneInString(msg[i:])
			if unicode.IsSpace(r) {
				multibyte = true
			}
			prevSpace = false
			i += size
		}
		if multibyte {
			break
		}
	}
	if !multibyte {
		return msg
	}
	return strings.Join(strings.Fields(msg), " ")
}

func hasSensitiveHint(msg string) bool {
	// 先用不分配的小写化与少量子串命中；只有命中才跑两条脱敏正则。
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "bearer ") ||
		strings.Contains(lower, "basic ") ||
		strings.Contains(lower, "access_token") ||
		strings.Contains(lower, "access-token") ||
		strings.Contains(lower, "refresh_token") ||
		strings.Contains(lower, "refresh-token") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "cookie") ||
		strings.Contains(lower, "password") ||
		strings.Contains(lower, "secret")
}

func validTask(task Task) bool {
	switch task {
	case TaskCheckin, TaskActivity, TaskTravel, TaskKeepalive, TaskDailyChat:
		return true
	default:
		return false
	}
}

func normalizeResult(result string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "", "all":
		return "", nil
	case "ok", "success", "succeeded":
		return "ok", nil
	case "failed", "fail", "failure":
		return "failed", nil
	default:
		return "", fmt.Errorf("%w: result=%q", ErrInvalidFilter, result)
	}
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func inTimeRange(ts, since, until time.Time) bool {
	if since.IsZero() && until.IsZero() {
		return true
	}
	if ts.IsZero() {
		return false
	}
	if !since.IsZero() && ts.Before(since) {
		return false
	}
	if !until.IsZero() && ts.After(until) {
		return false
	}
	return true
}

func startOfLocalDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FileName 返回默认历史文件名，便于调用方与数据目录拼接。
func FileName() string { return activityHistoryName }

// ValidateMessageUTF8 仅用于测试/诊断：报告字符串是否为合法 UTF-8。
func ValidateMessageUTF8(s string) bool { return utf8.ValidString(s) }
