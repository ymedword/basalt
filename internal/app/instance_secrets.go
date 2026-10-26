package app

// 本文件是实例首次初始化与加密三态判定的服务面（14D T085）。
//
// 三条纪律：
//   - 判定靠**三项联立**（初始化记录 / 密钥文件 / 现存托管密文），不看"文件在不在"。
//     格式正确但内容错误的密钥会让全部托管凭据永久不可用，而页面却显示「就绪」——
//     这是代价最大的一种假成功，adopt 因此必须在有密文时**真解一遍**；
//   - 恢复态**不得**生成替代密钥、**不得**回落到 env、**不得**删除来源不明的文件；
//   - 先写 `pending` 审计再生成，任一后续步骤失败都按判定表回到「未初始化 / 待 adopt」，
//     重试**不会**产生第二把密钥——发布是排他语义，第二次必然 EEXIST 并转入 adopt。
//
// The instance-secrets service decides the three-state verdict from three inputs
// together, never from file existence alone, and refuses to mint a replacement key in
// the recovery state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// SecretsAction 是判定表给出的下一步动作。`await_init` 与 `create` 的区别只在
// "谁发起"：`create` 由可证明的新实例在启动时自动完成，`await_init` 等人类动作。
// SecretsAction is the table's next step; create runs automatically on a provably-new
// instance while await_init waits for a human.
type SecretsAction string

const (
	// SecretsActionReady 表示已验证就绪，无需动作。
	SecretsActionReady SecretsAction = "ready"
	// SecretsActionCreate 表示可证明的新实例：自动生成密钥 → 写记录 → 审计。
	SecretsActionCreate SecretsAction = "create"
	// SecretsActionAdopt 表示密钥可用但缺初始化记录：只补写记录（**不**生成）。
	SecretsActionAdopt SecretsAction = "adopt"
	// SecretsActionAwaitInit 表示未初始化且不允许自动生成（存量库 / `auto_init: false`）。
	SecretsActionAwaitInit SecretsAction = "await_init"
	// SecretsActionRecover 表示恢复态：禁止生成替代密钥，只接受人工处置。
	SecretsActionRecover SecretsAction = "recover"
)

// SecretsDecision 是判定表的结论：三态、恢复细分原因与下一步动作。
// SecretsDecision is one row of the decision table.
type SecretsDecision struct {
	State  core.SecretState
	Reason core.SecretRecoveryReason
	Action SecretsAction
}

// CanInitialize 报告当前判定是否允许显式初始化（人触发或新实例自动）。
// adopt 分支钥匙已在盘上，只补记录——那不是"初始化"，不在此列。
// CanInitialize reports whether an explicit init may run in this state.
func (d SecretsDecision) CanInitialize() bool {
	return d.State == core.SecretStateUninitialized && d.Action != SecretsActionAdopt
}

// SecretsFacts 是判定表三项联立的输入事实。`ManagedCiphertexts`（有没有密文）与
// `CiphertextsVerified`（这把钥匙能不能解开它）必须分开：把"没试过"当成"解得开"
// 正是要防的假成功。
// SecretsFacts separates "there is ciphertext" from "this key can open it".
type SecretsFacts struct {
	// Configured 表示解析出了主密钥文件路径；未配置 = 纯 env 实例。
	Configured bool
	// FreshInstance 表示库与 `instance.json` 都是本次启动新建的（T089 口径）。
	FreshInstance bool
	// AutoInit 是 `secrets.auto_init` 的生效值（缺省 true）。
	AutoInit bool
	// Record 是 `instance_secrets` 记录；nil 表示没有记录。
	Record *storage.InstanceSecretRecord
	// KeyPresent 表示密钥文件存在（存在但读不回来时 KeyErr 非空）。
	KeyPresent bool
	// KeyID 是解析成功的主密钥标识；KeyErr 非空时无意义。
	KeyID string
	// KeyErr 是读取 / 解析密钥文件的错误。
	KeyErr error
	// ManagedCiphertexts 是现存托管密文的版本数。
	ManagedCiphertexts int
	// CiphertextsVerified 表示全部现存密文都用这把密钥解开了。
	CiphertextsVerified bool
	// CiphertextsErr 是试解路径本身的错误（读表失败），与"解不开"分开。
	CiphertextsErr error
}

