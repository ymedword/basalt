package web

// 本文件是实例加密状态的 HTTP 面（14D；契约 GET /instance/secrets 与
// POST /instance/secrets/init）。
//
// 三条纪律：
//  1. GET **只读**：不创建目录、不写文件、不写管理操作行；密钥材料与指纹永不进响应，
//     `key_file` 只回路径（供备份指引）。
//  2. POST 的两种 409（已初始化 / 恢复态）与"不可初始化"在**预写之前**判定，因此零
//     写入、零操作行；500 只在预写之后的失败出现，并带 `data.operation_id` 供查证。
//  3. `credential_summary` 与资源列表读**同一批**视图投影：总览的计数与列表的凭据列
//     不会各说各话；未声明来源的槽位按契约计入 `missing`。
// The console face of instance encryption: read-only GET, a pre-write 409 split, and a
// summary derived from the very views the resource lists use.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// InstanceSecretsStatus 是实例加密服务的投影形状（端口内部类型，不是 JSON 视图）：
// `CanInitialize` 让处理器在**预写之前**答出 409，而不是先动手再失败。
// `KeyFile` 为空串即"未配置主密钥路径"（纯环境变量部署）。
// InstanceSecretsStatus is the consumer-side shape; CanInitialize lets the handler
// answer 409 before anything is written, and an empty KeyFile means no path is
// configured at all.
type InstanceSecretsStatus struct {
	State         string
	Reason        string
	KeyID         string
	KeyFile       string
	InitializedAt time.Time
	CanInitialize bool
}

// InstanceSecretsService 是实例加密的消费方端口：`Init` 的第二个返回值是预写之后的
// 操作标识（未预写时为空串），处理器据此决定 500 是否携带 `data.operation_id`。
// The Init operation id is empty when nothing was pre-written.
type InstanceSecretsService interface {
	Status(ctx context.Context) (InstanceSecretsStatus, error)
	// Init 的 actor 是这次高权限变更的审计归因（管理台管理员）：初始化与
	// `*.credential.write` 是两行可查的操作，归因必须一致。
	Init(ctx context.Context, actor core.Actor) (operationID string, err error)
}

// InstanceSecretsView 是契约 `InstanceSecretsView`：状态三值、密钥身份摘要与凭据计数。
// 不含密钥材料、不含指纹原文；`key_id` 仅 `ready` 时非空。
// InstanceSecretsView is the contract projection: state, public key identity and the
// credential counts; it never carries key material.
type InstanceSecretsView struct {
	State          string `json:"state"`
	KeyID          string `json:"key_id"`
	InitializedAt  string `json:"initialized_at"`
	KeyFile        string `json:"key_file"`
	RecoveryReason string `json:"recovery_reason"`
	// CanInitialize 是"此刻点初始化会成功"的完整前置条件：判定允许，且已有路径或
	// 能在本次动作里建立实例身份与路径。页面据此决定是否显示初始化按钮——只按
	// `uninitialized` 显示会让纯 env 部署的按钮必然失败。
	// CanInitialize is the full precondition of one explicit init; the page shows the
	// button only when it is true.
	CanInitialize bool `json:"can_initialize"`
	// CredentialSummary 是四档计数：与 MCP / 模型连接 / 通知渠道列表的凭据列同源。
	CredentialSummary CredentialKeySummaryView `json:"credential_summary"`
}

// CredentialKeySummaryView 是契约 `CredentialKeySummary`：四档计数各自独立。
// `missing` 包含"声明托管却没有值"与"没有来源声明"两种（契约定义）。
// CredentialKeySummaryView is the four-bucket count; missing covers both an
// undeclared slot and a managed slot without a value.
type CredentialKeySummaryView struct {
	Managed        int `json:"managed"`
	ManagedRevoked int `json:"managed_revoked"`
	Env            int `json:"env"`
	Missing        int `json:"missing"`
}

