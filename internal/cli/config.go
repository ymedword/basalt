package cli

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// Config 是 basalt.yaml 的解析结果。
// Config is the parsed form of basalt.yaml.
//
// 已钉死键的段用类型化结构承载。sandbox / shell / http 三段自 US-5 起落为结构
// （§3.5 第 4 项：**沿用既有键名**，不新增也不删除）；memory 段自 US-6 起落为结构
// （§3.5 第 1 项——`backend` 是 `basalt.yaml.example` 里已有的键，零新增配置键）；
// author 段自 US-12 起由 ParseAuthorSection 类型化解析（颗粒度文档 §3.1.9），
// 因需要"未知键报错"而保留独立解析入口，不经 yaml 解码器落结构。
// Sections with fixed keys carry typed structures. The author section is parsed by
// ParseAuthorSection (module card §3.1.9): unknown-key rejection needs an explicit
// entry point, so it is not decoded by the yaml decoder into the struct.
type Config struct {
	Isolation     IsolationConfig     `yaml:"-"`
	Flows         flow.Config         `yaml:"-"`
	Observability ObservabilityConfig `yaml:"-"`
	// 服务入口需要区分键缺失与显式零值；不属于可序列化配置。
	ingressMissing []string         `yaml:"-"`
	HTTP           HTTPConfig       `yaml:"http"`
	Server         ServerConfig     `yaml:"server"`
	Log            LogConfig        `yaml:"log"`
	Providers      []ProviderConfig `yaml:"providers"`
	Budget         BudgetConfig     `yaml:"budget"`
	Sandbox        SandboxConfig    `yaml:"sandbox"`
	Shell          ShellConfig      `yaml:"shell"`
	Memory         MemoryConfig     `yaml:"memory"`
	Secrets        SecretsConfig    `yaml:"secrets"`
	// US-16 的七段（§3.3 的 43 个键）：装载层只做类型化解析与「取值不可能成立」
	// 的判定；缺省与必填按子命令分流（dsn）或按消费方落位时判（§3.1.9 / D-24）。
	// The seven US-16 sections: parsing and impossible-value checks only; defaults
	// and requiredness belong to the consumer or the subcommand gate.
	Database    DatabaseConfig    `yaml:"database"`
	Embedding   EmbeddingConfig   `yaml:"embedding"`
	Knowledge   KnowledgeConfig   `yaml:"knowledge"`
	Retrieval   RetrievalConfig   `yaml:"retrieval"`
	Callers     CallersConfig     `yaml:"callers"`
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`
	Compaction  CompactionConfig  `yaml:"compaction"`
	// Execution 是执行档位段（`execution`，14D）：**只作首次种子**——落库之后以库为准，
	// 改这里不会覆盖管理台写入的值（与 `http.allowed_domains` 同一口径）。
	// The execution section seeds the profile once; the database is authoritative after.
	Execution ExecutionConfig `yaml:"execution"`
	Learning  LearningConfig  `yaml:"learning"`
	// 分布式执行三段（US-24 §3.3）：显式解析入口、未知键拒绝、装载期逐条校验。
	// The three distributed-execution sections: explicit parsing, unknown keys refused,
	// every rule checked at load time.
	Worker    WorkerConfig      `yaml:"-"`
	Scheduler SchedulerConfig   `yaml:"-"`
	Assets    AssetsConfig      `yaml:"-"`
	Author    core.AuthorConfig `yaml:"-"`
}

// LearningConfig 是受控自我进化的启用段（`learning`，US-22 §3.1.11）。
// 出厂默认关闭：`auto_extract` 缺省 false，未启用时钩子零写入、零模型调用。
// 严格校验（启用但名单为空 / 重复名 / 非法名）在装载期由 validateLearningConfig
// 完成；本结构只承担类型化解码与生效判定。
// LearningConfig is the `learning` section: off by default, enabled only by an
// explicit agent allow-list. Strict validation lives in validateLearningConfig.
type LearningConfig struct {
	// AutoExtract 是唯一启用通道：true 才允许 Run 终态之后的同步抽取。
	// AutoExtract is the only enable switch.
	AutoExtract bool `yaml:"auto_extract"`
	// Agents 是抽取名单：启用时必须至少一个；名单内未注册的 Agent 只 WARN + 状态页
	// 列出，不抽取、不阻止启动（Q7）。
	// Agents is the extraction allow-list; unknown names warn instead of blocking.
	Agents []string `yaml:"agents"`
}

// ExecutionConfig 是执行档位段（`execution`，14D）。
// ExecutionConfig is the `execution` section (14D).
type ExecutionConfig struct {
	// AllowRunCode 是独立的静态授权，不是数据库档位的种子。
	// AllowRunCode explicitly authorizes generated code and is independent of profile.
	AllowRunCode bool `yaml:"allow_run_code"`
	// Profile 是首次种子档位（standard / strict）；空 = 新库按 standard、存量库由迁移写
	// strict。运行期切档只经管理台（`PUT /api/v1/execution-policy`）。
	// Profile seeds a fresh instance; runtime changes go through the console only.
	Profile string `yaml:"profile"`
}

// SecretsConfig 是托管凭据的部署段（`secrets` 段，颗粒度文档 §3.3）：段内只承载
// 主密钥文件的位置，密钥材料本身**不在**配置里。路径边界、链接与权限由
// `LoadMasterKey` 在每次解析时复核，装载层只做"段形状"这一档校验（T136）：
// 写了段就必须给路径；路径按「配置文件同级 `.basalt/`」做**词法**边界判定。
// 词法判定不碰文件系统——密钥文件由 `secrets init` 另行生成，装载期允许尚不存在。
// SecretsConfig carries the master-key file location; key material never lives in the
// configuration. Load-time checks are shape only (a written section requires a path,
// and a lexical outside-workspace check); LoadMasterKey re-validates boundary, links
// and permissions at every use.
type SecretsConfig struct {
	// MasterKeyFile 是主密钥文件路径（必须在工作区外）。空 = 按实例名册推导缺省路径；
	// 没有名册的存量实例不做推导（不猜旧路径），仍按"纯 env 实例"对待。
	// MasterKeyFile is the master-key path outside the workspace; empty means "derive the
	// default from the instance roster", and a roster-less instance stays pure-env.
	MasterKeyFile string `yaml:"master_key_file"`

	// AutoInit 是"允许新实例首次启动自动生成主密钥"的开关（缺省 true，14D T091）。
	// 用指针是为了区分"没写"（缺省开）与"显式 false"（纯 env / 离线只读部署：
	// 不生成密钥，也不推导缺省路径）。
	// AutoInit defaults to true; an explicit false keeps a pure-env instance and also
	// disables the default-path derivation.
	AutoInit *bool `yaml:"auto_init"`
}

// AutoInitEnabled 返回生效的 auto_init：未写 = true。
// AutoInitEnabled reports the effective auto_init; unset means true.
func (s SecretsConfig) AutoInitEnabled() bool {
	return s.AutoInit == nil || *s.AutoInit
}

// HTTPConfig 是服务段的监听与请求体上限（字面量出处 docs/contracts/openapi.yaml）。
// HTTPConfig is the service section: listen address and request body cap.
type HTTPConfig struct {
	// Addr 是监听地址；安全默认是本机，远程部署必须显式改。
	// Addr is the listen address; the safe default is loopback.
	Addr string `yaml:"addr"`

	// MaxRequestBytes 是请求体总上限（缺省 1 MiB，解析前拒绝）。
	// MaxRequestBytes caps the whole request body, enforced before parsing.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`

	// AllowedDomains 是出网域名白名单（大小写不敏感）；空 = 一个都不放行。
	// 键名出处 docs/Operations.md §2.2，**消费归 US-5** 的 http_get / http_post——
	// 本 US 只把键从原始结构搬进类型化字段（消费者未落位不等于键可以晚配：
	// 配置键的形状一变，运维侧就得改文件）。
	// AllowedDomains is the outbound domain allowlist, consumed by US-5's network tools.
	AllowedDomains []string `yaml:"allowed_domains"`

	// DeniedDomains 是出网禁止域名表（14D）：**只作首次种子**，落库之后以库为准。
	// 两档执行策略都先查它——命中即拒，白名单放行不了。
	// DeniedDomains seeds the denylist once; both profiles consult it before allowing.
	DeniedDomains []string `yaml:"denied_domains"`

	// Proxy 是显式出网代理（`http.proxy`，14D）：**空 = 直连**。环境变量
	// `HTTP_PROXY` / `HTTPS_PROXY` 在任何情况下都不读——环境变量代理会让"校验过的
	// 地址"与"实际连上的地址"分离，受保护地址段的判定因此形同虚设。
	// 配了代理时固定的是**代理地址**，目标域名由代理解析：本进程的网络防护仍校验
	// 代理主机，但目标地址不由本进程固定（页面在同一处标注这一点）。
	// Proxy is the only accepted proxy source; environment proxies are never read.
	// When set, the guard pins the proxy host and the proxy resolves the target.
	Proxy string `yaml:"proxy"`

	// AdminAuth 是管理面认证配置（`http.admin_auth` 段；键名逐字取自
	// docs/contracts/run-and-errors.md §9.1）。装载层只校验"配置本身是否成形"，
	// "实际监听是否回环 / 非回环是否显式列出"归 web.ValidateAdminAuth（那里才有
	// 真实监听地址）。
	// AdminAuth carries the management-auth section; only shape checks live at load
	// time — listener-dependent checks run after the listener is created.
	AdminAuth AdminAuthConfig `yaml:"admin_auth"`
}

