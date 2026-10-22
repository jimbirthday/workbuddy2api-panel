// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 管理面板只接受顶层管理密钥 api_key（Bearer）。客户端业务 key（api_keys[]）
//     永远不能进入管理面板；未配置管理密钥时面板 API 直接 403（fail-closed），
//     避免“只配了客户端 key”就误以为面板已受保护而匿名暴露；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder

	// Activity 账号每日活动历史（nil = 对应接口返回 501）。
	Activity *activity.Store

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex
	// mpPaceOnce/mp 是小程序口径成长任务的「真人节奏」事件调度器状态
	//（见 mppacer.go）：把 45s 级的间隔等待从请求/队列 worker 里摘出来。
	mpPaceOnce sync.Once
	mp         *mpPaceState

	// taskSnapOnce/ts 是账号成长任务列表的短 TTL 快照（见 tasksnapshot.go）：
	// 一次「一键做任务」原本会为每个动作各拉一遍列表，单号 50+ 次上游 GET。
	taskSnapOnce sync.Once
	ts           *taskSnapState

	// benefitRefMu/benefitRefs 是福利页动作的服务端短引用映射。页面只拿到
	// uid8 展示值与不可逆引用，完整 UID 不进入浏览器；引用随每次快照刷新重建，
	// 使已移除账号的旧按钮自然失效，且不会因 uid8 碰撞命中错误账号。
	benefitRefMu sync.Mutex
	benefitRefs  map[string]string

	// batchMu/batchRuns holds the latest manual task/welfare batch per kind.
	// The scheduler owns the real single-flight gate; this state only explains
	// the outcome in the panel (started, running, success/failure counts).
	batchMu   sync.Mutex
	batchRuns map[string]*batchRun

	// bgWG 跟踪面板后台异步任务（扫描、队列、导入等）。StopWaiting 先关
	// bgStop 再等待，保证 Wait 期间不会再有 Add（WaitGroup 不允许 Add 与
	// Wait 并发；关闭时的调度回调可能仍尝试启动成长队列）。没有登记的任务
	// 计入 bgRejected，供日志/测试观测；重复调用 StopWaiting 保持幂等。
	bgMu       sync.Mutex
	bgWG       sync.WaitGroup
	bgStop     bool
	bgRejected int
	// bgCtx/bgCancel 是面板后台作业的统一取消信号：任务链里所有「给上游留
	// 处理时间」的等待都挂在它上面，停机时立刻返回，不会让 docker stop 被
	// 最长的 45s 事件间隔拖住数分钟。
	bgCtx    context.Context
	bgCancel context.CancelFunc

	// 说明：四类「一键全部」批量任务的 per-kind 单飞闸门已下移到
	// scheduler.TryRunBatch（手动与定时共用同一把锁）。此处不再持有副本，
	// 避免两套闸门各自为政导致「手动 + 定时」仍可并发。

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// 全账号扫描后台状态（taskcenter.go）。异步 + 进度轮询，账号多时前端不再
	// 长时间无反馈；scanOnce 单飞，重复点击不会把上游请求翻倍。
	scanOnce sync.Once
	sc       *scanState

	// scanMu/scanCacheAt 记录最近一次完整扫描的完成时刻，供队列预扫在
	// 紧接着扫描后复用同一份任务列表，避免 470 账号再打约 940 次上游请求。
	// 只用时间戳做失效判据；TTL 内也只复用「未出错且账号集合一致」的结果。
	scanMu      sync.Mutex
	scanCacheAt time.Time

	// overviewMu 串行化概览快照构建，overviewSnap/overviewAt 缓存很短的 TTL。
	// 面板 5 秒轮询、手动刷新和账号动作后的刷新可能几乎同时到达；每次请求
	// 都做 CountsDetailed + Pool.List + ModelLockView 会把全池扫三遍（470 号
	// 约 0.2s），而三者都持池读锁，会阻塞选号/状态更新。同一时刻只构建一份，
	// 3 秒内的后续请求复用它；账号动作后前端另有即时刷新，感知延迟可忽略。
	// 前端 overview 轮询是 5 秒一轮，若 TTL 只有 1 秒，每个轮询仍会完整重建
	// Pool.List/ModelLockView；3 秒让同一轮多标签页/手动刷新共享一份快照，同时
	// 又把状态可见延迟限制在一个轮询周期内。
	overviewMu   sync.Mutex
	overviewSnap []byte
	overviewAt   time.Time
	// overviewETag 是 overviewSnap 的强校验值。前端 5 秒轮询可带
	// If-None-Match：快照未变时只回 304，不再传输整份 470 行账号表。
	// uptime_sec 不参与 ETag（它每秒都变，否则条件请求永远命中不了），
	// 由前端在 304 时本地递增；服务端仍保留该字段供新客户端首屏显示。
	overviewETag string

	// creditSnap 是「积分获取历史」的只读快照：只在 /packages 成功查询后更新，
	// 读取该视图时绝不逐账号回源。失败时保留上一次成功快照，由 fetched_at 明示
	// 数据新鲜度；账号被移除后读取侧主动过滤，避免过期账号长期挂在表里。
	creditSnapMu sync.RWMutex
	creditSnap   map[string]creditAccountSnapshot

	// grantActionCache 缓存「积分包 → 签到/活跃动作」关联索引。该索引由活动
	// 历史全文件扫描派生，而 /credits/grants 在用量页打开时会随前端刷新被反复
	// 请求；活动文件签名不变时复用，避免每次刷新都重扫 JSONL。签名变化或 TTL
	// 到期即重建，因此新增活动记录不会被长期掩盖。
	grantActionMu    sync.Mutex
	grantActionKey   string
	grantActionAt    time.Time
	grantActionCache map[string][]grantActionEntry

	// benefitsGrowthMu caches the per-account growth/task snapshot for the
	// welfare page. Hub does the same with a TTL: opening the page or switching
	// the focused account must not hammer every account's task endpoints again.
	// The cache is keyed by the exact account set and invalidated after actions.
	benefitsGrowthMu   sync.Mutex
	benefitsGrowthKey  string
	benefitsGrowthAt   time.Time
	benefitsGrowthSnap map[string]benefitGrowth
	// benefitsGrowthFlight 让同一账号集合的成长查询单飞。页面超时返回后
	// 后台 worker 仍可能继续跑；下一次焦点切换/刷新若再启动一轮，会对同一
	// 批账号重复打上游，也会让活动记录里的查询计数翻倍。
	benefitsGrowthFlightKey string
	benefitsGrowthFlight    chan struct{}
	// benefitsGrowthEpoch invalidates a flight that was started before a
	// task/welfare action. Workers that outlive invalidateBenefitsGrowth must
	// not republish the pre-action snapshot.
	benefitsGrowthEpoch uint64
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// GoBackground 启动一个可被停机等待的后台任务。所有不应跨越进程退出继续写
// 文件/队列的异步工作都应走这里，而不是裸 go。
//
// 返回 false 表示停机闸已关闭、任务未启动。调用方若在提交前做了 wg.Add(1)
// 或持有了账号锁，必须自行回滚，否则会永久等待或卡住该账号。
func (p *Panel) GoBackground(fn func()) bool {
	p.bgMu.Lock()
	if p.bgStop {
		p.bgRejected++
		p.bgMu.Unlock()
		return false
	}
	p.bgWG.Add(1)
	p.bgMu.Unlock()
	go func() { defer p.bgWG.Done(); fn() }()
	return true
}

