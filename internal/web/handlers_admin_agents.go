package web

// 本文件实现 Agent 生命周期的五个写面端点（颗粒度文档 §3.1.8 / §3.7.1）：
// `POST /agents`、`POST /agents/{name}/generate-files`、`GET` / `POST /agents/{name}/files`
// 与 `POST /agents/{name}/copy`。handler 只做四步——静态解码 → `If-Match` 形状校验 →
// 委托端口 → 投影视图；比较、落盘、管理操作行与回滚全部留在适配器（app 层）内，
// 本层**不得**自己读盘、补写第二套编排或做乐观更新。
// 多文件提交的 `If-Match` 原样传给端口：比较必须在适配器的同一把 Agent 写锁内完成
// （约束 28），本层不归一化、不弱比较。
// This file implements the five agent-lifecycle write endpoints. Handlers decode,
// check the If-Match shape, delegate, and project; comparison, disk writes, operation
// rows and rollback stay in the adapter, and If-Match is forwarded verbatim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// 本层自产的固定文案：解码器与适配器的原文一律不外传（约束 43）。
// Fixed public texts raised by this layer; decoder/adapter wording never leaves it.
const (
	// messageAgentCreateBody 是创建请求体不是合法 JSON / 含未知字段时的固定文案。
	messageAgentCreateBody = "请求体不是合法的 Agent 创建请求"
	// messageAgentGenerateBody 是草稿请求体不合法时的固定文案。
	messageAgentGenerateBody = "请求体不是合法的 Agent 草稿请求"
	// messageAgentScheduleBody 是定时候选请求体不合法时的固定文案。
	messageAgentScheduleBody = "请求体不是合法的 Agent 定时候选请求"
	// messageAgentCopyBody 是复制请求体不合法时的固定文案。
	messageAgentCopyBody = "请求体不是合法的 Agent 复制请求"
	// messageAgentFilesBody 是多文件提交请求体不合法时的固定文案。
	messageAgentFilesBody = "请求体不是合法的 Agent 文件提交请求"
	// messageAgentDraftDescription 点明草稿端点缺的是唯一一句描述（契约 required）。
	messageAgentDraftDescription = "description 必填：请用一句话描述要创建的 Agent"
	// messageAgentFilesRequired 点明多文件提交必须携带文件集合（契约 required）。
	messageAgentFilesRequired = "files 必填：至少提交包含 AGENT.md 的文件集合"
	// messageAgentFilesConflict 是集合 ETag 已过期时的固定文案：指向重新读取。
	messageAgentFilesConflict = "文件集合已被其他写入修改：请重新读取文件集合与 ETag 后再保存"
	// messageAgentConflict 是目标已存在 / 状态冲突且适配器未给文案时的兜底。
	messageAgentConflict = "目标已存在或与当前状态冲突"
	// messageAgentBusy 是 Agent 仍有未退出运行时的固定提示；run_id 由 data.run_ids 点名。
	messageAgentBusy = "Agent 仍有未退出的运行：请稍后重试"
	// messageAgentCopyBlocked 是复制遇源专属引用且未给映射时的兜底文案；适配器的
	// 文案会点名资源数量，`data.problems` 逐条列出类别与名称。
	messageAgentCopyBlocked = "源 Agent 引用了源专属资源：请用 resource_mapping 显式指定替代的共享资源"
	// messageAuthorNotConfigured 按契约点名配置键与配置位置（503）。
	messageAuthorNotConfigured = "作者模型未配置：请在 basalt.yaml 配置 author.provider 与 author.model"
	// messageAdminTimeout 是请求截止时间已过时的固定文案（504）。
	messageAdminTimeout = "请求超时：请稍后重试"
	// messageBindingMissing 是保存期缺绑定的兜底文案：不按名补绑，点名交由适配器。
	messageBindingMissing = "AGENT.md 引用的资源缺少对应绑定：请在 resource_bindings 中显式声明"
	// messageBindingInvalid 是绑定结构非法（UID 与当前指针不符 / 重复名 / 孤儿）的兜底。
	messageBindingInvalid = "resource_bindings 结构非法或已过期：请重新选择资源"
	// messageBindingUnavailable 是被绑定资源不存在 / 已删除 / 未发布的兜底文案。
	messageBindingUnavailable = "绑定的资源不存在、已删除或没有已发布版本：请重新选择"
	// messageNotEligible 是绑定资源对该 Agent 无资格时的兜底文案。
	messageNotEligible = "绑定的资源不属于当前 Agent 的可用范围：请改用共享资源或本人专属资源"
	// messageCredentialRevoked 是绑定凭据已撤销时的兜底文案：指向重新录入新值。
	// #nosec G101 -- 固定用户文案，值不是凭据（变量名含 credential 命中的误报）。
	messageCredentialRevoked = "绑定的凭据已撤销，请重新录入新值"
	// messageCredentialUnavailable 是主密钥不可用时的兜底文案（503）。
	// #nosec G101 -- 固定用户文案，值不是凭据（变量名含 credential 命中的误报）。
	messageCredentialUnavailable = "托管凭据在当前主密钥下不可用"
)