// instanceSecretsInitRequest 是契约 `InstanceSecretsInitRequest`：`confirm` 必须显式
// 为 true，缺省（false）按 400 拒绝——初始化会生成新主密钥，不能靠"省略即同意"。
// The confirm flag must be literally true; omission is a 400.
type instanceSecretsInitRequest struct {
	Confirm bool `json:"confirm"`
}

// 初始化端点的固定公开文案。
// Fixed public texts of the init endpoint.
// #nosec G101 -- 固定公开文案：谈论主密钥状态，不含任何材料（变量名命中）。
const (
	instanceSecretsInitBodyMessage = `请求体必须是 {"confirm": true}`
	instanceSecretsConfirmMessage  = "confirm 必须显式为 true：初始化会生成新的主密钥，需要人工确认"
	instanceSecretsReadyConflict   = "实例主密钥已初始化：不覆盖既有密钥；确需更换请先离线备份并按 docs/Operations.md 的主密钥恢复步骤处置"
	instanceSecretsAwaitAdopt      = "主密钥文件已存在但缺少初始化记录：重启实例会自动采用现有密钥（adopt），不得重新生成"
	instanceSecretsUnconfigured    = "此实例以纯环境变量方式部署（secrets.auto_init: false），不使用加密托管，无需初始化"
	instanceSecretsRaceRefused     = "实例当前状态不允许初始化（已初始化、恢复态或纯环境变量部署）：未生成任何新密钥"
	instanceSecretsRecoveryPrefix  = "实例主密钥处于恢复态，不生成替代密钥："
	instanceSecretsRecoverySuffix  = "；按 docs/Operations.md 的主密钥恢复步骤处置"
)

// instanceSecretsRecoveryText 把恢复原因码翻成固定中文短语；未知原因只说"无法判定"，
// 不把底层错误文本带进响应。
// instanceSecretsRecoveryText maps the frozen reason codes to fixed Chinese phrases;
// an unknown code degrades to "cannot determine" instead of echoing internals.
func instanceSecretsRecoveryText(reason string) string {
	switch core.SecretRecoveryReason(reason) {
	case core.SecretRecoveryMissing:
		return "密钥文件不存在"
	case core.SecretRecoveryCorrupt:
		return "密钥文件损坏或校验失败"
	case core.SecretRecoveryKeyIDMismatch:
		return "密钥与初始化记录不符"
	case core.SecretRecoveryUnprovableProvenance:
		return "存在无法证明来源的密钥文件"
	case core.SecretRecoveryUnavailable:
		return "无法判定（读取失败）"
	case core.SecretRecoveryKeyPathMissing:
		return "实例有初始化记录，但当前解析不出主密钥路径（实例名册或 secrets 配置缺失）"
	default:
		return "原因未知"
	}
}