// AdminAuthConfig 是管理面认证配置段（`http.admin_auth`，颗粒度文档 §3.3）。
// AdminAuthConfig is the management authentication section.
//
// 段整体缺失对不开放 HTTP 管理面的命令（chat / tool / session 等）合法——那是
// 约束 1 与注 12 的口径；`serve` 必须在开放服务前要求 mode 显式给出（校验落在
// web.ValidateAdminAuth，本结构只承载取值）。
// An entirely absent section is legal for commands that never open the management
// face; serve requires an explicit mode before it starts listening.
type AdminAuthConfig struct {
	// Mode 取 token / local_trust；`serve` 启动时必填。
	// Mode is token or local_trust; serve requires it.
	Mode string `yaml:"mode"`

	// Token 是管理凭据：只允许 ${VAR} 占位（credentialKey 覆盖本键，与
	// providers[i].api_key 同一套明文拒绝路径）；明文**不得**落盘、回显或入日志。
	// Token is a credential: only a ${VAR} placeholder is accepted, through the same
	// plaintext-rejection path as providers[i].api_key.
	Token string `yaml:"token"`

	// AllowedHosts 是允许的 Host（含端口）；空 = 由监听地址派生（仅回环监听）。
	// AllowedHosts lists allowed Host values including the port; empty derives from a
	// loopback listener.
	AllowedHosts []string `yaml:"allowed_hosts"`

	// AllowedOrigins 是允许的浏览器 Origin，**完整匹配**（scheme://host[:port]）。
	// AllowedOrigins matches the browser Origin exactly.
	AllowedOrigins []string `yaml:"allowed_origins"`

	// TrustedProxies 是可信反向代理的 CIDR；只有这些来源的转发头参与来源归因，
	// 且**不得**用于证明回环本机信任。
	// TrustedProxies lists proxy CIDRs whose forwarded headers may feed source
	// attribution; they never prove loopback trust.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// ServerConfig 是进程级服务参数（`server` 段，技术方案 §5.2）。
// ServerConfig is the process-level service section.
//
// 本段只承载本 US 落位的既有契约键 `max_concurrent_runs`；其余服务旋钮
// （停机宽限、运行时限）按需求边界保持不可配置。
// Only the already-contracted max_concurrent_runs key lands here; the shutdown grace
// and run timeout stay non-configurable by scope.
type ServerConfig struct {
	// MaxConcurrentRuns 是实例并发运行上限；0 / 缺省 = core 缺省 32，负数装载期拒绝。
	// MaxConcurrentRuns is the instance-wide run cap; zero defers to core's default 32.
	MaxConcurrentRuns int `yaml:"max_concurrent_runs"`
}

// SandboxConfig 是沙箱授权判定的配置面（`sandbox` 段）。
// SandboxConfig is the sandbox section.
//
// 语义读者是 `WhitelistPolicy`（授权判定只看"谁、要碰什么"）：本结构只承载取值，
// 判定规则不在这里——配置装载与策略求值分开，改规则不必改装载。
// The values live here; the rules live in the policy that consumes them.
type SandboxConfig struct {
	// CrossAgentRead 是允许被**跨 Agent 读取**的 Agent 名清单；空 = 一个都不许
	// （未授权即拒，拒绝文案点名本键——FR-011）。
	// CrossAgentRead lists the agents readable across agent boundaries; empty denies all.
	CrossAgentRead []string `yaml:"cross_agent_read"`
}

// ShellConfig 是受控子进程执行的配置面（`shell` 段）——三个键都是白名单，空即全拒。
// ShellConfig is the controlled-subprocess section; every key is an allowlist.
type ShellConfig struct {
	// AllowedCommands 是命令白名单：executable **整串逐字**比对（多词串因此天然不命中）；
	// 给 `bash` / `sh` 放行被硬性禁止——写进这里是配置错误，不是授权（在策略侧硬拒）。
	// AllowedCommands matches the executable byte for byte; bash and sh are refused
	// by the policy regardless of what the operator writes here.
	AllowedCommands []string `yaml:"allowed_commands"`

	// Interpreters 是受控解释器通道：只接受 `[脚本路径, 参数…]` 形态（argv 校验在执行侧）。
	// Interpreters is the controlled interpreter channel; argv shape is checked at execution.
	Interpreters []string `yaml:"interpreters"`

	// EnvPassthrough 是透传给子进程的环境变量名白名单——不写进来的变量一律不继承
	// （代理与凭证类变量因此默认不进子进程，FR-012）。
	// EnvPassthrough names the variables handed to child processes; everything else,
	// proxy and credential variables included, is dropped by default.
	EnvPassthrough []string `yaml:"env_passthrough"`
}

// LogConfig 是日志档位——本 US 唯一消费的配置（键名经 2026-10-15 用户裁决：
// 沿用全仓 `<段>.<键>` 惯例）。
// LogConfig is the log profile, the only section US-1 consumes.
type LogConfig struct {
	// Level 取 debug / info / warn / error；空 = info。
	// Level is one of debug, info, warn, error; empty means info.
	Level string `yaml:"level"`

	// Format 取 text（dev 可读）/ json（prod 结构化）；空 = text。
	// Format is text (dev) or json (prod); empty means text.
	Format string `yaml:"format"`
}

// ProviderConfig 是单个供应商条目。
// ProviderConfig is a single provider entry.
type ProviderConfig struct {
	// Name 是映射表的键——装配处据此一次构建显式映射（宪法第 3 条）。
	// Name keys the explicit provider-to-client map built at assembly time.
	Name string `yaml:"name"`

	// BaseURL 是兼容端点。
	// BaseURL is the OpenAI-compatible endpoint.
	BaseURL string `yaml:"base_url"`

	// APIKey 只允许 ${VAR} 占位；解析结果绝不会出现在错误文案或日志里。
	// APIKey accepts a ${VAR} placeholder only, and never appears in errors or logs.
	APIKey string `yaml:"api_key"`

	// CredentialMode 是 v3 的两模式开关（R4.8）：空串 = env（兼容 v2 定义）；
	// `managed` 时凭据存于凭据表，本文件只写模式、**没有** `api_key` 行。模式只由
	// 字段判定，不按 `api_key` 的取值形态推断（与 `workspace.ProviderEntry` 同口径）。
	// CredentialMode is the v3 two-mode switch: empty means env (v2 definitions);
	// a managed entry carries no api_key at all.
	CredentialMode string `yaml:"credential_mode"`

	// DefaultModel 是该供应商的缺省模型（可被 Profile 覆盖）。
	// DefaultModel is the provider default model.
	DefaultModel string `yaml:"default_model"`
}

// BudgetConfig 是运维侧的预算上限——模型只能缩短、不能放大。
// BudgetConfig holds the operator-defined budgets; a model may shorten but
// never enlarge them.
type BudgetConfig struct {
	MaxIterations  int   `yaml:"max_iterations"`
	MaxToolCalls   int   `yaml:"max_tool_calls"`
	MaxOutputBytes int64 `yaml:"max_output_bytes"`
	ContextTokens  int   `yaml:"context_tokens"`
}

// MemoryConfig 是长期记忆的配置面（`memory` 段，US-6 落位；US-16 起整段退役）。
// MemoryConfig is the memory section; it is retired as of US-16.
//
// 本段**没有合法键**：运行时长期记忆唯一载体为 PostgreSQL（D-18），载体档位不再可配。
// 保留结构体只是为了让 `memory.backend` 能被解码到可判错的形态——是否出现由
// `validateUS16Sections` 对原始树判（存在即拒绝启动），而不是靠解码器静默吞掉。
// No key is legal here any more: the runtime carrier is PostgreSQL only, and the
// validator refuses the retired key from the raw tree.
type MemoryConfig struct {
	// Backend **已退役**（D-18）：运行时长期记忆唯一载体为 PostgreSQL，字段只为让
	// `memory.backend` 能被解码到可判错的形态——键一出现，`validateUS16Sections`
	// 就在校验期拒绝启动。校验通过时它恒为空串，任何代码**不得**再按它分流。
	// Backend is retired: the field exists only so the key decodes to something the
	// validator can refuse. After validation it is always empty.
	Backend string `yaml:"backend"`
}

// DatabaseConfig 是 PostgreSQL 连接段（`database`，US-16 §3.3）。
// DatabaseConfig is the PostgreSQL connection section (US-16 §3.3).
//
// **本段不写缺省数字**：装载层只判「取值不可能成立」（负数），缺省取值归消费方
// ——连接池参数由 `internal/storage/postgres` 决定；`dsn` 的必填性按**子命令**
// 分流（§3.1.9）：`db export` / `db verify --dump|--handoff` 离线可用、不要求
// DSN，`serve` / `db migrate` / `db import` / `db verify --target` / `db switch`
// 启动即 fail-closed。
// No defaults live here: the loader only rejects impossible values and the
// consumer owns defaults. DSN requiredness is per-subcommand, not global.
type DatabaseConfig struct {
	// DSN 支持 `${ENV_VAR}`：变量缺失或解析后残留 `${` 由装载层拒绝（resolveString）；
	// 解析为空是合法装载结果，由要求 DSN 的子命令在启动时拒绝。日志与 `/info`
	// **不得**回显本值（它通常带口令）。
	// The DSN supports ${ENV_VAR}; unresolved placeholders are refused at load time,
	// while the empty value is refused by the subcommands that require a database.
	DSN string `yaml:"dsn"`

	// MaxConns / MinConns 是连接池上限与空闲保底；两者都不得为负，
	// min > max 同样不可能成立，一并拒绝。
	// Pool bounds; negatives and min > max are impossible and refused.
	MaxConns int `yaml:"max_conns"`
	MinConns int `yaml:"min_conns"`

	// ConnectTimeoutSeconds / StatementTimeoutSeconds / LockTimeoutSeconds 分属
	// 建连、每条语句（逐连接设置）与锁等待上限；负数拒绝，0 交消费方取缺省。
	// Per-connection timeouts; negatives are refused, zero defers to the consumer.
	ConnectTimeoutSeconds   int `yaml:"connect_timeout_seconds"`
	StatementTimeoutSeconds int `yaml:"statement_timeout_seconds"`
	LockTimeoutSeconds      int `yaml:"lock_timeout_seconds"`
}

// EmbeddingConfig 是向量化段（`embedding`，US-16 §3.3）：凭据不在这里——
// `model_connection` 复用 `model-connections` 的目标与凭据，不新立凭据类。
// EmbeddingConfig is the embedding section; credentials come from the referenced
// model connection rather than a second credential class.
//
// **条件必填**：`model_connection` 为空时本段是空操作（语义路径降级为关键词并在
// `/info` 与诊断事件里标 `vector_unavailable`），一旦写了连接，`model` 与
// `dimensions` 就必须给出——只配一半等于"以为开了语义检索、实际没开"。
// Empty model_connection disables the semantic path explicitly; a written
// connection requires model and dimensions.
type EmbeddingConfig struct {
	// ModelConnection 是 `model-connections` 里的连接名（沿用既有凭据面）。
	// ModelConnection names an existing model connection.
	ModelConnection string `yaml:"model_connection"`

	// Model 是模型 id（如 `text-embedding-v4`），连接非空时必填。
	// Model is the embedding model id; required when the connection is set.
	Model string `yaml:"model"`

	// Dimensions 是向量维度，连接非空时必填；启动时还要与 active 代际比对
	// （由 `internal/knowledge` 判定，装载层只判正数）。
	// Dimensions is required with a connection; the generation comparison runs at
	// startup in internal/knowledge, not here.
	Dimensions int `yaml:"dimensions"`

	// BatchSize / TimeoutSeconds 是单批上限与单批超时；负数拒绝。
	// Batch size and per-batch timeout; negatives are refused.
	BatchSize      int `yaml:"batch_size"`
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// KnowledgeConfig 是知识库段（`knowledge`，US-16 §3.3）。
// KnowledgeConfig is the knowledge section (US-16 §3.3).
type KnowledgeConfig struct {
	// MaxDocumentBytes 是单文件上限；负数拒绝。
	// MaxDocumentBytes caps one document; negatives are refused.
	MaxDocumentBytes int64 `yaml:"max_document_bytes"`

	// AllowedExtensions 是首批可摄取类型（带点，如 `.md`）；空表示一个都不放行，
	// 由摄取侧判"无白名单 = 全拒"，装载层不替它决定。
	// AllowedExtensions lists ingestible types; empty means nothing is accepted,
	// which the ingest path enforces rather than the loader.
	AllowedExtensions []string `yaml:"allowed_extensions"`

	// Chunk 是切分参数；`version` 进 `kb_versions` 行与装配记录，**不是**可选补充。
	// Chunk carries the split parameters; version is recorded, not optional.
	Chunk ChunkConfig `yaml:"chunk"`
}

// ChunkConfig 是切分参数段（`knowledge.chunk`，US-16 §3.3）。
// ChunkConfig is the chunking section.
type ChunkConfig struct {
	// MaxTokens / OverlapTokens 是切分窗口与相邻重叠；负数拒绝。
	// Window size and overlap; negatives are refused.
	MaxTokens     int `yaml:"max_tokens"`
	OverlapTokens int `yaml:"overlap_tokens"`

	// Version 是切分策略版本（如 `v1`），随条目落库，换策略即换版本。
	// Version is the split-strategy version recorded with every chunk.
	Version string `yaml:"version"`
}

// RetrievalConfig 是检索段（`retrieval`，US-16 §3.3）。
// RetrievalConfig is the retrieval section (US-16 §3.3).
type RetrievalConfig struct {
	// TopK / CandidateK 是最终返回与候选集规模；负数拒绝。
	// Final and candidate widths; negatives are refused.
	TopK       int `yaml:"top_k"`
	CandidateK int `yaml:"candidate_k"`

	// RRFK 是倒数排名融合的平滑参数；负数拒绝。
	// RRFK is the reciprocal-rank-fusion constant; negatives are refused.
	RRFK int `yaml:"rrf_k"`

	// Rerank 是重排子段（缺省关闭）。
	// Rerank is the rerank sub-section, off by default.
	Rerank RerankConfig `yaml:"rerank"`

	// EFSearch 是 HNSW 查询期参数（索引启用时生效）；负数拒绝。
	// EFSearch is the HNSW query-time parameter; negatives are refused.
	EFSearch int `yaml:"ef_search"`
}

// RerankConfig 是重排子段（`retrieval.rerank`，US-16 §3.3）。
// RerankConfig is the rerank sub-section.
type RerankConfig struct {
	// Enabled 缺省 false；启用时 `model_connection` 必填（装载期判），
	// 超时降级留痕由检索侧负责。
	// Off by default; enabling it requires a model connection.
	Enabled bool `yaml:"enabled"`

	// TimeoutSeconds 是重排调用超时；负数拒绝。
	// TimeoutSeconds bounds one rerank call; negatives are refused.
	TimeoutSeconds int `yaml:"timeout_seconds"`

	// ModelConnection 是重排使用的既有模型连接名。
	// ModelConnection names an existing model connection.
	ModelConnection string `yaml:"model_connection"`
}

// CallersConfig 是调用方接入段（`callers`，US-16 §3.3）。
// CallersConfig is the caller-ingress section (US-16 §3.3).
//
// 三组键的**必填**在消费方落位时判（限流器接线）：本段只承载取值与「不可能成立」
// 的判定，装载层**不**给阈值预填缺省——静默缺省等于悄悄换了一套限额。
// Requiredness of these keys is enforced when the limiter lands; the loader never
// fills a threshold default.
type CallersConfig struct {
	// UnauthenticatedQPS 是未鉴权入口的独立保护（每个来源每秒请求数）；负数拒绝。
	// UnauthenticatedQPS protects the unauthenticated ingress; negatives are refused.
	UnauthenticatedQPS int `yaml:"unauthenticated_qps"`

	// LocalAdmin 是内置调用方的限额段。
	// LocalAdmin is the built-in caller's budget.
	LocalAdmin LocalAdminConfig `yaml:"local_admin"`

	// Ingress 是三类入口的令牌桶参数。
	// Ingress carries the three ingress classes' token buckets.
	Ingress IngressConfig `yaml:"ingress"`
}

// LocalAdminConfig 是内置调用方段（`callers.local_admin`，US-16 §3.3）。
// LocalAdminConfig is the built-in caller section.
type LocalAdminConfig struct {
	// Unlimited 缺省 true（本机回环部署）；非回环部署建议显式 false。
	// 用指针是为了区分「没写」（缺省开）与「显式 false」——与 secrets.auto_init
	// 同一套口径。
	// Unlimited defaults to true; the pointer separates unset from an explicit false.
	Unlimited *bool `yaml:"unlimited"`
}

// UnlimitedEnabled 返回生效的 unlimited：未写 = true。
// UnlimitedEnabled reports the effective unlimited flag; unset means true.
func (c LocalAdminConfig) UnlimitedEnabled() bool {
	return c.Unlimited == nil || *c.Unlimited
}

// IngressConfig 是三类入口的限额段（`callers.ingress`，US-16 §3.3）。
// IngressConfig holds the three ingress classes' budgets.
type IngressConfig struct {
	// Run 覆盖 Run / Chat / 会话写三条路径。
	// Run covers the run, chat and session-write paths.
	Run IngressBudgetConfig `yaml:"run"`

	// Ingest 覆盖上传与摄取触发。
	// Ingest covers upload and ingest triggering.
	Ingest IngressBudgetConfig `yaml:"ingest"`

	// Read 覆盖历史 / 来源 / 列表读端点。
	// Read covers history, source and list reads.
	Read IngressBudgetConfig `yaml:"read"`
}

// IngressBudgetConfig 是单个入口类的令牌桶参数（qps / burst）。
// IngressBudgetConfig is one ingress class's token bucket.
type IngressBudgetConfig struct {
	// QPS 是每调用方稳态速率；Burst 是突发额度。两者都不得为负。
	// QPS is the steady rate per caller and Burst the burst allowance; neither may
	// be negative.
	QPS   int `yaml:"qps"`
	Burst int `yaml:"burst"`
}

// DiagnosticsConfig 是诊断事件段（`diagnostics`，US-16 §3.3）。
// DiagnosticsConfig is the diagnostics section (US-16 §3.3).
type DiagnosticsConfig struct {
	// RetentionDays 是诊断事件保留期；超期清理不阻塞业务（D-17）。负数拒绝。
	// RetentionDays bounds diagnostic retention; negatives are refused.
	RetentionDays int `yaml:"retention_days"`

	// SampleRate 是采样率（0–1）；**审计不受它影响**。指针区分「没写」（缺省 1.0）
	// 与「显式 0」（不采样）。
	// SampleRate is the sampling ratio and never affects audit; the pointer
	// separates unset from an explicit zero.
	SampleRate *float64 `yaml:"sample_rate"`
}

// SampleRateOrDefault 返回生效的采样率：未写 = 1.0。
// SampleRateOrDefault reports the effective sample rate; unset means 1.0.
func (d DiagnosticsConfig) SampleRateOrDefault() float64 {
	if d.SampleRate == nil {
		return 1
	}
	return *d.SampleRate
}

// CompactionConfig 是会话压缩段（`compaction`，US-16 §3.3）。
// CompactionConfig is the compaction section (US-16 §3.3).
//
// `enabled` 缺省 false（存量配置升级后行为与现状一致）；显式 true 时除
// `keep_recent_groups` 外的七个键**全部必填**，缺任一即启动拒绝且**一次列全**
// （D-24：不在代码里预填阈值数字）。`keep_recent_groups` 是唯一例外：省略时取
// `defaultKeepRecentGroups`（2026-11-05 用户裁决）；显式写 0 / 负数仍拒绝。
// Enabled defaults to false; besides keep_recent_groups, every other key is required.
// keep_recent_groups alone defaults to 4 when omitted; explicit zero stays invalid.
type CompactionConfig struct {
	// Enabled 开关；false 时行为等同现状（不压缩，超限走整组裁剪或显式拒绝）。
	// Enabled switches compaction on; false keeps the pre-US-16 behavior.
	Enabled bool `yaml:"enabled"`

	// TriggerRatio 是主动压缩触发比例（`0 < r < 1`）：投影超过
	// `r × usable_input` 即触发。
	// TriggerRatio is the proactive trigger ratio, strictly between 0 and 1.
	TriggerRatio float64 `yaml:"trigger_ratio"`

	// KeepRecentGroups 是保留最近完整工具组数。
	// KeepRecentGroups is how many recent complete tool groups stay intact.
	KeepRecentGroups int `yaml:"keep_recent_groups"`

	// SummaryMaxTokens 是**摘要调用自身**的输出上限（max_tokens），
	// 与 output_reserve_tokens 分开：前者是摘要请求的参数，后者进投影判定。
	// SummaryMaxTokens caps the summary call's own output.
	SummaryMaxTokens int `yaml:"summary_max_tokens"`

	// OutputReserveTokens 是**本轮回复**的预留，参与 `projected` 判定。
	// OutputReserveTokens reserves room for this round's reply.
	OutputReserveTokens int `yaml:"output_reserve_tokens"`

	// SafetyMarginRatio 是估算误差缓冲：`safety_margin = ceil(ratio × limit)`，
	// 同时收紧触发线与压缩后准入线。
	// SafetyMarginRatio buffers estimator error on both the trigger and the
	// post-compaction admission line.
	SafetyMarginRatio float64 `yaml:"safety_margin_ratio"`

	// TimeoutSeconds 是摘要调用超时；与 Run 取消共用 ctx。
	// TimeoutSeconds bounds one summary call.
	TimeoutSeconds int `yaml:"timeout_seconds"`

	// PromptVersion / StrategyVersion 是模板与策略版本，进条目与装配记录；
	// 变更即新版本，旧条目保持可读。
	// PromptVersion and StrategyVersion are recorded with every entry.
	PromptVersion   string `yaml:"prompt_version"`
	StrategyVersion string `yaml:"strategy_version"`
}

// defaultKeepRecentGroups 是 `compaction.keep_recent_groups` 省略时的部署初始值
// （2026-11-05 用户裁决）。它是 D-24「装载层不预填阈值」的唯一例外；其余七个压缩键
// 仍必须显式给出，显式写 0 / 负数仍按非法取值拒绝。4 是机制推导的初始值，不是容量
// 实验冻结值——文档与发布说明**不得**把它写成实测结论。
// defaultKeepRecentGroups is the only prefilled compaction threshold: 4 groups when
// the key is omitted. Explicit zero or negative values are still refused.
const defaultKeepRecentGroups = 4

// 凭证键路径：providers[i].api_key 与管理面 token（http.admin_auth.token）走同一套
// 占位形态、类型与明文拒绝检查；其余凭证形态（mem0 api-key、notify_channels url）
// 随各自消费 US 落位。
// Credential key paths: the provider API key and the admin token share the same
// placeholder, type and plaintext-rejection checks.
var credentialKey = regexp.MustCompile(`^(providers\[\d+\]\.api_key|http\.admin_auth\.token|observability\.export\.headers)$`)

// 单个占位符；${} 与 ${1X} 这类畸形形态不匹配，会在「残留检查」处被判红。
var placeholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// 日志级别与形态的合法取值——非法取值一律拒绝，不静默取默认。
var (
	validLogLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats = map[string]bool{"text": true, "json": true}
)

// 管理认证模式的两档取值（docs/contracts/run-and-errors.md §9.1）——取值域外一律
// 拒绝启动：未知模式既不是 token 也不是本机信任，放行等于"认证配置被静默忽略"。
var validAdminAuthModes = map[string]bool{"token": true, "local_trust": true}

// LoadConfig 装载并校验 basalt.yaml。
// LoadConfig loads and validates basalt.yaml.
//
// 校验一律 fail-closed：变量不存在、凭证解析为空、模板畸形、凭证明文、必填缺失、
// 类型非法——六类任一命中即拒绝，绝不静默取默认值。错误文案点名配置键路径
// （如 providers[0].api_key）与变量名，但**不得**回显凭证值（需求文档 §5.2）；
// 解析 / 解码失败同样只报行列与类别，库的源码摘录一律不外泄（见 yamlErrorSummary）。
// Validation is fail-closed on six classes: missing variable, empty credential,
// malformed placeholder, plaintext credential, missing required field and bad
// type. Messages name the key path and the variable, never the value.
//
// 文件缺失时返回的错误可用 errors.Is(err, fs.ErrNotExist) 判定——是否容忍由调用方
// 决定（status 容忍并明示路径，不计失败）。
// A missing file is reported so that callers can test errors.Is(err, fs.ErrNotExist).
func LoadConfig(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	raw, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		// 此处刻意不 %w 原错误：库的默认错误文本带源码摘录，保留链条等于保留
		// 泄露路径（凭证所在行的原文会被带进 stdout / stderr / 日志）。
		return nil, fmt.Errorf("解析配置 %s: %s", path, yamlErrorSummary(err))
	}

	root, ok := asMapping(tree)
	if !ok && tree != nil {
		return nil, fmt.Errorf("配置 %s 的顶层必须是映射（键值对），实际是 %T", path, tree)
	}

	resolved, err := resolveTree(root, "", lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}

	resolvedMap, ok := resolved.(map[string]any)
	if !ok {
		// 顶层映射经 resolveTree 后仍是映射——走到这里说明实现被改坏了。
		return nil, fmt.Errorf("配置 %s: 内部错误：解析结果顶层不是映射（%T）", path, resolved)
	}

	cfg, err := decodeConfig(resolvedMap)
	if err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	cfg.ingressMissing = missingKeyPaths(resolvedMap, ingressRequiredKeys...)
	// secrets 段的"段形状"要在解码之后单独判：整段缺失与"写了段但没给路径"在
	// 结构体里都是空串，只有原始树能区分两者（T136）。
	// The secrets section is checked against the raw tree: after decoding, "section
	// absent" and "section present without a path" are both the empty string.
	if err := validateSecretsSection(resolvedMap, cfg.Secrets, path); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	// US-16 七段要先于 validateConfig 判：退役键（`storage.backend` / `memory.backend`）
	// 的判据是「原始树里写没写」，解码之后的取值面看不出来。
	// The US-16 sections are checked against the raw tree: a retired key's presence is
	// invisible once decoded.
	if err := validateUS16Sections(resolvedMap, cfg); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	if err := cfg.Observability.ValidateHistoryWindow(cfg.Diagnostics.RetentionDays); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	return cfg, nil
}

var ingressRequiredKeys = []string{
	"callers.ingress.run.qps", "callers.ingress.run.burst",
	"callers.ingress.ingest.qps", "callers.ingress.ingest.burst",
	"callers.ingress.read.qps", "callers.ingress.read.burst",
}

// ValidateServeIngress 对服务入口要求六项显式正值；维护命令仍能装载旧配置。
func ValidateServeIngress(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("服务启动需要配置文件及入口限流项: %s", strings.Join(ingressRequiredKeys, ", "))
	}
	if len(cfg.ingressMissing) > 0 {
		return fmt.Errorf("服务启动缺少配置项: %s", strings.Join(cfg.ingressMissing, ", "))
	}
	var invalid []string
	for _, item := range []struct {
		name  string
		value int
	}{
		{ingressRequiredKeys[0], cfg.Callers.Ingress.Run.QPS},
		{ingressRequiredKeys[1], cfg.Callers.Ingress.Run.Burst},
		{ingressRequiredKeys[2], cfg.Callers.Ingress.Ingest.QPS},
		{ingressRequiredKeys[3], cfg.Callers.Ingress.Ingest.Burst},
		{ingressRequiredKeys[4], cfg.Callers.Ingress.Read.QPS},
		{ingressRequiredKeys[5], cfg.Callers.Ingress.Read.Burst},
	} {
		if item.value <= 0 {
			invalid = append(invalid, item.name)
		}
	}
	if len(invalid) > 0 {
		return fmt.Errorf("服务启动要求下列配置项大于零: %s", strings.Join(invalid, ", "))
	}
	return nil
}

