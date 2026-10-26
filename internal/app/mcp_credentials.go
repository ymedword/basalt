package app

// 本文件是 MCP 每槽位托管凭据的写面与解析面（14D / 设计 §2.3.2）：
//
//   - `Plan`：保存请求里的 `credentials: [{key, mode, value}]` 与既有来源声明合并，
//     只做**声明层**校验（全部 400）。在主写路径上它在预写操作行之前调用——请求本身
//     不合法的保存不留操作行；
//   - `Prepare`：声明层通过之后判**可用性**并加密新值（全部 503）；它以冻结契约
//     「`managed` 未取到值 → 503，预写后置 failed」为准，因此主写路径在预写之后、
//     写文件之前调用它——失败留一条 failed 操作行，文件 / 指针 / 版本零变化；
//   - `Apply`：写托管值与来源声明，**借用这次 upsert 自己的操作行**（契约里没有
//     `mcp_server.credential.write` 这个动作）。它先于定义文件落盘执行：宁可留下
//     无害的孤儿凭据行，也不发布一条声明托管却取不到值的配置；
//   - `Resolve`：连接前按**声明的来源**取值，`managed` 失败立即失败、**不回落 env**；
//   - `Statuses`：资源视图的 `credential_keys` 投影（只有名字、来源、状态与末 4 位）；
//   - `BackfillEnvSources`：存量配置的一次性事实补记（v5 只有进程环境一条路）。
//
// 三条纪律：
//   - **来源显式**：没有声明的槽位保存时即被拒绝，运行期也绝不按"托管行是否存在"
//     推断；撤销之后同一槽位不回落进程环境；
//   - **普通编辑不清凭据**：请求里省略的槽位沿用既有声明与托管值，改 command / args /
//     其它键不触碰凭据行；
//   - **材料不出边界**：值只进内存与密文，不进日志、审计 detail、响应或模型上下文。
//
// This file is the per-slot managed-credential surface for MCP: declarations are always
// explicit, a managed failure never falls back to the process environment, ordinary
// edits keep managed values, and no value ever leaves the process.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// MCPCredentialRequest 是保存请求里的逐槽声明（契约 `McpCredentialInput`）：`key` 为槽位
// 名，`mode` 必填（`managed` / `env`），`value` 只在 `managed` 时给。`Value == nil` 与
// 空串是两种意思——省略表示沿用既有托管值，给空串是非法值。
// MCPCredentialRequest is one per-slot declaration in a save request.
type MCPCredentialRequest struct {
	Key   string  `json:"key"`
	Mode  string  `json:"mode"`
	Value *string `json:"value,omitempty"`
}

// mcpCredentialAction 是一个槽位在本次保存里的动作：只写来源声明，或写一个新托管版本。
// mcpCredentialAction is what one save does to one slot.
type mcpCredentialAction int

const (
	// mcpCredentialDeclare 只写来源声明（env，或 managed 且无需改动值）。
	mcpCredentialDeclare mcpCredentialAction = iota
	// mcpCredentialWrite 写一个新托管版本（新建身份或追加版本）。
	mcpCredentialWrite
)

// mcpCredentialStep 是计划里的一步：槽位、终态模式与来源声明动作。`pending` 是本次给
// 值时的**已规范化明文**，只在 `Prepare` 加密前存活，加密后立即清零。
// mcpCredentialStep is one planned action; `pending` holds the normalised plaintext
// only until Prepare seals it.
type mcpCredentialStep struct {
	key           string
	mode          core.CredentialMode
	declareSource bool
	pending       []byte

	// 以下字段由 Prepare 填写；Plan 之后为零值。
	action        mcpCredentialAction
	identityFound bool
	credentialID  string
	versionNo     int
	keyID         string
	version       core.CredentialVersionRecord
}

// mcpCredentialPlan 是一次保存的凭据部分计划；`changed` 为假表示本次没有任何凭据行
// 需要改动（全部沿用）。
// mcpCredentialPlan is the credential half of one save.
type mcpCredentialPlan struct {
	steps   []mcpCredentialStep
	changed bool
}

// needsWrite 报告本次保存是否需要写凭据材料或来源声明。
// needsWrite reports whether any credential row changes.
func (p mcpCredentialPlan) needsWrite() bool { return p.changed }

// MCPCredentialService 是 MCP 槽位凭据的唯一写面与解析面。
// MCPCredentialService is the only writer and resolver of per-slot MCP credentials.
type MCPCredentialService struct {
	deps MCPCredentialServiceDeps
}

// MCPCredentialServiceDeps 是服务依赖：两个仓储、主密钥位置与（可注入的）环境读取。
// `Lookup` 缺省用 `os.LookupEnv`；用例注入替身，不为测试新增导出 API。
// MCPCredentialServiceDeps carries the two stores and the (injectable) env lookup.
type MCPCredentialServiceDeps struct {
	Credentials *storage.CredentialStore
	Sources     *storage.MCPCredentialSourceStore
	// MasterKeyFile 是静态初值；KeyPath 是共享的生效路径（用户显式初始化后立即切换）。
	// MasterKeyFile is the static seed; KeyPath is the live shared path when present.
	MasterKeyFile string
	KeyPath       *MasterKeyLocator
	WorkspaceRoot string
	Lookup        func(string) (string, bool)
	Logger        *slog.Logger
}