// EvaluateSecrets 按 §2.4.1 判定表给出结论；纯函数，不做 I/O。
// EvaluateSecrets applies the decision table without any I/O.
func EvaluateSecrets(facts SecretsFacts) SecretsDecision {
	if !facts.Configured {
		// 启动时没有解析出密钥路径。先看实例**是否已经是**加密实例：有初始化记录或
		// 有托管密文都说明曾经有过一把密钥——不能因为"这次没找到路径"就当新实例
		// 再生成一把。两者都没有才是"托管从未启用"，可等用户显式初始化。
		if facts.CiphertextsErr != nil {
			return recoveryDecision(core.SecretRecoveryUnavailable)
		}
		if facts.Record != nil {
			return recoveryDecision(core.SecretRecoveryKeyPathMissing)
		}
		if facts.ManagedCiphertexts > 0 {
			return recoveryDecision(core.SecretRecoveryUnprovableProvenance)
		}
		return SecretsDecision{
			State:  core.SecretStateUninitialized,
			Reason: core.SecretRecoveryNone,
			Action: SecretsActionAwaitInit,
		}
	}
	if facts.Record != nil {
		switch {
		case !facts.KeyPresent:
			return recoveryDecision(core.SecretRecoveryMissing)
		case facts.KeyErr != nil:
			return recoveryDecision(core.SecretRecoveryCorrupt)
		case facts.KeyID != facts.Record.KeyID:
			return recoveryDecision(core.SecretRecoveryKeyIDMismatch)
		case facts.CiphertextsErr != nil:
			return recoveryDecision(core.SecretRecoveryUnavailable)
		default:
			return SecretsDecision{
				State:  core.SecretStateReady,
				Reason: core.SecretRecoveryNone,
				Action: SecretsActionReady,
			}
		}
	}

	// 以下都是"没有初始化记录"的分支。
	if !facts.KeyPresent {
		if facts.CiphertextsErr != nil {
			return recoveryDecision(core.SecretRecoveryUnavailable)
		}
		if facts.ManagedCiphertexts > 0 {
			// 有密文却既无记录也无密钥：来源不可证明，**不得**当成未初始化去生成。
			return recoveryDecision(core.SecretRecoveryUnprovableProvenance)
		}
		if facts.FreshInstance && facts.AutoInit {
			return SecretsDecision{
				State:  core.SecretStateUninitialized,
				Reason: core.SecretRecoveryNone,
				Action: SecretsActionCreate,
			}
		}
		return SecretsDecision{
			State:  core.SecretStateUninitialized,
			Reason: core.SecretRecoveryNone,
			Action: SecretsActionAwaitInit,
		}
	}
	if facts.KeyErr != nil {
		return recoveryDecision(core.SecretRecoveryCorrupt)
	}
	if facts.CiphertextsErr != nil {
		return recoveryDecision(core.SecretRecoveryUnavailable)
	}
	if facts.ManagedCiphertexts > 0 && !facts.CiphertextsVerified {
		return recoveryDecision(core.SecretRecoveryUnprovableProvenance)
	}
	// 记录还没写：状态仍是"未初始化"（页面不谎报就绪），动作是 adopt。
	return SecretsDecision{
		State:  core.SecretStateUninitialized,
		Reason: core.SecretRecoveryNone,
		Action: SecretsActionAdopt,
	}
}

// recoveryDecision 收敛恢复态的三元组，避免四处手写字面量。
// recoveryDecision builds one recovery-state verdict.
func recoveryDecision(reason core.SecretRecoveryReason) SecretsDecision {
	return SecretsDecision{
		State:  core.SecretStateRecoveryRequired,
		Reason: reason,
		Action: SecretsActionRecover,
	}
}

// InstanceSecretsStatus 是判定结果与可公开的密钥身份摘要。`Fingerprint` 是可出示的比对值，
// 密钥材料本身**不**出现在任何返回值里。
// InstanceSecretsStatus carries the verdict plus the public key identity summary.
type InstanceSecretsStatus struct {
	Decision           SecretsDecision
	KeyID              string
	Fingerprint        string
	ManagedCiphertexts int
	// KeyFile / InitializedAt / Configured 是管理台投影需要的只读事实（T121）：路径
	// 供备份指引，时间来自初始化记录（无记录时为零值），Configured 区分"本次启动
	// 已解析出路径"与"尚未启用托管"——三者都**不含**密钥材料。
	// The console projection needs the key path, the record time and whether a path
	// is configured at all; none of them carries key material.
	KeyFile       string
	InitializedAt time.Time
	Configured    bool
	// CanInitialize 报告此刻点击"初始化"会不会成功：判定允许，且要么已有路径、
	// 要么能在本次动作里建立身份与路径（`auto_init` 未关闭、工作目录可用）。
	// 页面据此显示或隐藏按钮，处理器据此在预写之前答出 409。
	// CanInitialize is the full precondition of one explicit init, not just the verdict.
	CanInitialize bool
}

// SecretsInitError 表示**预写之后**失败的初始化：`operation_id` 指向可查证的 pending
// 行（契约 /instance/secrets/init 的 500 出口）。调用方据非空标识去操作详情查结果，
// 不得盲目重做；预写之前的拒绝不带本错误，因此不会出现"有标识却没有操作行"。
// SecretsInitError marks a failure after the audit pre-write; the operation id lets
// the caller verify the outcome instead of replaying the action.
type SecretsInitError struct {
	OperationID string
	Err         error
}

// Error 与 Unwrap 保持原错误文本与分类（`errors.Is` 不受包装影响）。
// Error and Unwrap preserve the wrapped error's text and classification.
func (e *SecretsInitError) Error() string { return e.Err.Error() }

