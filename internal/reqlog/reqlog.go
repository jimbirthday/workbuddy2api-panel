// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
	// pruneInterval 是归档保留策略的检查周期。此前随 1 秒 flush ticker 每次都
	// ReadDir + Stat + 排序，即使没有任何新事件也会持续扫目录；保留/容量都是以
	// 天/百 MiB 为尺度的策略，没必要时时检查。新事件写入会触发一次到期检查，
	// 因此流量到来时清理仍然及时。
	pruneInterval = time.Minute
	// archiveStatsTTL 是面板「归档文件数/占用」统计的缓存窗口。归档策略以天/百
	// MiB 为尺度，5 秒的统计延迟不影响运维判断；而每 5 秒对全部归档文件
	// ReadDir + Stat 会在 100 MiB / 多个轮转文件时形成固定 I/O 成本。
	archiveStatsTTL = 5 * time.Second
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	// KeyID 是调用方命中的 API key 条目 id（不含密钥本身）。未鉴权/旧单 key
	// 兼容路径为空；归档、指标与导出只允许出现这个 id。
	KeyID            string  `json:"key_id,omitempty"`
	Account          string  `json:"account,omitempty"`
	Model            string  `json:"model,omitempty"`
	Status           int     `json:"status"`
	OK               bool    `json:"ok"`
	Outcome          string  `json:"outcome"`
	DurationMs       int64   `json:"duration_ms"`
	TTFBMs           int64   `json:"ttfb_ms,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	HasCredit        bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	KeyID     string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// asciiFastPath 判断过滤条件能否走按字节预筛。strings.ToLower 对非 ASCII
// 字符做 Unicode 映射（长度可能变化），此时必须回退完整解码后比较。
func (f Filter) asciiFastPath() bool {
	for _, s := range []string{f.KeyID, f.Account, f.Model, f.ClientIP, f.UserAgent} {
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				return false
			}
		}
	}
	return true
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu sync.Mutex
	// readMu 串行化归档窗口缓存的计算与校验。面板 5 秒轮询、手动刷新与导出
	// 可能并发进来；只缓存「结果 + 目录签名」，命中时完全不读 JSONL。
	readMu      sync.Mutex
	readCache   readCacheEntry
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	// recent 是最近事件的环形缓冲：recentHead 指向下一写入位，recentLen 为有效条数。
	// 旧实现每请求 append([]Event{e}, r.recent...) 复制最多 100 条 Event（含字符串头），
	// 在请求热路径上是无谓的 O(100) 分配；环形写入为 O(1)，只在 Snapshot 时按时间倒序展开。
	recent     []Event
	recentHead int
	recentLen  int
	archive    *archiveWriter

	// perfCache 缓存性能分位的最近一次结果。归档查询要遍历全部 JSONL，
	// 面板刷新/切维度时会重复做同一件事；按查询键 + 目录签名在 2 秒内复用。
	perfMu    sync.Mutex
	perfCache perfCacheEntry
}

const perfCacheTTL = 2 * time.Second

// readCacheEntry 缓存最近一次 ReadArchive 的窗口结果与目录签名。目录签名由
// 文件名/大小/mtime 组成；签名不变说明没有新事件、轮转或清理，直接复用结果。
// 缓存仅保存脱敏 Event，不保存请求正文或凭证。
type readCacheEntry struct {
	key    string
	sig    string
	hit    bool
	events []Event
	limit  int
}

type perfCacheEntry struct {
	key   string
	sig   string
	at    time.Time
	stats PerfStats
	hit   bool
}

// perfDirSignature 用文件名/大小/mtime 表示归档目录版本。归档只在写入、
// 轮转、清理时变化；签名一致即可复用聚合结果，避免每次面板刷新全量重扫。
type perfDirSignature struct {
	sig     string
	entries []os.DirEntry
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	// 导出类型允许零值 Recorder（旧实现靠 append 自动分配）；环形缓冲必须在锁内
	// 懒初始化，否则 `var r Recorder; r.Record(...)` 会在空切片上越界 panic。
	if len(r.recent) != recentLimit {
		r.recent = make([]Event, recentLimit)
		r.recentHead, r.recentLen = 0, 0
	}
	if r.recentLen < recentLimit {
		r.recentLen++
	}
	r.recent[r.recentHead] = e
	r.recentHead = (r.recentHead + 1) % recentLimit
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    r.recentSnapshotLocked(),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// recentSnapshotLocked 把环形缓冲展开成「最新在前」的切片。调用方必须已持 r.mu。
// 条数达到 recentLimit 后 head 指向最旧一条；未满时从 0 开始。
func (r *Recorder) recentSnapshotLocked() []Event {
	if r.recentLen == 0 {
		return nil
	}
	out := make([]Event, r.recentLen)
	start := (r.recentHead - r.recentLen + recentLimit) % recentLimit
	for i := 0; i < r.recentLen; i++ {
		// 环形起点是最旧、顺时针到 head-1 是次新；倒序写入即最新在前。
		out[i] = r.recent[(start+r.recentLen-1-i+recentLimit)%recentLimit]
	}
	return out
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	events, _, err := r.ReadArchiveChanged(limit, filter)
	return events, err
}

// ReadArchiveChanged 与 ReadArchive 同语义，额外返回窗口是否发生了变化。
// 面板轮询用它跳过无变化时的整表重绘；事件内容逐条一致，只是复用了上一次
// 的只读结果快照。changed=false 时调用方应直接保留当前 DOM。
func (r *Recorder) ReadArchiveChanged(limit int, filter Filter) (events []Event, changed bool, err error) {
	return r.readArchiveChanged(limit, filter, true)
}

// ReadArchiveChangedIfChanged 与 ReadArchiveChanged 相同，但窗口未变化时
// 返回 nil 而不是复制整窗事件。面板 5 秒轮询只关心 changed 标志，用它可把
// 稳态分配从 O(窗口行数) 降到 O(1)；需要完整窗口的导出/include_entries=1
// 仍调用 ReadArchiveChanged。
func (r *Recorder) ReadArchiveChangedIfChanged(limit int, filter Filter) (events []Event, changed bool, err error) {
	return r.readArchiveChanged(limit, filter, false)
}

func (r *Recorder) readArchiveChanged(limit int, filter Filter, copyUnchanged bool) (events []Event, changed bool, err error) {
	if r == nil || r.archive == nil {
		return nil, false, nil
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	entries, err := os.ReadDir(r.archive.cfg.Dir)
	if err != nil {
		return nil, false, err
	}
	sig := archiveDirSignature(entries)
	key := archiveReadCacheKey(limit, filter)
	if r.readCache.hit && r.readCache.key == key && r.readCache.sig == sig {
		if !copyUnchanged {
			return nil, false, nil
		}
		// 返回副本，避免调用方追加/截断污染下一次命中。面板默认轮询虽会
		// 丢弃 entries，但导出与 include_entries=1 依赖这里的完整窗口，
		// 省流由 handler 在写响应前统一处理。
		out := append([]Event(nil), r.readCache.events...)
		return out, false, nil
	}
	out, err := r.archive.readEntries(entries, limit, filter)
	if err != nil {
		return out, true, err
	}
	r.readCache = readCacheEntry{key: key, sig: sig, hit: true, events: append([]Event(nil), out...)}
	return out, true, nil
}

// archiveDirSignature 用文件名/大小/mtime 生成目录版本签名。读取窗口只关心
// 归档文件本身；非 requests-*.jsonl 的变化不影响结果。
func archiveDirSignature(entries []os.DirEntry) string {
	files := make([]struct {
		name  string
		size  int64
		mtime int64
	}, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, struct {
			name  string
			size  int64
			mtime int64
		}{name, info.Size(), info.ModTime().UnixNano()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.name)
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(f.size, 10))
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(f.mtime, 10))
		b.WriteByte('\n')
	}
	return b.String()
}

// archiveReadCacheKey 只包含影响结果集的参数；字符串字段用规范化后的值，
// 与 readFileLimit 的匹配口径一致。
func archiveReadCacheKey(limit int, filter Filter) string {
	f := filter.normalized()
	var b strings.Builder
	b.WriteString(strconv.Itoa(limit))
	b.WriteByte('|')
	b.WriteString(f.Outcome)
	b.WriteByte('|')
	b.WriteString(f.KeyID)
	b.WriteByte('|')
	b.WriteString(f.Account)
	b.WriteByte('|')
	b.WriteString(f.Model)
	b.WriteByte('|')
	b.WriteString(f.ClientIP)
	b.WriteByte('|')
	b.WriteString(f.UserAgent)
	b.WriteByte('|')
	b.WriteString(f.From.UTC().Format(time.RFC3339Nano))
	b.WriteByte('|')
	b.WriteString(f.To.UTC().Format(time.RFC3339Nano))
	return b.String()
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64

	statsMu    sync.Mutex
	statsAt    time.Time
	statsFiles int
	statsBytes int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastPrune := time.Now()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
			if time.Since(lastPrune) >= pruneInterval {
				w.prune()
				lastPrune = time.Now()
			}
		case <-ticker.C:
			w.flush()
			if time.Since(lastPrune) >= pruneInterval {
				w.prune()
				lastPrune = time.Now()
			}
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	removed := false
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
				removed = true
			}
		}
	}
	if removed {
		// stats 缓存可能在 prune 前刚统计过；删除文件后必须失效，否则面板会
		// 在 TTL 内继续显示已不存在的归档文件。
		w.invalidateStats()
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	return w.readEntries(entries, limit, filter)
}

// readEntries 在已取得目录条目的前提下读取窗口结果，供缓存命中路径复用
// 同一次 ReadDir 结果。
func (w *archiveWriter) readEntries(entries []os.DirEntry, limit int, filter Filter) ([]Event, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	// 归档文件名固定为 requests-<day>.jsonl / requests-<day>.N.jsonl，N 是当天内的
	// 轮转序号。按「日期倒序、同日序号倒序」排列只是常见的读取优先级，不能据此
	// 推断事件时间后提前停止。不依赖 mtime：同一秒内连续轮转写出的多个文件 mtime 经常
	// 完全相同（Linux 文件时间戳粒度粗），字典序又会把装着最早事件的基准文件
	// requests-<day>.jsonl 排到 requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复
	// 备份后 mtime 更不可信（见 TestReadArchiveOrdersByEventTimeNotFileMtime）。
	names := archiveFileNamesNewestFirst(entries)
	out := make([]Event, 0, limit)
	// 按文件名从新到旧读取只是让常见情况下先拿到较新数据；不能因此提前停止。
	// 文件名中的日期是「写入当天」，不保证等于事件时间：恢复备份、补写历史事件、
	// 手工归档都会让旧文件名里出现更新的事件。若读到 limit 条就 break，
	// 会返回较旧记录并漏掉后面文件里的真正最新记录（见跨文件名乱序测试）。
	// 因此每个文件仍只保留局部最新窗口（内存有界），但必须遍历全部文件；
	// 最终按事件时间倒序取全局最新 limit 条。
	for _, name := range names {
		// 单个文件可能远大于 limit（轮转文件最大 8 MiB）；只需要保留本文件内最新的
		// limit 条即可。文件的物理顺序不保证等于事件时间顺序（见下面的倒序测试），
		// 因此不能读到第一条命中就停，但可以把内存窗口固定为 limit 条。
		//
		// 每个文件固定只取局部最新 limit 条，不能按「还能贡献多少」缩小：
		// 已累计满 limit 时 remaining 为 0，而 readFileLimit(keep=0) 表示不限量，
		// 会让后续大文件把整份命中集读进内存。全局拼接前始终裁到 limit，
		// 因此峰值仍是有界的 2×limit + 单文件读缓冲。
		rows, err := readFileLimit(filepath.Join(w.cfg.Dir, name), filter, limit)
		if err != nil {
			// 与旧实现一致：出错时返回已累积结果 + 错误，不静默吞掉。
			sortArchiveNewestFirst(out)
			return out, err
		}
		out = append(out, rows...)
		if len(out) > limit {
			// 跨文件窗口同样不能增长到 文件数 × limit：保留全局最新 limit 条，
			// 后续文件仍可贡献更晚的事件。
			sortArchiveNewestFirst(out)
			out = out[:limit]
		}
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sortArchiveNewestFirst(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// archiveFileNamesNewestFirst 从归档目录条目中筛出归档文件，按「日期倒序、同日
// 轮转序号倒序」返回，即从可能覆盖最新事件的文件开始。
func archiveFileNamesNewestFirst(entries []os.DirEntry) []string {
	type archFile struct {
		name string
		day  string
		idx  int
	}
	files := make([]archFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day, idx := parseArchiveName(name)
		files = append(files, archFile{name: name, day: day, idx: idx})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].day != files[j].day {
			return files[i].day > files[j].day // 日期倒序：新的在前
		}
		return files[i].idx > files[j].idx // 同日序号倒序：轮转更晚的在前
	})
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.name
	}
	return out
}

// parseArchiveName 从 requests-<day>.jsonl / requests-<day>.N.jsonl 解析出日期与
// 轮转序号。解析失败时返回 (name, 0)，保证文件仍被读到（只是排序位置靠后）。
func parseArchiveName(name string) (day string, idx int) {
	base := strings.TrimPrefix(name, "requests-")
	base = strings.TrimSuffix(base, ".jsonl")
	if i := strings.LastIndex(base, "."); i >= 0 {
		if n, err := strconv.Atoi(base[i+1:]); err == nil {
			return base[:i], n
		}
	}
	return base, 0
}

// sortArchiveNewestFirst 按事件时间倒序排序（同一时刻 Stable 保留落盘先后）。
func sortArchiveNewestFirst(events []Event) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.After(events[j].Time) })
}

func readFile(path string, filter Filter) ([]Event, error) {
	return readFileLimit(path, filter, 0)
}

// readFileLimit 读取一个归档文件，只保留按事件时间最新的 keep 条。
// keep<=0 表示不限制（仅供 readFile 兼容调用）；ReadArchive 热路径始终传入正数。
// 事件时间倒序；同一时刻按落盘顺序（行号）正序输出。实现与「全量 Stable 排序后
// 截断前 keep 条」逐位一致，但只保留 2*keep 的候选窗口，不随文件行数增长。
func readFileLimit(path string, filter Filter, keep int) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	filter = filter.normalized()
	if !filter.asciiFastPath() {
		return readFileLimitSlow(f, filter, keep)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	type seqEvent struct {
		e      Event
		rawOff int
		rawLen int
		seq    int64
	}
	capacity := 16
	if keep > 0 {
		capacity = 2 * keep
		if capacity > 4096 {
			capacity = 4096
		}
	}
	window := make([]seqEvent, 0, capacity)
	// arena 保存尚未解码的原始行。每批窗口裁剪后只把仍存活的行压紧，
	// 已在窗口外淘汰的历史行不需要复制或解码。
	var arena, scratch []byte
	var line int64
	// 每个候选窗口只排序一次即可：本函数只要求按「时间倒序、同刻行号正序」
	// 返回，截断后不需要恢复另一种顺序供后续批次使用，因为 seq 始终保留原始行号。
	sortWindow := func() {
		sort.SliceStable(window, func(i, j int) bool {
			if !window[i].e.Time.Equal(window[j].e.Time) {
				return window[i].e.Time.After(window[j].e.Time)
			}
			return window[i].seq < window[j].seq
		})
	}
	compactWindow := func() {
		sortWindow()
		window = window[:keep]
		need := 0
		for i := range window {
			if window[i].rawLen > 0 {
				need += window[i].rawLen
			}
		}
		if cap(scratch) < need {
			scratch = make([]byte, 0, need)
		} else {
			scratch = scratch[:0]
		}
		for i := range window {
			if window[i].rawLen <= 0 {
				continue
			}
			off := len(scratch)
			scratch = append(scratch, arena[window[i].rawOff:window[i].rawOff+window[i].rawLen]...)
			window[i].rawOff = off
		}
		arena, scratch = scratch, arena[:0]
	}
	// 回退行直接解码到窗口末尾，避免每行都有一个 Event 逃逸到堆上。
	appendDecoded := func(raw []byte, line int64) {
		window = append(window, seqEvent{rawLen: -1, seq: line})
		if json.Unmarshal(raw, &window[len(window)-1].e) != nil {
			window = window[:len(window)-1]
			return
		}
		if !filter.matchNormalized(window[len(window)-1].e) {
			window = window[:len(window)-1]
		}
	}
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		fields, ok := parseArchiveLineFields(raw)
		if ok {
			switch filter.matchArchiveFields(fields) {
			case archiveMatchNo:
				continue
			case archiveMatchFallback:
				ok = false
			}
			if ok {
				// 只暂存字节偏移；完整解码推迟到窗口裁剪之后，避免为不会再
				// 返回的历史行分配字符串与时间对象。
				off := len(arena)
				arena = append(arena, raw...)
				window = append(window, seqEvent{rawOff: off, rawLen: len(raw), seq: line, e: Event{Time: fields.at}})
			}
		}
		if !ok {
			appendDecoded(raw, line)
		}
		if keep > 0 && len(window) >= 2*keep {
			compactWindow()
		}
	}
	sortWindow()
	if keep > 0 && len(window) > keep {
		window = window[:keep]
	}
	out := make([]Event, 0, len(window))
	for i := range window {
		if window[i].rawLen < 0 {
			out = append(out, window[i].e)
			continue
		}
		var e Event
		raw := arena[window[i].rawOff : window[i].rawOff+window[i].rawLen]
		if json.Unmarshal(raw, &e) != nil {
			// 预筛接受的行必须与完整解码同样成功；出现意外时回退旧路径，
			// 宁可慢也不能返回与全量语义不一致的结果。
			return readFileLimitSlow(f, filter, keep)
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

// archiveLineFields 是归档 JSONL 行中参与「时间窗口 + 过滤」的顶层字段。
// at 是零拷贝解析出的时间；set 标记该字段是否可控地解析成功。
type archiveLineFields struct {
	at        time.Time
	outcome   []byte
	keyID     []byte
	account   []byte
	model     []byte
	clientIP  []byte
	userAgent []byte

	atSet       bool
	outcomeSet  bool
	keyIDSet    bool
	accountSet  bool
	modelSet    bool
	clientIPSet bool
	uaSet       bool
}

// archiveMatch 表示预筛结果。fallback 意味着该行必须交给 encoding/json
// 完整解码，不能按字节字段直接判定。
type archiveMatch uint8

const (
	archiveMatchNo archiveMatch = iota
	archiveMatchYes
	archiveMatchFallback
)

// parseArchiveLineFields 解析归档行的顶层字段，返回的切片与 raw 共享底层字节。
// 只用于当前行的预筛，不把结果返回给调用方。
// 只接受单行、无转义字符串与标准 RFC3339 时间；其余情况返回 !ok，由调用方
// 回退到完整 json.Unmarshal。这样性能路径不会扩大原有语义。
func parseArchiveLineFields(raw []byte) (archiveLineFields, bool) {
	raw = bytesTrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return archiveLineFields{}, false
	}
	var out archiveLineFields
	seen := uint32(0)
	rest := raw[1 : len(raw)-1]
	for len(rest) > 0 {
		rest = bytesTrimSpace(rest)
		if len(rest) == 0 {
			break
		}
		if rest[0] != '"' {
			return archiveLineFields{}, false
		}
		key, n, ok := scanJSONStringNoEscape(rest)
		if !ok {
			return archiveLineFields{}, false
		}
		rest = bytesTrimSpace(rest[n:])
		if len(rest) == 0 || rest[0] != ':' {
			return archiveLineFields{}, false
		}
		rest = bytesTrimSpace(rest[1:])
		val, n, ok := scanJSONValue(rest)
		if !ok {
			return archiveLineFields{}, false
		}
		switch string(key) {
		case "time":
			t, ok := parseRFC3339Bytes(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.at, out.atSet = t, true
			seen |= 1 << 0
		case "outcome":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.outcome, out.outcomeSet = s, true
			seen |= 1 << 1
		case "key_id":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.keyID, out.keyIDSet = s, true
			seen |= 1 << 2
		case "account":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.account, out.accountSet = s, true
			seen |= 1 << 3
		case "model":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.model, out.modelSet = s, true
			seen |= 1 << 4
		case "client_ip":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.clientIP, out.clientIPSet = s, true
			seen |= 1 << 5
		case "user_agent":
			s, ok := unquoteSimple(val)
			if !ok {
				return archiveLineFields{}, false
			}
			out.userAgent, out.uaSet = s, true
			seen |= 1 << 6
		}
		rest = bytesTrimSpace(rest[n:])
		if len(rest) == 0 {
			break
		}
		if rest[0] != ',' {
			return archiveLineFields{}, false
		}
		rest = rest[1:]
		if len(bytesTrimSpace(rest)) == 0 {
			// 尾逗号不是合法 JSON；旧路径会跳过该行，快速路径也不能接受。
			return archiveLineFields{}, false
		}
	}
	// 必须包含 Event.Time/json 的必选字段；其余字段缺失等价于零值。
	if seen&(1<<0) == 0 || seen&(1<<1) == 0 {
		return archiveLineFields{}, false
	}
	return out, true
}

// matchArchiveFields 用预解析字段执行与 matchNormalized 相同的筛选。
// 返回 fallback 表示该行含缺失/异常字段，必须完整解码以确保与旧语义一致。
func (f Filter) matchArchiveFields(v archiveLineFields) archiveMatch {
	if f.Outcome != "" {
		if !v.outcomeSet {
			return archiveMatchNo
		}
		if !isASCII(v.outcome) {
			return archiveMatchFallback
		}
		if string(v.outcome) != f.Outcome {
			return archiveMatchNo
		}
	}
	if f.KeyID != "" {
		if !v.keyIDSet {
			return archiveMatchNo
		}
		if !isASCII(v.keyID) {
			return archiveMatchFallback
		}
		if !containsFoldLowerASCII(v.keyID, f.KeyID) {
			return archiveMatchNo
		}
	}
	if f.Account != "" {
		if !v.accountSet {
			return archiveMatchNo
		}
		if !isASCII(v.account) {
			return archiveMatchFallback
		}
		if !containsFoldLowerASCII(v.account, f.Account) {
			return archiveMatchNo
		}
	}
	if f.Model != "" {
		if !v.modelSet {
			return archiveMatchNo
		}
		if !isASCII(v.model) {
			return archiveMatchFallback
		}
		if !containsFoldLowerASCII(v.model, f.Model) {
			return archiveMatchNo
		}
	}
	if f.ClientIP != "" {
		if !v.clientIPSet {
			return archiveMatchNo
		}
		if !isASCII(v.clientIP) {
			return archiveMatchFallback
		}
		if !containsFoldLowerASCII(v.clientIP, f.ClientIP) {
			return archiveMatchNo
		}
	}
	if f.UserAgent != "" {
		if !v.uaSet {
			return archiveMatchNo
		}
		if !isASCII(v.userAgent) {
			return archiveMatchFallback
		}
		if !containsFoldLowerASCII(v.userAgent, f.UserAgent) {
			return archiveMatchNo
		}
	}
	if !f.From.IsZero() && v.at.Before(f.From) {
		return archiveMatchNo
	}
	if !f.To.IsZero() && v.at.After(f.To) {
		return archiveMatchNo
	}
	return archiveMatchYes
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

func containsFoldLowerASCII(haystack []byte, needleLower string) bool {
	if needleLower == "" {
		return true
	}
	if len(needleLower) > len(haystack) {
		return false
	}
	for i := 0; i+len(needleLower) <= len(haystack); i++ {
		j := 0
		for ; j < len(needleLower); j++ {
			c := haystack[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needleLower[j] {
				break
			}
		}
		if j == len(needleLower) {
			return true
		}
	}
	return false
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

func scanJSONStringNoEscape(b []byte) ([]byte, int, bool) {
	if len(b) < 2 || b[0] != '"' {
		return nil, 0, false
	}
	for i := 1; i < len(b); i++ {
		switch b[i] {
		case '\\':
			return nil, 0, false
		case '"':
			return b[1:i], i + 1, true
		}
	}
	return nil, 0, false
}

func unquoteSimple(b []byte) ([]byte, bool) {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return nil, false
	}
	for i := 1; i < len(b)-1; i++ {
		if b[i] == '\\' || b[i] == '"' {
			return nil, false
		}
	}
	return b[1 : len(b)-1], true
}

func scanJSONValue(b []byte) ([]byte, int, bool) {
	if len(b) == 0 {
		return nil, 0, false
	}
	if b[0] == '"' {
		for i := 1; i < len(b); i++ {
			switch b[i] {
			case '\\':
				i++
			case '"':
				return b[:i+1], i + 1, true
			}
		}
		return nil, 0, false
	}
	depth := 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return b[:i], i, true
			}
			depth--
		case ',':
			if depth == 0 {
				return b[:i], i, true
			}
		}
	}
	return b, len(b), true
}

func parseRFC3339Bytes(b []byte) (time.Time, bool) {
	s, ok := unquoteSimple(b)
	if !ok {
		return time.Time{}, false
	}
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(s))
	return t, err == nil
}

// readFileLimitSlow 是旧的全量解码路径，供包含非 ASCII 过滤词时回退使用。
// 非 ASCII 的 strings.ToLower 可能做 Unicode 大小写映射（甚至改变长度），
// 不能与下面的 ASCII 快速预筛共用同一套字节匹配规则。
func readFileLimitSlow(f *os.File, filter Filter, keep int) ([]Event, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	type seqEvent struct {
		e   Event
		seq int64
	}
	capacity := 16
	if keep > 0 {
		capacity = 2 * keep
		if capacity > 4096 {
			capacity = 4096
		}
	}
	window := make([]seqEvent, 0, capacity)
	var line int64
	sortWindow := func() {
		sort.SliceStable(window, func(i, j int) bool {
			if !window[i].e.Time.Equal(window[j].e.Time) {
				return window[i].e.Time.After(window[j].e.Time)
			}
			return window[i].seq < window[j].seq
		})
	}
	for scanner.Scan() {
		line++
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.matchNormalized(e) {
			continue
		}
		window = append(window, seqEvent{e: e, seq: line})
		if keep > 0 && len(window) >= 2*keep {
			sortWindow()
			window = window[:keep]
		}
	}
	sortWindow()
	if keep > 0 && len(window) > keep {
		window = window[:keep]
	}
	out := make([]Event, len(window))
	for i := range window {
		out[i] = window[i].e
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	return f.normalized().matchNormalized(e)
}

// normalized 返回把字符串 needle 预先转小写的副本。readFile 对每个文件只算一次，
// 避免每一行、每个字段都重复 ToLower（归档大/过滤条件多时是明显的 CPU 放大）。
func (f Filter) normalized() Filter {
	f.KeyID = strings.ToLower(f.KeyID)
	f.Account = strings.ToLower(f.Account)
	f.Model = strings.ToLower(f.Model)
	f.ClientIP = strings.ToLower(f.ClientIP)
	f.UserAgent = strings.ToLower(f.UserAgent)
	return f
}

// containsFoldLower 与 containsFold 同义，但 needle 已是小写，省一次 ToLower。
func containsFoldLower(haystack, needleLower string) bool {
	if needleLower == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), needleLower)
}

// matchNormalized 与 match 同义，但要求 f 已经 normalized() 预处理。
func (f Filter) matchNormalized(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFoldLower(e.KeyID, f.KeyID) {
		return false
	}
	if !containsFoldLower(e.Account, f.Account) {
		return false
	}
	if !containsFoldLower(e.Model, f.Model) {
		return false
	}
	if !containsFoldLower(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFoldLower(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	w.statsMu.Lock()
	if !w.statsAt.IsZero() && time.Since(w.statsAt) < archiveStatsTTL {
		s.Files, s.Bytes = w.statsFiles, w.statsBytes
		w.statsMu.Unlock()
		return s
	}
	w.statsMu.Unlock()

	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	var files int
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			files++
			bytes += info.Size()
		}
	}
	w.statsMu.Lock()
	w.statsAt, w.statsFiles, w.statsBytes = time.Now(), files, bytes
	w.statsMu.Unlock()
	s.Files, s.Bytes = files, bytes
	return s
}

// invalidateStats 让下一次 Snapshot 重新统计归档目录。调用方必须已经释放
// statsMu，避免与正在进行的目录扫描交叉持锁。
func (w *archiveWriter) invalidateStats() {
	w.statsMu.Lock()
	w.statsAt = time.Time{}
	w.statsMu.Unlock()
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