// readConfigFile 读取配置文件内容——读取范围经 os.Root 钉在配置文件所在目录。
// readConfigFile reads the configuration file, scoping the read through os.Root
// to the directory the file lives in.
//
// 为什么不用裸 os.ReadFile(path)：路径来自调用方参数，静态安全扫描对此判 G304
// （CWE-22 文件包含），而抑制手段（gosec 的 nosec 标注）默认禁用。以父目录为 root 读取既消除
// 该判定，又真把「跳到目录之外的软连接」挡在门外——fail-closed，与开发操作 §7
// 陷阱 7 的真实路径口径同向。错误经 %w 保留 fs.ErrNotExist 链，LoadConfig 对
// 调用方的判态契约（errors.Is(err, fs.ErrNotExist)）不变（已实测）。
// A read through the parent directory is both gate-clean and fail-closed; the
// %w keeps the fs.ErrNotExist chain intact for callers.
func readConfigFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()

	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		// os.Root 的错误只带基名——补回完整路径，缺失时调用方能明示路径。
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	return raw, nil
}

// decodeConfig 把解析后的树解成类型化配置——类型非法（如 log.level 写成数字）
// 在这一步拒绝。
// decodeConfig maps the resolved tree onto the typed configuration; type errors
// (a numeric log.level, say) are rejected here.
//
// 两处 yaml 错误都经 yamlErrorSummary 压成「位置 + 类别」并再抹一遍凭证值：
// 这一步的输入是**解析后**的树（凭证已是真实值），库的默认错误文本会把它当源码
// 摘录原样回显——实测：providers[0].default_model 类型非法时，解析后的 api_key
// 值出现在错误摘录里并流进 stdout / stderr。%w 同样刻意省略（理由同 LoadConfig）。
// Both paths render through yamlErrorSummary and re-redact credential values:
// the input here holds resolved secrets, which the library would echo verbatim.
func decodeConfig(tree map[string]any) (*Config, error) {
	buf, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("内部错误：解析结果无法回写: %s",
			redactCredentialValues(yamlErrorSummary(err), tree))
	}
	cfg := new(Config)
	if err := yaml.Unmarshal(buf, cfg); err != nil {
		return nil, fmt.Errorf("配置项类型非法: %s",
			redactCredentialValues(yamlErrorSummary(err), tree))
	}

	// author 段走独立解析入口（`yaml:"-"`，解码器不碰它）：未知键与 temperature
	// 取值范围需要显式判定，yaml 解码器对未知键是静默忽略的。
	// The author section is parsed explicitly: the yaml decoder silently ignores
	// unknown keys, which is exactly what this section must refuse.
	section, err := authorSection(tree)
	if err != nil {
		return nil, err
	}
	author, err := ParseAuthorSection(section)
	if err != nil {
		return nil, err
	}
	cfg.Author = author
	observabilitySection, observabilityPresent := tree["observability"]
	observability, err := parseObservabilityConfig(observabilitySection, observabilityPresent)
	if err != nil {
		return nil, err
	}
	cfg.Observability = observability
	flowSection, flowPresent := tree["flows"]
	flowConfig, err := parseFlowConfig(flowSection, flowPresent, cfg.Server.MaxConcurrentRuns)
	if err != nil {
		return nil, err
	}
	cfg.Flows = flowConfig

	// US-24 三段（§3.3）：三段各自的段内规则在 parse* 里解，跨段上限（dispatch_batch
	// 与 flows.max_active 共同容纳于 max_inflight）在 validateDistributedConfig 里判。
	// 顺序固定在 flows 之后——跨段校验需要 flows 的上限。
	workerSection, workerPresent := tree["worker"]
	if cfg.Worker, err = parseWorkerConfig(workerSection, workerPresent); err != nil {
		return nil, err
	}
	schedulerSection, schedulerPresent := tree["scheduler"]
	if cfg.Scheduler, err = parseSchedulerConfig(schedulerSection, schedulerPresent); err != nil {
		return nil, err
	}
	assetsSection, assetsPresent := tree["assets"]
	if cfg.Assets, err = parseAssetsConfig(assetsSection, assetsPresent); err != nil {
		return nil, err
	}
	if err := validateDistributedConfig(cfg.Worker, cfg.Scheduler, cfg.Assets, cfg.Flows); err != nil {
		return nil, err
	}

	isolationSection, isolationPresent := tree["isolation"]
	cfg.Isolation, err = parseIsolationConfig(isolationSection, isolationPresent)
	if err != nil {
		return nil, err
	}
	if err := validateIsolationConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// yamlErrorSummary 把 YAML 库的错误压成「[行:列] 自产类别」，供对外错误使用。