// Unwrap 返回被包装的原因。
// Unwrap returns the wrapped cause.
func (e *SecretsInitError) Unwrap() error { return e.Err }

// InstanceSecretsDeps 是实例加密服务面的装配输入。`Actor` 是审计归因：自动初始化
// 由启动流程发起，固定 `migration` / `startup`（契约 §9.3），不冒充管理台身份。
// `KeyPath` 是与模型连接 / MCP 凭据共享的路径持有者；`WorkDir` 用于在用户显式初始化
// 时建立实例名册（没有名册的存量实例在启动时解析不出路径）。
// InstanceSecretsDeps wires the instance-secrets surface; KeyPath is shared with the
// credential components and WorkDir supplies the roster location for an explicit init.
type InstanceSecretsDeps struct {
	Store         *storage.InstanceSecretsStore
	Operations    core.OperationStore
	MasterKeyFile string
	KeyPath       *MasterKeyLocator
	WorkDir       string
	WorkspaceRoot string
	AutoInit      bool
	FreshInstance bool
	Actor         core.Actor
	Logger        *slog.Logger
	Now           func() time.Time
}

// InstanceSecretsService 是实例加密三态与首次初始化的唯一实现。
// InstanceSecretsService is the only implementation of the instance-secrets surface.
type InstanceSecretsService struct {
	deps    InstanceSecretsDeps
	keyPath *MasterKeyLocator
}

// NewInstanceSecretsService 组装服务；仓储与操作台账缺失即拒绝构造——判定表没有
// "读不到记录就当没有记录"这条退路。
// NewInstanceSecretsService refuses to construct without the store and operation ledger.
func NewInstanceSecretsService(deps InstanceSecretsDeps) (*InstanceSecretsService, error) {
	if deps.Store == nil || deps.Operations == nil {
		return nil, errors.New("实例加密服务面装配缺少 instance_secrets 仓储或管理操作台账")
	}
	// 没有共享持有者时自建一个（只在测试与半截装配里出现）：静态字段成为初值，
	// 显式初始化之后本服务看到的是新路径，行为与生产装配一致。
	// Without a shared holder the service seeds its own from the static field.
	keyPath := deps.KeyPath
	if keyPath == nil {
		keyPath = NewMasterKeyLocator(deps.MasterKeyFile)
	}
	return &InstanceSecretsService{deps: deps, keyPath: keyPath}, nil
}

// masterKeyFile 返回本服务当前生效的主密钥路径（可能为空串 = 尚未启用托管）。
// masterKeyFile returns the currently effective key path, possibly empty.
func (s *InstanceSecretsService) masterKeyFile() string {
	return s.keyPath.Path()
}

// Status 只读计算当前判定。有密文而缺记录时**会**真解密文（只读，不落任何东西）；
// 解不开就是恢复态，不把"格式合法"当成"就绪"。
// Status computes the verdict read-only; adoption still decrypts every existing
// ciphertext without writing anything.
func (s *InstanceSecretsService) Status(ctx context.Context) (InstanceSecretsStatus, error) {
	facts, key, err := s.gatherFacts(ctx)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	if key != nil {
		defer key.Zero()
	}
	status := InstanceSecretsStatus{
		Decision:           EvaluateSecrets(facts),
		ManagedCiphertexts: facts.ManagedCiphertexts,
		KeyFile:            s.masterKeyFile(),
		Configured:         s.configured(),
	}
	status.CanInitialize = status.Decision.CanInitialize() &&
		(status.Configured || s.canEstablishKeyPath())
	if facts.Record != nil {
		status.InitializedAt = facts.Record.InitializedAt
	}
	if key != nil && facts.KeyErr == nil {
		status.KeyID = key.KeyID
		fingerprint, fingerprintErr := MasterKeyFingerprint(*key)
		if fingerprintErr != nil {
			return InstanceSecretsStatus{}, fingerprintErr
		}
		status.Fingerprint = fingerprint
	}
	return status, nil
}

// Ensure 是启动路径的唯一入口：在实例锁内**先**做发布残留恢复，**再**读判定表，
// 按结论执行 create / adopt，其余情况原样返回结论供页面显示。
// 调用方必须已持有实例锁——同一工作区只能有一个初始化者。
// Ensure runs inside the caller's instance lock: residue recovery first, then the
// decision table, then create or adopt.
func (s *InstanceSecretsService) Ensure(ctx context.Context) (InstanceSecretsStatus, error) {
	if !s.configured() {
		return s.Status(ctx)
	}
	residueRecovered, err := recoverPublishResidue(s.masterKeyFile(), s.deps.WorkspaceRoot)
	if errors.Is(err, errPublishResidueUnprovable) {
		// 来源不可证明：不删文件、不放宽链接数检查，直接进恢复态。
		s.logger().Warn("实例加密：主密钥发布残留来源不可证明，进入恢复态",
			"path", s.masterKeyFile())
		return InstanceSecretsStatus{
			Decision: recoveryDecision(core.SecretRecoveryUnprovableProvenance),
		}, nil
	}
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	if residueRecovered {
		s.logger().Info("实例加密：已清理可证明来源的发布残留", "path", s.masterKeyFile())
	}

	status, err := s.Status(ctx)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	switch status.Decision.Action {
	case SecretsActionCreate:
		return s.create(ctx, residueRecovered, s.actor())
	case SecretsActionAdopt:
		return s.adopt(ctx, residueRecovered, s.actor())
	default:
		return status, nil
	}
}