// StopWaiting 停止接收新的后台任务并等待已登记任务退出。先关闸再 Wait，
// 避免 WaitGroup 的 Add/Wait 竞态（关闭后调度回调仍可能触发成长队列）。
// 不会强制取消在途上游请求，超时由调用方控制。
func (p *Panel) StopWaiting() {
	p.bgMu.Lock()
	p.bgStop = true
	cancel := p.bgCancel
	if cancel == nil {
		// 零值 Panel（测试里直接 &Panel{}）没有构造过 ctx：补一个已取消的，
		// 让可取消等待立刻返回而不是永久阻塞。
		cancel = func() {}
	}
	p.bgMu.Unlock()
	// 先取消再等待：后台任务链里的可取消等待会立即返回，队列/任务不会为了
	// 跑完 45s 的事件间隔而把停机拖成分钟级。
	cancel()
	p.bgWG.Wait()
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	p := &Panel{
		cfg:         cfg,
		bgCtx:       bgCtx,
		bgCancel:    bgCancel,
		mux:         http.NewServeMux(),
		started:     time.Now(),
		logs:        NewRing(500),
		logins:      map[string]loginSession{},
		benefitRefs: map[string]string{},
		batchRuns:   map[string]*batchRun{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/request_logs/export", p.withAuth(p.requestLogsExport))
	p.perfRoutes(p.mux)
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("GET /panel/api/accounts/export", p.withAuth(p.exportAccounts))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/profile", p.withAuth(p.accountProfile))
	p.mux.HandleFunc("POST /panel/api/accounts/set_all", p.withAuth(p.accountsSetAll))
	p.mux.HandleFunc("POST /panel/api/accounts/remove_all", p.withAuth(p.accountsRemoveAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/test", p.withAuth(p.accountTest))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/sync_profile", p.withAuth(p.accountSyncProfile))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("GET /panel/api/tasks/scan_status", p.withAuth(p.tasksScanStatus))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/tasks", p.withAuth(p.tasksOverview))
	p.mux.HandleFunc("POST /panel/api/tasks/run", p.withAuth(p.tasksRun))
	p.mux.HandleFunc("GET /panel/api/activity/history", p.withAuth(p.activityHistory))
	p.mux.HandleFunc("GET /panel/api/benefits", p.withAuth(p.benefitsOverview))
	p.mux.HandleFunc("GET /panel/api/benefits/batches", p.withAuth(p.benefitsBatches))
	p.mux.HandleFunc("GET /panel/api/benefits/log", p.withAuth(p.benefitsLog))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/daily_chat_all", p.withAuth(p.dailyChatAll))
	p.mux.HandleFunc("POST /panel/api/streak_all", p.withAuth(p.streakAll))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/daily_chat", p.withAuth(p.benefitsDailyChat))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/daily_chat_web", p.withAuth(p.benefitsDailyChatWeb))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/streak", p.withAuth(p.benefitsStreak))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/travel", p.withAuth(p.benefitsTravel))
	p.mux.HandleFunc("GET /panel/api/benefits/{ref}/tasks", p.withAuth(p.benefitsTasks))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/tasks/accept", p.withAuth(p.benefitsTaskAction))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/tasks/accept_all", p.withAuth(p.benefitsTaskAction))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/tasks/claim", p.withAuth(p.benefitsTaskAction))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/tasks/auto", p.withAuth(p.benefitsTaskAction))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/tasks/auto_all", p.withAuth(p.benefitsTaskAction))
	p.mux.HandleFunc("POST /panel/api/benefits/{ref}/checkin", p.withAuth(p.benefitsCheckin))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("GET /panel/api/usage/remaining", p.withAuth(p.usageRemaining))
	p.mux.HandleFunc("GET /panel/api/usage/export", p.withAuth(p.usageExport))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.creditHistoryRoutes(p.mux)
	p.creditGrantsRoutes(p.mux)
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/connect", p.withAuth(p.connect))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 只接受管理密钥。面板权限高于业务 API：api_keys[] 中的客户端 key
// 可以调用 /v1/*，但绝不能查看账号、导出用量或修改配置。管理身份只认顶层
// legacy api_key；未配置时 fail-closed，拒绝一切管理 API。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.authRequired() {
			writeErr(w, http.StatusForbidden, "管理面板未配置 api_key，已拒绝匿名访问；请在配置中设置顶层 api_key 后重启")
			return
		}
		if _, ok := p.authenticate(r); !ok {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// authRequired 当前是否配置了管理密钥（供首页显示登录态）。客户端业务 key
// 不参与面板登录态判断，避免把“有客户端 key”误显示成“面板已受保护”。
func (p *Panel) authRequired() bool {
	return strings.TrimSpace(p.apiKey()) != ""
}

// authenticate 只校验管理密钥（顶层 legacy api_key），不做多 Key 匹配，
// 防止客户端业务 key 获得账号管理权限。
func (p *Panel) authenticate(r *http.Request) (httpauth.Identity, bool) {
	key := p.apiKey()
	return httpauth.VerifyBearerKey(r, httpauth.Key{ID: httpauth.DefaultLegacyKeyID, Key: key, Enabled: true})
}

// apiKey 当前生效的管理密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	p.overviewMu.Lock()
	if len(p.overviewSnap) > 0 && time.Since(p.overviewAt) < overviewCacheTTL {
		raw := p.overviewSnap
		etag := p.overviewETag
		p.overviewMu.Unlock()
		writeOverviewJSON(w, r, raw, etag)
		return
	}
	// 慢路径在锁内构建并序列化，但必须先释放锁再写响应体：慢客户端/大响应
	// 会让其他标签页的轮询在 overviewMu 上排队，表现为整个面板卡住。
	// 构建完成后统一释放；写失败不影响缓存中的完整快照。
	raw, etag, status, msg := p.buildOverviewLocked()
	if raw != nil {
		p.overviewSnap = raw
		p.overviewAt = time.Now()
		p.overviewETag = etag
	}
	p.overviewMu.Unlock()
	if status != http.StatusOK {
		writeErr(w, status, msg)
		return
	}
	writeOverviewJSON(w, r, raw, etag)
}

