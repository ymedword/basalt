package app

// 本文件是通知渠道写面的编排层（颗粒度文档 §3.1.7 / 约束 38）：创建、更新、删除。
// 纯请求校验（名称、类型表、`${VAR}` 占位、环境变量解析、出网白名单预检、名称存在性）
// 一律在 `BeginOperation` **之前**完成——400 / 404 不留操作行、零写入；一旦落了 pending，
// 后续失败回填 failed，并在错误 `data` 里带回可查的 `operation_id`（约束 1a）。
//
// 三条凭证红线：
//   - 库里只存精确 `${VAR}` 占位或空串；写面解析环境变量只为预跑白名单校验，解析结果
//     **不落库、不进响应、不进摘要**；
//   - 错误文案只含变量名与 scheme + host，**不得**回显完整 URL、查询串或解析后的地址；
//   - `NotifyChannelSummary` 携带的是库里原样值（占位或空），响应投影只取 host——这一
//     投影由 web 层既有 `notifyChannelView` 完成，本层不另造第二套。
//
// 删除闸门按删除前解析出的**当前指针 UID** 收集三类证据：绑定投影（含归档 Agent）、
// 未释放的活动 Run 冻结、活动与归档 `AGENT.md` 正文的**整词**引用。任一收集失败即
// 拒绝删除（fail-closed）——`force=true` 只跳过"命中"这一档，**不得**跳过"证明不了
// 没人引用"这一档；强删在 `detail_json` 记 `forced=true`。
// This file orchestrates the notify-channel write surface: create, update and delete.
// Pure request validation runs before the pending row is written, so a rejected
// request leaves no operation row and no bytes behind. Resolved environment values
// never reach storage, summaries or error text. The delete gate collects three evidence
// classes keyed by the current pointer uid - the binding projection, unreleased run
// freezes and whole-word AGENT.md hits - and any collection failure refuses the delete
// (fail-closed); force only bypasses a hit, never an unprovable absence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// NotifyChannelStore 是 `notify_channels` 管理写面的消费方端口（实现在 internal/storage）：
// 写面带 `keepURL` 开关（更新留空 = 保留原值），读面用于写后回读的权威投影。
// NotifyChannelStore is the consumer-side port of the admin write face; keepURL carries
// the contract's "blank url keeps the stored reference" rule.
type NotifyChannelStore interface {
	Create(ctx context.Context, ch core.NotifyChannel) error
	Update(ctx context.Context, ch core.NotifyChannel, keepURL bool) error
	// Delete 按名称解析活动行后写墓碑；`operationID` 是墓碑行与身份行的操作来源
	// （v3 主键是 UID，删除不再物理移除行）。
	// Delete tombstones the active row; the operation id is recorded on the row itself.
	Delete(ctx context.Context, name, operationID string) error
	Get(ctx context.Context, name string) (core.NotifyChannel, error)
}

// NotifyBindingReferrer 是按资源 UID 反查绑定者的读取面（`agent_resource_bindings`
// 投影，R4.6）：活动与归档 Agent 都算——归档目录被重投影后仍能回答"谁还绑着它"。
// 删除闸门只读这张投影，**不得**用"当前没有运行"之类的间接证据推断无引用。
// NotifyBindingReferrer reverse-looks-up the agents bound to one resource uid.
type NotifyBindingReferrer interface {
	ReferencingAgents(ctx context.Context, uid core.ResourceUID) ([]core.AgentResourceBinding, error)
}

// NotifyRunRefReferrer 是按渠道 UID 反查未释放冻结 Run 的读取面（`run_notify_refs`，
// R4.7）：有行即说明还有活动 Run 握着这个渠道的发送授权。
// NotifyRunRefReferrer reports the runs that still freeze one channel.
type NotifyRunRefReferrer interface {
	UnreleasedRunsByChannel(ctx context.Context, uid core.ResourceUID) ([]string, error)
}

// NotifyChannelRequest 是一次通知渠道写入请求（web 层解码后原样交给编排层）。
// 地址三字段按契约 NotifyChannelUpsert / R4.8 解释：`URLSet=false`（字段省略）表示
// 保留已存地址；字段出现即按 `CredentialMode` 的形态校验——env 要精确 `${VAR}` 占位，
// managed 是 write-only 的完整新地址；空串 / 纯空白一律 400，既不保留也不清除。
// 创建时省略地址 = 渠道暂不可发送。
// NotifyChannelRequest is one channel upsert; URLSet distinguishes an omitted address
// (keep) from an explicit value, and a blank value is the contract's 400.
type NotifyChannelRequest struct {
	Name        string
	Type        string
	URL         string
	URLSet      bool
	Description string
	// CredentialMode 是请求显式声明的模式（空 = 省略；创建省略只按 env 规则）。
	CredentialMode string
	// Scope / OwnerAgent 只在**创建**时生效；后续切换走 /scope 端点。
	Scope      string
	OwnerAgent string
}

// 写操作的返回值是**存储行**（`core.NotifyChannel`），不是另造一份摘要：回读到的行
// 带着 UID / 模式 / 范围 / 修订号，响应才能如实回显 managed 渠道与专属范围；另造摘要
// 就得在两处维护同一份字段集。`URL` 仍是库里原样值（`${VAR}` 占位或空串），解析后的
// 真实地址不出上层——web 只按 host 投影。
// The write surface returns the stored row rather than a second summary shape, so the
// response can echo the uid, mode, scope and revision truthfully.

// notifyChannelOperationDetail 是通知渠道写操作的**预写**详情：只记写入前后两次行投影的
// 脱敏摘要（`channelRequestHash` 同一材料，不含解析后的凭证），启动恢复据此把崩溃遗留的
// pending 行判定为 applied / not_applied / indeterminate。空 before = 创建，空 after = 删除。
// notifyChannelOperationDetail carries only redacted row digests written before the change;
// startup recovery classifies the pending row from them and never replays the write.
type notifyChannelOperationDetail struct {
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
}