// NewMCPCredentialService 构造服务；缺仓储依赖即拒绝构造。
// NewMCPCredentialService refuses to construct without its stores.
func NewMCPCredentialService(deps MCPCredentialServiceDeps) (*MCPCredentialService, error) {
	if deps.Credentials == nil || deps.Sources == nil {
		return nil, errors.New("MCP 凭据服务装配缺少仓储依赖")
	}
	return &MCPCredentialService{deps: deps}, nil
}

// MCPCredentialStatus 是一个槽位的视图投影（契约 `McpCredentialKeyView`）：只有名字、
// 来源与状态，`Last4` 是唯一与材料沾边的字段，且永远只是末 4 位。
// MCPCredentialStatus is the view projection of one slot; only the tail may be shown.
type MCPCredentialStatus struct {
	Key   string
	Mode  core.CredentialMode
	State core.MCPCredentialKeyState
	Last4 string
}

// Plan 把请求里的逐槽声明与既有来源合并成计划，**只做声明层校验**（全部 400）：主
// 写路径在预写操作行之前调用，不合法的请求因此不留操作行。
//
// 合并规则（契约 McpServerUpsert.credentials）：
//   - 请求里出现的键必须是本次配置的槽位，且不得重复；
//   - 未声明的键沿用既有来源声明；合并后仍无声明 → 400 点名该键（不隐式按 env 推断）；
//   - `mode=env` → 不接受值；该槽位仍有活动托管值 → 400，要求先显式撤销再改来源；
//   - `mode=managed` 且给值 → 规范化后记入计划（加密在 `Prepare`）；
//   - `mode=managed` 且省略值 → 沿用现有托管值（是否真的可用由 `Prepare` 判 503）。
//
// Plan merges the request's declarations with the recorded sources and validates only
// the declaration layer; availability and sealing happen in Prepare.
func (s *MCPCredentialService) Plan(ctx context.Context, serverName string, serverUID core.ResourceUID,
	cfg core.MCPServerConfig, inputs []MCPCredentialRequest,
) (mcpCredentialPlan, error) {
	slots, err := core.MCPCredentialSlots(cfg)
	if err != nil {
		return mcpCredentialPlan{}, &core.InvalidInputError{Tool: "保存 MCP 凭据来源", Reason: err.Error()}
	}
	declared, err := indexMCPCredentialInputs(slots, inputs)
	if err != nil {
		return mcpCredentialPlan{}, err
	}
	existing, err := s.sourcesByKey(ctx, serverUID)
	if err != nil {
		return mcpCredentialPlan{}, err
	}

	plan := mcpCredentialPlan{steps: make([]mcpCredentialStep, 0, len(slots))}
	for _, slot := range slots {
		source, sourceFound := existing[slot]
		step, stepErr := s.planSlot(ctx, serverUID, slot, declared[slot], source, sourceFound)
		if stepErr != nil {
			zeroMCPPending(plan.steps)
			return mcpCredentialPlan{}, stepErr
		}
		if step.pending != nil || step.declareSource {
			plan.changed = true
		}
		plan.steps = append(plan.steps, step)
	}
	return plan, nil
}

// indexMCPCredentialInputs 校验请求里的逐槽声明：键必须在本次配置里、不得重复、模式
// 显式、env 不得带值。**错误不回显值**——它可能就是用户误贴的凭证。
// indexMCPCredentialInputs validates the per-slot declarations without echoing values.
func indexMCPCredentialInputs(slots []string,
	inputs []MCPCredentialRequest,
) (map[string]MCPCredentialRequest, error) {
	known := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		known[slot] = struct{}{}
	}
	declared := make(map[string]MCPCredentialRequest, len(inputs))
	for _, input := range inputs {
		key, ok := core.MCPBindingKey(input.Key)
		if !ok {
			return nil, &core.InvalidInputError{
				Tool:   "保存 MCP 凭据来源",
				Reason: "凭据槽位名非法（只接受 env 键名或请求头名）",
			}
		}
		if _, isSlot := known[key]; !isSlot {
			return nil, &core.InvalidInputError{
				Tool:   "保存 MCP 凭据来源",
				Reason: fmt.Sprintf("凭据槽位 %s 不在本次配置的 env / headers 里", key),
			}
		}
		if _, dup := declared[key]; dup {
			return nil, &core.InvalidInputError{
				Tool:   "保存 MCP 凭据来源",
				Reason: fmt.Sprintf("凭据槽位 %s 重复声明", key),
			}
		}
		mode, err := core.ParseCredentialMode(strings.TrimSpace(input.Mode))
		if err != nil {
			return nil, &core.InvalidInputError{
				Tool:   "保存 MCP 凭据来源",
				Reason: fmt.Sprintf("凭据槽位 %s 的来源必须显式给出 managed 或 env", key),
			}
		}
		if mode == core.CredentialModeEnv && input.Value != nil {
			return nil, &core.InvalidInputError{
				Tool:   "保存 MCP 凭据来源",
				Reason: fmt.Sprintf("凭据槽位 %s 声明为 env，不得带值；改来源请先撤销已有托管值", key),
			}
		}
		declared[key] = MCPCredentialRequest{Key: key, Mode: string(mode), Value: input.Value}
	}
	return declared, nil
}