// yamlErrorSummary renders a YAML library error as position plus a locally
// produced category.
//
// 为什么连库的**首行文案**也不能用：库文案本身会带出错处的原文——未定义别名的
// 错误带别名名（实测：`api_key: *SENTINEL` 把 SENTINEL 送进 stdout / stderr）、
// 重复映射键带所重复的键、溢出错误带源数字。故这里的口径是**库文案一个字都不
// 外传**：位置取自错误的**结构化字段**（yaml.Error 接口的 GetToken → 行列），
// 类别由具体错误类型自产（类型名是代码事实，不是配置数据）。
// The library message itself can quote the offending source (alias names,
// duplicated keys, numeric literals), so nothing of it leaves this package:
// the position comes from the structured token, the category from the error type.
func yamlErrorSummary(err error) string {
	category := yamlErrorCategory(err)
	var yamlErr yaml.Error
	if errors.As(err, &yamlErr) {
		if tk := yamlErr.GetToken(); tk != nil {
			return fmt.Sprintf("[%d:%d] %s", tk.Position.Line, tk.Position.Column, category)
		}
	}
	return category
}

// yamlErrorCategory 由错误**类型**给出安全类别——这些字面量全部是本包自产的
// 固定文案，不含任何来自配置的数据。库没有给出可识别类型时用兜底类别，同样
// 不携带原文。
// yamlErrorCategory maps the concrete error type to a fixed local label; the
// messages never carry any configuration data.
func yamlErrorCategory(err error) string {
	var (
		syntaxErr     *yaml.SyntaxError
		typeErr       *yaml.TypeError
		overflowErr   *yaml.OverflowError
		dupKeyErr     *yaml.DuplicateKeyError
		unknownFldErr *yaml.UnknownFieldError
		unexpectedErr *yaml.UnexpectedNodeTypeError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return "YAML 语法错误"
	case errors.As(err, &typeErr):
		return "配置项类型非法"
	case errors.As(err, &overflowErr):
		return "配置项数值溢出"
	case errors.As(err, &dupKeyErr):
		return "映射键重复"
	case errors.As(err, &unknownFldErr):
		return "配置项含未知字段"
	case errors.As(err, &unexpectedErr):
		return "配置节点类型不符"
	default:
		return "YAML 解析失败"
	}
}