// NotifyAdminDeps 是通知渠道写面的装配输入。
// `Types` 是与 `GET /notify-channels/types` **同一装配来源**的有序类型表（约束 39）：
// 本层只按它做成员判定，**不**遍历适配器 map。`Policy` 用于写前预跑出网白名单校验；
// 非空 `url` 的写入缺它即装配错误（fail-closed，不静默放行）。
// NotifyAdminDeps wires the write surface. Types is the same ordered slice served by
// GET /notify-channels/types; Policy pre-runs the outbound allowlist check.
type NotifyAdminDeps struct {
	// Root 是工作区根（`.basalt`）：删除闸门的整词扫描在其 `agents/` 与 `archive/`
	// 上进行；空串会在扫描时被 `os.OpenRoot` 拒绝（fail-closed）。
	Root string
	// Store 是渠道管理写仓储（与发送读面同表，零缓存）。
	Store NotifyChannelStore
	// Bindings 是绑定投影的反查面：删除闸门按 UID 找还绑着该渠道的 Agent（含归档）。
	// nil = 删除时装配错误、拒绝删除（fail-closed：证明不了没人引用）。
	// Bindings is the reverse lookup of the binding projection; a nil port refuses deletes.
	Bindings NotifyBindingReferrer
	// RunRefs 是活动 Run 冻结引用的反查面：删除闸门按 UID 找还握着该渠道的运行。
	// nil 同 Bindings 处理（fail-closed）。
	// RunRefs is the reverse lookup of unreleased run-notify refs.
	RunRefs NotifyRunRefReferrer
	// Operations 是管理操作仓储：预写 pending、回填终态。
	Operations core.OperationStore
	// Types 是渠道类型的有序真值表（装配层唯一来源）。
	Types []string
	// Policy 是出网授权面：写前用它预跑 `http.allowed_domains` 判定。
	Policy core.Policy
	// LookupEnv 是 `${VAR}` 解析的唯一来源；nil 时用 `os.LookupEnv`。
	LookupEnv func(string) (string, bool)
	// Logger 记录拒绝与失败（凭证与解析值一律不进日志）。
	Logger *slog.Logger
	// Now 是操作标识与时间戳的唯一时钟来源。
	Now func() time.Time
	// CredentialWriter 是保存路径上的托管凭据写入面（`*CredentialOps`，R4.8）：managed
	// 渠道在落行**之后**写凭据（地址要封进密文），env 切换用它退役身份。nil 表示本次
	// 装配没有凭据写面：managed 写入一律拒绝（503），env 照常。
	CredentialWriter CredentialSaveWriter
	// Scopes 是使用范围解析与转换编排：创建时声明 `scope: exclusive` 在写行之前解析
	// 属主（渠道行自己带范围列）。nil 时专属创建拒绝。
	Scopes *ScopeService
	// TestSender 是渠道连通性测试的发送面：与系统代投共用**同一个** DeliverySender
	// 实例（同一套适配器、同一份凭据解析与策略判定），不另建发送实现。nil 表示本次
	// 装配没有发送面，测试端点按 503 失败关闭、不伪造一次"测试"。
	// TestSender shares the very delivery sender used by scheduled delivery.
	TestSender NotifyTestSender
	// ConnectionTests 是最后测试结果的投影仓储；nil 表示本次装配不保留投影
	// （历史仍在管理操作台账），测试照常执行并返回结果。
	// ConnectionTests is the last-test projection; nil simply skips the projection.
	ConnectionTests core.ResourceConnectionTestStore
	// TestTimeout 是连通性测试的固定超时：缺省 15 秒（与模型连接测试同一预算），
	// 测试可缩短。超时记为 unknown，**不得**自动重发。
	// TestTimeout defaults to 15s; a timeout is unknown and never resent.
	TestTimeout time.Duration
}

// NotifyTestSender 是连通性测试对发送面的最小需求：按已读到的渠道行发一次固定文案。
// 资格（使用范围）不由这里判断——测试是管理员动作，不是某个 Agent 的运行；发送面
// 仍会重新解析凭据、经策略判定，凭据撤销 / 白名单拒绝都会如实成为失败结论。
// NotifyTestSender is the minimal send face for the connectivity test; eligibility is an
// admission concern, while credential resolution and policy still apply per send.
type NotifyTestSender interface {
	Deliver(ctx context.Context, channel core.NotifyChannel, agentName, content string) core.DeliveryOutcome
}

// NotifyAdmin 是通知渠道写面的编排实现；字段全部私有，构造后不再变化。
// NotifyAdmin implements the channel write surface; its fields are fixed at construction.
type NotifyAdmin struct {
	root        string
	store       NotifyChannelStore
	bindings    NotifyBindingReferrer
	runRefs     NotifyRunRefReferrer
	operations  core.OperationStore
	types       []string
	policy      core.Policy
	lookupEnv   func(string) (string, bool)
	logger      *slog.Logger
	now         func() time.Time
	credentials CredentialSaveWriter
	scopes      *ScopeService
	tester      NotifyTestSender
	tests       core.ResourceConnectionTestStore
	testTimeout time.Duration
}

// NewNotifyAdmin 组装通知渠道写面；Logger / Now / LookupEnv 为 nil 时回落安全缺省值。
// 类型表克隆一份，调用方事后改原切片不影响判定。
// NewNotifyAdmin assembles the write surface and clones the type slice.
func NewNotifyAdmin(deps NotifyAdminDeps) *NotifyAdmin {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	lookupEnv := deps.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	testTimeout := deps.TestTimeout
	if testTimeout <= 0 {
		testTimeout = defaultModelTestTimeout
	}
	return &NotifyAdmin{
		root:        deps.Root,
		store:       deps.Store,
		bindings:    deps.Bindings,
		runRefs:     deps.RunRefs,
		operations:  deps.Operations,
		types:       slices.Clone(deps.Types),
		policy:      deps.Policy,
		lookupEnv:   lookupEnv,
		logger:      logger,
		now:         now,
		credentials: deps.CredentialWriter,
		scopes:      deps.Scopes,
		tester:      deps.TestSender,
		tests:       deps.ConnectionTests,
		testTimeout: testTimeout,
	}
}