// planSlot 合并一个槽位：请求里有声明以请求为准，否则沿用既有来源；两者都没有即 400。
// planSlot merges one slot; a slot with neither a request declaration nor a recorded
// source is refused.
func (s *MCPCredentialService) planSlot(ctx context.Context,
	serverUID core.ResourceUID, slot string, input MCPCredentialRequest,
	source core.MCPCredentialSource, sourceFound bool,
) (mcpCredentialStep, error) {
	if !sourceFound && input.Key == "" {
		return mcpCredentialStep{}, &core.InvalidInputError{
			Tool:   "保存 MCP 凭据来源",
			Reason: fmt.Sprintf("凭据槽位 %s 没有来源声明——请显式选择 managed 或 env", slot),
		}
	}
	step := mcpCredentialStep{key: slot, mode: source.Mode}
	if input.Key != "" {
		step.mode = core.CredentialMode(input.Mode)
	}
	step.declareSource = !sourceFound || source.Mode != step.mode

	identity, identityFound, err := s.mcpIdentity(ctx, serverUID, slot)
	if err != nil {
		return mcpCredentialStep{}, err
	}
	switch step.mode {
	case core.CredentialModeEnv:
		// 改来源是显式动作：还有活动托管值时不静默丢弃它，要求先撤销。
		if identityFound && identity.State == core.CredentialIdentityActive {
			return mcpCredentialStep{}, &core.InvalidInputError{
				Tool: "保存 MCP 凭据来源",
				Reason: fmt.Sprintf(
					"凭据槽位 %s 仍有活动托管值——改为 env 前请先撤销（撤销后不回落进程环境）", slot),
			}
		}
	case core.CredentialModeManaged:
		if input.Value != nil {
			value, normErr := normaliseCredentialValue(core.CredentialModeManaged,
				"保存 MCP 凭据来源", []byte(*input.Value))
			if normErr != nil {
				return mcpCredentialStep{}, mcpSlotValueError(slot, normErr)
			}
			step.pending = value
		}
	default:
		return mcpCredentialStep{}, &core.InvalidInputError{
			Tool:   "保存 MCP 凭据来源",
			Reason: fmt.Sprintf("凭据槽位 %s 的来源 %q 非法", slot, step.mode),
		}
	}
	return step, nil
}

// mcpSlotValueError 给凭据值错误补上槽位名；哨兵类别保持不变（web 层据此映射 400）。
// mcpSlotValueError names the slot while preserving the error's sentinel class.
func mcpSlotValueError(slot string, err error) error {
	var invalid *core.InvalidInputError
	if errors.As(err, &invalid) {
		return &core.InvalidInputError{
			Tool:   invalid.Tool,
			Reason: fmt.Sprintf("凭据槽位 %s：%s", slot, invalid.Reason),
		}
	}
	return fmt.Errorf("凭据槽位 %s: %w", slot, err)
}

// mcpEnvSlotFailure 标记"环境来源这一档取不到值"：保存闸门的 detail 据此报
// `env_missing`，而把托管侧失败留在 `credential_unavailable`——两类都返回 503，但原因
// 不同，合并不利于排障。测试记录的 `failure_class` 枚举（D-05）不受影响，那套码只描述
// 一次真实连接尝试。
// mcpEnvSlotFailure marks an env-source miss so the save gate can report env_missing
// while managed failures stay credential_unavailable.
type mcpEnvSlotFailure struct{ err error }

// Error 返回底层消息（只点名 server / 槽位 / 变量名，不含值）。
// Error forwards the underlying message.
func (e mcpEnvSlotFailure) Error() string { return e.err.Error() }

// Unwrap 保留 `core.ErrCredentialUnavailable` 哨兵，web 层映射 503 不变。
// Unwrap keeps the credential-unavailable sentinel so the web layer still maps 503.
func (e mcpEnvSlotFailure) Unwrap() error { return e.err }

// mcpCredentialStateFailure 标记"**这个 server** 的托管凭据当前不可用"（未录入 / 已撤销 /
// 当前版本非 active）。它是启动隔离（设计 §2.5.1 / 验收 E3，T115）的**唯一**跳过依据：
// 启动装配只跳过这一类，其余失败——主密钥缺失 / 损坏、存储读取、密文归属不符、槽位未声明
// 来源——仍失败关闭。把后者也标成"该 server 没配好"，会把"整个实例的托管凭据都解不开"
// 伪装成一次普通的缺值，启动看似成功、所有托管条目同时静默失效。
// mcpCredentialStateFailure marks one server's managed credential as currently unusable;
// it is the only class startup isolation may skip. Instance-level failures stay closed.
type mcpCredentialStateFailure struct{ err error }

