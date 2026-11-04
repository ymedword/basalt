package core

import (
	"strings"
)

// 本文件声明调用方身份与执行者身份（US-16 §3.1.0 / C-28 / C-32）。
//
// 三条硬约束：
//  1. **零值非法**——`Principal{}` 的 `Valid()` 为 false，任何路径都不得把它补成
//     内置管理员；身份只能由认证中间件或显式构造入口产生。
//  2. **归属与执行者分开**——`CallerID` 恒为任务归属方（配额、审计、会话归属都记
//     它），`Actor` 只记录"这次是谁在执行"，两者不得互相替代、不得互相推导。
//  3. **授权来源显式二分**——`AuthorizingKind` 必须与归属一起持久化，不得在运行时
//     按"有没有 key_id"猜测。
//
// This file declares the caller and execution-actor identities. Three hard rules:
// the zero value is invalid, ownership (CallerID) is distinct from the executor
// (Actor), and the authorizing source is an explicit two-way kind, never inferred.

// PrincipalKind 是 Principal 的来源类别：外部调用方 Key（caller）或内置管理员
// （local-admin）。它只有这两个取值，且**不得**互相冒充。
// PrincipalKind names where a principal comes from: a caller key or built-in admin.
type PrincipalKind string

const (
	// PrincipalCaller 表示以调用方 Key 认证的外部请求。
	// PrincipalCaller is a request authenticated by a caller key.
	PrincipalCaller PrincipalKind = "caller"
	// PrincipalLocalAdmin 表示内置管理员——admin token 中间件或进程内 CLI 管理命令
	// 显式构造；`caller_keys` 无法表达该 Kind。
	// PrincipalLocalAdmin is the built-in administrator, constructed explicitly only.
	PrincipalLocalAdmin PrincipalKind = "local-admin"
)

// LocalAdminCallerID 是内置管理员的固定归属标识（`sessions.caller_id`、
// `scheduled_tasks.caller_id`、`runs.caller_id` 同值）。它是保留标识：Kind 为
// PrincipalCaller 的身份**不得**使用它——那正是"零值默认成管理员"之外的第二条
// 提权路径。
// LocalAdminCallerID is the reserved ownership id of the built-in administrator;
// a caller principal must never claim it.
const LocalAdminCallerID = "local-admin"

// Valid 判定来源类别是否是闭集内的取值；零值与未知值一律拒绝。
// Valid accepts exactly the two literals; the zero value and unknown kinds fail.
func (k PrincipalKind) Valid() bool {
	return k == PrincipalCaller || k == PrincipalLocalAdmin
}

// ActorKind 是**执行者**类别：谁发起了这次执行。它与归属方（Principal.CallerID）
// 正交——调度器替调用方执行时，归属仍是调用方，执行者是 scheduler。
//
// 命名裁决（2026-10-28，用户授权 Claude 代行技术评审与裁决）：设计卡 §3.1.0 的
// 类型名 `Actor` 与 `internal/core/management.go:82` 既有的管理操作主体
// `type Actor struct{ Subject, Method, Source string }`（US-11 交付、全仓 260+
// 处引用）冲突。为不改动已验收的公共类型，新枚举取名为 `ActorKind`；四个常量
// `ActorCaller` / `ActorScheduler` / `ActorRecovery` / `ActorCLIAdmin` 逐字保留，
// 设计卡中的调用示例（`core.OwnedPrincipal(..., core.ActorCaller)`）因此原样成立。
//
// ActorKind is the executor category, orthogonal to ownership. The design card calls
// it `Actor`, which collides with the existing management-actor struct; the enum is
// therefore named ActorKind with the four constants kept verbatim.
type ActorKind string

const (
	// ActorCaller 表示外部请求（HTTP / 调用方 Key）。
	// ActorCaller is an external request.
	ActorCaller ActorKind = "caller"
	// ActorScheduler 表示定时触发。
	// ActorScheduler is a scheduled trigger.
	ActorScheduler ActorKind = "scheduler"
	// ActorRecovery 表示崩溃续跑。
	// ActorRecovery is crash recovery.
	ActorRecovery ActorKind = "recovery"
	// ActorCLIAdmin 表示运维命令（进程内 CLI 管理操作）。
	// ActorCLIAdmin is a local administrative command.
	ActorCLIAdmin ActorKind = "admin-cli"
)