// handleInstanceSecrets 处理 `GET /api/v1/instance/secrets`：只读投影，缺端口按 500
// 失败关闭（不是"未初始化"的 200——那会把装配错误说成一个状态）。
// The read-only projection; a missing port is a 500, never an "uninitialized" 200.
func (s *Server) handleInstanceSecrets(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.InstanceSecrets == nil {
		s.writeAdminPortMissing(w, "InstanceSecrets", "GET /instance/secrets")
		return
	}
	view, err := s.instanceSecretsView(r.Context())
	if err != nil {
		s.logger().Error("读取实例加密状态失败", "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	s.writeOK(w, view)
}

// handleAdminInitInstanceSecrets 处理 `POST /api/v1/instance/secrets/init`：
// 先判状态再动手——`ready` / 恢复态 / 不可初始化三种情形各回 409，零写入、零操作行；
// 只有真正进入服务层之后失败才回 500 并带 `data.operation_id`。
// The status is judged before anything is written; only a post-pre-write failure
// carries an operation id.
func (s *Server) handleAdminInitInstanceSecrets(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.InstanceSecrets == nil {
		s.writeAdminPortMissing(w, "InstanceSecrets", "POST /instance/secrets/init")
		return
	}
	var body instanceSecretsInitRequest
	if err := s.decodeAdminWriteBody(w, r, &body, instanceSecretsInitBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if !body.Confirm {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, instanceSecretsConfirmMessage)
		return
	}
	status, err := s.opts.InstanceSecrets.Status(r.Context())
	if err != nil {
		s.logger().Error("判断实例加密状态失败", "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	switch {
	case status.State == string(core.SecretStateReady):
		s.writeError(w, http.StatusConflict, codeConflict, instanceSecretsReadyConflict)
		return
	case status.State == string(core.SecretStateRecoveryRequired):
		s.writeError(w, http.StatusConflict, codeSecretsRecoveryRequired,
			instanceSecretsRecoveryPrefix+instanceSecretsRecoveryText(status.Reason)+instanceSecretsRecoverySuffix)
		return
	case !status.CanInitialize:
		// 两种子情形分开说：有待 adopt 的密钥文件 vs 纯环境变量部署（`auto_init: false`）。
		// 两者都在预写之前，且都**不**生成任何新密钥。存量无名册实例不在这里——它的
		// `CanInitialize` 为 true，初始化时由服务层建立实例标识与路径。
		if status.KeyFile == "" {
			s.writeError(w, http.StatusConflict, codeConflict, instanceSecretsUnconfigured)
			return
		}
		s.writeError(w, http.StatusConflict, codeConflict, instanceSecretsAwaitAdopt)
		return
	}
	operationID, err := s.opts.InstanceSecrets.Init(r.Context(), actor)
	if err != nil {
		if errors.Is(err, core.ErrCredentialUnavailable) && operationID == "" {
			// 预写之前的并发拒绝（另一初始化者已抢先发布）：409、零写入。
			s.writeError(w, http.StatusConflict, codeConflict, instanceSecretsRaceRefused)
			return
		}
		s.writeInstanceSecretsInitError(w, err, operationID)
		return
	}
	// 成功响应是**重新读出**的投影（含新写入的 initialized_at 与凭据计数），不回显
	// 服务层的中间状态。
	view, err := s.instanceSecretsView(r.Context())
	if err != nil {
		s.logger().Error("初始化后重读实例加密状态失败", "error", err, "operation_id", operationID)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	s.writeOK(w, view)
}

// writeInstanceSecretsInitError 把预写之后的失败映射为 500：有操作标识就带上，
// 供页面查证结果；没有标识（理论上只有预写失败）只回固定文案。
// A post-pre-write failure maps to 500 and carries the operation id when one exists.
func (s *Server) writeInstanceSecretsInitError(w http.ResponseWriter, err error, operationID string) {
	s.logger().Error("实例主密钥初始化失败", "error", err, "operation_id", operationID)
	if operationID == "" {
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	s.writeEnvelope(w, http.StatusInternalServerError, Envelope{
		Code: codeInternal, Message: messageInternal,
		Data:      ErrorDataView{OperationID: &operationID},
		Timestamp: s.timestamp(),
	})
}

// instanceSecretsView 组装只读投影：状态与密钥身份来自端口，凭据计数来自资源视图。
// 端口给出契约闭集之外的状态时返回错误（回 500），**不**把未定义取值写进响应。
// Out-of-enum values from the port are an error, not a response value.
func (s *Server) instanceSecretsView(ctx context.Context) (InstanceSecretsView, error) {
	status, err := s.opts.InstanceSecrets.Status(ctx)
	if err != nil {
		return InstanceSecretsView{}, err
	}
	state := core.SecretState(status.State)
	reason := core.SecretRecoveryReason(status.Reason)
	if !state.Valid() || !reason.Valid() {
		return InstanceSecretsView{}, fmt.Errorf("实例加密服务返回契约外取值：state=%q reason=%q",
			status.State, status.Reason)
	}
	summary, err := s.credentialKeySummary(ctx)
	if err != nil {
		return InstanceSecretsView{}, err
	}
	view := InstanceSecretsView{
		State:             string(state),
		KeyFile:           status.KeyFile,
		RecoveryReason:    string(reason),
		CanInitialize:     status.CanInitialize,
		CredentialSummary: summary,
	}
	// 契约：`key_id` 仅 ready 时非空；`initialized_at` 未初始化时为空串。
	if state == core.SecretStateReady {
		view.KeyID = status.KeyID
	}
	if !status.InitializedAt.IsZero() {
		view.InitializedAt = status.InitializedAt.UTC().Format(time.RFC3339)
	}
	return view, nil
}

// credentialKeySummary 从资源只读面聚合四档计数（契约 `CredentialKeySummary`）：
//
//	MCP 槽位   逐键——`credential_keys` 的声明槽位按状态计数，未声明来源的槽位按
//	           `missing` 计（契约把"没有来源声明"归入 missing）
//	模型连接   每个连接一个凭据槽位：托管 + active → managed，托管 + revoked →
//	           managed_revoked，其余（含切回 env 的退役身份）→ env
//	通知渠道   同上；渠道的空模式按 env 读（与视图投影同一口径）
//
// 读不到任一资源面就报错（回 500），**不**用 0 冒充"没有凭据"。
// The summary groups MCP slots per key and other owners per owner; an unreadable
// resource face is an error, never a zero count.
func (s *Server) credentialKeySummary(ctx context.Context) (CredentialKeySummaryView, error) {
	if s.opts.Resources == nil {
		return CredentialKeySummaryView{}, errors.New("装配缺少 Resources 端口（凭据计数）")
	}
	if s.opts.Notify == nil {
		return CredentialKeySummaryView{}, errors.New("装配缺少 Notify 端口（凭据计数）")
	}
	var summary CredentialKeySummaryView

	servers, err := s.opts.Resources.MCPServers(ctx)
	if err != nil {
		return CredentialKeySummaryView{}, fmt.Errorf("统计 MCP 凭据槽位: %w", err)
	}
	for _, server := range servers {
		declared := 0
		for _, slot := range server.CredentialKeys {
			declared++
			switch core.MCPCredentialKeyState(slot.State) {
			case core.MCPCredentialManaged:
				summary.Managed++
			case core.MCPCredentialManagedRevoked:
				summary.ManagedRevoked++
			case core.MCPCredentialEnv:
				summary.Env++
			default:
				// 含契约的 missing：声明托管却没有可用的值。
				summary.Missing++
			}
		}
		if undeclared := len(server.EnvKeys) + len(server.HeaderKeys) - declared; undeclared > 0 {
			summary.Missing += undeclared
		}
	}

	connections, err := s.opts.Resources.ModelConnections(ctx)
	if err != nil {
		return CredentialKeySummaryView{}, fmt.Errorf("统计模型连接凭据: %w", err)
	}
	for _, connection := range connections {
		addOwnerCredential(&summary, connection.CredentialMode, connection.CredentialState)
	}

	channels, err := s.opts.Notify.ListNotifyChannels(ctx)
	if err != nil {
		return CredentialKeySummaryView{}, fmt.Errorf("统计通知渠道凭据: %w", err)
	}
	for _, channel := range channels {
		addOwnerCredential(&summary, string(channel.CredentialMode), string(channel.CredentialState))
	}
	return summary, nil
}

// addOwnerCredential 计一个"整条资源一个凭据槽位"的来源（模型连接 / 通知渠道）。
// 空模式按 env 读（`channelCredentialMode` 的同一口径）；托管模式下只有 active
// 算 managed，revoked / 退役以外的一切非 active 都记 managed_revoked——"说得出
// 不可用"比"沉默地少算一格"诚实。
// addOwnerCredential counts one single-slot owner; an empty mode reads as env.
func addOwnerCredential(summary *CredentialKeySummaryView, mode, state string) {
	if core.CredentialMode(mode) != core.CredentialModeManaged {
		summary.Env++
		return
	}
	if core.CredentialIdentityState(state) == core.CredentialIdentityActive {
		summary.Managed++
		return
	}
	summary.ManagedRevoked++
}