// buildOverviewLocked 在持有 overviewMu 时构建并编码总览快照。返回 nil 表示
// 构建失败；调用方负责解锁、错误响应和缓存发布。把响应写出移出锁外，避免
// 慢连接阻塞同一份快照的后续请求。
func (p *Panel) buildOverviewLocked() ([]byte, string, int, string) {

	// Overview 一次遍历同时产出计数、账号行和模型锁；旧实现分别调用
	// CountsDetailedWithPaused/List/ModelLockView，470 账号时整池扫三遍，且
	// 三次 time.Now 可能落在不同冷却边界上。口径保持不变。
	snap := p.cfg.Pool.Overview()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	body := struct {
		pool.OverviewSnapshot
		Version        string `json:"version"`
		UptimeSec      int    `json:"uptime_sec"`
		AuthRequired   bool   `json:"auth_required"`
		RedisMode      string `json:"redis_mode"`
		StickySessions int    `json:"sticky_sessions"`
	}{
		OverviewSnapshot: snap,
		Version:          p.cfg.Version,
		UptimeSec:        int(time.Since(p.started).Seconds()),
		AuthRequired:     p.authRequired(),
		RedisMode:        p.cfg.RedisMode,
		StickySessions:   sticky,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, "encode overview: " + err.Error()
	}
	// ETag 排除 uptime_sec：运行秒数每秒变化会让条件请求永远无法命中，
	// 失去 304 省流量的意义。账号/模型/余额等稳定字段仍参与摘要。
	// 旧实现对 470 行快照再完整 Marshal 一次（约 280 KB、数百次分配）。这里
	// 直接把稳定表示里的 uptime 字节替换成 0 后再算摘要：encoding/json 对该
	// 整数字段输出固定为 "uptime_sec":<数字>，不会有空格/引号/转义差异，省掉
	// 第二次整包编码。找不到字段时回退二次 Marshal（防御结构体标签被改）。
	stable := overviewStableJSON(raw)
	if stable == nil {
		body.UptimeSec = 0
		stable, err = json.Marshal(body)
		if err != nil {
			return nil, "", http.StatusInternalServerError, "encode overview etag: " + err.Error()
		}
	}
	return raw, staticETag(stable), http.StatusOK, ""
}