// Valid 判定执行者类别是否是闭集内的取值；空值同样拒绝——**不得**退化成 caller
// 默认值，否则"漏设置"会被静默记成外部调用。
// Valid accepts exactly the four literals; the empty value fails so that a missing
// setting can never be silently recorded as an external call.
func (a ActorKind) Valid() bool {
	switch a {
	case ActorCaller, ActorScheduler, ActorRecovery, ActorCLIAdmin:
		return true
	default:
		return false
	}
}

// AuthorizingKind 是**执行授权来源**的二分：调用方 Key 还是部署内置的管理员能力。
// 它必须与归属一起持久化（`scheduled_tasks.authorizing_kind` /
// `runs.authorizing_kind`），**不得**在运行时按"有没有 key_id"猜测——迁移与旧行
// 需要显式判定；两类**不得**互相转换。
// AuthorizingKind is the explicit two-way authorizing source persisted next to the
// owner; it is never inferred from the presence of a key id.
type AuthorizingKind string

const (
	// AuthorizingKindCallerKey 表示由调用方的一把具体 Key 授权：`keyID` 必须非空，
	// 且执行前要重读这一把 Key 的当前状态。
	// AuthorizingKindCallerKey means a specific caller key authorizes the run.
	AuthorizingKindCallerKey AuthorizingKind = "caller-key"
	// AuthorizingKindAdminBuiltin 表示由部署内置的管理员能力授权：归属固定为
	// LocalAdminCallerID，且没有授权 Key。
	// AuthorizingKindAdminBuiltin means the deployment's built-in admin capability.
	AuthorizingKindAdminBuiltin AuthorizingKind = "admin-builtin"
)

// Valid 判定授权来源是否是闭集内的取值；空值与未知值一律拒绝。
// Valid accepts exactly the two literals; the zero value and unknown kinds fail.
func (k AuthorizingKind) Valid() bool {
	return k == AuthorizingKindCallerKey || k == AuthorizingKindAdminBuiltin
}

// AdminPosture 是**实例管理能力**的启动期姿态（US-16 §2.14 / C-42 / D-31）：
// 它回答"这台实例的管理能力是否可用、以哪种形态可用"，不是"本次请求带了什么
// 凭据"——后者由 Principal 表达，两者**不得**互相推导。
//
// 三条硬约束：
//  1. **两个合法启用态**——`token`（启动期已解析管理凭据）与 `local_trust`
//     （全部监听回环、无转发头）都让 `admin-builtin` 后台任务**有权运行**；
//     `local_trust` **不得**被当成"认证关闭"。
//  2. **没有"认证关闭"第三态**——配置缺失 / 非法在监听前就拒绝启动，进程不存在
//     就没有姿态；也**不得**由"token 是否为空"推断管理能力（那正是被否决的
//     `Authenticator.AdminEnabled()` 的错误）。
//  3. **`unavailable` 仅测试可注入**——真实启动不可达（非法配置已被拒绝）；注入后
//     后台执行 fail-closed（原因码 `admin_auth_unavailable`）。**不得**用它表达
//     "认证关闭"，也**不得**为它新增配置开关。
//
// AdminPosture is the startup-time posture of the instance's admin capability. Both
// token and local_trust are enabled states; unavailable exists for tests only and is
// never produced by the real startup path.
type AdminPosture string

const (
	// AdminPostureToken 表示 admin token 已在启动期解析成功，管理端点接受该凭据。
	// AdminPostureToken means a resolved admin token guards the management surface.
	AdminPostureToken AdminPosture = "token"
	// AdminPostureLocalTrust 表示全部监听回环且无转发头；它是**合法启用态**，只说明
	// 本机管理请求受信，不表示认证被关闭，也不表示远程请求受信。
	// AdminPostureLocalTrust is a valid enabled state, never "authentication off".
	AdminPostureLocalTrust AdminPosture = "local_trust"
	// AdminPostureUnavailable 表示管理能力不可用；真实启动不可达，只允许测试注入，
	// 此时后台执行 fail-closed 且不外发。
	// AdminPostureUnavailable is test-injectable only; the real startup never reaches it.
	AdminPostureUnavailable AdminPosture = "unavailable"
)