// Error 返回底层消息（只点名 server / 槽位，不含值）。
// Error forwards the underlying message.
func (e mcpCredentialStateFailure) Error() string { return e.err.Error() }

// Unwrap 保留底层哨兵（`ErrCredentialUnavailable` / `ErrCredentialRevoked`），
// web 层 503 与 409 的映射不变。
// Unwrap keeps the underlying sentinels so the web mapping is unchanged.
func (e mcpCredentialStateFailure) Unwrap() error { return e.err }

// mcpCredentialUnavailable 判定一个解析错误是否属于"该 server 的凭据当前取不到值"：
// env 变量缺失 / 为空，或托管凭据未录入 / 已撤销 / 版本非 active。只有这一类允许在启动
// 装配里跳过（E3）；实例级与数据完整性失败（主密钥、存储读取、密文归属）**不**算。
// mcpCredentialUnavailable reports whether resolution failed for this server's own
// credential state; only that class may be skipped at startup.
func mcpCredentialUnavailable(err error) bool {
	var envFailure mcpEnvSlotFailure
	var stateFailure mcpCredentialStateFailure
	return errors.As(err, &envFailure) || errors.As(err, &stateFailure)
}

// Prepare 判**可用性**并加密新值（全部 503）。主写路径在预写操作行之后、写文件之前调用：
// 失败留一条 failed 操作行，文件 / 发布指针 / 版本零变化。
//
//	managed + 新值   → 加密（主密钥不可装载即失败）
//	managed + 沿用值 → 真解一遍：无版本 / 已撤销 / 解密失败都按「未取到值」失败
//	env              → 现在就按进程环境判一次（`env` 与 `http.headers` 同一闸门，
//	                   未设置与空值同档拒绝），不留到连接失败才发现
//
// Prepare validates availability and seals new values; every failure is a 503-class
// credential error and no row is written. Env-sourced slots are checked here too, so
// `http.headers` placeholders go through the same gate as `env`.
func (s *MCPCredentialService) Prepare(ctx context.Context, serverName string,
	serverUID core.ResourceUID, cfg core.MCPServerConfig, plan mcpCredentialPlan,
) (mcpCredentialPlan, error) {
	steps := make([]mcpCredentialStep, len(plan.steps))
	copy(steps, plan.steps)
	lookup := s.deps.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for i := range steps {
		step := &steps[i]
		// 缺省动作显式写出来：只声明来源、不动值。写值由 sealMCPVersion 改写成
		// mcpCredentialWrite——动作字段不靠 iota 的零值含义被读。
		step.action = mcpCredentialDeclare
		switch {
		case step.pending != nil:
			sealed, err := s.sealMCPVersion(ctx, step, serverUID)
			if err != nil {
				zeroMCPPending(steps)
				return mcpCredentialPlan{}, err
			}
			steps[i] = sealed
		case step.mode == core.CredentialModeManaged:
			value, err := s.resolveManaged(ctx, serverName, serverUID, step.key)
			if err != nil {
				zeroMCPPending(steps)
				return mcpCredentialPlan{}, err
			}
			zeroCredentialValue([]byte(value))
		case step.mode == core.CredentialModeEnv:
			value, err := resolveMCPEnvSlot(serverName, cfg, step.key, lookup)
			if err != nil {
				zeroMCPPending(steps)
				return mcpCredentialPlan{}, mcpEnvSlotFailure{err: err}
			}
			zeroCredentialValue([]byte(value))
		}
	}
	prepared := mcpCredentialPlan{steps: steps}
	for _, step := range steps {
		if step.action == mcpCredentialWrite || step.declareSource {
			prepared.changed = true
		}
	}
	return prepared, nil
}

// zeroMCPPending 清零计划里尚未加密的明文。
// zeroMCPPending zeroes plaintext still held by a failed plan.
func zeroMCPPending(steps []mcpCredentialStep) {
	for i := range steps {
		zeroCredentialValue(steps[i].pending)
		steps[i].pending = nil
	}
}