// handleAdminCreateAgent 处理 `POST /agents`：模板创建默认停用，响应是落盘后的真值
// （含集合 ETag 与生命周期摘要）。契约只在 files 端点定义 `ETag` 响应头，这里只把
// ETag 放进 `data.etag`，**不**新增契约外响应头。
// handleAdminCreateAgent answers the post-create truth; the ETag travels in data only.
func (s *Server) handleAdminCreateAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /agents")
		return
	}
	var body AgentCreateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageAgentCreateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	set, err := s.opts.AgentAdminService.Create(r.Context(), actor, core.AgentSpec{
		Name:        body.Name,
		Description: body.Description,
		Model:       body.Model,
	})
	if err != nil {
		s.writeAgentAdminError(w, err, body.Name, "创建")
		return
	}
	s.writeOK(w, agentFilesView(set.Files, set.ETag))
}

// handleAdminGenerateAgentFiles 处理 `POST /agents/{name}/generate-files`：一次作者
// 模型调用，返回内存草稿；**不**写管理操作行、不落盘、不注册（约束 1 的唯一例外）。
// 未配置作者模型由适配器返回 `core.ErrProviderNotConfigured`，这里映射 503。
// The draft endpoint has no management operation row and never touches disk.
func (s *Server) handleAdminGenerateAgentFiles(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /agents/{name}/generate-files")
		return
	}
	var body AgentGenerateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageAgentGenerateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	// 契约把 description 列为 required；JSON 解码拿不到"键是否存在"的元数据，
	// 空串同时覆盖缺失与空值——两者都生成不了有意义的草稿，统一 400 点名。
	// The contract marks description required; an empty string covers both an absent
	// key and an empty value, and neither can produce a meaningful draft.
	if body.Description == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageAgentDraftDescription)
		return
	}
	answers := make([]core.AuthorAnswer, len(body.Answers))
	for i, answer := range body.Answers {
		answers[i] = core.AuthorAnswer{ID: answer.ID, Question: answer.Question, Answer: answer.Answer}
	}
	result, err := s.opts.AgentAdminService.GenerateFiles(r.Context(), name, body.Description, answers)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "生成草稿")
		return
	}
	s.writeOK(w, agentDraftView(result))
}

// handleAdminScheduleDraft 处理 `POST /agents/{name}/schedule-draft`（R6.4）：一次作者
// 模型调用生成**未保存**的定时候选；未来三次触发时间由适配器在服务端计算，本层只
// 投影。指令空 / 超限 / 候选非法由适配器按 400 点名，作者未配置 503，调用失败 500。
// The schedule-draft handler returns an unsaved candidate plus server-computed triggers.
func (s *Server) handleAdminScheduleDraft(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /agents/{name}/schedule-draft")
		return
	}
	var body ScheduleDraftWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageAgentScheduleBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	result, err := s.opts.AgentAdminService.ScheduleDraft(r.Context(), name, body.Instruction)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "生成定时候选")
		return
	}
	s.writeOK(w, scheduleDraftView(result))
}