// Create 登记一条渠道：名称 / 类型 / 占位 / 白名单全部在预写前判定（400 零操作行）；
// 同名创建按契约走 400 `BAD_REQUEST`（不是 409），并留一条 failed 操作行。
// Create registers one channel; every request check runs before the pending row, and a
// duplicate name is the contract's 400 BAD_REQUEST with one failed operation row.
func (s *NotifyAdmin) Create(ctx context.Context, actor core.Actor,
	req NotifyChannelRequest,
) (core.NotifyChannel, error) {
	if err := s.checkReady(); err != nil {
		return core.NotifyChannel{}, err
	}
	// 专属范围在写行之前解析属主：渠道行自带范围列，创建失败零残余，不留"先共享再
	// 转换"的中间态。
	scope, ownerUID, err := s.requestedCreateScope(ctx, core.ActionNotifyChannelCreate,
		req.Name, req.Scope, req.OwnerAgent)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	// 创建没有「当前模式」：省略模式只能落 env；省略地址 = 渠道暂不可发送。
	channel, plan, err := s.validatedChannel(ctx, req, core.ActionNotifyChannelCreate,
		core.CredentialModeEnv, true)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	channel.Scope = scope
	channel.OwnerAgentUID = ownerUID
	// 凭据段要落新密文时先预检主密钥：不可用 → 503，零行、零操作行。
	if err := s.checkCredentialWriteReady(ctx, actor, channel.Name, plan); err != nil {
		return core.NotifyChannel{}, err
	}

	// 身份在预写之前生成：UID 属于这次创建请求本身，任何一种失败路径都不该留下
	// "先有 UID 再判失败"的半成品，但生成失败必须先于操作行拒绝（fail-closed）。
	// The uid is minted before the pending row: a generation failure must leave no row.
	uid, err := core.NewResourceUID()
	if err != nil {
		return core.NotifyChannel{}, fmt.Errorf("创建通知渠道 %s: 生成资源 UID: %w", channel.Name, err)
	}
	channel.ResourceUID = uid

	requestHash := channelRequestHash(channel)
	operationID, err := s.beginOperation(ctx, actor, core.ActionNotifyChannelCreate,
		channel, requestHash,
		notifyChannelOperationDetail{AfterSHA256: requestHash})
	if err != nil {
		return core.NotifyChannel{}, err
	}
	// 渠道行的 `operation_id` 是创建它的管理操作；预写成功之后才知道操作标识。
	// The row records the operation that created it, which exists only after the pre-write.
	channel.OperationID = operationID

	if createErr := s.store.Create(ctx, channel); createErr != nil {
		if errors.Is(createErr, core.ErrConflict) {
			// 同名是契约既有的 400（POST /notify-channels 只列 200/400），不是 409：
			// 判定与文案在这里固化，web 层只按错误类型投影。
			message := fmt.Sprintf("通知渠道 %s 已存在，未执行创建", channel.Name)
			s.logger.Warn("通知渠道同名创建被拒绝",
				"channel", channel.Name, "operation_id", operationID)
			s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, message, nil)
			return core.NotifyChannel{}, postPendingError(&core.InvalidInputError{
				Tool:   core.ActionNotifyChannelCreate,
				Reason: message,
			}, operationID)
		}
		s.logger.Error("创建通知渠道失败",
			"channel", channel.Name, "operation_id", operationID, "error", createErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"创建通知渠道失败", nil)
		return core.NotifyChannel{}, operationFailedError(operationID, "创建通知渠道失败")
	}

	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", nil)

	// 凭据段在行之后：托管地址要封进密文，而密文的 AAD 绑定按 UID 找这一行。失败时
	// 渠道已存在但凭据未就绪（页面重新读取即可看到 managed + 无版本），错误原样上抛。
	if _, err := writeSavedCredential(ctx, s.credentials, actor,
		core.BindingNotifyChannel, channel.Name, uid, plan); err != nil {
		s.logger.Error("通知渠道已创建但凭据写入失败",
			"channel", channel.Name, "operation_id", operationID, "error", err)
		return core.NotifyChannel{}, err
	}
	return s.notifyStoredChannel(ctx, channel), nil
}

// Update rewrites one channel. A missing name is a pre-write 404 and an omitted address
// keeps the stored one; a read-back failure after success is logged, never surfaced as
// a failed update.
func (s *NotifyAdmin) Update(ctx context.Context, actor core.Actor, name string,
	req NotifyChannelRequest,
) (core.NotifyChannel, error) {
	if err := s.checkReady(); err != nil {
		return core.NotifyChannel{}, err
	}
	if strings.TrimSpace(name) == "" {
		return core.NotifyChannel{}, &core.InvalidInputError{
			Tool:   core.ActionNotifyChannelUpdate,
			Reason: "更新通知渠道: 名称不得为空",
		}
	}
	if req.Name != "" && req.Name != name {
		return core.NotifyChannel{}, &core.InvalidInputError{
			Tool:   core.ActionNotifyChannelUpdate,
			Reason: "请求体 name 与路径名称不一致（名称是渠道主键，不支持经更新改名）",
		}
	}

	// 存在性先判：不存在的渠道回 404，不留操作行（失败表通用行）。读到的旧行同时是
	// 预写详情的 before 面、当前模式与"留空保留地址"的来源。
	existing, err := s.store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return core.NotifyChannel{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
		}
		return core.NotifyChannel{}, fmt.Errorf("读取通知渠道 %s: %w", name, err)
	}

	req.Name = name
	channel, plan, err := s.validatedChannel(ctx, req, core.ActionNotifyChannelUpdate,
		existing.CredentialMode, false)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	// 范围与凭据身份不属于更新面：范围切换走 /scope 端点，凭据身份只能由凭据动作改写。
	// 这里把读到的值原样带上，写入形状才与库里那一行自洽（存储层还会再核对一次 UID）。
	channel.ResourceUID = existing.ResourceUID
	channel.Scope = existing.Scope
	channel.OwnerAgentUID = existing.OwnerAgentUID
	channel.CredentialID = existing.CredentialID
	// 重复声明当前范围按幂等接受；真要切换 → 400 指向 /scope，零写入、零操作行。
	if err := checkScopeUpdate(ctx, s.scopes, core.ActionNotifyChannelUpdate,
		req.Scope, req.OwnerAgent, existing.Scope, existing.OwnerAgentUID); err != nil {
		return core.NotifyChannel{}, err
	}

	keepURL := !req.URLSet
	if keepURL {
		// 保留地址时地址相关列整体不动（含 url_host / 凭据模式）：不让"留空更新"顺手
		// 清掉 v3 的凭据列。
		// A keep-url update leaves every address column untouched, so the v3 credential
		// columns come from the stored row instead of being cleared.
		channel.URL = existing.URL
		channel.URLHost = existing.URLHost
	}
	if plan.Retire {
		// managed → env：地址列已是新的 `${VAR}` 占位，撤销托管后行上不得再挂凭据身份。
		channel.CredentialID = ""
	}
	if err := s.checkCredentialWriteReady(ctx, actor, name, plan); err != nil {
		return core.NotifyChannel{}, err
	}

	requestHash := channelRequestHash(channel)
	operationID, err := s.beginOperation(ctx, actor, core.ActionNotifyChannelUpdate,
		channel, requestHash, notifyChannelOperationDetail{
			BeforeSHA256: channelRequestHash(existing),
			AfterSHA256:  requestHash,
		})
	if err != nil {
		return core.NotifyChannel{}, err
	}
	channel.OperationID = operationID

	if updateErr := s.store.Update(ctx, channel, keepURL); updateErr != nil {
		if errors.Is(updateErr, core.ErrResourceNotFound) {
			// 预写与更新之间被别人删掉：变更未发生，但 pending 行已经落下，必须回填。
			message := fmt.Sprintf("通知渠道 %s 不存在", name)
			s.finish(ctx, operationID, core.OperationFailed, agentResultNotFound, message, nil)
			return core.NotifyChannel{}, postPendingError(
				fmt.Errorf("%w: %s", core.ErrResourceNotFound, message), operationID)
		}
		s.logger.Error("更新通知渠道失败",
			"channel", name, "operation_id", operationID, "error", updateErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"更新通知渠道失败", nil)
		return core.NotifyChannel{}, operationFailedError(operationID, "更新通知渠道失败")
	}

	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", nil)

	// 凭据段在地址列之后（托管地址要封进密文，退役要留下新的 `${VAR}` 地址）；它只覆盖
	// 凭据列，不会回写地址。失败时地址已生效、凭据未就绪，错误原样上抛。
	if _, err := writeSavedCredential(ctx, s.credentials, actor,
		core.BindingNotifyChannel, name, existing.ResourceUID, plan); err != nil {
		s.logger.Error("通知渠道地址已更新但凭据写入失败",
			"channel", name, "operation_id", operationID, "error", err)
		return core.NotifyChannel{}, err
	}

	// 写后重新读取权威行（不做乐观更新）：留空更新时 URL 只能是库里的旧值，只有回读
	// 才能如实投影；回读失败不得把已生效的写入改写成错误。
	return s.notifyStoredChannel(ctx, channel), nil
}