// redactCredentialValues 把文案里出现的凭证值替换为占位——纵深防御：即便上游库
// 某天改回带值的错误形态，凭证也不会经错误路径外泄。
// redactCredentialValues scrubs resolved credential values from a message.
func redactCredentialValues(msg string, tree map[string]any) string {
	for _, value := range credentialValues(tree) {
		msg = strings.ReplaceAll(msg, value, "<已隐去>")
	}
	return msg
}

// credentialValues 收集解析结果里凭证键的值（键路径口径与 resolveTree 一致——
// 同一套层级生长规则，凭证键判定才能落在同一批键上）。
// credentialValues collects the values under credential key paths.
func credentialValues(tree map[string]any) []string {
	var values []string
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				if credentialKey.MatchString(childPath) {
					if value, ok := child.(string); ok && value != "" {
						values = append(values, value)
					}
					continue
				}
				walk(child, childPath)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(tree, "")
	return values
}

// validateConfig 做取值域与必填校验（类型校验已由解码完成）。
// validateConfig checks value domains and required fields.
func validateConfig(cfg *Config) error {
	// server.max_concurrent_runs：负数拒绝、0 交给 core 的缺省逻辑（32）——装载期
	// 只判"不可能取值"，缺省语义只在一处（core.NewAgentService）。
	// Negative values fail closed; zero defers to the single default in core.
	if cfg.Server.MaxConcurrentRuns < 0 {
		return fmt.Errorf("配置项 server.max_concurrent_runs 不得为负：%d（0 表示缺省 32）", cfg.Server.MaxConcurrentRuns)
	}
	if cfg.Log.Level != "" && !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("配置项 log.level 取值非法（%s）——只允许 debug / info / warn / error", cfg.Log.Level)
	}
	if cfg.Log.Format != "" && !validLogFormats[cfg.Log.Format] {
		return fmt.Errorf("配置项 log.format 取值非法（%s）——只允许 text / json", cfg.Log.Format)
	}
	// memory.backend 的退役判定在 `validateUS16Sections`（判原始树里键是否出现），
	// 这里**不**再做取值归一：那个键现在一个合法取值都没有。
	// The retirement check lives in validateUS16Sections; there is no value left to
	// normalize here.
	// 管理认证段：缺席合法（轻命令不开 HTTP 管理面），半截配置不合法——见 validateAdminAuth。
	// The admin-auth section may be absent for light commands; a partial section is
	// never legal.
	if err := validateAdminAuth(cfg.HTTP.AdminAuth); err != nil {
		return err
	}
	// execution.profile：空 = 首次种子按新 / 存量实例各自决定，合法；写了就必须是契约
	// 闭集内的值——拼错档名时静默落缺省等于换了一档授权，必须拒绝启动。
	// A misspelled profile would silently change the authorization scope, so it is refused.
	if profile := strings.TrimSpace(cfg.Execution.Profile); profile != "" {
		parsed := core.ExecutionProfile(profile)
		if !parsed.Valid() {
			return fmt.Errorf("配置项 execution.profile 取值非法（%s）——只允许 standard 或 strict", profile)
		}
		cfg.Execution.Profile = string(parsed)
	}
	// http.proxy：**唯一**认可的代理来源（环境变量代理一律不读，14D §2.2.2）。
	// 形态错了拒绝启动，不静默直连——静默直连等于"以为走了代理、实际换了一个出口"。
	// A malformed proxy refuses startup; falling back to direct would silently change
	// the egress path.
	if err := validateHTTPProxy(cfg.HTTP.Proxy); err != nil {
		return err
	}

	// learning 段：与 compaction 同形——开关关闭时也判已写名单的合法性（拼错的名单
	// 等打开开关才报，只会把问题推迟到切换那一刻）；启用但名单为空是启动错误，
	// 绝不把空名单解释为「全部 Agent」。
	// The learning section: written names are validated even while the switch is off,
	// and an enabled-but-empty allow-list refuses startup.
	if err := validateLearningConfig(cfg.Learning); err != nil {
		return err
	}

	// seen 记条目名 → **首次出现**的下标：重名拒绝要同时点名两条，只报后一条会让
	// 用户不知道跟谁撞了。
	// seen maps a name to its first index so a duplicate can name both entries.
	seen := make(map[string]int, len(cfg.Providers))
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("必填配置项 providers[%d].name 缺失——供应商条目必须有名字（映射表的键）", i)
		}
		if first, dup := seen[p.Name]; dup {
			// 第 7 类 fail-closed：重名不拒绝，映射表就会静默保留最后一条——前面的
			// 条目从此不可达，而失败形态只是"配了却路由不过去"，没有任何报错。
			// A duplicate would leave one entry silently unreachable.
			return fmt.Errorf(
				"配置项 providers[%d].name 与 providers[%d] 重复（%q）——映射表的键必须唯一，否则重名的条目静默不可达",
				first, i, p.Name)
		}
		seen[p.Name] = i

		// base_url 自本 US 起必填：没有地址就构造不出客户端（端点 = base_url + /chat/completions）。
		if p.BaseURL == "" {
			return fmt.Errorf("必填配置项 providers[%d].base_url 缺失——无地址无法构造客户端", i)
		}
		if err := validateProviderBaseURL(p.BaseURL, i); err != nil {
			return err
		}

		// 凭据两模式（R4.8）：env 条目必须有 `${VAR}` 占位；managed 条目由服务端在
		// 定义文件里写 `credential_mode: managed` 且**没有** `api_key` 行——把"缺
		// api_key"当成缺项会让实例在下一次启动时拒绝装载自己的定义文件。
		// The two credential modes: env requires the placeholder, managed must not
		// carry an api_key line; treating its absence as missing would make the
		// instance refuse to load the definition file it wrote itself.
		switch mode := strings.TrimSpace(p.CredentialMode); mode {
		case "", "env":
			if p.APIKey == "" {
				return fmt.Errorf("必填配置项 providers[%d].api_key 缺失——凭证一律写 ${VAR} 占位", i)
			}
		case "managed":
			if strings.TrimSpace(p.APIKey) != "" {
				return fmt.Errorf(
					"配置项 providers[%d].api_key 在 credential_mode: managed 下不得出现——托管凭据存于凭据表，定义文件只写模式", i)
			}
		default:
			return fmt.Errorf(
				"配置项 providers[%d].credential_mode 取值非法（%s）——只允许 env（缺省）或 managed", i, mode)
		}
	}
	return nil
}

// learningAgentNamePattern 是 `learning.agents` 名单的合法名形态（§3.1.11 冻结的
// 字面量 `[a-z0-9][a-z0-9_-]*`）：小写字母 / 数字开头，后续只允许小写字母、数字、
// 下划线与连字符。名单值是 Agent 目录名的精确匹配键——拼错一个字符就会让该 Agent
// 静默不抽取，因此不合法形态一律拒绝启动，不做"尽力匹配"。
// learningAgentNamePattern is the frozen allow-list name shape: a typo would silently
// stop extraction for that agent, so malformed names refuse startup.
var learningAgentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validateLearningConfig 校验受控自我进化段（§3.1.11）：空条目、非法名、重名都是
// 启动错误；启用（auto_extract: true）但名单为空同样是启动错误——空名单**不得**被
// 解释为「全部 Agent」。重名拒绝同时点名两条下标（与 providers 同形），只报后一条
// 会让部署者不知道跟谁撞了。名单内未注册的 Agent 不在这里判：装载层看不到目录，
// 由装配层记 WARN 并列入状态页（Q7）。
// validateLearningConfig rejects empty, malformed and duplicate allow-list entries and
// an enabled-but-empty list; unknown-but-legal names are left to the assembly layer,
// which warns without blocking startup.
func validateLearningConfig(cfg LearningConfig) error {
	seen := make(map[string]int, len(cfg.Agents))
	for i, name := range cfg.Agents {
		if name == "" {
			return fmt.Errorf("配置项 learning.agents[%d] 为空——名单条目必须是非空 Agent 名", i)
		}
		if !learningAgentNamePattern.MatchString(name) {
			return fmt.Errorf(
				"配置项 learning.agents[%d] 名字非法（%q）——只允许小写字母 / 数字开头，后续字符限小写字母、数字、下划线与连字符",
				i, name)
		}
		if first, dup := seen[name]; dup {
			return fmt.Errorf(
				"配置项 learning.agents[%d] 与 learning.agents[%d] 重复（%q）——名单决定抽取归属，重名会让同一 Agent 被列出两次",
				first, i, name)
		}
		seen[name] = i
	}
	if cfg.AutoExtract && len(cfg.Agents) == 0 {
		return errors.New(
			"配置项 learning.agents 为空——learning.auto_extract: true 时必须显式列出至少一个 Agent；空名单不等于「全部 Agent」")
	}
	return nil
}