// Init 是显式初始化（CLI `secrets init` 与页面按钮）：只在「未初始化且没有密文」
// 时生成；已就绪 / 待 adopt / 恢复态一律拒绝并点名当前状态，**不**代之以生成新密钥。
//
// 启动时没有解析出路径的存量实例（无名册）走 `establishKeyPath`：由这次显式动作
// 建立实例标识与工作区外的密钥路径，**然后**才按判定表生成。初始化成功后路径立即
// 写进共享 locator——模型连接 / MCP 凭据在同一进程内即可加密写入，无需重启。
// Init also establishes the instance identity and key path for a roster-less instance
// when the user explicitly asks for managed credentials, and publishes the path to the
// shared locator so every credential component sees it in this very process.
func (s *InstanceSecretsService) Init(ctx context.Context, actor core.Actor) (InstanceSecretsStatus, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	if !s.configured() {
		if err := s.establishKeyPath(status); err != nil {
			return InstanceSecretsStatus{}, err
		}
		if status, err = s.Status(ctx); err != nil {
			return InstanceSecretsStatus{}, err
		}
	}
	if _, err := recoverPublishResidue(s.masterKeyFile(), s.deps.WorkspaceRoot); err != nil {
		return InstanceSecretsStatus{}, err
	}
	switch {
	case status.Decision.Action == SecretsActionAdopt:
		return InstanceSecretsStatus{}, fmt.Errorf(
			"%w: 密钥文件已存在且可用于现有密文——请改用初始化流程的 adopt（自动补写记录），不要重新生成",
			core.ErrCredentialUnavailable)
	case !status.Decision.CanInitialize():
		return InstanceSecretsStatus{}, fmt.Errorf(
			"%w: 当前实例状态为 %s（原因 %q），拒绝生成新密钥",
			core.ErrCredentialUnavailable, status.Decision.State, status.Decision.Reason)
	}
	return s.create(ctx, false, actor)
}

// EnsureForManagedWrite 是「首次保存托管凭据」的受控入口（R2-1）：在同一次保存请求里
// 完成检查 → （仅限可证明从未启用托管的存量实例）建立实例身份与密钥路径 → 生成密钥与
// 记录。路径经共享 locator 立即生效，同一进程内的凭据写面随即可加密，不需要额外的
// 「初始化」点击或重启。
//
// 三条边界：
//   - 缺少 `secrets` 段**不是**纯 env 部署的证明；只有显式 `secrets.auto_init: false`
//     才进入「不启用托管」的拒绝分支，其余情况缺少名册时按存量实例建立身份；
//   - 有初始化记录 / 有托管密文 / 有来源不可证明的发布残留 → 走既有恢复规则，**不得**
//     生成第二把密钥（`establishKeyPath` 与判定表各自拒绝，覆盖同一事实）；
//   - 已就绪直接返回；待 adopt 只补记录；审计归因到发起保存的管理员，不冒充启动流程。
//
// EnsureForManagedWrite folds the first managed save on a legacy instance into one
// audited flow; it never mints over a record, ciphertext or unprovable residue.
func (s *InstanceSecretsService) EnsureForManagedWrite(ctx context.Context, actor core.Actor) error {
	status, err := s.Status(ctx)
	if err != nil {
		return err
	}
	if !s.configured() {
		// establishKeyPath 自己拒绝恢复态与显式 auto_init: false，并在此建立名册；
		// 之后重新判一次，后续分支只看事实。
		if err := s.establishKeyPath(status); err != nil {
			return err
		}
		if status, err = s.Status(ctx); err != nil {
			return err
		}
	}
	residueRecovered, err := recoverPublishResidue(s.masterKeyFile(), s.deps.WorkspaceRoot)
	if errors.Is(err, errPublishResidueUnprovable) {
		return fmt.Errorf(
			"%w: 实例主密钥发布残留来源不可证明——托管凭据已失败关闭，按 docs/Operations.md 的主密钥恢复步骤处置",
			core.ErrCredentialUnavailable)
	}
	if err != nil {
		return fmt.Errorf("%w: 检查主密钥发布残留: %v", core.ErrCredentialUnavailable, err)
	}
	switch status.Decision.Action {
	case SecretsActionReady:
		return nil
	case SecretsActionAdopt:
		_, err := s.adopt(ctx, residueRecovered, actor)
		return err
	case SecretsActionCreate:
		_, err := s.create(ctx, residueRecovered, actor)
		return err
	}
	if status.Decision.State == core.SecretStateRecoveryRequired {
		return fmt.Errorf(
			"%w: 实例主密钥处于恢复态（原因 %q）——不生成新密钥、不回落环境变量",
			core.ErrCredentialUnavailable, status.Decision.Reason)
	}
	if !status.Decision.CanInitialize() {
		// 走到这里只剩 await_init + auto_init: false：显式纯 env 部署。
		return fmt.Errorf(
			"%w: secrets.auto_init 已显式关闭（纯环境变量部署）——如需加密托管请先配置 secrets.master_key_file",
			core.ErrCredentialUnavailable)
	}
	// await_init 且允许初始化：本次保存就是用户显式启用托管的动作。
	_, err = s.create(ctx, residueRecovered, actor)
	return err
}