// requestedCreateScope 解析创建请求声明的使用范围：缺省 shared；exclusive 必须给属主，
// 属主按范围转换的同一口径解析（存在、可证明身份、未归档）。形状错误是 400，属主解析
// 失败沿用 404 / 409——全部在写行之前。
// requestedCreateScope validates the create-time scope request before any row is written.
func (s *NotifyAdmin) requestedCreateScope(ctx context.Context, action, name, rawScope,
	owner string,
) (core.ResourceScope, core.AgentUID, error) {
	raw := strings.TrimSpace(rawScope)
	owner = strings.TrimSpace(owner)
	if raw == "" && owner == "" {
		return core.ScopeShared, "", nil
	}
	if s.scopes == nil {
		return "", "", errors.New("通知渠道写面缺少范围转换服务，拒绝创建专属渠道")
	}
	return s.scopes.RequestedScope(ctx, action, "通知渠道 "+name, raw, owner)
}

// checkCredentialWriteReady 在写行 / 写盘之前确认凭据段可执行：需要写面而没有 → 503；
// 要落新密文时先让托管写入就绪（必要时完成实例初始化，再校验主密钥可装载；不可用 →
// 503，零写入）。退役只改状态，不需要主密钥。
// checkCredentialWriteReady gates the credential half before any write happens; a first
// managed save may initialize the instance inside the same audited flow.
func (s *NotifyAdmin) checkCredentialWriteReady(ctx context.Context, actor core.Actor,
	name string, plan credentialSavePlan,
) error {
	if !plan.needsWrite() {
		return nil
	}
	if s.credentials == nil {
		return credentialUnavailableError(
			"通知渠道 " + name + ": 本次装配没有凭据写面，拒绝保存托管凭据")
	}
	if plan.Value == nil {
		return nil
	}
	return s.credentials.PrepareManagedWrite(ctx, actor)
}

// Delete 删除一条渠道：存在性（404）与显式身份（409）都在预写之前；随后按**当前
// 指针 UID** 收集三类引用证据——绑定投影（含归档 Agent）、未释放的活动 Run 冻结、
// 活动与归档 `AGENT.md` 的整词引用。任一命中且未带 `force` → 400 点名（操作 failed，
// 行不动）。三类证据只要有一类证明不了，就拒绝删除（fail-closed）——`force` 只跳过
// "命中"这一档，**不得**跳过"证明不了没人引用"这一档；强删在 `detail_json` 记
// `forced=true`。`resourceUID` 必须等于当前指针 UID（契约 §9.5）：缺失、非法或指向
// 旧身份一律 409 `RESOURCE_UID_MISMATCH` 并带当前 UID，**不**按名称猜测目标。
// Delete removes one channel after collecting three evidence classes keyed by the current
// pointer uid. The explicit resource_uid is compared before any operation row exists;
// force only skips a hit, never an unproven absence of references.
func (s *NotifyAdmin) Delete(ctx context.Context, actor core.Actor, name, resourceUID string, force bool) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	if err := s.checkDeleteReady(); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return &core.InvalidInputError{
			Tool:   core.ActionNotifyChannelDelete,
			Reason: "删除通知渠道: 名称不得为空",
		}
	}
	existing, err := s.store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
		}
		return fmt.Errorf("读取通知渠道 %s: %w", name, err)
	}
	if uidErr := expectNotifyResourceUID(name, resourceUID, existing); uidErr != nil {
		return uidErr
	}

	operationID, err := s.beginDeleteOperation(ctx, actor, name, force,
		channelRequestHash(existing))
	if err != nil {
		return err
	}

	bindings, bindingErr := s.bindings.ReferencingAgents(ctx, existing.ResourceUID)
	if bindingErr != nil {
		s.logger.Error("反查资源绑定失败，拒绝删除通知渠道",
			"channel", name, "operation_id", operationID, "error", bindingErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"反查资源绑定失败，无法证明渠道未被引用", nil)
		return operationFailedError(operationID, "删除通知渠道失败：无法核对资源绑定")
	}
	runIDs, runErr := s.runRefs.UnreleasedRunsByChannel(ctx, existing.ResourceUID)
	if runErr != nil {
		s.logger.Error("反查活动运行冻结失败，拒绝删除通知渠道",
			"channel", name, "operation_id", operationID, "error", runErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"反查活动运行冻结失败，无法证明渠道未被引用", nil)
		return operationFailedError(operationID, "删除通知渠道失败：无法核对活动运行引用")
	}
	documents, scanErr := workspace.ScanAgentDocuments(s.root)
	if scanErr != nil {
		// 证明不了"没人引用"时拒绝删除：force 只跳过命中，不跳过扫描失败。
		s.logger.Error("扫描 Agent 定义失败，拒绝删除通知渠道",
			"channel", name, "operation_id", operationID, "error", scanErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"扫描 Agent 定义失败，无法证明渠道未被引用", nil)
		return operationFailedError(operationID, "删除通知渠道失败：无法扫描 Agent 定义")
	}

	// 证据清单在 JSON 里统一成数组（空 = 没有该类引用），不留 null。
	// Evidence lists always encode as arrays; an empty class is [], never null.
	if runIDs == nil {
		runIDs = []string{}
	}
	agents := notifyReferrerAgents(bindings, notifyReferenceHits(documents, name))
	if (len(agents) > 0 || len(runIDs) > 0) && !force {
		message := notifyReferrersMessage(name, agents, runIDs)
		detail, marshalErr := json.Marshal(map[string]any{"agents": agents, "run_ids": runIDs})
		if marshalErr != nil {
			detail = nil
		}
		s.logger.Warn("通知渠道正在被引用，拒绝删除",
			"channel", name, "operation_id", operationID, "agents", agents, "run_ids", runIDs)
		s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, message, detail)
		return &core.AdminError{
			Kind:    &core.InvalidInputError{Tool: core.ActionNotifyChannelDelete, Reason: message},
			Message: message,
			Data:    map[string]any{"agents": agents, "run_ids": runIDs, "operation_id": operationID},
		}
	}

	if deleteErr := s.store.Delete(ctx, name, operationID); deleteErr != nil {
		if errors.Is(deleteErr, core.ErrResourceNotFound) {
			message := fmt.Sprintf("通知渠道 %s 不存在", name)
			s.finish(ctx, operationID, core.OperationFailed, agentResultNotFound, message, nil)
			return postPendingError(fmt.Errorf("%w: %s", core.ErrResourceNotFound, message), operationID)
		}
		s.logger.Error("删除通知渠道失败",
			"channel", name, "operation_id", operationID, "error", deleteErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"删除通知渠道失败", nil)
		return operationFailedError(operationID, "删除通知渠道失败")
	}

	var detail []byte
	if force {
		if raw, marshalErr := json.Marshal(map[string]any{"forced": true}); marshalErr == nil {
			detail = raw
		}
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)
	return nil
}