// sealMCPVersion 加密一个槽位的新值并填好候选版本行；明文在返回前清零。
// sealMCPVersion seals one slot's new value and zeroes the plaintext.
func (s *MCPCredentialService) sealMCPVersion(ctx context.Context, step *mcpCredentialStep,
	serverUID core.ResourceUID,
) (mcpCredentialStep, error) {
	value := step.pending
	defer func() {
		zeroCredentialValue(value)
		step.pending = nil
	}()

	target, err := MCPCredentialTargetBinding(serverUID, step.key)
	if err != nil {
		return mcpCredentialStep{}, err
	}
	key, err := LoadMasterKey(effectiveMasterKeyFile(s.deps.KeyPath, s.deps.MasterKeyFile), s.deps.WorkspaceRoot)
	if err != nil {
		return mcpCredentialStep{}, err
	}
	defer key.Zero()

	identity, identityFound, err := s.mcpIdentity(ctx, serverUID, step.key)
	if err != nil {
		return mcpCredentialStep{}, err
	}
	step.action = mcpCredentialWrite
	step.keyID = key.KeyID
	step.identityFound = identityFound
	if identityFound {
		step.credentialID = identity.ID
		step.versionNo = identity.CurrentVersion + 1
	} else {
		credentialID, mintErr := newCredentialID()
		if mintErr != nil {
			return mcpCredentialStep{}, mintErr
		}
		step.credentialID = credentialID
		step.versionNo = 1
	}
	nonce, ciphertext, err := SealCredential(key, CredentialAAD{
		OwnerKind:     core.BindingMCPServer,
		OwnerUID:      serverUID,
		CredentialID:  step.credentialID,
		VersionNo:     step.versionNo,
		TargetBinding: target,
	}, value)
	if err != nil {
		return mcpCredentialStep{}, err
	}
	step.version = core.CredentialVersionRecord{
		KeyID:         key.KeyID,
		TargetBinding: target,
		Last4:         credentialLast4(value),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	}
	return *step, nil
}

// Apply 按计划落库：先写托管值、再写来源声明。凭据写入借用调用方这次保存的操作行
// （契约里没有 `mcp_server.credential.write`），因此 `operationID` 必须已经存在。
//
// 它在定义文件落盘**之前**执行：顺序上宁可留下无害的孤儿凭据行（同一槽位的下一次保存
// 会复用身份），也不让"声明托管却取不到值"的配置被发布出去。
// Apply writes values then declarations, before the definition file is committed, so a
// published config never declares a managed slot without a value.
func (s *MCPCredentialService) Apply(ctx context.Context, serverName string,
	serverUID core.ResourceUID, plan mcpCredentialPlan, operationID string, at time.Time,
) error {
	if !plan.needsWrite() {
		return nil
	}
	if operationID == "" {
		return errors.New("写入 MCP 托管凭据: operation_id 不能为空")
	}
	for _, step := range plan.steps {
		if step.action == mcpCredentialWrite {
			if err := s.writeMCPVersion(ctx, serverName, serverUID, step, operationID, at); err != nil {
				return err
			}
		}
		if err := s.declareSource(ctx, serverUID, step, operationID, at); err != nil {
			return err
		}
	}
	return nil
}

// writeMCPVersion 写一个托管版本：没有身份就新建，已有身份（含已撤销）就追加版本。
// 追加会让已撤销的身份迁回 active——纪元不复位，被撤销的密文永不复活。
// writeMCPVersion creates the identity or appends a version; a revoked identity comes
// back active without its epoch resetting.
func (s *MCPCredentialService) writeMCPVersion(ctx context.Context, serverName string,
	serverUID core.ResourceUID, step mcpCredentialStep, operationID string, at time.Time,
) error {
	if step.identityFound {
		if err := s.deps.Credentials.AppendVersion(ctx, step.credentialID, step.keyID, operationID,
			step.version, at); err != nil {
			return fmt.Errorf("写入 MCP server %s 的槽位 %s 托管凭据: %w", serverName, step.key, err)
		}
		return nil
	}
	err := s.deps.Credentials.Create(ctx, core.CredentialRecord{
		ID:          step.credentialID,
		OwnerKind:   core.BindingMCPServer,
		OwnerUID:    serverUID,
		BindingKey:  step.key,
		OwnerName:   serverName,
		State:       core.CredentialIdentityActive,
		KeyID:       step.keyID,
		OperationID: operationID,
		CreatedAt:   at.UnixMilli(),
		UpdatedAt:   at.UnixMilli(),
	}, step.version)
	if err != nil {
		return fmt.Errorf("创建 MCP server %s 的槽位 %s 托管凭据: %w", serverName, step.key, err)
	}
	return nil
}

// declareSource 写来源声明；同一模式重复保存不升修订号（声明没变就不是变更）。
// declareSource records the declared source and skips an unchanged declaration.
func (s *MCPCredentialService) declareSource(ctx context.Context, serverUID core.ResourceUID,
	step mcpCredentialStep, operationID string, at time.Time,
) error {
	if !step.declareSource {
		return nil
	}
	if err := s.deps.Sources.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   serverUID,
		BindingKey:  step.key,
		Mode:        step.mode,
		OperationID: operationID,
	}, at); err != nil {
		return fmt.Errorf("记录 MCP 槽位 %s 的凭据来源: %w", step.key, err)
	}
	return nil
}