// handleAdminAgentFiles 处理 `GET /agents/{name}/files`：受管文件清单 + 集合 ETag，
// 响应头与 `data.etag` 逐字一致（契约 AgentFilesView）。只读，不写管理操作行。
// handleAdminAgentFiles publishes the managed file set with the same ETag in the
// header and in data.
func (s *Server) handleAdminAgentFiles(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "GET /agents/{name}/files")
		return
	}
	set, err := s.opts.AgentAdminService.Files(r.Context(), name)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "读取文件集合")
		return
	}
	w.Header().Set("ETag", set.ETag)
	s.writeOK(w, agentFilesView(set.Files, set.ETag))
}

// handleAdminSubmitAgentFiles 处理 `POST /agents/{name}/files`：解码 → `If-Match`
// 形状 → 原样委托。形状非法的请求在进写锁之前就 412；形状合法但值过期由适配器在
// 锁内比较后返回 `core.ErrPreconditionFailed`，这里映射 412 且不重试、不自动修复
// （零写入、零操作行由适配器保证）。
// handleAdminSubmitAgentFiles forwards If-Match verbatim; stale values are judged
// inside the adapter's write lock, and this layer never retries.
func (s *Server) handleAdminSubmitAgentFiles(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /agents/{name}/files")
		return
	}
	var body AgentFilesWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageAgentFilesBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	// nil 与空数组区分：`null` / 缺键 = 没提交文件；`[]` 会由适配器按缺 AGENT.md
	// 拒绝并点名——本层不替适配器做内容级判断。
	// nil (absent or null) is rejected here; an empty array reaches the adapter, which
	// names the missing AGENT.md.
	if body.Files == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageAgentFilesRequired)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	files := make([]core.AuthorFile, len(body.Files))
	for i, file := range body.Files {
		files[i] = core.AuthorFile{Path: file.Path, Content: file.Content}
	}
	set, err := s.opts.AgentAdminService.SubmitFiles(r.Context(), actor, name, ifMatch, files)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "提交文件")
		return
	}
	w.Header().Set("ETag", set.ETag)
	s.writeOK(w, agentFilesView(set.Files, set.ETag))
}

// handleAdminCopyAgent 处理 `POST /agents/{name}/copy`：源必须是活动区 Agent，目标名
// 由适配器校验；改名、清空定时与停用 Bootstrap 都是适配器的编排，本层只解码与投影。
// The copy handler decodes and projects; target validation and the disabled bootstrap
// stay in the adapter.
func (s *Server) handleAdminCopyAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /agents/{name}/copy")
		return
	}
	var body AgentCopyWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageAgentCopyBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	mapping, mapErr := parseAgentCopyMapping(body.ResourceMapping)
	if mapErr != nil {
		s.writeAgentAdminError(w, mapErr, name, "复制")
		return
	}
	set, err := s.opts.AgentAdminService.Copy(r.Context(), actor, name, body.NewName, mapping)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "复制")
		return
	}
	s.writeOK(w, agentFilesView(set.Files, set.ETag))
}

// parseAgentCopyMapping 把线上 `resource_mapping[]` 解析成领域映射：类别字面量未知、
// 任一 UID 为空都按 400 拒绝（纯请求校验，发生在任何操作行之前）。UID **形状**与
// 存在性、资格、不得沿用源 UID 的判定留给适配器——本层不读数据库，也不按名称猜。
// parseAgentCopyMapping rejects malformed entries as 400 before any operation row;
// uid shape, existence, eligibility and the no-reuse rule stay in the adapter.
func parseAgentCopyMapping(entries []AgentCopyMappingWrite) ([]core.ResourceMapping, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]core.ResourceMapping, 0, len(entries))
	for i, entry := range entries {
		kind, err := core.ParseBindingKind(entry.ResourceKind)
		if err != nil {
			return nil, &core.InvalidInputError{
				Tool:   core.ActionAgentCopy,
				Reason: fmt.Sprintf("resource_mapping[%d]：resource_kind 只接受 model_connection / notify_channel", i),
			}
		}
		if entry.SourceResourceUID == "" || entry.TargetResourceUID == "" {
			return nil, &core.InvalidInputError{
				Tool:   core.ActionAgentCopy,
				Reason: fmt.Sprintf("resource_mapping[%d]：source_resource_uid 与 target_resource_uid 都必须给出", i),
			}
		}
		out = append(out, core.ResourceMapping{
			Kind:      kind,
			SourceUID: core.ResourceUID(entry.SourceResourceUID),
			TargetUID: core.ResourceUID(entry.TargetResourceUID),
		})
	}
	return out, nil
}