// expectNotifyResourceUID 校验删除请求显式携带的 `resource_uid`：缺失 / 非法 /
// 与渠道当前指针不一致都回 409 分类的 `core.ErrResourceUIDMismatch`，并在 data 里
// 给出当前 UID，供页面重新读取（契约 §9.5「涉及资源身份的动作」）。
// expectNotifyResourceUID enforces the explicit identity of a channel delete and
// reports the current uid; the target is never guessed by name.
func expectNotifyResourceUID(name, raw string, channel core.NotifyChannel) error {
	current := string(channel.ResourceUID)
	if strings.TrimSpace(raw) == "" {
		return &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("通知渠道 %s 的资源身份缺失：请求必须携带与当前指针一致的 resource_uid", name),
			Data:    map[string]any{"resource_uid": current},
		}
	}
	uid, err := core.ParseResourceUID(raw)
	if err != nil {
		return &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("通知渠道 %s 的 resource_uid 不是合法的资源身份", name),
			Data:    map[string]any{"resource_uid": current},
		}
	}
	if uid != channel.ResourceUID {
		return &core.AdminError{
			Kind: core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("通知渠道 %s 当前身份是 %s，请求给出的 resource_uid 不是当前身份",
				name, current),
			Data: map[string]any{"resource_uid": current},
		}
	}
	return nil
}

// TestChannel 测试一条通知渠道的连通性（R7 范围修订）：先写 pending，再经**与系统
// 代投同一个**发送面发出固定文案「Basalt 连通性测试」；不创建 Run、不写 `llm_calls`、
// 不改渠道行。结果三分：
//
//   - 服务端确认受理（飞书还要求业务回执 code=0）→ 操作 success，`status=success`。
//   - 确定失败（白名单拒绝 / 凭据撤销 / 非 2xx / 飞书回执非 0）→ 操作 success（这次
//     尝试确实完成，结论是失败），`status=failed` + 脱敏原因。
//   - 结果未知（超时 / 传输歧义）→ 操作 unknown，`status=unknown`；**不得**自动重发。
//   - 渠道从未配置地址 → 操作 failed + `NOT_CONFIGURED`，错误映射 503，点名补录入口。
//
// 每次测试都覆盖 `resource_connection_tests` 的最后结果投影；历史以管理操作台账为准。
// TestChannel pre-writes a pending row and sends the fixed connectivity text through the
// very same sender as scheduled delivery; the outcome is three-valued and never resent.
func (s *NotifyAdmin) TestChannel(ctx context.Context, actor core.Actor, name string) (core.NotifyChannelTestResult, error) {
	if err := s.checkReady(); err != nil {
		return core.NotifyChannelTestResult{}, err
	}
	if err := validateResourceID(name); err != nil {
		return core.NotifyChannelTestResult{}, &core.InvalidInputError{
			Tool: core.ActionNotifyChannelTest, Reason: err.Error(),
		}
	}
	// 发送面缺失是装配错误：测试端点不得伪造一次"已测试"，也不留下 last_test。
	// A missing send face is an assembly error; the endpoint never fabricates a test.
	if s.tester == nil {
		return core.NotifyChannelTestResult{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: "通知发送面尚未装配，无法执行连通性测试",
		}
	}

	channel, err := s.store.Get(ctx, name)
	if err != nil {
		return core.NotifyChannelTestResult{}, err
	}

	operationID, err := s.beginOperation(ctx, actor, core.ActionNotifyChannelTest, channel,
		agentRequestHash(core.ActionNotifyChannelTest+"\n"+name),
		notifyChannelOperationDetail{})
	if err != nil {
		return core.NotifyChannelTestResult{}, err
	}

	if !notifyChannelHasAddress(channel) {
		const reason = "渠道未配置地址（创建时可省略）；请先在渠道详情补录地址后重试"
		detail, marshalErr := json.Marshal(notifyTestOperationDetail{
			Status: core.ConnectionTestFailed, Detail: "渠道未配置地址",
		})
		if marshalErr != nil {
			detail = nil
		}
		s.finish(ctx, operationID, core.OperationFailed, resourceResultNotConfigured,
			"渠道未配置地址，无法测试", detail)
		s.recordConnectionTest(ctx, channel, core.ConnectionTestFailed, "渠道未配置地址", operationID)
		return core.NotifyChannelTestResult{Status: core.ConnectionTestFailed, Detail: "渠道未配置地址"},
			&core.AdminError{
				Kind:    core.ErrProviderNotConfigured,
				Message: fmt.Sprintf("通知渠道 %s 未配置地址：%s", name, reason),
				Data:    map[string]any{"operation_id": operationID},
			}
	}

	testCtx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	started := time.Now()
	outcome := s.tester.Deliver(testCtx, channel, "", core.NotifyConnectivityTestText)
	latency := time.Since(started).Milliseconds()

	status := notifyTestStatus(outcome.Status)
	result := core.NotifyChannelTestResult{
		OK:        status == core.ConnectionTestSuccess,
		Status:    status,
		LatencyMS: latency,
		Detail:    outcome.Detail,
	}
	detail, marshalErr := json.Marshal(notifyTestOperationDetail{
		Status:      status,
		LatencyMS:   latency,
		HTTPStatus:  outcome.HTTPStatus,
		ReceiptCode: outcome.ReceiptCode,
		Detail:      outcome.Detail,
	})
	if marshalErr != nil {
		detail = nil
	}

	switch status {
	case core.ConnectionTestSuccess:
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)
	case core.ConnectionTestFailed:
		// 尝试已完成（请求已发出或明确被拒），操作记 success，结论在 status 里。
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)
	default:
		code := lifecycleResultInternal
		if testCtx.Err() != nil {
			code = resourceResultTimeout
		}
		s.finish(ctx, operationID, core.OperationUnknown, code,
			"渠道连通性测试结果未知（可能已送达，请查证后手动重试）", detail)
	}
	s.recordConnectionTest(ctx, channel, status, outcome.Detail, operationID)
	return result, nil
}