// establishKeyPath 为「托管从未启用」的实例建立身份与路径（R2-1）：判定表必须证明
// 无初始化记录、无托管密文（否则是恢复态，先找回原密钥而不是造第二把）；`auto_init`
// 不得被显式关闭（那是纯 env 部署，按钮点不动是正确结果）。
//
// 建立的是**新**名册，所以推导出的私有目录此前不可能存在——不会覆盖既有密钥，也不会
// 把旧密文变成孤儿。写入只在本进程内生效；重启后由名册推导，与本进程看到的是同一路径。
// A brand-new roster makes the derived private directory provably fresh, so nothing is
// overwritten; the locator carries the path to every credential component immediately.
func (s *InstanceSecretsService) establishKeyPath(status InstanceSecretsStatus) error {
	if status.Decision.State == core.SecretStateRecoveryRequired {
		return fmt.Errorf(
			"%w: 实例处于恢复态（原因 %q）——先找回原主密钥或实例名册，不建立新的加密身份",
			core.ErrCredentialUnavailable, status.Decision.Reason)
	}
	if !s.deps.AutoInit {
		return fmt.Errorf(
			"%w: secrets.auto_init 已显式关闭（纯环境变量部署）——如需加密托管请先配置 secrets.master_key_file",
			core.ErrCredentialUnavailable)
	}
	if !status.Decision.CanInitialize() {
		return fmt.Errorf(
			"%w: 当前实例状态为 %s（动作 %s），拒绝生成新密钥",
			core.ErrCredentialUnavailable, status.Decision.State, status.Decision.Action)
	}
	if _, err := ensureInstanceMarker(s.deps.WorkDir); err != nil {
		return fmt.Errorf("%w: 建立实例标识: %v", core.ErrCredentialUnavailable, err)
	}
	path, err := defaultMasterKeyFile(s.deps.WorkDir)
	if err != nil {
		return fmt.Errorf("%w: 推导实例私有目录: %v", core.ErrCredentialUnavailable, err)
	}
	s.keyPath.Set(path)
	s.logger().Info("实例加密：已建立实例标识与主密钥路径（用户显式初始化）", "path", path)
	return nil
}

// canEstablishKeyPath 报告显式初始化能否就地建立实例身份与密钥路径：`auto_init` 未关闭
// 且工作目录可用。纯 env 部署（`auto_init: false`）返回 false——此时页面上的"初始化"
// 必然失败，处理器必须在预写之前拒绝。
// canEstablishKeyPath reports whether this service may create the roster and key path.
func (s *InstanceSecretsService) canEstablishKeyPath() bool {
	return s.deps.AutoInit && strings.TrimSpace(s.deps.WorkDir) != ""
}

// Adopt 是显式补写记录（外部预置密钥的运维路径）；条件不满足时拒绝。
// Adopt writes the missing record for an externally provisioned key.
func (s *InstanceSecretsService) Adopt(ctx context.Context) (InstanceSecretsStatus, error) {
	if !s.configured() {
		return InstanceSecretsStatus{}, fmt.Errorf(
			"%w: 未配置主密钥文件（secrets.master_key_file）", core.ErrCredentialUnavailable)
	}
	status, err := s.Status(ctx)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	if status.Decision.Action != SecretsActionAdopt {
		return InstanceSecretsStatus{}, fmt.Errorf(
			"%w: 当前实例状态为 %s（原因 %q），不满足 adopt 条件",
			core.ErrCredentialUnavailable, status.Decision.State, status.Decision.Reason)
	}
	return s.adopt(ctx, false, s.actor())
}