// overviewStableJSON 返回将 "uptime_sec":<n> 数值清零后的 JSON 字节副本。
// 只识别整数字面量（可带负号；负数不会出现，但保持 JSON 数值形态），遇到
// 小数点/指数/非数字立即放弃返回 nil，由调用方回退完整编码。字段值可能正好
// 出现在某个字符串内容里，因此只在找到 `"uptime_sec":` 后跳过空白再解析数字；
// 返回副本而非原地改 raw（raw 仍是响应体，需保留真实运行秒数）。
func overviewStableJSON(raw []byte) []byte {
	const key = `"uptime_sec":`
	i := bytes.Index(raw, []byte(key))
	if i < 0 {
		return nil
	}
	j := i + len(key)
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
		j++
	}
	start := j
	if j < len(raw) && raw[j] == '-' {
		j++
	}
	digits := j
	for j < len(raw) && raw[j] >= '0' && raw[j] <= '9' {
		j++
	}
	if j == digits {
		return nil
	}
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
		j++
	}
	// 后一个字节必须是 JSON 结构分隔符，排掉 "uptime_sec":1234.5 / 1e9 等。
	if j < len(raw) && raw[j] != ',' && raw[j] != '}' {
		return nil
	}
	out := make([]byte, 0, len(raw)-(j-start)+1)
	out = append(out, raw[:start]...)
	out = append(out, '0')
	out = append(out, raw[j:]...)
	return out
}

// overviewCacheTTL 略大于前端 5 秒轮询间隔：窗口内的重复请求直接复用快照，
// 又不会因为轮询抖动（5s ± 网络延迟）每次落空重建。账号动作会立即
// invalidateOverview，不牺牲变更后的可见性。
const overviewCacheTTL = 6 * time.Second