// notifyChannelHasAddress 判定渠道行是否有可发送的地址配置：env 模式看 `${VAR}` 引用，
// managed 模式一律交给发送面（地址在密文里，能不能解开由解析器判定）。它只做
// 「有没有配置」这一级判断，不预判网络可达性。
// notifyChannelHasAddress only answers "is an address configured"; reachability belongs
// to the send path.
func notifyChannelHasAddress(channel core.NotifyChannel) bool {
	if channel.CredentialMode == core.CredentialModeManaged {
		return true
	}
	return strings.TrimSpace(channel.URL) != ""
}

// notifyTestStatus 把投递三分映射成连接测试三值：sent → success，failed → failed，
// 其余（含未知字面量）一律 unknown——"证明不了成功"不能写成失败，更不能写成成功。
// notifyTestStatus maps sent/failed to the test literals and treats anything else as
// unknown: an unprovable outcome is never rewritten as either success or failure.
func notifyTestStatus(status core.DeliveryStatus) string {
	switch status {
	case core.DeliverySent:
		return core.ConnectionTestSuccess
	case core.DeliveryFailed:
		return core.ConnectionTestFailed
	default:
		return core.ConnectionTestUnknown
	}
}

// recordConnectionTest 覆盖最后结果投影。投影写失败只记 ERROR：权威记录是操作台账，
// 把已完成的测试说成失败会让管理员以为没测过而重复发送。
// recordConnectionTest upserts the projection; a failure is logged because the
// operation ledger is authoritative and a completed test is never reported as failed.
func (s *NotifyAdmin) recordConnectionTest(ctx context.Context, channel core.NotifyChannel,
	status, detail, operationID string,
) {
	if s.tests == nil {
		return
	}
	rec := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: channel.ResourceUID,
		TestedAt:    s.now().UTC(),
		Status:      status,
		Detail:      detail,
		OperationID: operationID,
	}
	if err := s.tests.RecordConnectionTest(context.WithoutCancel(ctx), rec); err != nil {
		s.logger.Error("写入通知渠道最后测试结果失败（操作台账仍为权威记录）",
			"channel", channel.Name, "operation_id", operationID, "error", err)
	}
}