// create 执行「生成 → 发布 → 读回 → 写记录 → 审计」。
// 审计**先**预写 pending：预写失败即拒绝生成，不留半个动作。
// create mints and publishes the key, then writes the record; the audit row is
// pre-written first so a failed pre-write refuses the whole action.
func (s *InstanceSecretsService) create(ctx context.Context, residueRecovered bool,
	actor core.Actor,
) (InstanceSecretsStatus, error) {
	opID, err := s.beginSecretsInit(ctx, actor)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	key, keyErr := s.generateAndPublish(ctx)
	if keyErr != nil {
		// 文件已存在 = 另一个初始化者抢先发布：本次**没有**生成任何东西，审计记 failed /
		// CONFLICT（写成 success 会让台账里出现一行"初始化成功但没有 key_id"的假动作），
		// 随后按判定表转 adopt——两把密钥绝不允许同时有效，也绝不覆盖。
		if errors.Is(keyErr, errMasterKeyAlreadyExists) {
			s.finishSecretsInit(ctx, opID, core.OperationFailed, "CONFLICT",
				"另一个初始化者已发布主密钥：本次未生成、未覆盖，按 adopt 处理",
				secretsInitDetail{Recovered: residueRecovered})
			status, statusErr := s.Status(ctx)
			if statusErr != nil {
				return InstanceSecretsStatus{}, statusErr
			}
			if status.Decision.Action == SecretsActionAdopt {
				return s.adopt(ctx, residueRecovered, actor)
			}
			return status, nil
		}
		s.finishSecretsInit(ctx, opID, core.OperationFailed, "INTERNAL", keyErr.Error(),
			secretsInitDetail{Recovered: residueRecovered})
		return InstanceSecretsStatus{}, &SecretsInitError{OperationID: opID, Err: keyErr}
	}
	defer key.Zero()

	fingerprint, err := MasterKeyFingerprint(key)
	if err != nil {
		s.finishSecretsInit(ctx, opID, core.OperationFailed, "INTERNAL", err.Error(),
			secretsInitDetail{KeyID: key.KeyID, Recovered: residueRecovered})
		return InstanceSecretsStatus{}, &SecretsInitError{OperationID: opID, Err: err}
	}
	// 一个时间点写进记录、投影进返回值：页面显示的时间与库里的初始化时间同源。
	// One timestamp feeds both the record and the returned projection.
	initializedAt := s.now()
	recordErr := s.deps.Store.Insert(ctx, storage.InstanceSecretRecord{
		Scope:         core.InstanceSecretScopeMasterKey,
		KeyID:         key.KeyID,
		Fingerprint:   fingerprint,
		InitializedAt: initializedAt,
		OperationID:   opID,
	})
	if recordErr != nil {
		// 记录写失败：密钥文件**保留**在盘上，下次启动按"无记录 + 有文件"走 adopt。
		// 删掉它才是真正的数据丢失面（外部可能已开始用它加密），而且会让重试必然换钥匙。
		s.finishSecretsInit(ctx, opID, core.OperationFailed, "INTERNAL", recordErr.Error(),
			secretsInitDetail{KeyID: key.KeyID, Fingerprint: fingerprint, Recovered: residueRecovered})
		if errors.Is(recordErr, storage.ErrInstanceSecretExists) {
			// 并发竞争：别人已经写下记录。以记录为准重新判定（key_id 一致即就绪）。
			return s.Status(ctx)
		}
		return InstanceSecretsStatus{}, &SecretsInitError{OperationID: opID, Err: recordErr}
	}
	s.finishSecretsInit(ctx, opID, core.OperationSuccess, "OK", "",
		secretsInitDetail{KeyID: key.KeyID, Fingerprint: fingerprint, Recovered: residueRecovered})
	s.logger().Info("实例加密：主密钥已生成并写入初始化记录", "key_id", key.KeyID, "fingerprint", fingerprint)
	return InstanceSecretsStatus{
		Decision:           SecretsDecision{State: core.SecretStateReady, Reason: core.SecretRecoveryNone, Action: SecretsActionReady},
		KeyID:              key.KeyID,
		Fingerprint:        fingerprint,
		ManagedCiphertexts: 0,
		KeyFile:            s.masterKeyFile(),
		InitializedAt:      initializedAt,
		Configured:         true,
	}, nil
}

// generateAndPublish 生成密钥并发布到磁盘，返回仍可用的密钥材料（调用方负责 Zero）。
// generateAndPublish mints the key and publishes it; the caller owns Zero.
func (s *InstanceSecretsService) generateAndPublish(_ context.Context) (MasterKey, error) {
	if err := prepareSecretDirectory(secretDirectory(s.masterKeyFile())); err != nil {
		return MasterKey{}, err
	}
	key, err := GenerateMasterKey()
	if err != nil {
		return MasterKey{}, err
	}
	if err := publishMasterKey(s.masterKeyFile(), s.deps.WorkspaceRoot, key); err != nil {
		key.Zero()
		return MasterKey{}, err
	}
	return key, nil
}