// Valid 判定姿态是否是闭集内的取值；空值与未知值一律拒绝——零值**不得**被当作
// 可运行姿态（拒绝启动时返回的就是零值）。
// Valid accepts exactly the three literals; the zero value is never a runnable posture.
func (p AdminPosture) Valid() bool {
	switch p {
	case AdminPostureToken, AdminPostureLocalTrust, AdminPostureUnavailable:
		return true
	default:
		return false
	}
}

// —— scope 字面量（契约 `openapi.yaml` / `run-and-errors.md` 冻结，代码里只此一份）——
// Scope literals frozen by the contracts; this is their single Go encoding.

const (
	// ScopeSessionsRead 允许列出会话、检索历史、精确读取**自己的**会话消息。
	// ScopeSessionsRead allows listing sessions and reading one's own messages.
	ScopeSessionsRead = "sessions:read"
	// ScopeSessionsWrite 允许创建会话、发送消息 / 发起 Run、取消自己的 Run。
	// ScopeSessionsWrite allows creating sessions, sending messages and running.
	ScopeSessionsWrite = "sessions:write"
	// ScopeKnowledgeRead 允许检索、来源视图与文档 / 版本列表（仍受集合 ACL 二次过滤）。
	// ScopeKnowledgeRead allows retrieval and source views.
	ScopeKnowledgeRead = "knowledge:read"
	// ScopeKnowledgeWrite 允许建集合 / 文档、删除与重建索引。
	// ScopeKnowledgeWrite allows building and maintaining collections and documents.
	ScopeKnowledgeWrite = "knowledge:write"
	// ScopeFlowsRead allows safe definition metadata and one's own durable tasks.
	ScopeFlowsRead = "flows:read"
	// ScopeFlowsWrite allows accepting and controlling one's own durable tasks.
	ScopeFlowsWrite = "flows:write"
	// ScopeDiagnosticsRead 只随内置管理员生效，**不得**发给调用方 Key。
	// ScopeDiagnosticsRead is admin-only and never issued to caller keys.
	ScopeDiagnosticsRead = "diagnostics:read"
	// ScopeCallersAdmin 只随内置管理员生效，**不得**发给调用方 Key。
	// ScopeCallersAdmin is admin-only and never issued to caller keys.
	ScopeCallersAdmin = "callers:admin"
	// AgentInvokeScopePrefix 是 Agent 调用 scope 的前缀，完整形态为
	// `agents:invoke:<name>`。
	// AgentInvokeScopePrefix prefixes the per-agent invoke scope.
	AgentInvokeScopePrefix = "agents:invoke:"
	// AgentInvokeWildcard 是通配调用 scope——**只允许**管理员发放，调用方身份里
	// 出现它即判非法（通配等价于"全部 Agent"，不得由 Key 携带）。
	// AgentInvokeWildcard is admin-issued only; a caller principal must not carry it.
	AgentInvokeWildcard = AgentInvokeScopePrefix + "*"
)

// KnownScope 判定 scope 字面量是否属于冻结词表：四个常规 scope、两个管理面
// scope，以及 `agents:invoke:<name>`（名称非空且不是通配）。
//
// 这是 `Principal.Valid()` "全部可识别"的判据，也是 `internal/callers` 发放期
// 校验的公共词表来源——两处共用一份字面量，不各写一份。
// KnownScope is the single vocabulary shared by principal validation and issuance.
func KnownScope(scope string) bool {
	switch scope {
	case ScopeSessionsRead, ScopeSessionsWrite, ScopeKnowledgeRead, ScopeKnowledgeWrite,
		ScopeDiagnosticsRead, ScopeCallersAdmin, ScopeFlowsRead, ScopeFlowsWrite:
		return true
	}
	name, ok := strings.CutPrefix(scope, AgentInvokeScopePrefix)
	if !ok {
		return false
	}
	return name != "" && name != "*"
}