// notifyTestOperationDetail 是渠道测试操作 `detail_json` 的脱敏投影：状态、耗时、
// HTTP 状态与飞书回执码、固定短语；**不含**地址原值、凭据或响应正文。
// notifyTestOperationDetail is the redacted detail of one connectivity test.
type notifyTestOperationDetail struct {
	Status      string `json:"status"`
	LatencyMS   int64  `json:"latency_ms"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	ReceiptCode *int   `json:"receipt_code,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// checkReady 判定装配是否完整：缺仓储或操作台账是装配错误（500），不是空渠道表。
// checkReady reports an incomplete assembly; a missing repository is never an empty
// registry.
func (s *NotifyAdmin) checkReady() error {
	if s.store == nil || s.operations == nil {
		return errors.New("通知渠道写面依赖不完整：仓储与操作台账必须同时提供（装配错误）")
	}
	return nil
}

// checkDeleteReady 是删除闸门的额外装配判据：绑定投影与活动 Run 冻结引用缺一不可
// （fail-closed）。创建 / 更新不读这两张表，因此不在 `checkReady` 里拦截。
// checkDeleteReady guards the delete gate only: both reverse lookups are mandatory,
// because a missing one would turn "cannot prove" into "nothing references it".
func (s *NotifyAdmin) checkDeleteReady() error {
	if s.bindings == nil || s.runRefs == nil {
		return errors.New("通知渠道删除闸门依赖不完整：绑定投影与活动运行引用必须同时提供（装配错误）")
	}
	return nil
}

// validatedChannel 是 Create / Update 共用的纯请求校验：名称 → 类型表 → 模式与地址
// 形态 →（env）环境变量解析 + 出网白名单预检 /（managed）地址合法性与白名单预检。
// 任何一步失败都在预写之前返回，零落库、零操作行。解析结果只用于白名单判定，
// **不进入**返回值；managed 的完整地址只进凭据密文，行上只留 scheme + host。
// validatedChannel is the shared request check; a managed address never enters the row,
// only its scheme+host does.
func (s *NotifyAdmin) validatedChannel(ctx context.Context, req NotifyChannelRequest,
	action string, currentMode core.CredentialMode, creating bool,
) (core.NotifyChannel, credentialSavePlan, error) {
	if strings.TrimSpace(req.Name) == "" {
		return core.NotifyChannel{}, credentialSavePlan{}, &core.InvalidInputError{
			Tool:   action,
			Reason: "通知渠道: 名称不得为空",
		}
	}
	if req.Type == "" {
		return core.NotifyChannel{}, credentialSavePlan{}, &core.InvalidInputError{
			Tool:   action,
			Reason: "通知渠道 " + req.Name + ": 类型不得为空",
		}
	}
	if !slices.Contains(s.types, req.Type) {
		return core.NotifyChannel{}, credentialSavePlan{}, &core.InvalidInputError{
			Tool: action,
			Reason: fmt.Sprintf("通知渠道 %s: 类型 %s 不在支持类型表（可选：%s）",
				req.Name, req.Type, strings.Join(s.types, "、")),
		}
	}

	plan, err := planCredentialSave(action, "通知渠道 "+req.Name, req.CredentialMode,
		req.URL, req.URLSet, currentMode, creating)
	if err != nil {
		return core.NotifyChannel{}, credentialSavePlan{}, err
	}
	channel := core.NotifyChannel{
		Name:           req.Name,
		Type:           req.Type,
		Description:    req.Description,
		CredentialMode: plan.Mode,
	}
	switch {
	case plan.Mode == core.CredentialModeManaged:
		if !req.URLSet {
			// 省略 = 保留已封存的地址（managed 行上 URL 恒为空，没有可写回的值）。
			return channel, plan, nil
		}
		host, err := s.managedAddressHost(ctx, action, req.Name, req.URL)
		if err != nil {
			return core.NotifyChannel{}, credentialSavePlan{}, err
		}
		// 完整地址进凭据密文；展示列只留 scheme + host。
		channel.URLHost = host
		return channel, plan, nil
	case req.URLSet:
		host, err := s.envAddressHost(ctx, action, req.Name, req.URL)
		if err != nil {
			return core.NotifyChannel{}, credentialSavePlan{}, err
		}
		channel.URL = req.URL
		channel.URLHost = host
	}
	return channel, plan, nil
}

// envAddressHost 解析 `${VAR}` 占位并预跑出网白名单，返回展示用的 scheme + host。
// 解析值只用于判定，不落库、不进响应、不进摘要。
// envAddressHost resolves the placeholder for the allowlist verdict and returns the
// display host only.
func (s *NotifyAdmin) envAddressHost(ctx context.Context, action, name, placeholder string,
) (string, error) {
	varName, ok := core.EnvPlaceholderName(placeholder)
	if !ok {
		return "", &core.InvalidInputError{
			Tool:   action,
			Reason: "通知渠道 " + name + " 的地址必须是 ${VAR} 占位（凭证不得明文入库）",
		}
	}
	value, ok := s.lookupEnv(varName)
	if !ok || value == "" {
		return "", &core.InvalidInputError{
			Tool:   action,
			Reason: fmt.Sprintf("环境变量 ${%s} 未设置或为空，拒绝登记该渠道", varName),
		}
	}
	if err := s.allowedAddress(ctx, action,
		fmt.Sprintf("环境变量 ${%s} 指向的地址未通过出网白名单校验", varName), value); err != nil {
		return "", err
	}
	// `url_host` 只存 scheme + host（契约：对外一律只回主机部分）；解析后的完整地址
	// 仍然只用于白名单判定，不落库、不进响应。
	return notifyTargetHost(value), nil
}

// managedAddressHost 校验托管渠道的完整新地址（绝对 URL / http 或 https / 无 userinfo）
// 并预跑出网白名单，返回展示用的 scheme + host。地址本身只进凭据密文：本函数不把它
// 写进行、错误或日志。
// managedAddressHost validates a managed address and returns its display host; the
// address itself only ever reaches the credential ciphertext.
func (s *NotifyAdmin) managedAddressHost(ctx context.Context, action, name, address string,
) (string, error) {
	if err := validateManagedAddress(address); err != nil {
		var invalid *core.InvalidInputError
		if errors.As(err, &invalid) {
			return "", &core.InvalidInputError{
				Tool:   action,
				Reason: "通知渠道 " + name + ": " + invalid.Reason,
			}
		}
		return "", err
	}
	if err := s.allowedAddress(ctx, action,
		"通知渠道 "+name+" 的托管地址未通过出网白名单校验", address); err != nil {
		return "", err
	}
	return notifyTargetHost(address), nil
}

// allowedAddress 用出网策略预跑一次地址判定。`reason` 是调用方给固定短语，失败时只追加
// scheme + host——完整地址、查询串与凭证值一律不回显。装配缺策略按错误拒绝（fail-closed）。
// allowedAddress pre-runs the outbound allowlist verdict; the message never echoes the
// address, only its scheme+host.
func (s *NotifyAdmin) allowedAddress(ctx context.Context, action, reason, address string) error {
	if s.policy == nil {
		return errors.New("通知渠道写面缺少出网策略，拒绝解析地址（装配错误）")
	}
	if policyErr := s.policy.Check(ctx, core.Action{
		Type:   core.ActionHTTPRequest,
		Target: address,
	}); policyErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if host := notifyTargetHost(address); host != "" {
			reason += "（" + host + "）"
		}
		return &core.InvalidInputError{Tool: action, Reason: reason}
	}
	return nil
}

// beginOperation 预写一条 pending 操作行（含行摘要详情）并返回操作标识；失败即拒绝执行，
// 目标零写入。详情必须在改行**之前**落库，否则崩溃后的恢复没有判据。
// beginOperation pre-writes the pending row together with its row-digest detail; the
// detail must land before the change so startup recovery has a verdict material.
func (s *NotifyAdmin) beginOperation(ctx context.Context, actor core.Actor, action string,
	channel core.NotifyChannel, requestHash string, detail notifyChannelOperationDetail,
) (string, error) {
	detailJSON, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		// 结构只有字符串字段，此分支不可达；仍不吞错误，按预写失败处理。
		return "", fmt.Errorf("%s 序列化预写详情失败，拒绝执行: %w", action, marshalErr)
	}
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindNotifyChannel,
		TargetID:     channel.Name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := s.operations.BeginOperation(ctx, record); beginErr != nil {
		s.logger.Error("通知渠道写预写操作记录失败，拒绝执行",
			"action", action, "channel", channel.Name, "error", beginErr)
		return "", fmt.Errorf("%s 预写操作记录失败，拒绝执行: %w", action, beginErr)
	}
	return operationID, nil
}