// adopt 为已存在的密钥补写初始化记录。写入前**已经**在 Status 里做过完整校验
// （读回 + 有密文时逐条试解），这里只落记录。
// adopt writes the missing record for a key that already passed full verification.
func (s *InstanceSecretsService) adopt(ctx context.Context, residueRecovered bool,
	actor core.Actor,
) (InstanceSecretsStatus, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	if status.Decision.Action != SecretsActionAdopt {
		return status, nil
	}
	opID, err := s.beginSecretsInit(ctx, actor)
	if err != nil {
		return InstanceSecretsStatus{}, err
	}
	initializedAt := s.now()
	insertErr := s.deps.Store.Insert(ctx, storage.InstanceSecretRecord{
		Scope:         core.InstanceSecretScopeMasterKey,
		KeyID:         status.KeyID,
		Fingerprint:   status.Fingerprint,
		InitializedAt: initializedAt,
		OperationID:   opID,
	})
	if insertErr != nil {
		s.finishSecretsInit(ctx, opID, core.OperationFailed, "INTERNAL", insertErr.Error(),
			secretsInitDetail{KeyID: status.KeyID, Fingerprint: status.Fingerprint, Recovered: residueRecovered})
		if errors.Is(insertErr, storage.ErrInstanceSecretExists) {
			return s.Status(ctx)
		}
		return InstanceSecretsStatus{}, &SecretsInitError{OperationID: opID, Err: insertErr}
	}
	// 记录刚写：页面随即显示的时间与库里的初始化时间同源。
	status.InitializedAt = initializedAt
	s.finishSecretsInit(ctx, opID, core.OperationSuccess, "OK", "",
		secretsInitDetail{KeyID: status.KeyID, Fingerprint: status.Fingerprint, Recovered: residueRecovered})
	s.logger().Info("实例加密：外部预置密钥已验证，初始化记录已补写",
		"key_id", status.KeyID, "fingerprint", status.Fingerprint)
	status.Decision = SecretsDecision{State: core.SecretStateReady, Reason: core.SecretRecoveryNone, Action: SecretsActionReady}
	return status, nil
}

// gatherFacts 读取三项联立的事实。返回的密钥（可能为 nil）由调用方 Zero。
// gatherFacts reads the three inputs; the returned key is owned by the caller.
func (s *InstanceSecretsService) gatherFacts(ctx context.Context) (SecretsFacts, *MasterKey, error) {
	facts := SecretsFacts{Configured: s.configured(), FreshInstance: s.deps.FreshInstance, AutoInit: s.deps.AutoInit}
	record, exists, err := s.deps.Store.Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		// 读不到记录 = 无法判定，不是"没有记录"。
		facts.CiphertextsErr = err
		return facts, nil, nil
	}
	if exists {
		facts.Record = &record
	}
	count, err := s.deps.Store.CountManagedCiphertext(ctx)
	if err != nil {
		facts.CiphertextsErr = err
		return facts, nil, nil
	}
	facts.ManagedCiphertexts = count
	if !facts.Configured {
		return facts, nil, nil
	}

	present, err := masterKeyFileExists(s.masterKeyFile())
	if err != nil {
		facts.KeyErr = err
		return facts, nil, nil
	}
	facts.KeyPresent = present
	if !present {
		return facts, nil, nil
	}
	key, loadErr := LoadMasterKey(s.masterKeyFile(), s.deps.WorkspaceRoot)
	if loadErr != nil {
		facts.KeyErr = loadErr
		return facts, nil, nil
	}
	facts.KeyID = key.KeyID
	if count > 0 {
		if verifyErr := s.verifySealedCredentials(ctx, key); verifyErr != nil {
			if errors.Is(verifyErr, errSealedCredentialsUnreadable) {
				facts.CiphertextsVerified = false
			} else {
				facts.CiphertextsErr = verifyErr
			}
		} else {
			facts.CiphertextsVerified = true
		}
	}
	return facts, &key, nil
}

// errSealedCredentialsUnreadable 表示"这把密钥解不开现有密文"（与读表失败区分）。
// errSealedCredentialsUnreadable separates "this key cannot open the ciphertext"
// from a storage read failure.
var errSealedCredentialsUnreadable = errors.New("当前主密钥无法解开现存托管密文")

// verifySealedCredentials 逐条试解现存密文，解开的明文立即清零。任一条失败即返回
// `errSealedCredentialsUnreadable`——"大部分解得开"不是就绪。
// verifySealedCredentials opens every sealed version and zeroes each plaintext.
func (s *InstanceSecretsService) verifySealedCredentials(ctx context.Context, key MasterKey) error {
	sealed, err := s.deps.Store.SealedCredentials(ctx)
	if err != nil {
		return err
	}
	for _, item := range sealed {
		aad := CredentialAAD{
			OwnerKind:     core.BindingKind(item.OwnerKind),
			OwnerUID:      core.ResourceUID(item.OwnerUID),
			CredentialID:  item.CredentialID,
			VersionNo:     item.VersionNo,
			TargetBinding: item.TargetBinding,
		}
		plaintext, openErr := OpenCredential(key, aad, item.Nonce, item.Ciphertext)
		if openErr != nil {
			return fmt.Errorf("%w: 凭据 %s 版本 %d", errSealedCredentialsUnreadable, item.CredentialID, item.VersionNo)
		}
		zeroCredentialValue(plaintext)
	}
	return nil
}

// SecretsInitDetail 是 `system.secrets_init` 的 `detail_json` 固定键集（契约 §9.3）：
// 只记可公开的密钥身份摘要与是否走了残留恢复，**不**记路径、**不**记密钥材料。
// SecretsInitDetail is the fixed detail_json key set of system.secrets_init.
type SecretsInitDetail struct {
	KeyID       string `json:"key_id"`
	Fingerprint string `json:"fingerprint"`
	Recovered   bool   `json:"recovered"`
}