// AdminOnlyScope 判定 scope 是否只允许内置管理员持有；调用方身份携带即非法，
// 调用方 Key 发放期同样**必须**拒绝。导出原因是 `internal/callers` 的发放校验
// 要复用同一份判据——两处各写一份会在新增管理面 scope 时漂移。
// AdminOnlyScope reports scopes reserved to the built-in administrator; issuance must
// reject them too. It is exported so the caller package reuses the single rule instead
// of keeping a second copy that drifts when a new admin-only scope appears.
func AdminOnlyScope(scope string) bool {
	return scope == ScopeDiagnosticsRead || scope == ScopeCallersAdmin ||
		scope == AgentInvokeWildcard
}

// Principal 是调用方身份的完整表达。它是**纯类型**：认证（Key 校验、admin token
// 校验）在消费方包内完成，本包只做结构与默认拒绝判定。
//
// 约束：
//   - 零值非法（`Valid() == false`），任何路径**不得**把零值补成内置管理员；
//   - `PrincipalLocalAdmin`：`CallerID` 固定 `local-admin`、`Builtin` 为真、
//     `KeyID` 为空；`Scopes` 可为空且 `HasScope` 恒真；
//   - `PrincipalCaller`：`CallerID` 非空且不是保留标识、`Builtin` 为假、
//     `Scopes` 非空且全部可识别、**不得**携带管理面 scope；
//   - `Actor` 为空同样非法——漏设执行者时要报错，不要退化成 caller。
//
// `Scopes` 由构造入口复制为不可变副本（`OwnedPrincipal` 复制；结构体字面量构造
// 由调用方负责不再改写）。
// Principal is the pure caller identity: the zero value is invalid, ownership and
// executor are distinct, and caller principals may not carry admin-only scopes.
type Principal struct {
	// Kind 是来源类别；空值非法。
	Kind PrincipalKind
	// CallerID 恒为**归属调用方**——配额、审计与会话归属都记它。
	CallerID string
	// KeyID 是认证所用调用方 Key 的标识（内置管理员为空）。
	KeyID string
	// Project 是可选的调用方项目标签，不参与授权判定。
	Project string
	// Scopes 是授予的 scope 集合；构造入口复制为不可变副本。
	Scopes []string
	// Builtin 表示这是部署内置的管理员身份（只有 PrincipalLocalAdmin 为真）。
	Builtin bool
	// Actor 是**这次执行**的执行者类别，不改变 CallerID，也不放大 scope。
	Actor ActorKind
}