// Statuses 按槽位字典序投影来源与状态；没有来源声明的槽位不出现在列表里（契约
// `McpCredentialKeyView.mode` 只有两个取值——无声明是配置错误，由保存闸门与诊断点名）。
// Statuses projects every declared slot; an undeclared slot is left for diagnostics.
func (s *MCPCredentialService) Statuses(ctx context.Context, serverUID core.ResourceUID,
	cfg core.MCPServerConfig,
) ([]MCPCredentialStatus, error) {
	slots, err := core.MCPCredentialSlots(cfg)
	if err != nil {
		return nil, err
	}
	sources, err := s.sourcesByKey(ctx, serverUID)
	if err != nil {
		return nil, err
	}
	statuses := make([]MCPCredentialStatus, 0, len(slots))
	for _, slot := range slots {
		source, declared := sources[slot]
		if !declared {
			continue
		}
		identity, found, identityErr := s.mcpIdentity(ctx, serverUID, slot)
		if identityErr != nil {
			return nil, identityErr
		}
		var record *core.CredentialRecord
		if found {
			record = &identity
		}
		status := MCPCredentialStatus{
			Key:   slot,
			Mode:  source.Mode,
			State: core.MCPCredentialStateOf(source.Mode, record),
		}
		if found {
			status.Last4 = s.last4Of(ctx, identity)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// last4Of 读当前版本的末 4 位；读不出来返回空串——展示字段不值得让整份视图失败。
// last4Of reads the tail of the current version and degrades to an empty string.
func (s *MCPCredentialService) last4Of(ctx context.Context, identity core.CredentialRecord) string {
	version, err := s.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("读取 MCP 凭据版本末 4 位失败，视图留空",
				"credential_id", identity.ID, "error", err)
		}
		return ""
	}
	return version.Last4
}

// Resolve 按声明来源解析一个 server 的全部槽位，返回槽位 → 明文值的副本。调用方用完
// 必须清零（`ZeroMCPCredentialValues`）。
//
//	合并后仍无来源声明 → 错误（配置校验失败，要求显式选择）；
//	env                → 只读进程环境，缺失 / 空值即失败；
//	managed            → 只查托管库；缺失 / 撤销 / 解密失败立即失败，**不回落 env**。
//
// Resolve returns the plaintext of every slot strictly by its declared source; a
// managed failure never falls back to the process environment.
func (s *MCPCredentialService) Resolve(ctx context.Context, serverName string,
	serverUID core.ResourceUID, cfg core.MCPServerConfig,
) (map[string]string, error) {
	slots, err := core.MCPCredentialSlots(cfg)
	if err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return map[string]string{}, nil
	}
	sources, err := s.sourcesByKey(ctx, serverUID)
	if err != nil {
		return nil, err
	}
	lookup := s.deps.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	values := make(map[string]string, len(slots))
	for _, slot := range slots {
		source, declared := sources[slot]
		if !declared {
			ZeroMCPCredentialValues(values)
			return nil, fmt.Errorf("%w: MCP server %s 的槽位 %s 没有凭据来源声明，请显式选择 managed 或 env",
				core.ErrCredentialUnavailable, serverName, slot)
		}
		var value string
		var resolveErr error
		switch source.Mode {
		case core.CredentialModeEnv:
			value, resolveErr = resolveMCPEnvSlot(serverName, cfg, slot, lookup)
			if resolveErr != nil {
				// 统一打上"环境来源缺失"标记：发布 / 测试路径据此把 detail 报成
				// env_missing，而不是笼统的 credential_unavailable。
				resolveErr = mcpEnvSlotFailure{err: resolveErr}
			}
		case core.CredentialModeManaged:
			value, resolveErr = s.resolveManaged(ctx, serverName, serverUID, slot)
		default:
			resolveErr = fmt.Errorf("%w: MCP server %s 的槽位 %s 来源 %q 非法",
				core.ErrCredentialUnavailable, serverName, slot, source.Mode)
		}
		if resolveErr != nil {
			ZeroMCPCredentialValues(values)
			return nil, resolveErr
		}
		values[slot] = value
	}
	return values, nil
}

// resolveMCPEnvSlot 读一个 env 槽位：变量名取自槽位的 `${VAR}`（与 14D 之前的行为逐字
// 相同），未设置与空值同档拒绝，错误只点名 server / 槽位 / 变量名。
// resolveMCPEnvSlot reads one env slot; missing and empty values are refused alike.
func resolveMCPEnvSlot(serverName string, cfg core.MCPServerConfig, slot string,
	lookup func(string) (string, bool),
) (string, error) {
	varName, ok := core.MCPSlotPlaceholder(cfg, slot)
	if !ok {
		return "", fmt.Errorf("%w: MCP server %s 的槽位 %s 不是 `${VAR}` 占位",
			core.ErrCredentialUnavailable, serverName, slot)
	}
	value, found := lookup(varName)
	if !found || value == "" {
		return "", fmt.Errorf("%w: MCP server %s 的槽位 %s 引用的 %s 未设置或为空",
			core.ErrCredentialUnavailable, serverName, slot, varName)
	}
	return value, nil
}