// decodeAdminWriteBody 解析写面请求体：包上请求体上限、拒绝未知字段（管理写不接受
// 拼错的键）、解码一次后要求读到 EOF（第二份 JSON 或垃圾 → 400）。超限与解析失败
// 都返回**本层自产**的固定文案；`message` 按端点区分，便于页面定位。
// decodeAdminWriteBody bounds the body, rejects unknown fields and trailing content,
// and only ever returns this layer's fixed texts.
func (s *Server) decodeAdminWriteBody(w http.ResponseWriter, r *http.Request, dst any, message string) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("请求体超过 %d 字节上限", s.opts.MaxRequestBytes)
		}
		return errors.New(message)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("请求体超过 %d 字节上限", s.opts.MaxRequestBytes)
		}
		return errors.New(message)
	}
	return nil
}

// writeAdminPortMissing 是装配缺失的唯一出口：缺端口是装配错误，不是空结果，也不是
// 客户端的错——记 ERROR 并回固定 500（与 US-11 只读面的缺端口口径一致）。
// A missing write port is an assembly error: ERROR log plus a fixed 500.
func (s *Server) writeAdminPortMissing(w http.ResponseWriter, port, endpoint string) {
	s.logger().Error("装配缺少写面端口", "port", port, "endpoint", endpoint)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// writeAgentAdminError 把 Agent 写面失败按类型映射为固定响应（约束 43；只认
// errors.Is / errors.As，**不**比对文本）：
//   - *core.AgentNotFoundError / fs.ErrNotExist / core.ErrResourceNotFound → 404
//     NOT_FOUND（点名 Agent；恢复端点对归档标识另有更精确的分支）
//   - *core.InvalidInputError → 400 BAD_REQUEST（Reason 已由适配器点名且不含路径）
//   - core.ErrProviderNotConfigured → 503 NOT_CONFIGURED（点名 author.provider / model）
//   - core.ErrPreconditionFailed → 412 PRECONDITION_FAILED（零写入、零操作行）
//   - core.ErrBusy → 409 AGENT_BUSY + `data.run_ids`（契约点名的活动运行）
//   - core.ErrConflict → 409 CONFLICT（目标已存在 / 归档标识占用）
//   - context.DeadlineExceeded → 504 TIMEOUT
//   - 其余（含作者上游调用失败）→ 500 INTERNAL，底层原因只进日志
//
// 每个分支都经 `writeAdminDataError` 出站：`pending` 之后的失败带 `operation_id`
// （约束 1a），白名单没有字段时它与 `writeError` 完全等价。
// writeAgentAdminError maps adapter failures by type only; every branch goes through
// writeAdminDataError so a post-pending operation id reaches the client.
func (s *Server) writeAgentAdminError(w http.ResponseWriter, err error, agent, action string) {
	var notFoundErr *core.AgentNotFoundError
	if errors.As(err, &notFoundErr) {
		s.writeAdminDataError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+notFoundErr.Name, err)
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		s.writeAdminDataError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+agent, err)
		return
	}
	// 归档记录缺失等资源级 404：用调用方传入的标识点名（恢复端点在此分支之前
	// 已按归档标识给出更精确的文案）。
	// Resource-level misses are named by the identifier the caller passed in.
	if errors.Is(err, core.ErrResourceNotFound) {
		s.writeAdminDataError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+agent, err)
		return
	}
	var invalidErr *core.InvalidInputError
	if errors.As(err, &invalidErr) {
		s.writeAdminDataError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason, err)
		return
	}
	// 绑定与凭据专用码排在通用码之前（保存期口径）：同一个 400 / 409，码决定该改什么。
	// Binding and credential sentinels win over the generic codes on the save path.
	if s.writeBindingError(w, err) {
		return
	}
	if s.writeCredentialError(w, err) {
		return
	}
	// 多文件提交的候选装载期拒绝：按类型化分类出中文（SC-033），英文只进日志。
	// A typed candidate rejection maps to Chinese; the English text stays in the log.
	var rejected *core.AgentDefinitionRejectedError
	if errors.As(err, &rejected) {
		s.writeAdminDataError(w, http.StatusBadRequest, codeBadRequest,
			agentRejectMessage(rejected.Kind, rejected.Detail), err)
		return
	}
	if errors.Is(err, core.ErrProviderNotConfigured) {
		s.writeAdminDataError(w, http.StatusServiceUnavailable, codeNotConfigured,
			adminMessageOr(err, messageAuthorNotConfigured), err)
		return
	}
	// 作者模型调用已发出但未得到可用结果（R7 问题 8）：码仍是 INTERNAL，文案点名
	// provider 与修复入口（模型连接 / llm_calls 审计），不得退回"服务器内部错误"。
	// The call was made but produced no usable result: keep INTERNAL, surface the hint.
	if errors.Is(err, core.ErrAuthorModelUnavailable) {
		s.writeAdminDataError(w, http.StatusInternalServerError, codeInternal,
			adminMessageOr(err, messageInternal), err)
		return
	}
	if errors.Is(err, core.ErrPreconditionFailed) {
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			messageAgentFilesConflict, err)
		return
	}
	if errors.Is(err, core.ErrBusy) {
		s.writeAdminDataError(w, http.StatusConflict, codeAgentBusy,
			adminMessageOr(err, messageAgentBusy), err)
		return
	}
	// 源专属引用未映射：409 且 `data.problems` 列出待映射的资源（契约 §8 / FR-070）；
	// 必须排在通用 ErrConflict 之前——两者都是 409，但码与文案不同。
	// The blocked-copy branch must precede the generic conflict branch.
	if errors.Is(err, core.ErrAgentCopyBlocked) {
		s.writeAdminDataError(w, http.StatusConflict, codeAgentCopyBlocked,
			adminMessageOr(err, messageAgentCopyBlocked), err)
		return
	}
	if errors.Is(err, core.ErrConflict) {
		s.writeAdminDataError(w, http.StatusConflict, codeConflict,
			adminMessageOr(err, messageAgentConflict), err)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.writeAdminDataError(w, http.StatusGatewayTimeout, codeTimeout,
			adminMessageOr(err, messageAdminTimeout), err)
		return
	}
	s.logger().Error("Agent 管理写失败", "agent", agent, "action", action, "error", err)
	s.writeAdminDataError(w, http.StatusInternalServerError, codeInternal, messageInternal, err)
}

// adminMessageOr 取 `core.AdminError` 的对外文案；没有文案时退回本层固定的兜底。
// adminMessageOr prefers the adapter's public wording over a local fallback.
func adminMessageOr(err error, fallback string) string {
	var adminErr *core.AdminError
	if errors.As(err, &adminErr) && adminErr.Message != "" {
		return adminErr.Message
	}
	return fallback
}

// writeAdminDataError 组装带 `data` 的失败信封：`data` 只经 `errorDataView` 的
// 固定白名单投影（agents / active_runs / run_ids / problems / current_etag /
// operation_id），没有白名单字段时退回不带 data 的 `writeError`——**不**另开信封
// 序列化出口。
// writeAdminDataError emits a failure envelope whose data only passes the fixed
// whitelist; without whitelisted keys it falls back to writeError.
func (s *Server) writeAdminDataError(w http.ResponseWriter, status int, code, message string, err error) {
	data, ok := errorDataView(err)
	if !ok {
		s.writeError(w, status, code, message)
		return
	}
	s.writeEnvelope(w, status, Envelope{
		Code:      code,
		Message:   message,
		Data:      data,
		Timestamp: s.timestamp(),
	})
}