// Valid 判定身份是否可用。任何一条不满足都返回 false，调用方**必须**据此拒绝
// 请求（映射 `ErrUnauthorized`），**不得**补默认值后继续。
// Valid reports whether the principal is usable; callers must reject when it is not.
func (p Principal) Valid() bool {
	if !p.Kind.Valid() || !p.Actor.Valid() || p.CallerID == "" {
		return false
	}
	switch p.Kind {
	case PrincipalLocalAdmin:
		// 内置管理员只有一种形态：归属固定、Builtin 为真、没有授权 Key。
		// There is exactly one built-in admin shape: fixed owner, Builtin, no key.
		return p.CallerID == LocalAdminCallerID && p.Builtin && p.KeyID == ""
	case PrincipalCaller:
		// 调用方不得冒充内置管理员，也不得携带管理面 scope。
		// A caller must not impersonate the built-in admin or hold admin scopes.
		if p.Builtin || p.CallerID == LocalAdminCallerID {
			return false
		}
		if len(p.Scopes) == 0 {
			return false
		}
		for _, scope := range p.Scopes {
			if !KnownScope(scope) || AdminOnlyScope(scope) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// HasScope 判定是否持有某个具体 scope。内置管理员恒真（`Scopes` 可为空）；
// 调用方按字面量精确匹配——不做通配展开、不做前缀推断。
// HasScope reports whether a concrete scope is held; the built-in admin always
// holds every scope, callers match literally.
func (p Principal) HasScope(scope string) bool {
	if p.Kind == PrincipalLocalAdmin {
		return true
	}
	for _, held := range p.Scopes {
		if held == scope {
			return true
		}
	}
	return false
}

// HasAgentScope 判定是否可调用指定 Agent（`agents:invoke:<name>`）。内置管理员
// 恒真；调用方只认**具名** scope——通配 `agents:invoke:*` 仅随管理员生效，
// 出现在调用方身份里时 `Valid()` 已经拒绝，这里也不放行。
// HasAgentScope reports whether the named agent may be invoked; the wildcard is
// admin-only and never honours a caller principal.
func (p Principal) HasAgentScope(name string) bool {
	if p.Kind == PrincipalLocalAdmin {
		return true
	}
	if name == "" {
		return false
	}
	want := AgentInvokeScopePrefix + name
	for _, held := range p.Scopes {
		if held == want {
			return true
		}
	}
	return false
}

// LocalAdminPrincipal 是**管理操作**的唯一构造入口（admin token 中间件与 CLI
// 管理命令显式调用）：`CallerID=local-admin`、`Builtin=true`、`KeyID` 为空、
// `Actor=ActorCaller`。它**不得**被用作零值默认，也**不得**用于调度器或恢复路径
// ——后台执行必须经 `OwnedPrincipal`，否则审计会把调度误记成外部调用。
// LocalAdminPrincipal is the only constructor for administrative operations; it is
// never a default value and never used by scheduler or recovery paths.
func LocalAdminPrincipal() Principal {
	return Principal{
		Kind:     PrincipalLocalAdmin,
		CallerID: LocalAdminCallerID,
		Builtin:  true,
		Actor:    ActorCaller,
	}
}

// LocalAdminCLIPrincipal 是 CLI 管理命令用的管理员身份：与
// `LocalAdminPrincipal` 权限相同，区别只在审计标注 `Actor=ActorCLIAdmin`。
// LocalAdminCLIPrincipal is the same authority as LocalAdminPrincipal, marked for
// the command line in the audit trail.
func LocalAdminCLIPrincipal() Principal {
	p := LocalAdminPrincipal()
	p.Actor = ActorCLIAdmin
	return p
}

// OwnerReason 是身份构造失败的原因码（C-32 `owner_reason` 的子集）。它进审计、
// 诊断与 Run 的失败原因，**不得**用自由文本替代。
// OwnerReason is the machine-checkable reason code of a rejected construction.
type OwnerReason string

const (
	// OwnerReasonKeyMissing 表示 caller-key 授权缺少任务绑定的原授权 Key。
	// OwnerReasonKeyMissing means a caller-key construction carried no key id.
	OwnerReasonKeyMissing OwnerReason = "key_missing"
	// OwnerReasonKindMismatch 表示归属与授权来源的组合不成立。
	// OwnerReasonKindMismatch means the owner/authorizing-kind combination is invalid.
	OwnerReasonKindMismatch OwnerReason = "kind_mismatch"
	// OwnerReasonActorInvalid 表示执行者类别非法（后台路径只接受 scheduler / recovery）。
	// OwnerReasonActorInvalid means the executor kind is not scheduler or recovery.
	OwnerReasonActorInvalid OwnerReason = "actor_invalid"
	// OwnerReasonKeyRevoked 表示原授权 Key 已被吊销（吊销优先于过期判定）。
	// OwnerReasonKeyRevoked means the bound key itself was revoked.
	OwnerReasonKeyRevoked OwnerReason = "key_revoked"
	// OwnerReasonKeyExpired 表示原授权 Key 已过期。
	// OwnerReasonKeyExpired means the bound key passed its expiry.
	OwnerReasonKeyExpired OwnerReason = "key_expired"
	// OwnerReasonScopeMissing 表示原授权 Key 现存 scope 不再覆盖该 Agent 的调用权。
	// OwnerReasonScopeMissing means the key's current scopes no longer cover this agent.
	OwnerReasonScopeMissing OwnerReason = "scope_missing"
	// OwnerReasonCallerDisabled 表示调用方本身已停用（Key 状态不再具有意义）。
	// OwnerReasonCallerDisabled means the owning caller was disabled.
	OwnerReasonCallerDisabled OwnerReason = "caller_disabled"
	// OwnerReasonAdminAuthUnavailable 表示启动期管理认证姿态为 `unavailable`——
	// 后台 admin-builtin 任务没有可满足的授权路径（§2.14 判定表）。
	// OwnerReasonAdminAuthUnavailable means the startup admin posture is unavailable, so
	// an admin-builtin background task has no satisfiable authorization path.
	OwnerReasonAdminAuthUnavailable OwnerReason = "admin_auth_unavailable"
)

// PrincipalError 是身份构造失败的错误载体。它 Unwrap 到 `ErrUnauthorized`，
// 因此调度 / 恢复路径既能用 `errors.Is` 判定"授权不可用"，又能用 `errors.As`
// 取出原因码写审计——两者**不得**合并成字符串比对。
// PrincipalError carries a rejected construction; it unwraps to ErrUnauthorized so
// callers can both classify and read the reason code structurally.
type PrincipalError struct {
	// Reason 是机器可判的原因码。
	Reason OwnerReason
	// Detail 是给人看的一句话说明（英文，进日志；不含凭据值）。
	Detail string
}

// Error 返回原因码与说明。
// Error renders the reason code and the human-readable detail.
func (e *PrincipalError) Error() string {
	if e.Detail == "" {
		return "principal rejected: " + string(e.Reason)
	}
	return "principal rejected: " + string(e.Reason) + ": " + e.Detail
}

// Unwrap 暴露 `ErrUnauthorized`：零值 / 构造失败都属于"授权不可用"这一大类。
// Unwrap exposes ErrUnauthorized, the sentinel for unavailable authorization.
func (e *PrincipalError) Unwrap() error { return ErrUnauthorized }

// OwnedPrincipal 是调度 / 恢复 / 代执行路径的**唯一**身份构造入口。
//
//   - `AuthorizingKindCallerKey`：继承任务原始归属，`keyID` 必须是任务绑定的
//     **原授权 Key**（非空），`scopes` 必须取自该 Key 的当前记录——本函数不查库，
//     调用方**必须**先 `KeyStore.LookupByID` 复核；拿不到该 Key、Key 失效或
//     scope 不满足时调用方直接失败，**不得**回退成管理员，**不得**改用同一调用方
//     的其他 Key。
//   - `AuthorizingKindAdminBuiltin`：要求 `callerID=local-admin`、`keyID` 为空、
//     `scopes` 为空，返回内置管理员身份并保留传入的 `Actor`。
//   - `actor` 只接受 scheduler / recovery——外部请求与 CLI 管理命令走各自的构造
//     入口，不得借后台路径提权。
//
// 其余组合一律返回带原因码的 `*PrincipalError`，**不得**静默补默认值。
// OwnedPrincipal is the only construction entry for scheduler, recovery and
// delegated runs; every other combination is rejected with a reason code.
func OwnedPrincipal(
	callerID string,
	kind AuthorizingKind,
	keyID string,
	scopes []string,
	actor ActorKind,
) (Principal, error) {
	if actor != ActorScheduler && actor != ActorRecovery {
		return Principal{}, &PrincipalError{
			Reason: OwnerReasonActorInvalid,
			Detail: "background identity requires scheduler or recovery actor",
		}
	}

	switch kind {
	case AuthorizingKindCallerKey:
		if callerID == "" || callerID == LocalAdminCallerID {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKindMismatch,
				Detail: "caller-key identity requires a non-admin owner",
			}
		}
		if keyID == "" {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKeyMissing,
				Detail: "caller-key identity requires the originally bound key id",
			}
		}
		p := Principal{
			Kind:     PrincipalCaller,
			CallerID: callerID,
			KeyID:    keyID,
			Scopes:   append([]string(nil), scopes...),
			Actor:    actor,
		}
		if !p.Valid() {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKindMismatch,
				Detail: "caller-key identity is invalid (empty or unknown scopes)",
			}
		}
		return p, nil
	case AuthorizingKindAdminBuiltin:
		if callerID != LocalAdminCallerID || keyID != "" || len(scopes) != 0 {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKindMismatch,
				Detail: "admin-builtin identity requires local-admin owner, no key and no scopes",
			}
		}
		p := LocalAdminPrincipal()
		p.Actor = actor
		return p, nil
	default:
		return Principal{}, &PrincipalError{
			Reason: OwnerReasonKindMismatch,
			Detail: "unknown authorizing kind",
		}
	}
}