// validateUS16Sections 校验 US-16 七段的取值域与**条件必填**（§3.3 / D-24）。
// validateUS16Sections checks the US-16 sections' value domains and conditional
// requiredness.
//
// 为什么用原始树判「缺失」而不是结构体：解码后「没写」与「写了零值」不可区分
// （`trigger_ratio: 0` 与不写都是 0），而两者的处置完全不同——前者是缺键（**一次
// 列全**），后者是非法取值（点名该键）。`secrets` 段同款理由。
// The raw tree separates "absent" from "written as zero", which the decoded struct
// cannot; the two need different messages.
//
// 判据边界：本函数**不**判 `database.dsn` 的必填（按子命令分流，§3.1.9）也**不**
// 预填任何缺省阈值（缺省归消费方）；它只判「不可能成立」的取值与「开了开关却缺
// 配套键」两类。
// Scope: no DSN requiredness (per-subcommand) and no default filling; only
// impossible values and switch-driven requiredness.
func validateUS16Sections(tree map[string]any, cfg *Config) error {
	// storage.backend 是**不存在的键**（图 1：运行时存储唯一后端为 PostgreSQL，
	// D-01 不切换、不回退）。解码器对未知键静默忽略，留着它被无声吞掉，等于把
	// 「我写了切换开关」变成「看起来生效了」——因此存在即拒绝启动，而不是忽略。
	// There is no storage.backend key: its presence is a configuration error, not a
	// silently ignored unknown key (D-01: one runtime backend, no switch, no fallback).
	if _, present := lookupKeyPath(tree, "storage.backend"); present {
		return fmt.Errorf(
			"配置项 storage.backend 不得出现——运行时存储唯一后端为 PostgreSQL（D-01）：" +
				"本版本没有存储后端开关，存在该键即拒绝启动，不静默忽略、不回退 SQLite")
	}

	// memory.backend 与 storage.backend 同一形态（D-18 / §3.3）：markdown / sqlite 两档
	// 已退役，运行时长期记忆唯一载体为 PostgreSQL。留着这个键被解码器静默吞掉，等于把
	// 「我指定了载体」变成「看起来生效了」——所以存在即拒绝启动，并给出可执行的迁移路径。
	// There is no memory.backend key any more: its presence is refused with the migration
	// path, never silently ignored.
	if _, present := lookupKeyPath(tree, "memory.backend"); present {
		return fmt.Errorf(
			"配置项 memory.backend 不得出现——运行时长期记忆唯一载体为 PostgreSQL（D-18）：" +
				"markdown / sqlite 两档已退役，存在该键即拒绝启动，不静默忽略。" +
				"迁移路径：从配置删去该键 → 旧 SQLite 记忆随 `basalt db import` 进 PG → " +
				"旧 markdown 记忆（memory/MEMORY.md 与 memory/agents/<name>/MEMORY.md）由 " +
				"`basalt db switch` 一次性只读导入并逐条对账；原文件保留作回退层（D-22）")
	}

	// 负数一次列全（七段合扫）：负值是「取值不可能成立」，与缺键同属部署者要
	// 一次改完的错——按段逐个报会让同一份配置反复重启。
	// All negatives are collected across the seven sections in one pass.
	if err := rejectNegative(
		"database.max_conns", cfg.Database.MaxConns,
		"database.min_conns", cfg.Database.MinConns,
		"database.connect_timeout_seconds", cfg.Database.ConnectTimeoutSeconds,
		"database.statement_timeout_seconds", cfg.Database.StatementTimeoutSeconds,
		"database.lock_timeout_seconds", cfg.Database.LockTimeoutSeconds,
		"embedding.dimensions", cfg.Embedding.Dimensions,
		"embedding.batch_size", cfg.Embedding.BatchSize,
		"embedding.timeout_seconds", cfg.Embedding.TimeoutSeconds,
		"knowledge.max_document_bytes", int(cfg.Knowledge.MaxDocumentBytes),
		"knowledge.chunk.max_tokens", cfg.Knowledge.Chunk.MaxTokens,
		"knowledge.chunk.overlap_tokens", cfg.Knowledge.Chunk.OverlapTokens,
		"retrieval.top_k", cfg.Retrieval.TopK,
		"retrieval.candidate_k", cfg.Retrieval.CandidateK,
		"retrieval.rrf_k", cfg.Retrieval.RRFK,
		"retrieval.ef_search", cfg.Retrieval.EFSearch,
		"retrieval.rerank.timeout_seconds", cfg.Retrieval.Rerank.TimeoutSeconds,
		"callers.unauthenticated_qps", cfg.Callers.UnauthenticatedQPS,
		"callers.ingress.run.qps", cfg.Callers.Ingress.Run.QPS,
		"callers.ingress.run.burst", cfg.Callers.Ingress.Run.Burst,
		"callers.ingress.ingest.qps", cfg.Callers.Ingress.Ingest.QPS,
		"callers.ingress.ingest.burst", cfg.Callers.Ingress.Ingest.Burst,
		"callers.ingress.read.qps", cfg.Callers.Ingress.Read.QPS,
		"callers.ingress.read.burst", cfg.Callers.Ingress.Read.Burst,
		"diagnostics.retention_days", cfg.Diagnostics.RetentionDays,
		"compaction.keep_recent_groups", cfg.Compaction.KeepRecentGroups,
		"compaction.summary_max_tokens", cfg.Compaction.SummaryMaxTokens,
		"compaction.output_reserve_tokens", cfg.Compaction.OutputReserveTokens,
		"compaction.timeout_seconds", cfg.Compaction.TimeoutSeconds,
	); err != nil {
		return err
	}

	// database：min > max 不可能成立。0 表示「未配置」，由连接池取缺省——
	// 装载层不替它决定。
	if cfg.Database.MaxConns > 0 && cfg.Database.MinConns > cfg.Database.MaxConns {
		return fmt.Errorf(
			"配置项 database.min_conns（%d）大于 database.max_conns（%d）——空闲保底不可能超过池上限",
			cfg.Database.MinConns, cfg.Database.MaxConns)
	}

	// embedding：条件必填——只写连接不写模型 / 维度，等于「以为开了语义检索、
	// 实际没开」，必须拒绝；三段都写了则由启动期与 active 代际比对维度。
	// 语义路径的降级（vector_unavailable）只在 model_connection 为空时发生。
	if cfg.Embedding.ModelConnection != "" {
		missing := missingKeyPaths(tree, "embedding.model", "embedding.dimensions")
		if len(missing) > 0 {
			return fmt.Errorf(
				"配置项 %s 缺失——写了 embedding.model_connection 就必须同时给出模型 id 与维度"+
					"（只配一半会让语义检索看起来已启用，实际没有）",
				strings.Join(missing, "、"))
		}
		if cfg.Embedding.Model == "" {
			return fmt.Errorf("配置项 embedding.model 不得为空——写了 embedding.model_connection 就必须点名模型 id")
		}
		if cfg.Embedding.Dimensions <= 0 {
			return fmt.Errorf("配置项 embedding.dimensions 必须为正数（当前 %d）——向量维度不可能为 0", cfg.Embedding.Dimensions)
		}
	}

	// knowledge：重叠不得吃掉整个窗口（overlap >= max_tokens 时切分无法前进）。
	if cfg.Knowledge.Chunk.MaxTokens > 0 && cfg.Knowledge.Chunk.OverlapTokens >= cfg.Knowledge.Chunk.MaxTokens {
		return fmt.Errorf(
			"配置项 knowledge.chunk.overlap_tokens（%d）不得大于或等于 knowledge.chunk.max_tokens（%d）——重叠吃掉整个窗口时切分无法前进",
			cfg.Knowledge.Chunk.OverlapTokens, cfg.Knowledge.Chunk.MaxTokens)
	}

	// retrieval：重排开关打开就必须点名连接（否则启用即失败，却报在调用时
	// 而不是启动时）。
	if cfg.Retrieval.Rerank.Enabled && strings.TrimSpace(cfg.Retrieval.Rerank.ModelConnection) == "" {
		return fmt.Errorf(
			"配置项 retrieval.rerank.model_connection 缺失——retrieval.rerank.enabled: true 时必须点名重排使用的模型连接")
	}

	// diagnostics：采样率是概率，越界即拒绝（**审计不受它影响**，因此采样率写错
	// 不得静默夹到边界上）。阈值本身由 D-30 实测冻结，装载层不预填。
	if cfg.Diagnostics.SampleRate != nil {
		rate := *cfg.Diagnostics.SampleRate
		if rate < 0 || rate > 1 {
			return fmt.Errorf("配置项 diagnostics.sample_rate 取值非法（%v）——采样率必须落在 0–1 之间", rate)
		}
	}

	// 2026-11-05 用户裁决：`keep_recent_groups` 省略时取部署初始值 4。这是 D-24
	// 「装载层不预填阈值」的唯一例外——其余七个键仍必须显式给出，显式 0 / 负数
	// 仍拒绝。默认只在开关打开时落值；关闭时保持零值，不制造"看起来配了"的假象。
	if cfg.Compaction.Enabled {
		if _, present := lookupKeyPath(tree, "compaction.keep_recent_groups"); !present {
			cfg.Compaction.KeepRecentGroups = defaultKeepRecentGroups
		}
	}
	return validateCompactionSection(tree, cfg.Compaction)
}

// validateCompactionSection 校验压缩段：开关关闭时只判已写取值的合法性，
// 打开时除 `keep_recent_groups` 外的七个键**全部必填并一次列全**（D-24）；
// `keep_recent_groups` 省略时已由调用方补 4，显式 0 / 负数仍拒绝。
// validateCompactionSection checks the compaction section: with the switch off only
// written values are validated; with it on the seven required keys are listed at once.
// keep_recent_groups is defaulted to 4 when omitted by the caller; explicit zero fails.
func validateCompactionSection(tree map[string]any, cfg CompactionConfig) error {
	// 比例键无论在开关哪一档都要判：写了越界值说明配置本身有错，等打开时
	// 才报只会把问题推迟到切换开关的那一刻。
	if _, present := lookupKeyPath(tree, "compaction.trigger_ratio"); present {
		if cfg.TriggerRatio <= 0 || cfg.TriggerRatio >= 1 {
			return fmt.Errorf(
				"配置项 compaction.trigger_ratio 取值非法（%v）——触发比例必须严格落在 0 与 1 之间", cfg.TriggerRatio)
		}
	}
	if _, present := lookupKeyPath(tree, "compaction.safety_margin_ratio"); present {
		if cfg.SafetyMarginRatio < 0 || cfg.SafetyMarginRatio >= 1 {
			return fmt.Errorf(
				"配置项 compaction.safety_margin_ratio 取值非法（%v）——误差缓冲比例必须落在 0（含）与 1（不含）之间，否则可用输入被扣成非正数",
				cfg.SafetyMarginRatio)
		}
	}

	if !cfg.Enabled {
		return nil
	}
	// 开关打开：D-24 要求阈值显式给出，缺任一即拒绝启动，且**一次列全**——
	// 逐键试错会让部署者反复重启。`keep_recent_groups` 是唯一例外：调用方已在
	// 省略时补 4；若显式写出，仍按 ≥1 的取值校验。
	required := []string{
		"compaction.trigger_ratio",
		"compaction.summary_max_tokens",
		"compaction.output_reserve_tokens",
		"compaction.safety_margin_ratio",
		"compaction.timeout_seconds",
		"compaction.prompt_version",
		"compaction.strategy_version",
	}
	missing := missingKeyPaths(tree, required...)
	if len(missing) > 0 {
		return fmt.Errorf(
			"配置项 %s 缺失——compaction.enabled: true 时除 keep_recent_groups 外的七个键全部必填（D-24：装载层不预填阈值；keep_recent_groups 省略时缺省 %d），请补齐后重启",
			strings.Join(missing, "、"), defaultKeepRecentGroups)
	}
	if _, present := lookupKeyPath(tree, "compaction.keep_recent_groups"); present && cfg.KeepRecentGroups < 1 {
		return fmt.Errorf(
			"配置项 compaction.keep_recent_groups 取值非法（%d）——必须是 ≥1 的整数；省略时缺省 %d",
			cfg.KeepRecentGroups, defaultKeepRecentGroups)
	}
	if cfg.TriggerRatio <= 0 || cfg.TriggerRatio >= 1 {
		return fmt.Errorf(
			"配置项 compaction.trigger_ratio 取值非法（%v）——触发比例必须严格落在 0 与 1 之间", cfg.TriggerRatio)
	}
	if cfg.SafetyMarginRatio < 0 || cfg.SafetyMarginRatio >= 1 {
		return fmt.Errorf(
			"配置项 compaction.safety_margin_ratio 取值非法（%v）——误差缓冲比例必须落在 0（含）与 1（不含）之间",
			cfg.SafetyMarginRatio)
	}
	if strings.TrimSpace(cfg.PromptVersion) == "" {
		return fmt.Errorf("配置项 compaction.prompt_version 不得为空——摘要模板版本要进条目与装配记录")
	}
	if strings.TrimSpace(cfg.StrategyVersion) == "" {
		return fmt.Errorf("配置项 compaction.strategy_version 不得为空——切点 / 预算策略版本要进条目与装配记录")
	}
	return nil
}