// resolveManaged 解一个托管槽位：身份与当前版本都必须 active，目标绑定必须仍指向
// 同一个 (server, 槽位)，主密钥每次重新装载。任何一步失败都**不**回落 env。
// resolveManaged decrypts one managed slot; every failure stays a failure.
func (s *MCPCredentialService) resolveManaged(ctx context.Context, serverName string,
	serverUID core.ResourceUID, slot string,
) (string, error) {
	identity, found, err := s.mcpIdentity(ctx, serverUID, slot)
	if err != nil {
		return "", err
	}
	if !found {
		return "", mcpCredentialStateFailure{err: fmt.Errorf("%w: MCP server %s 的槽位 %s 声明托管但没有托管值",
			core.ErrCredentialUnavailable, serverName, slot)}
	}
	switch identity.State {
	case core.CredentialIdentityActive:
	case core.CredentialIdentityRevoked:
		return "", mcpCredentialStateFailure{err: fmt.Errorf("%w: MCP server %s 的槽位 %s 的托管凭据已撤销（不回落进程环境）",
			core.ErrCredentialRevoked, serverName, slot)}
	default:
		return "", mcpCredentialStateFailure{err: fmt.Errorf("%w: MCP server %s 的槽位 %s 的凭据状态为 %s",
			core.ErrCredentialUnavailable, serverName, slot, identity.State)}
	}
	version, err := s.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		// 存储读取失败属实例级问题：不套"该 server 凭据不可用"标记，启动仍失败关闭。
		return "", fmt.Errorf("%w: MCP server %s 的槽位 %s 的当前版本不可读",
			core.ErrCredentialUnavailable, serverName, slot)
	}
	if version.State != core.CredentialVersionActive {
		return "", mcpCredentialStateFailure{err: fmt.Errorf("%w: MCP server %s 的槽位 %s 的当前版本状态为 %s",
			core.ErrCredentialUnavailable, serverName, slot, version.State)}
	}
	expected, err := MCPCredentialTargetBinding(serverUID, slot)
	if err != nil {
		return "", err
	}
	if version.TargetBinding != expected {
		return "", fmt.Errorf("%w: MCP server %s 的槽位 %s 的密文归属不符，请重新录入",
			core.ErrCredentialUnavailable, serverName, slot)
	}
	masterKey, err := LoadMasterKey(effectiveMasterKeyFile(s.deps.KeyPath, s.deps.MasterKeyFile), s.deps.WorkspaceRoot)
	if err != nil {
		// 主密钥是实例级依赖：补上受影响的 server / 槽位便于排障，但**不**套下面的
		// 状态标记——启动装配不会把它当作"某条没配好"跳过。
		return "", fmt.Errorf("MCP server %s 的槽位 %s: %w", serverName, slot, err)
	}
	defer masterKey.Zero()
	if masterKey.KeyID != version.KeyID {
		return "", fmt.Errorf("%w: 主密钥 key_id %s 与 MCP server %s 槽位 %s 的凭据版本不匹配",
			core.ErrCredentialUnavailable, masterKey.KeyID, serverName, slot)
	}
	plaintext, err := OpenCredential(masterKey, CredentialAAD{
		OwnerKind:     core.BindingMCPServer,
		OwnerUID:      serverUID,
		CredentialID:  identity.ID,
		VersionNo:     version.VersionNo,
		TargetBinding: version.TargetBinding,
	}, version.Nonce, version.Ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// mcpCredentialFreeze 是一个槽位在 apply 第 2 步的冻结读数：来源模式与被引用托管凭据
// 的版本号 / 主密钥 key_id。`env` 槽位没有版本可冻，撤销判定对它不适用（环境变量不是
// 版本化资源）。
// mcpCredentialFreeze is one slot's frozen reading; env slots have no version to freeze.
type mcpCredentialFreeze struct {
	key       string
	mode      core.CredentialMode
	versionNo int
	keyID     string
}

// Freeze 冻结本次 apply 引用的全部托管凭据版本（第 2 步）。冻结值只在内存里参与第 6 步
// 的并发校验：契约把 `detail_json` 的键集定死为结果对象的 7 个键，凭据版本号不属于
// 其中，因此**不**落操作行。
// Freeze snapshots the managed credential versions an apply depends on. The frozen
// numbers stay in memory: the contract fixes detail_json to the seven result keys.
func (s *MCPCredentialService) Freeze(ctx context.Context, serverName string,
	serverUID core.ResourceUID, cfg core.MCPServerConfig,
) ([]mcpCredentialFreeze, error) {
	slots, err := core.MCPCredentialSlots(cfg)
	if err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, nil
	}
	sources, err := s.sourcesByKey(ctx, serverUID)
	if err != nil {
		return nil, err
	}
	frozen := make([]mcpCredentialFreeze, 0, len(slots))
	for _, slot := range slots {
		source, declared := sources[slot]
		if !declared {
			return nil, fmt.Errorf("%w: MCP server %s 的槽位 %s 没有凭据来源声明，请显式选择 managed 或 env",
				core.ErrCredentialUnavailable, serverName, slot)
		}
		entry := mcpCredentialFreeze{key: slot, mode: source.Mode}
		if source.Mode == core.CredentialModeManaged {
			identity, found, identityErr := s.mcpIdentity(ctx, serverUID, slot)
			if identityErr != nil {
				return nil, identityErr
			}
			if found {
				entry.versionNo = identity.CurrentVersion
				entry.keyID = identity.KeyID
			}
		}
		frozen = append(frozen, entry)
	}
	return frozen, nil
}