// secretsInitDetail 是内部别名，避免到处写类型名。
type secretsInitDetail = SecretsInitDetail

// beginSecretsInit 预写 pending 操作行；失败即拒绝执行（宪法第 5 条）。
// beginSecretsInit pre-writes the pending row; a failure refuses the action.
func (s *InstanceSecretsService) beginSecretsInit(ctx context.Context, actor core.Actor) (string, error) {
	attribution := s.auditActor(actor)
	now := s.now()
	opID := core.NewOperationID(now)
	detail, err := json.Marshal(secretsInitDetail{})
	if err != nil {
		return "", fmt.Errorf("序列化初始化审计详情: %w", err)
	}
	err = s.deps.Operations.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionSystemSecretsInit,
		TargetKind:   core.TargetKindSystem,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: attribution.Subject,
		AuthMethod:   attribution.Method,
		Source:       attribution.Source,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	})
	if err != nil {
		return "", fmt.Errorf("%w: 预写初始化审计失败: %v", core.ErrCredentialUnavailable, err)
	}
	return opID, nil
}

// finishSecretsInit 回填终态；用 `context.WithoutCancel` 让取消不再改变审计结论
// ——动作已经发生，审计必须落地。
// finishSecretsInit writes the terminal state even when the caller's context is gone.
func (s *InstanceSecretsService) finishSecretsInit(ctx context.Context, opID string,
	status core.OperationStatus, resultCode, message string, detail secretsInitDetail,
) {
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = nil
	}
	if finishErr := s.deps.Operations.FinishOperation(context.WithoutCancel(ctx), opID, status,
		resultCode, message, raw, s.now()); finishErr != nil {
		s.logger().Error("实例加密：回填初始化审计失败（动作已执行）",
			"operation_id", opID, "error", finishErr)
	}
}

// configured 报告是否解析出主密钥路径。
// configured reports whether a master-key path is set.
func (s *InstanceSecretsService) configured() bool {
	return strings.TrimSpace(s.masterKeyFile()) != ""
}

// actor 返回审计归因；未显式给出时固定为启动流程（不冒充管理台身份）。
// actor returns the audit attribution, defaulting to the startup flow.
func (s *InstanceSecretsService) actor() core.Actor {
	if s.deps.Actor.Subject != "" {
		return s.deps.Actor
	}
	return core.Actor{Subject: core.ActorSubjectMigration, Method: core.AuthMethodMigration, Source: "startup"}
}

// auditActor 用调用方给出的归因（网页保存时是管理员的真实身份），留空才退回启动归因
// ——初始化审计行必须说得出"谁让它发生的"，不把管理员的动作记成迁移流程。
// auditActor prefers the caller's attribution and only falls back to the startup one.
func (s *InstanceSecretsService) auditActor(actor core.Actor) core.Actor {
	if strings.TrimSpace(actor.Subject) != "" {
		return actor
	}
	return s.actor()
}

func (s *InstanceSecretsService) logger() *slog.Logger {
	if s.deps.Logger != nil {
		return s.deps.Logger
	}
	return slog.Default()
}

func (s *InstanceSecretsService) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
}

// verifySecretsInitOperation 是 `system.secrets_init` 的启动恢复查证（契约 §9.4）：
// 记录存在 + `key_id` 一致 + 文件校验通过且 `nlink` 已回到 1 → applied；
// 记录不存在且可证明未创建任何文件（含临时链接）→ not_applied；
// 其余（记录缺失但文件存在、文件存在而来源不可证明）→ indeterminate。
// **不得**删除文件、**不得**生成第二把密钥。
// verifySecretsInitOperation verifies a pending init without ever deleting or minting.
func verifySecretsInitOperation(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.InstanceSecrets == nil || strings.TrimSpace(deps.MasterKeyFile) == "" {
		return core.VerificationIndeterminate
	}
	var detail SecretsInitDetail
	if len(op.DetailJSON) > 0 {
		if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
			return core.VerificationIndeterminate
		}
	}
	record, exists, err := deps.InstanceSecrets.Load(ctx, core.InstanceSecretScopeMasterKey)
	if err != nil {
		return core.VerificationIndeterminate
	}
	if !exists {
		present := publishArtifactsPresent(deps.MasterKeyFile)
		if present == artifactsNone {
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}
	if detail.KeyID != "" && record.KeyID != detail.KeyID {
		return core.VerificationIndeterminate
	}
	key, loadErr := LoadMasterKey(deps.MasterKeyFile, deps.Root)
	if loadErr != nil || key.KeyID != record.KeyID {
		if loadErr == nil {
			key.Zero()
		}
		return core.VerificationIndeterminate
	}
	key.Zero()
	info, statErr := os.Lstat(deps.MasterKeyFile)
	if statErr != nil {
		return core.VerificationIndeterminate
	}
	if links, known := hardLinkCount(deps.MasterKeyFile, info); !known || links != 1 {
		return core.VerificationIndeterminate
	}
	return core.VerificationApplied
}