// rejectNegative 按「键名, 取值」成对入参收集负数，**一次列全**——负数不是
// 「配置风格问题」，而是取值不可能成立，逐个报会让部署者反复重启。
// rejectNegative collects every negative pair and reports them in one message.
func rejectNegative(pairs ...any) error {
	if len(pairs)%2 != 0 {
		return errors.New("内部错误：rejectNegative 需要成对的键名与取值")
	}
	var bad []string
	// 上界写 `i+1 < len(pairs)`：偶数长度在入口已判过，两种写法等价；这种写法让
	// "成对读取"在静态扫描里也是自明的（gosec G602 的索引上界判据认它）。
	for i := 0; i+1 < len(pairs); i += 2 {
		name, _ := pairs[i].(string)
		value := toInt64(pairs[i+1])
		if value < 0 {
			bad = append(bad, fmt.Sprintf("%s=%d", name, value))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("配置项取值不得为负：%s——这些是计数 / 时长 / 容量，负数不可能成立", strings.Join(bad, "、"))
}

// toInt64 把 rejectNegative 收到的整型取值统一成 int64；非整型返回 0
// （类型错误已由解码器拒绝，走到这里的只可能是 int / int64）。
// toInt64 normalizes the integer kinds rejectNegative accepts.
func toInt64(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// lookupKeyPath 按「段.键」路径在原始树里取值；路径每一层都必须是映射。
// lookupKeyPath reads a dotted path from the raw tree.
func lookupKeyPath(tree map[string]any, path string) (any, bool) {
	var node any = tree
	for _, segment := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		child, present := m[segment]
		if !present {
			return nil, false
		}
		node = child
	}
	return node, true
}

// missingKeyPaths 返回树里不存在的路径，保持入参顺序——调用方据此一次列全。
// missingKeyPaths returns the absent paths in input order.
func missingKeyPaths(tree map[string]any, paths ...string) []string {
	var missing []string
	for _, path := range paths {
		if _, present := lookupKeyPath(tree, path); !present {
			missing = append(missing, path)
		}
	}
	return missing
}

// validateSecretsSection 校验 `secrets` 段的装载期形状（T136）。
// validateSecretsSection checks the load-time shape of the secrets section.
//
// 两条规则：
//  1. 整段缺失合法——纯 env 实例不托管凭据，不因缺主密钥而启动失败（Operations §6.1）。
//     但只要写了 `secrets:`（哪怕是空小节），就必须给出 `master_key_file`：写了段
//     等于声明"这台实例托管凭据"，空路径会让部署者以为已经配好，实际每次托管写入
//     才报 CREDENTIAL_UNAVAILABLE。
//  2. 路径不得落在工作区内。装载层只知道配置文件位置，按既有布局（`basalt.yaml`
//     与 `.basalt/` 同级，见 status / init / profile 的工作区口径）做**词法**判定；
//     真实工作区根、符号链接 / junction 与权限属运行期判定，由 `LoadMasterKey`
//     每次使用前复核（本函数不碰文件系统：密钥文件由 `secrets init` 另行生成）。
//
// Two rules: an absent section is legal (pure-env deployments); a written section
// requires the path, and the path may not lexically sit inside the sibling `.basalt/`
// workspace. Runtime validation remains authoritative.
func validateSecretsSection(tree map[string]any, cfg SecretsConfig, configPath string) error {
	if _, present := tree["secrets"]; !present {
		return nil
	}
	if strings.TrimSpace(cfg.MasterKeyFile) == "" && cfg.AutoInitEnabled() {
		// 半截的 secrets 段仍然拒绝：省略路径只在 `auto_init: false`（显式纯 env）
		// 时才是有意的，否则多半是漏写——那种"看着配过、其实没配"的现场必须报出来。
		// A path-less section is legal only with an explicit auto_init: false; otherwise
		// it reads as "configured" while meaning nothing.
		return fmt.Errorf(
			"必填配置项 secrets.master_key_file 缺失——写了 secrets 段就必须给出主密钥文件路径，" +
				"或用 auto_init: false 显式声明纯 env 实例；不托管凭据的实例请整段省略")
	}
	if strings.TrimSpace(cfg.MasterKeyFile) == "" {
		// 纯 env 显式声明：没有路径可做边界判定。
		return nil
	}

	root := filepath.Dir(configPath)
	target := cfg.MasterKeyFile
	if !filepath.IsAbs(target) {
		// 相对路径在运行期按进程工作目录解析；装载层按配置文件所在目录换算，
		// 与文档化的部署布局（配置与工作区同级）一致。
		target = filepath.Join(root, target)
	}
	if pathWithin(filepath.Join(root, workspaceDirName), target) {
		return fmt.Errorf(
			"配置项 secrets.master_key_file 不得位于工作区内（%s/）——主密钥必须放在工作区之外",
			workspaceDirName)
	}
	return nil
}

// pathWithin 判定 target 是否落在 root 之内（含 root 本身）；不同卷视为不包含。
// pathWithin reports whether target lies inside root (root included); a different
// volume never counts as inside.
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// validateAdminAuth 校验 `http.admin_auth` 段的配置形状（约束 1 / 2 / 4 / 5）。
// validateAdminAuth checks the shape of the management-auth section.
//
// 段整体缺失合法：chat / tool / session 等命令不开放 HTTP 管理面（注 12），
// `serve` 的"mode 必填"由监听器创建后的 web.ValidateAdminAuth 判定——那里同时
// 拥有真实监听地址，能判"非回环监听必须显式列出 allowed_hosts"与 `local_trust`
// 只能用于全回环监听。
// An absent section is legal here; serve's presence requirement and the
// listener-dependent rules live in web.ValidateAdminAuth.
//
// 但只要写了段里任何一键而 mode 为空，就在装载期拒绝：半截认证配置是最危险的
// 形态——文件里"看着配过"，实际没有模式，任何"缺省放行"的解读都会让管理面裸奔。
// A section carrying any key without a mode is refused: a half-written auth
// configuration must never be read as "some default mode".
func validateAdminAuth(auth AdminAuthConfig) error {
	if auth.Mode == "" {
		if auth.Token != "" || len(auth.AllowedHosts) > 0 || len(auth.AllowedOrigins) > 0 ||
			len(auth.TrustedProxies) > 0 {
			return fmt.Errorf(
				"必填配置项 http.admin_auth.mode 缺失——一旦配置管理认证段就必须显式给出 mode（token / local_trust），不得留空")
		}
		return nil
	}
	if !validAdminAuthModes[auth.Mode] {
		return fmt.Errorf(
			"配置项 http.admin_auth.mode 取值非法（%s）——只允许 token / local_trust", auth.Mode)
	}
	// token 模式**允许省略** token：第三来源是实例私有目录的 `admin-token` 文件
	// （14D T090，颗粒度文档 §2.4.3）。"三处都没有"由装配层在开放监听之前判定并以
	// 起点名报错——装载层看不到文件系统，在这里补判会把合法的密码文件部署判死。
	// Token may be omitted: the instance file is the third source. The "all three
	// missing" verdict lives in the assembly layer, which can actually see the file.
	if err := validateAdminAllowedHosts(auth.AllowedHosts); err != nil {
		return err
	}
	if err := validateAdminAllowedOrigins(auth.AllowedOrigins); err != nil {
		return err
	}
	return validateAdminTrustedProxies(auth.TrustedProxies)
}

// validateAdminAllowedHosts 逐项校验 Host 白名单的**形状**：不得通配、不得带
// scheme / 路径 / 查询、必须含 1–65535 的数字端口（IPv6 用 [::1]:8080）。
// validateAdminAllowedHosts checks each allowed-host entry: no wildcard, no scheme
// or path, and a numeric port is mandatory.
//
// 为什么省略端口也拒绝：Host 比较要求"端口必须一致"（约束 4）——没有端口的条目
// 永远匹配不上任何带端口的 Host，却会让运维误以为"已经列进去了"。
// A port-less entry can never match a Host carrying a port, yet reads like it does.
func validateAdminAllowedHosts(hosts []string) error {
	for i, entry := range hosts {
		// 文案按**下标**定位，不回显条目原文：host 段可能被人写成 `user:token@host:port`
		// 这类带 userinfo 的形态，回显等于把其中的凭证带进 stdout / 日志。
		// Messages point at the index rather than echoing the entry, because a
		// malformed entry can carry userinfo that must not reach stdout or logs.
		position := i + 1
		if entry == "*" {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项不得使用通配符 *——必须逐项列出 host:port", position)
		}
		if strings.Contains(entry, "@") {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项不得包含 userinfo（@）——只写 host:port", position)
		}
		if strings.Contains(entry, "://") || strings.ContainsAny(entry, "/?#") {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项非法——只写 host:port（不带 scheme、路径或查询）", position)
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项缺端口——必须写成 host:port（IPv6 写成 [::1]:8080）", position)
		}
		if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项的端口非法——必须是 1-65535 的整数", position)
		}
	}
	return nil
}

// validateAdminAllowedOrigins 校验浏览器 Origin 白名单：每项 `scheme://host[:port]`，
// 无路径 / 查询 / userinfo / 通配符；字面量 `null` 同样拒绝（约束 5）。
// validateAdminAllowedOrigins requires each entry to be exactly
// scheme://host[:port]; the literal `null` is refused as well.
//
// 完整匹配是比对面唯一的语义：带尾斜杠或路径的条目永远匹配不上浏览器发来的
// Origin（Origin 只到端口），放行它等于给运维一个不会生效的"白名单"。
// A trailing slash or path can never match a browser Origin, so accepting such an
// entry would only create the illusion of an allowlist.
func validateAdminAllowedOrigins(origins []string) error {
	for i, entry := range origins {
		// 同 Host 白名单：按**下标**定位，不回显原文——Origin 语法允许 userinfo，
		// 而 userinfo 里可能正是凭证。
		// Index-based messages for the same reason: Origin syntax admits userinfo.
		position := i + 1
		if strings.Contains(entry, "*") {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_origins 第 %d 项不得使用通配符——Origin 必须完整列出", position)
		}
		if strings.Contains(entry, "@") {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_origins 第 %d 项不得包含 userinfo（@）——只写 scheme://host[:port]", position)
		}
		parsed, err := url.Parse(entry)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
			parsed.Fragment != "" || strings.Contains(entry, "#") {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_origins 第 %d 项非法——必须写成 scheme://host[:port]（无路径、查询、userinfo 或通配）", position)
		}
	}
	return nil
}

// validateAdminTrustedProxies 校验可信代理网段：每项必须是可解析的 CIDR（约束 6）。
// validateAdminTrustedProxies requires every entry to parse as a CIDR.
//
// 裸 IP 也拒绝：解析失败的可信代理如果被当成"整段放行"或"忽略"，前者扩大信任面、
// 后者让来源归因静默退化——两种失败都没人会发现。
// A bare IP is refused too: silently widening or silently ignoring a bad proxy entry
// both fail invisibly.
func validateAdminTrustedProxies(proxies []string) error {
	for _, entry := range proxies {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return fmt.Errorf(
				"配置项 http.admin_auth.trusted_proxies 条目 %q 不是合法 CIDR——写成 10.0.0.0/8 或 127.0.0.1/32 这样的网段", entry)
		}
	}
	return nil
}