// StillCurrent 判定冻结的托管凭据是否仍然全部有效（第 6 步的撤销校验）：任一槽位换了
// 版本、换了主密钥、被撤销或删除，本次切换都必须丢弃。
// StillCurrent reports whether every frozen managed credential is still the active one.
func (s *MCPCredentialService) StillCurrent(ctx context.Context, serverUID core.ResourceUID,
	frozen []mcpCredentialFreeze,
) (bool, error) {
	for _, entry := range frozen {
		if entry.mode != core.CredentialModeManaged {
			continue
		}
		identity, found, err := s.mcpIdentity(ctx, serverUID, entry.key)
		if err != nil {
			return false, err
		}
		if !found || identity.CurrentVersion != entry.versionNo || identity.KeyID != entry.keyID ||
			identity.State != core.CredentialIdentityActive {
			return false, nil
		}
	}
	return true, nil
}

// BackfillEnvSources 为存量 server 的每个槽位补记 `mode=env`：v5 只有进程环境一条路，
// 这是**记录当前真实来源**，不是运行时回落——补记之后用户改成 managed 再撤销，仍然
// 不会退回 env。已有声明的槽位不动（`ON CONFLICT DO NOTHING`）。
// BackfillEnvSources records the v5 fact (process environment) for every slot lacking a
// declaration; it is a one-time record, not a runtime fallback.
func (s *MCPCredentialService) BackfillEnvSources(ctx context.Context, serverName string,
	serverUID core.ResourceUID, cfg core.MCPServerConfig, operationID string, at time.Time,
) (int, error) {
	slots, err := core.MCPCredentialSlots(cfg)
	if err != nil {
		return 0, fmt.Errorf("补记 MCP server %s 的凭据来源: %w", serverName, err)
	}
	if len(slots) == 0 {
		return 0, nil
	}
	added, err := s.deps.Sources.BackfillEnvSources(ctx, serverUID, slots, operationID, at)
	if err != nil {
		return added, fmt.Errorf("补记 MCP server %s 的凭据来源: %w", serverName, err)
	}
	return added, nil
}

// ZeroMCPCredentialValues 清零解析结果里的明文并清空映射（best-effort）。
// ZeroMCPCredentialValues clears the resolved plaintext in place.
func ZeroMCPCredentialValues(values map[string]string) {
	for key, value := range values {
		zeroCredentialValue([]byte(value))
		delete(values, key)
	}
}

// ApplyResolvedMCP 把解析出的明文装配回配置：`env` 与 `headers` 的值变成真实值，只在
// 内存里存在。头值不加 `Bearer`——认证头的装配规则在传输层（`Authorization` 加前缀、
// 其它头原样），本层只负责取值。
// ApplyResolvedMCP substitutes the resolved values into the configuration in memory.
func ApplyResolvedMCP(cfg core.MCPServerConfig, values map[string]string) core.MCPServerConfig {
	resolved := cfg
	if len(cfg.Env) > 0 {
		env := make(map[string]string, len(cfg.Env))
		for key := range cfg.Env {
			env[key] = values[key]
		}
		resolved.Env = env
	}
	if len(cfg.Headers) > 0 {
		headers := make(map[string]string, len(cfg.Headers))
		for name := range cfg.Headers {
			headers[name] = values[name]
		}
		resolved.Headers = headers
	}
	return resolved
}

// mcpIdentity 按 (server, 槽位) 读托管身份；没有行 → found=false。
// mcpIdentity reads the managed identity of one slot.
func (s *MCPCredentialService) mcpIdentity(ctx context.Context, serverUID core.ResourceUID,
	slot string,
) (core.CredentialRecord, bool, error) {
	identity, err := s.deps.Credentials.CredentialByOwnerKey(ctx, core.BindingMCPServer, serverUID, slot)
	if errors.Is(err, core.ErrResourceNotFound) {
		return core.CredentialRecord{}, false, nil
	}
	if err != nil {
		return core.CredentialRecord{}, false,
			fmt.Errorf("读取 MCP 槽位 %s 的托管凭据: %w", slot, err)
	}
	return identity, true, nil
}

// sourcesByKey 读一个 server 的全部来源声明，按槽位名索引。
// sourcesByKey indexes the recorded sources by slot name.
func (s *MCPCredentialService) sourcesByKey(ctx context.Context,
	serverUID core.ResourceUID,
) (map[string]core.MCPCredentialSource, error) {
	rows, err := s.deps.Sources.List(ctx, serverUID)
	if err != nil {
		return nil, fmt.Errorf("读取 MCP server %s 的凭据来源: %w", serverUID, err)
	}
	sources := make(map[string]core.MCPCredentialSource, len(rows))
	for _, row := range rows {
		sources[row.BindingKey] = row
	}
	return sources, nil
}