// invalidateOverview 丢弃缓存的概览快照。账号状态或配置在面板内变更后调用，
// 保证下一次轮询/动作刷新不会读到变更前的 1 秒快照。
func (p *Panel) invalidateOverview() {
	p.overviewMu.Lock()
	p.overviewSnap = nil
	p.overviewAt = time.Time{}
	p.overviewMu.Unlock()
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	snap := p.cfg.RequestLog.Snapshot()
	// 归档目录与底层错误可能包含部署路径，面板只返回状态数字；路径细节仅留
	// 服务端日志/本地排障使用，避免一次带管理密钥的只读请求泄露目录结构。
	snap.Archive.Dir = ""
	if snap.Archive.LastError != "" {
		snap.Archive.LastError = "request archive write failed"
	}
	writeJSON(w, http.StatusOK, snap)
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/key_id/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	includeEntries := r.URL.Query().Get("include_entries") == "1"
	var rows []reqlog.Event
	var changed bool
	var err error
	if includeEntries {
		rows, changed, err = p.cfg.RequestLog.ReadArchiveChanged(limit, requestLogFilter(r))
	} else {
		// 默认轮询只需要 changed 标志；窗口未变时不要复制最多 1000 行，
		// 否则 5 秒一次的无变化轮询仍会产生整窗分配。
		rows, changed, err = p.cfg.RequestLog.ReadArchiveChangedIfChanged(limit, requestLogFilter(r))
	}
	if err != nil {
		log.Printf("panel: 读取请求归档失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "read request archive failed")
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}
	if !changed && !includeEntries {
		// 目录签名未变时窗口与上一轮逐条相同。默认省略 entries 大数组，否则
		// 5 秒轮询仍要下载/反序列化最多 1000 行；前端此时本来就会跳过重绘。
		// include_entries=1 保留完整旧语义，方便调试与依赖旧字段的脚本。
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit, "unchanged": !changed})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	p.invalidateOverview()
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, pool.ManualDisableReason)
	p.invalidateOverview()
	log.Printf("panel: disable uid=%s（人工禁用）", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.invalidateOverview()
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.invalidateOverview()
	log.Printf("panel: resume uid=%s（恢复参与选号）", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountProfile 更新账号备注/手动调度权重（weight 1-100，1 = 不干预）。
// 只接受这两个面板字段；凭证、UID、realm 不在此接口的可写范围，避免误改鉴权数据。
func (p *Panel) accountProfile(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		Weight *int    `json:"weight"`
		Note   *string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Weight == nil && body.Note == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update: pass weight and/or note")
		return
	}
	if body.Weight != nil && (*body.Weight < 1 || *body.Weight > 100) {
		writeErr(w, http.StatusBadRequest, "weight must be an integer between 1 and 100")
		return
	}
	ok, err := p.cfg.Pool.SetProfile(uid, body.Weight, body.Note)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.invalidateOverview()
	if err != nil {
		log.Printf("panel: profile uid=%s（auth 落盘失败: %v）", logfmt.UID8(uid), err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": false, "file_error": "auth 文件写入失败，已暂存到运行状态；请检查挂载目录权限"})
		return
	}
	log.Printf("panel: profile uid=%s weight/note updated", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": true})
}

// accountsSetAll 批量启停：enabled=false 走人工禁用，true 走解冻（清冷却/熔断）。
// 逐号操作，避免整池长时间持锁。
//
// 作用域红线：uids 必须显式给出。历史版本把缺失/null 的 uids 解释为“整池”，
// 直接调用 API 或旧缓存页面漏传时会把一次按钮操作放大成启停全部账号。全量
// 启停请显式传 uids（前端「全部启用/禁用」会先取当前池快照再发送）；审计日志
// 因此也不会再出现 scope=all 的隐式全池操作。
func (p *Panel) accountsSetAll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool    `json:"enabled"`
		UIDs    []string `json:"uids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled (boolean) is required")
		return
	}
	if len(body.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids is required: batch actions only apply to explicitly selected accounts")
		return
	}
	if len(body.UIDs) > 2000 {
		writeErr(w, http.StatusBadRequest, "too many uids (max 2000)")
		return
	}
	seen := make(map[string]bool, len(body.UIDs))
	targets := make([]string, 0, len(body.UIDs))
	for _, uid := range body.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		targets = append(targets, uid)
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "uids must contain at least one non-empty account id")
		return
	}
	n := p.cfg.Pool.SetEnabledFor(targets, *body.Enabled)
	p.invalidateOverview()
	action := "disabled"
	if *body.Enabled {
		action = "enabled"
	}
	log.Printf("panel: accounts set-%s selected n=%d", action, n)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "changed": n, "enabled": *body.Enabled, "scope": "selected",
	})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)
	defer p.invalidateBenefitsGrowth()
	defer p.invalidateOverview()
	checkinMsg := ""
	checkinDone := false
	// 企业版无签到体系（上游 400 code 10001「企业账号不支持该操作」）：不发起该调用，
	// 端点退化为「刷新额度」——企业额度走 get-enterprise-user-usage 口径（见 upstream）。
	// 前端对企业号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存前端仍可能调用）。
	if a.IsEnterprise() {
		checkinMsg = "企业账号无签到体系（已跳过签到，仅刷新额度）"
	} else {
		checkinDone, checkinMsg = p.checkinAccount(a)
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone, "success": checkinDone || a.IsEnterprise()}
	if a.IsEnterprise() {
		resp["enterprise"] = true
	}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	p.invalidateOverview()
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", logfmt.UID8(uid), checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，并按余额恢复解冻积分冷却。
// 解冻语义必须与签到/全量刷新一致：余额耗尽进入 CoolHard 的账号，余额回到
// 正数就是权威恢复证据；只更新 credits 会让它继续停在冷却里，直到下一轮
// 签到或 5 分钟全量刷新，用户手动点「余额」后仍看到「冷却中」。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先把 auth 文件原子改名到隔离目录，再出池（立即
// 落盘 state）。顺序不能反：若先出池、删除文件失败，auths/ 下仍留有凭证，
// 下一次 LoadDir/SyncToDir 会把账号复活。改名在同一文件系统内原子完成；
// 失败时账号保持原状并返回 500，让运维先修复权限/挂载问题。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	// 同一账号的移除串行化。并发删除会对同一个源文件各自 rename：失败者可能
	// 在 Remove 返回 nil 时把已被胜者隔离的文件 rename 回来，导致凭证复活。
	// 复用账号任务锁，确保任何时刻只有一个移除请求操作该 UID。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 记录源文件是否存在：只有确实由本请求 rename 成功（或源文件本就不存在）
	// 才允许在后续 Remove 失败时回滚；避免并发路径把别人的隔离文件改名回来。
	ownedSource := false
	trashPath := ""
	if a.FilePath != "" {
		trashPath = a.FilePath + ".removed-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := os.Rename(a.FilePath, trashPath); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("panel: remove uid=%s（auth 文件隔离失败: %v）", logfmt.UID8(uid), err)
				writeErr(w, http.StatusInternalServerError, "凭证文件无法移出 auths 目录，账号未移除；请检查目录权限")
				return
			}
		} else {
			ownedSource = true
		}
	}
	if p.cfg.Pool.Remove(uid) == nil {
		// 只有在源文件确由本请求隔离时才回滚，避免并发请求互相恢复文件。
		if ownedSource {
			_ = os.Rename(trashPath, a.FilePath)
		}
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if trashPath != "" && ownedSource {
		if err := os.Remove(trashPath); err != nil && !os.IsNotExist(err) {
			// 账号已经从池和 auths/ 主目录移除；隔离文件位于 auths/ 之外不会被
			// LoadDir 匹配，清理失败只记日志，不把一次成功移除报成失败。
			log.Printf("panel: remove uid=%s（隔离文件清理失败: %v）", logfmt.UID8(uid), err)
		}
	}
	p.forgetCreditSnapshot(uid)
	p.invalidateOverview()
	log.Printf("panel: remove uid=%s（已隔离凭证文件并出池）", logfmt.UID8(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("checkin", p.cfg.Scheduler.RunCheckinNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动全量签到已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("travel", p.cfg.Scheduler.RunTravelNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("activity", p.cfg.Scheduler.RunActivityNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("keepalive", p.cfg.Scheduler.RunKeepaliveNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// dailyChatAll 手动触发国际版每日活跃对话（异步执行）。
func (p *Panel) dailyChatAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("daily_chat", p.cfg.Scheduler.RunDailyChatNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动国际版每日活跃已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// streakAll 手动触发国内版连登兑换与抽奖（异步执行，幂等跳过未解锁档位/无次数）。
func (p *Panel) streakAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.startBenefitBatch("streak", p.cfg.Scheduler.RunStreakBonusNow) {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	log.Printf("panel: 手动连登兑换与抽奖已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// benefitRefFor 为完整 UID 生成稳定的服务端短引用。引用是 sha256 前 16 字节，
// 不可逆；同一批快照内同一账号稳定，便于前端把行与动作关联起来。
func benefitRefFor(uid string) string {
	sum := sha256.Sum256([]byte("wb2api-benefit-ref\x00" + strings.TrimSpace(uid)))
	return fmt.Sprintf("%x", sum[:16])
}

// rememberBenefitRefs 重建福利页的引用表。每次快照刷新整体替换，账号移除后
// 旧引用立即失效；只保留当前池内账号，避免历史 UID 常驻内存。
func (p *Panel) rememberBenefitRefs(statuses []pool.Status) {
	refs := make(map[string]string, len(statuses))
	for _, st := range statuses {
		uid := strings.TrimSpace(st.UID)
		if uid == "" {
			continue
		}
		refs[benefitRefFor(uid)] = uid
	}
	p.benefitRefMu.Lock()
	p.benefitRefs = refs
	p.benefitRefMu.Unlock()
}

// benefitsAccount 取福利单号动作的目标账号，并检查它属于该动作的体系。
// 入参是页面持有的短引用；完整 UID 只在服务端解析，不随页面下发。
// benefitUIDForRef 把页面短引用解析为完整 UID；仅供服务端内部查询使用。
// 未知/过期引用返回空串，调用方不得据此回退到客户端输入。
func (p *Panel) benefitUIDForRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	p.benefitRefMu.Lock()
	defer p.benefitRefMu.Unlock()
	return p.benefitRefs[ref]
}

func (p *Panel) benefitsAccount(ref string, wantGlobal bool) (*auth.Auth, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "invalid benefit ref"
	}
	p.benefitRefMu.Lock()
	uid, ok := p.benefitRefs[ref]
	p.benefitRefMu.Unlock()
	if !ok || uid == "" {
		return nil, "benefit ref expired; refresh the benefits page"
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return nil, "account not found"
	}
	if a.AccessTokenValue() == "" {
		return nil, "account has no access token"
	}
	if a.IsEnterprise() {
		return nil, "enterprise accounts have no personal benefits"
	}
	if a.IsGlobal() != wantGlobal {
		if wantGlobal {
			return nil, "daily chat is only for international accounts"
		}
		return nil, "this action is only for CN accounts"
	}
	return a, ""
}

// benefitsDailyChat 单号国际版每日活跃（同步返回结果，便于页面立刻刷新）。
// 与 Hub /accounts/daily-chat 同口径：桌面通道 + 网页通道。网页-only 的补打
// 入口是 benefitsDailyChatWeb，两者不能互相替代。
func (p *Panel) benefitsDailyChat(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	a, msg := p.benefitsAccount(r.PathValue("ref"), true)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	uid := a.UID
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)
	ok, message, err := p.cfg.Scheduler.RunDailyChatFor(a, activity.TriggerManual)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.invalidateBenefitsGrowth()
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "success": ok, "message": message})
}

// benefitsDailyChatWeb 单号网页通道打卡。与 Hub 的 daily-chat-web 对齐：
// 只建网页端会话，不发桌面腿；手动补打不写“今日已打卡”闸门，避免影响定时巡检。
func (p *Panel) benefitsDailyChatWeb(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client unavailable")
		return
	}
	a, msg := p.benefitsAccount(r.PathValue("ref"), true)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	uid := a.UID
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)
	res := p.cfg.Upstream.DailyChatWeb(a, "deepseek-v4.1-flash")
	p.invalidateBenefitsGrowth()
	_ = p.cfg.Activity.Append(activity.Record{
		UID: uid, Nickname: a.NicknameValue(), Realm: a.Realm(), Task: activity.TaskDailyChat,
		Trigger: activity.TriggerManual, OK: res.OK,
		Message: "网页通道打卡：" + dailyChatResultMessage(res),
	})
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "success": res.OK, "conversation": res.Conversation,
		"status": res.Status, "chunks": res.Chunks, "elapsed_ms": res.ElapsedMS,
		"error": res.Error, "message": dailyChatResultMessage(res),
	})
}

func dailyChatResultMessage(res upstream.WebDailyResult) string {
	if res.OK {
		return "每日活跃完成（网页通道 " + res.Status + "，" + strconv.FormatInt(res.Chunks, 10) + " 段输出）"
	}
	if res.Error != "" {
		return "每日活跃失败：" + res.Error
	}
	return "每日活跃失败"
}

// benefitsStreak 单号连登兑换/抽奖（复用调度器幂等动作）。
func (p *Panel) benefitsStreak(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	a, msg := p.benefitsAccount(r.PathValue("ref"), false)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	uid := a.UID
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)
	if err := p.cfg.Scheduler.RunStreakBonusFor(a, activity.TriggerManual); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.invalidateBenefitsGrowth()
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "连登兑换与抽奖检查完成"})
}

// benefitsTravel 单号旅行巡检（与调度器同一条领养→派出→领奖链路）。
func (p *Panel) benefitsTravel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	a, msg := p.benefitsAccount(r.PathValue("ref"), false)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !p.tryLockAccount(a.UID) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "旅行巡检已排队"})
	if !p.GoBackground(func() {
		defer p.unlockAccount(a.UID)
		_ = p.cfg.Scheduler.RunTravelFor(a)
		p.invalidateBenefitsGrowth()
		p.invalidateOverview()
	}) {
		p.unlockAccount(a.UID)
	}
}

// benefitsTasks 与 accountTasks 同一条查询链路，但入参是福利页的短引用。
// 页面按行点「成长任务」时不应把完整 UID 写进 DOM；服务端解析引用后再复用
// 既有任务查询逻辑，避免两条口径漂移。
func (p *Panel) benefitsTasks(w http.ResponseWriter, r *http.Request) {
	a, msg := p.benefitsAccount(r.PathValue("ref"), false)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	next := r.Clone(r.Context())
	next.SetPathValue("uid", a.UID)
	p.accountTasks(w, next)
}

// benefitsTaskAction 把福利弹窗内的任务动作转交给既有账号动作端点。URL 的
// 最后一段决定动作（accept/accept_all/claim/auto/auto_all），路径参数 uid
// 由服务端从短引用解析后注入；请求体原样透传。
func (p *Panel) benefitsTaskAction(w http.ResponseWriter, r *http.Request) {
	a, msg := p.benefitsAccount(r.PathValue("ref"), false)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	next := r.Clone(r.Context())
	next.SetPathValue("uid", a.UID)
	suffix := strings.TrimPrefix(r.URL.Path, "/panel/api/benefits/"+r.PathValue("ref")+"/tasks/")
	switch suffix {
	case "accept":
		p.accountTaskAccept(w, next)
	case "accept_all":
		p.taskAcceptAll(w, next)
	case "claim":
		p.accountTaskClaim(w, next)
	case "auto":
		p.accountTaskAuto(w, next)
	case "auto_all":
		p.accountTaskAutoAll(w, next)
	default:
		writeErr(w, http.StatusNotFound, "unknown task action")
	}
}

// benefitsCheckin 单号签到（福利页短引用入口）。完整 UID 只在服务端解析，
// 之后复用账号签到端点，保持余额刷新/解冻语义一致。
func (p *Panel) benefitsCheckin(w http.ResponseWriter, r *http.Request) {
	a, msg := p.benefitsAccount(r.PathValue("ref"), false)
	if a == nil {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	next := r.Clone(r.Context())
	next.SetPathValue("uid", a.UID)
	p.accountCheckin(w, next)
	p.invalidateBenefitsGrowth()
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	// 单飞：若定时余额刷新正在跑，本次手动刷新直接跳过（不并发扫同一批账号）。
	// 如实告知面板"已跳过"，不要谎报"完成"。
	if !p.cfg.Scheduler.RunBalanceRefreshNow() {
		writeErr(w, http.StatusConflict, "already_running")
		return
	}
	// 昵称同步是逐号 /console/account 的额外上游 RPC。此前它串行挂在余额刷新后，
	// 470 号时会让按钮长时间无响应；现在余额结果先返回，昵称在后台固定并发补齐。
	// SetNickname 到期后由下一次 overview 轮询自然体现，不牺牲余额刷新的确定性。
	p.syncNicknamesAsync()
	p.invalidateOverview()
	log.Printf("panel: 手动全量余额刷新完成")
	// 前端拿到 200 后会立即拉 /overview（已有 3 秒缓存与 ETag 304）；此接口
	// 再序列化一份完整账号表既不会被消费，也会让 470 号场景多付一次整表投影
	// 与 JSON 编码。保持 {"ok":true} 之外的既有响应字段无变化。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// syncNicknamesAsync 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径触发——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过。
//
// 不再同步阻塞 balanceAll：昵称是展示字段，余额刷新返回后才在后台补齐；
// 固定 3 并发，避免 470 号一次性打出同阶上游请求。停机时 GoBackground
// 会等待本轮结束，不会留下写池悬空。
func (p *Panel) syncNicknamesAsync() {
	if p == nil || p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return
	}
	accts := p.cfg.Pool.TaskAccounts(false)
	if len(accts) == 0 {
		return
	}
	p.GoBackground(func() {
		var (
			mu      sync.Mutex
			updated int
			failed  int
		)
		if !p.runBackgroundBounded(3, len(accts), func(k int) {
			acct := accts[k]
			if acct.Auth == nil || acct.Auth.AccessTokenValue() == "" {
				return
			}
			nick, err := p.cfg.Upstream.FetchAccountProfile(acct.Auth)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if p.cfg.Pool.SetNickname(acct.UID, nick) {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}) {
			return
		}
		if updated > 0 || failed > 0 {
			log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
			p.invalidateOverview()
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	writeJSON(w, http.StatusOK, p.usageSnapshot(r))
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	// 只需 uid/昵称/realm 与凭证引用：用 TaskAccounts 一次性取引用，避免为每个
	// 账号构建完整 Status（冷却/限流模型/成本台账，470 号约 250KiB 分配）。
	tas := p.cfg.Pool.TaskAccounts(true)
	accts := make([]struct {
		UID      string
		Nickname string
		Realm    string
	}, 0, len(tas))
	for _, ta := range tas {
		accts = append(accts, struct {
			UID      string
			Nickname string
			Realm    string
		}{ta.UID, ta.Auth.NicknameValue(), ta.Auth.Realm()})
	}
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	if !p.runBackgroundBounded(3, len(accts), func(k int) {
		s := accts[k]
		it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
		a := p.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			it.Error = "account not loaded"
			out[k] = it
			return
		}
		packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
		if err != nil {
			it.Error = err.Error()
			out[k] = it
			return
		}
		it.Packages = packs
		it.Remain = remain
		it.Size = size
		// 成功查询顺带刷新「积分获取历史」的只读快照。失败时保留上一次成功快照，
		// 由响应里的 fetched_at 明示新鲜度，而不是把旧数据伪装成本次结果。
		p.rememberCreditSnapshot(it.UID, it.Nickname, it.Realm, packs)
		out[k] = it
	}) {
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	writeRawJSONStatus(w, status, raw)
}

// writeRawJSON 输出已经序列化的 JSON。缓存策略必须与 writeJSON 完全一致：
// overview 的快路径曾直接写 header，漏掉 no-store，账号余额/UID 可能被浏览器
// 或共享代理缓存。两条出口统一收敛到这里，避免以后再有分支绕过。
func writeRawJSON(w http.ResponseWriter, raw []byte) {
	writeRawJSONStatus(w, http.StatusOK, raw)
}

func writeRawJSONStatus(w http.ResponseWriter, status int, raw []byte) {
	// 面板接口一律包含账号池、用量或配置状态，不能被共享代理/浏览器缓存：
	// 同一浏览器切换面板实例或退出后，旧响应不应在无鉴权请求中复用。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeOverviewJSON 输出 overview 快照并支持条件 GET。ETag 只由稳定字段
// 计算（不含 uptime_sec），账号状态变化会 invalidateOverview 并换新 ETag；
// 304 仍带 no-store，避免共享缓存把鉴权响应留存。响应头与 writeJSON 一致。
func writeOverviewJSON(w http.ResponseWriter, r *http.Request, raw []byte, etag string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if etag != "" {
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