// beginDeleteOperation 是删除动作的预写：请求摘要只含渠道名与 force 开关，详情只记删除
// 前那一行的脱敏摘要（after 留空 = 删除）。
// beginDeleteOperation pre-writes the delete row; its detail carries only the digest of
// the row being removed.
func (s *NotifyAdmin) beginDeleteOperation(ctx context.Context, actor core.Actor,
	name string, force bool, beforeSHA256 string,
) (string, error) {
	return s.beginOperation(ctx, actor, core.ActionNotifyChannelDelete,
		core.NotifyChannel{Name: name},
		agentRequestHash(fmt.Sprintf("%s\n%t", name, force)),
		notifyChannelOperationDetail{BeforeSHA256: beforeSHA256})
}

// finish 回填终态；终态回填不随请求取消而放弃，失败只记 ERROR（留待启动恢复查证）。
// finish writes the terminal state detached from request cancellation; a failure is
// logged and left to startup recovery.
func (s *NotifyAdmin) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail []byte,
) {
	if err := s.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, s.now()); err != nil {
		s.logger.Error("通知渠道管理写回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// notifyReferencePort 把工作区 Agent 文档的整词扫描适配成 web 的反向引用端口
// （US-12 R3.6）：与删除闸门复用同一份 `ScanAgentDocuments` / `notifyReferenceHits`，
// 不另建引用索引——页面显示与删除判定同源。它是**文档引用**，不是精确绑定：
// 正文提及（含注释、归档）都算；精确的可用范围在 AGENT.md 的 notify_channels。
// notifyReferencePort adapts the delete gate's document scan to the web reverse-reference
// port, so the page and the delete gate can never disagree. It is a textual reference,
// not the exact binding declared by notify_channels.
type notifyReferencePort struct{ root string }

// NotifyReferences 扫描一次工作区，按请求的渠道名分组返回引用者；
// 扫描失败原样上抛（读面按 500 失败关闭，不得降级成空集合）。
// NotifyReferences scans once and groups hits per channel; failures propagate.
func (p notifyReferencePort) NotifyReferences(ctx context.Context, channels []string) (map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	documents, err := workspace.ScanAgentDocuments(p.root)
	if err != nil {
		return nil, err
	}
	references := make(map[string][]string, len(channels))
	for _, name := range channels {
		references[name] = notifyReferenceHits(documents, name)
	}
	return references, nil
}

// notifyReferenceHits 返回正文整词命中该渠道名的文档点名：活动文档取目录名、归档文档
// 取 `archive:<archive_id>`；结果升序去重（同一份输入两次调用得到同一顺序）。
// notifyReferenceHits names the documents whose body matches the channel as a whole
// word; archived documents are named archive:<id>, and the result is sorted and deduped.
func notifyReferenceHits(documents []workspace.AgentDocument, channel string) []string {
	hits := make([]string, 0, len(documents))
	for _, doc := range documents {
		if !workspace.WholeWordMatch(doc.Text, channel) {
			continue
		}
		if doc.Archived {
			hits = append(hits, "archive:"+doc.ArchiveID)
			continue
		}
		hits = append(hits, doc.Name)
	}
	slices.Sort(hits)
	return slices.Compact(hits)
}

// notifyReferrerAgents 合并两类点名证据：绑定投影（精确的 notify_channels 声明，含
// 归档 Agent 被重投影后的行）与 `AGENT.md` 正文整词命中（含注释、归档目录）。
// 两类去重后排序——同一个 Agent 从两条证据路径被点到时只出现一次；归档目录没有活动
// 名称，沿用扫描给出的 `archive:<id>` 形态，**不**自行编造名称。
// notifyReferrerAgents merges exact binding referrers with whole-word document hits,
// deduplicated and sorted; archived directories keep the scan's `archive:<id>` label.
func notifyReferrerAgents(bindings []core.AgentResourceBinding, hits []string) []string {
	agents := make([]string, 0, len(bindings)+len(hits))
	for _, binding := range bindings {
		if binding.AgentName == "" {
			continue
		}
		agents = append(agents, binding.AgentName)
	}
	agents = append(agents, hits...)
	slices.Sort(agents)
	return slices.Compact(agents)
}

// notifyReferrersMessage 拼出 400 的固定文案：只点名引用者，不回显渠道地址或凭据。
// notifyReferrersMessage builds the 400 text; it names referrers only and never echoes
// the channel address or any credential.
func notifyReferrersMessage(channel string, agents, runIDs []string) string {
	parts := make([]string, 0, 2)
	if len(agents) > 0 {
		parts = append(parts, "Agents: "+strings.Join(agents, ", "))
	}
	if len(runIDs) > 0 {
		parts = append(parts, "Runs: "+strings.Join(runIDs, ", "))
	}
	return fmt.Sprintf("通知渠道 %s 正在被引用，解除引用后才能删除（%s）",
		channel, strings.Join(parts, "；"))
}

// notifyStoredChannel 是写成功之后的权威回读：回读失败只记 ERROR 并退回本次写入的
// 行——写入已生效，**不得**把回读失败改写成写失败（与 Update 的回读纪律同一条）。
// 退回的行带着 UID / 模式 / 范围 / 修订号（Create 的修订号是列默认值 1），只是缺少
// 凭据展示事实与 ETag；调用方据此仍能如实回显身份，只是不承诺条件写值。
// notifyStoredChannel re-reads the stored row after a successful write; a read-back
// failure is logged and never turned into a failed write.
func (s *NotifyAdmin) notifyStoredChannel(ctx context.Context, stored core.NotifyChannel) core.NotifyChannel {
	read, err := s.store.Get(context.WithoutCancel(ctx), stored.Name)
	if err != nil {
		s.logger.Error("通知渠道写后回读失败（写入已生效）",
			"channel", stored.Name, "error", err)
		if stored.Revision == 0 {
			// Create 不写 revision 列，列默认值是 1；回读失败时按同一默认值回显，
			// 不让"刚建好的渠道修订号是 0"这种不可能状态出现在响应里。
			stored.Revision = 1
		}
		return stored
	}
	return read
}

// channelRequestHash 摘要一次渠道写入：材料只含占位符与公开字段，**不含**解析后的
// 凭证值（与模型连接写面同一纪律）。
// channelRequestHash digests one channel upsert; the material never includes the
// resolved credential.
func channelRequestHash(channel core.NotifyChannel) string {
	return agentRequestHash(strings.Join([]string{
		channel.Name, channel.Type, channel.URL, channel.Description,
	}, "\n"))
}

// notifyTargetHost 把解析后的出网地址投影成 scheme + host；解析不出（含 IP 字面量之外
// 的畸形值）回空串——错误文案只允许出现 host，**不得**退回原文。
// notifyTargetHost projects an outbound address to scheme+host and returns "" when it
// cannot; the raw value is never echoed.
func notifyTargetHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