// validateProviderBaseURL 是第 8 类 fail-closed：`base_url` 只支持 scheme + host +
// path 三部分，含 userinfo / query / fragment 的取值**拒绝**（缺失 scheme 不代补；
// 非 http(s) 方案同拒——两种情形文案各说各的，原因不同不混写）。
// validateProviderBaseURL rejects every base_url carrying a component outside
// scheme + host + path; it never rewrites the user's configuration.
//
// 为什么拒绝而不是静默裁剪：删掉这些部分会**改写用户配置的含义**——userinfo 里可能
// 就是认证信息、query 里可能带着网关路由参数；裁剪把认证 / 路由错误推迟成运行时才
// 暴露、且难定位的故障。
// Silently dropping them would rewrite the configuration's meaning and turn an
// authentication or routing error into a hard-to-place runtime failure.
//
// 文案纪律：只点名 `providers[i].base_url` 与不支持的**成分名**，**不回显原 URL、
// 也不回显其中的值**（userinfo 里可能就是一份凭证）——`url.Parse` 的原始错误自带
// 整条 URL，**不得**直传。
// The message names the key path and the offending component only, never the URL
// or any of its values.
func validateProviderBaseURL(raw string, i int) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		// 原错误直传会把整条 URL 带进文案——这里只保留"解析不了"这个事实。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 无法解析——只支持 scheme + host + path（不支持的成分: scheme）", i)
	}
	switch {
	case parsed.Scheme == "":
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: scheme）——缺失 scheme 不代补 https", i)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		// 与上一条分开：方案存在但不受支持时，说"缺失 scheme"是错的事实陈述——
		// 文案只点成分名 + 支持面，不描述被拒的具体取值（取值来自配置，不外带）。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: scheme）——只接受 http 与 https 方案", i)
	case parsed.Host == "":
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: host）", i)
	case parsed.User != nil:
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: userinfo）", i)
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: query）", i)
	case parsed.Fragment != "" || strings.Contains(raw, "#"):
		// 尾随的空 `#`（`…/v1#`）解析后 Fragment 是**空串**，只看成分字段会漏——而拼接端点后
		// `/chat/completions` 整段落进 fragment，请求实际打到 base path（静默改写端点）。
		// `url.URL` 没有 ForceFragment 字段（query 侧才有 ForceQuery），故按字面 `#` 判；
		// 路径里的 `%23` 是合法编码，不含字面 `#`，不连坐。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: fragment）", i)
	}
	return nil
}

// validateHTTPProxy 校验 `http.proxy` 的形态：`http(s)://[userinfo@]host:port`，
// 不带路径 / 查询 / 片段；空串合法（= 直连）。这是**唯一**认可的代理来源——环境变量
// `HTTP_PROXY` / `HTTPS_PROXY` 在任何情况下都不读（14D §2.2.2：环境变量代理会让
// "校验过的地址"与"实际连上的地址"分离）。
// validateHTTPProxy checks the explicit proxy shape; empty means direct egress.
//
// 形态错了拒绝启动，不静默直连：静默直连等于"以为走了代理、实际换了一个出口"。
// 文案**不携带取值**——代理 URL 的 userinfo 可能是一份凭证，回显整条 URL 等于把
// 凭证写进日志与终端。
// A malformed proxy refuses startup; messages never echo the value, whose userinfo may
// carry credentials.
func validateHTTPProxy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("配置项 http.proxy 无法解析——需要 http(s)://[userinfo@]host:port 形态（取值不回显：可能含凭证）")
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return fmt.Errorf("配置项 http.proxy 只接受 http 与 https 方案（取值不回显：可能含凭证）")
	case parsed.Host == "":
		return fmt.Errorf("配置项 http.proxy 缺少主机——需要 http(s)://[userinfo@]host:port")
	case parsed.Port() == "":
		return fmt.Errorf("配置项 http.proxy 必须显式给端口——缺端口会在建连时才失败，装载期就拒绝")
	case parsed.Path != "" && parsed.Path != "/":
		return fmt.Errorf("配置项 http.proxy 不得带路径——代理只由 scheme、主机与端口定位（取值不回显：可能含凭证）")
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return fmt.Errorf("配置项 http.proxy 不得带查询串（取值不回显：可能含凭证）")
	case parsed.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("配置项 http.proxy 不得带片段（取值不回显：可能含凭证）")
	}
	return nil
}

// resolveTree 递归解析 ${VAR} 占位，路径随层级生长（providers[0].api_key）。
// resolveTree walks the decoded tree and resolves ${VAR} placeholders, growing
// the key path as it descends so errors can name the exact location.
//
// 凭证键路径在此先过**类型关**（必须字符串）再进 resolveString 的**形态关**
// （必须整体 ${VAR}）——两关都在解码器上游，数字 / 布尔绕不过去。
// Credential paths pass a type gate here and a shape gate in resolveString, both
// ahead of the decoder.
func resolveTree(node any, path string, lookupEnv func(string) (string, bool)) (any, error) {
	if credentialKey.MatchString(path) {
		raw, ok := node.(string)
		if !ok {
			// 类型关必须在解析**之前**：数字 / 布尔等会经解码器被转成字符串，
			// 连占位形态检查一起绕过（实测：完整工作区下 `api_key: 123456789`
			// 与 `api_key: true` 的 status 判 exit 0）。先要求"字符串"，再谈
			// 占位形态——两关都在解码器上游，明文凭证没有旁路。
			// The type check runs before decoding: numbers and booleans would be
			// coerced into strings by the decoder and bypass the placeholder
			// check entirely.
			return nil, fmt.Errorf(
				"配置项 %s 必须是 ${VAR} 形态的字符串（当前为 %s）——凭证不得写死，也不得以其他类型承载",
				path, nodeTypeName(node))
		}
		return resolveString(raw, path, lookupEnv)
	}
	switch v := node.(type) {
	case string:
		return resolveString(v, path, lookupEnv)
	case map[string]any:
		out := make(map[string]any, len(v))
		// 键序固定（排序后遍历）：同一份坏配置两次运行必须点名同一个键——map 遍历
		// 顺序在 Go 里是随机的，不排序时"先报到哪个缺失变量"每次都不同，排障的人
		// 会以为配置变了。解析本身与顺序无关，这里只为错误文案确定性。
		// Sorted traversal: resolution is order-independent, but which missing key
		// gets reported first must not change between runs.
		for _, k := range slices.Sorted(maps.Keys(v)) {
			child := v[k]
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			rv, err := resolveTree(child, childPath, lookupEnv)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			rv, err := resolveTree(child, fmt.Sprintf("%s[%d]", path, i), lookupEnv)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return node, nil
	}
}

// resolveString 解析单个字符串值。
// resolveString resolves one string value.
//
// 凭证键先判形态再解析：原始值整体不是 ${VAR} 即明文，直接拒绝且不回显该值。
// 非凭证键允许「部分占位」形态（如 http://${HOST}:8080），解析为空不判失败。
// Credential keys are shape-checked first: anything that is not exactly one
// ${VAR} is plaintext and is rejected without echoing it.
func resolveString(raw, path string, lookupEnv func(string) (string, bool)) (any, error) {
	credential := credentialKey.MatchString(path)

	if _, isRef := core.EnvPlaceholderName(raw); credential && !isRef {
		return nil, fmt.Errorf(
			"配置项 %s 是明文凭证——凭证一律写 ${VAR} 占位并经环境变量提供", path)
	}

	var missing string
	resolved := placeholder.ReplaceAllStringFunc(raw, func(token string) string {
		name := token[2 : len(token)-1]
		value, ok := lookupEnv(name)
		if !ok {
			// 记录第一个缺失变量即可——「空串替换」不能绕过（lookupEnv 判的是存在性）。
			if missing == "" {
				missing = name
			}
			return token
		}
		return value
	})
	if missing != "" {
		return nil, fmt.Errorf(
			"配置项 %s 引用的环境变量 %s 不存在——凭证与占位值必须由环境变量提供，不得写死", path, missing)
	}
	if strings.Contains(resolved, "${") {
		return nil, fmt.Errorf(
			"配置项 %s 解析后仍残留 ${ 占位（模板畸形）——检查占位符是否闭合、变量名是否合法", path)
	}
	if credential && resolved == "" && path != "observability.export.headers" {
		return nil, fmt.Errorf("配置项 %s 解析为空——凭证不得为空值", path)
	}
	return resolved, nil
}

// ResolveEnvPlaceholder 用装载层**同一实现**展开单个字符串里的 `${VAR}` 占位。
//
// 只给"不装载整份配置、但必须与在线装载同口径解读占位"的离线命令用
// （`db verify --handoff` 读 `secrets.master_key_file`）：另写一份展开逻辑就会让
// 离线核对与在线行为的差异变成第二套语义。
// ResolveEnvPlaceholder expands `${VAR}` through the very same loader routine so
// offline checks cannot drift from online configuration semantics.
func ResolveEnvPlaceholder(raw, path string, lookupEnv func(string) (string, bool)) (string, error) {
	resolved, err := resolveString(raw, path, lookupEnv)
	if err != nil {
		return "", err
	}
	text, ok := resolved.(string)
	if !ok {
		// resolveString 对字符串输入只会返回字符串；这里是编译期契约的运行时兜底。
		return "", fmt.Errorf("配置项 %s 解析结果不是字符串（类型 %s）", path, nodeTypeName(resolved))
	}
	return text, nil
}

// nodeTypeName 给出节点类型名供错误文案使用——只报类型（%T 的代码事实），
// 不回显值本身；空值也要有人话可读。
// nodeTypeName names the node type for error messages without echoing the value.
func nodeTypeName(node any) string {
	if node == nil {
		return "空值"
	}
	return fmt.Sprintf("%T", node)
}

// asMapping 把解码结果规整成映射；空文档（nil）视为空配置。
// asMapping normalizes the decoded root into a mapping; an empty document is an
// empty configuration.
func asMapping(tree any) (map[string]any, bool) {
	if tree == nil {
		return map[string]any{}, true
	}
	m, ok := tree.(map[string]any)
	return m, ok
}
