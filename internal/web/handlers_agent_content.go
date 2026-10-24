package web

// 本文件实现 Agent 配置内容的条件读写端点（颗粒度文档 §3.1.7 / 图 D）：
// `GET /agents/{name}/content` 给出原文与强 ETag；`PUT` 要求 `If-Match` 必须是
// 当前内容的强 ETag——缺失、弱比较（`W/`）、通配 `*`、形状非法或不匹配一律 412。
// handler 只做「静态解码 → 强 ETag 形状校验 → 委托 → 投影」：比较与写入必须在
// 适配器的同一把 Agent 写锁内完成（约束 25 / 27），本层**不得**自己读盘比较。
// 形状非法的 `If-Match` 在调用适配器之前就拒绝，保证这类请求连写锁都不进。
// This file implements the conditional content endpoints: GET publishes the raw
// text with its strong ETag, PUT requires If-Match to be the current strong ETag,
// and every precondition failure answers 412 without a disk write.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本层自产的固定文案：解析器与适配器的原文一律不外传（约束 45）。
// 412 的两种文案分别点名"补什么"与"下一步做什么"——契约 §9.5 允许 412 的 data
// 携带 current_etag，但失败信封在本仓固定不带 data（envelope.go 的唯一失败出口），
// 因此文案直接指向重新读取；这不改变"给出当前 ETag"的可操作性。
// Fixed texts raised by this layer; decoder and adapter wording never leave it.
const (
	// messageContentBody 是请求体不是合法 JSON / 含未知字段时的固定文案。
	messageContentBody = "请求体不是合法的 Agent 内容更新请求"
	// messageIfMatchRequired 点名缺失或形状非法的 If-Match 要改什么（含合法形状）。
	messageIfMatchRequired = `If-Match 必须是当前内容的强 ETag（形如 "sha256:<64 位小写十六进制>"）`
	// messageContentConflict 是形状合法但与磁盘当前值不等的固定文案。
	messageContentConflict = "内容已被其他写入修改：请重新读取当前内容与 ETag 后再保存"
	// messageEditUnsupported 是补丁无法安全保留原文时的兜底提示（契约 §8 要求
	// 说明改用整份原文）。
	messageEditUnsupported = "该补丁无法在保留注释与未改内容的前提下完成；请改用整份原文保存"
)

// contentUpdateBody 用指针区分「字段未给」与合法零值：`content` 与 `changes`
// 必须恰给一个（契约 AgentContentUpdate 的 oneOf）；`changes` 的每个子段同规，
// nil 表示不改，非 nil 空数组表示"改成空列表"。
// Pointer fields preserve the difference between "absent" and a legitimate zero.
type contentUpdateBody struct {
	Content *string      `json:"content"`
	Changes *changesBody `json:"changes"`
}

// changesBody 是前端 frontmatter 补丁的线上形状（snake_case，契约
// AgentContentChanges）：与 core.AgentContentChanges 分离是因为契约字段名与 Go
// 导出名不同，且 `schedules` 的元素允许附加字段（契约 additionalProperties:
// true），必须逐项宽松解析而不是整体 DisallowUnknownFields。
// changesBody is the wire shape of the frontmatter patch; schedules stay lenient
// because the contract allows additional properties on each item.
type changesBody struct {
	Description *string `json:"description"`
	// DisplayName 是展示名补丁（R6）：nil = 不改，空串 = 移除该行；长度与控制字符
	// 由补丁应用阶段判定（400 点名）。
	// DisplayName patches the display-only name; the patch stage judges the value.
	DisplayName *string `json:"display_name"`
	// Body 是围栏外正文的整段替换（契约 agent-md §2.3）：nil = 不改，非 nil 空串
	// = 清空；**不得**用「未给 body」推断清空。
	// Body replaces the whole body verbatim; a nil field leaves it untouched.
	Body       *string              `json:"body"`
	Identity   *identityChangesBody `json:"identity"`
	Provider   *providerChangesBody `json:"provider"`
	Tools      *[]string            `json:"tools"`
	Skills     *[]string            `json:"skills"`
	MCPServers *[]string            `json:"mcp_servers"`
	Bootstrap  *[]string            `json:"bootstrap"`
	// NotifyChannels 是通知渠道白名单补丁：nil = 不改，`[]` = 清空（不限制）。
	// NotifyChannels patches the allowlist; nil leaves it unchanged, [] clears it.
	NotifyChannels *[]string `json:"notify_channels"`
	// NotifyMode 是通知意图三态补丁（none / all_eligible / allowlist）；nil = 不改。
	// NotifyMode patches the three-state notification intent.
	NotifyMode *string `json:"notify_mode"`
	// Bindings 是 `resource_bindings` 的整块替换（契约 agent-md §2.3）：nil = 不改，
	// 非 nil（含空数组）= 以该数组为唯一内容。服务端不按名补绑、不重新解析 UID。
	// Bindings replaces the whole resource_bindings block; nil leaves it untouched.
	Bindings  *[]bindingChangeBody `json:"bindings"`
	Settings  *settingsChangesBody `json:"settings"`
	Schedules *[]json.RawMessage   `json:"schedules"`
}

// bindingChangeBody 是 `changes.bindings` 的一个元素（契约 agent-md §2.3）：
// 名称只是 AGENT.md 里的标签，UID 是不可变的资源身份，由客户端显式提交。
// bindingChangeBody is one wire element of changes.bindings.
type bindingChangeBody struct {
	ResourceKind string `json:"resource_kind"`
	RefName      string `json:"ref_name"`
	ResourceUID  string `json:"resource_uid"`
}

// identityChangesBody 是 identity 段补丁的线上形状。
// identityChangesBody is the wire shape of the identity patch.
type identityChangesBody struct {
	AgentName *string `json:"agent_name"`
	Prompt    *string `json:"prompt"`
}

// providerChangesBody 是 provider 段补丁的线上形状；温度是三态：省略 = 不修改，
// 数字 = 写入（含显式 0），`null` = 移除该字段（回到供应商缺省）。
// providerChangesBody is the wire shape of the provider patch; temperature is
// tri-state: omitted = unchanged, a number (including 0) = set, null = remove.
type providerChangesBody struct {
	Name        *string           `json:"name"`
	Model       *string           `json:"model"`
	Temperature optionalFloatBody `json:"temperature"`
}

// optionalFloatBody 区分“字段未出现”和“显式 null”：encoding/json 对 null 同样会
// 调用 UnmarshalJSON，因此只能用 Set 标记出现与否。只有出现且为 null 才表示清除；
// 未出现（Set=false）仍是“不修改”。
// optionalFloatBody distinguishes an absent field from an explicit null, so only a
// present null clears the value.
type optionalFloatBody struct {
	Set   bool
	Value *float64
}

func (o *optionalFloatBody) UnmarshalJSON(raw []byte) error {
	o.Set = true
	if string(raw) == "null" {
		o.Value = nil
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

// settingsChangesBody 是 settings 段补丁的线上形状。
// settingsChangesBody is the wire shape of the settings patch.
type settingsChangesBody struct {
	MaxIterations   *int `json:"max_iterations"`
	MaxHistoryTurns *int `json:"max_history_turns"`
}

// handleGetAgentContent 读取 AGENT.md 原文与强 ETag：ETag 同时写响应头与
// `data.etag`（契约 AgentContent）。只读观测不写管理操作记录（FR6），因此
// actor 不参与本端点——认证闸门已保证调用者身份合法。
// handleGetAgentContent returns the raw text plus its strong ETag in both the
// response header and the body; a read never writes a management operation row.
func (s *Server) handleGetAgentContent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdmin == nil {
		s.logger().Error("装配缺少 AgentAdmin 端口", "agent", name)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	content, err := s.opts.AgentAdmin.ReadContent(r.Context(), name)
	if err != nil {
		s.writeAgentContentError(w, err, name, "读取")
		return
	}
	w.Header().Set("ETag", content.ETag)
	s.writeOK(w, AgentContentView{Content: content.Content, ETag: content.ETag})
}

// handlePutAgentContent 执行一次条件写：解码 → If-Match 形状 → 委托适配器 →
// 投影 AgentFilesView。解码先于 If-Match（图 D 的 ① 在 ② 之前）：请求体本身
// 不合法时 400 优先于 412。成功后必须重新读一次内容才能给出新 ETag——契约的
// AgentFiles 不带 ETag 字段（核心类型形状由模块卡固定），而响应头与后续 GET
// 必须同口径（注 19）。
// handlePutAgentContent runs the conditional write; the success path re-reads the
// content so the new ETag in the header matches what the next GET will report.
func (s *Server) handlePutAgentContent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdmin == nil {
		s.logger().Error("装配缺少 AgentAdmin 端口", "agent", name)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes)
	update, err := decodeContentUpdate(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusBadRequest, codeBadRequest,
				fmt.Sprintf("请求体超过 %d 字节上限", s.opts.MaxRequestBytes))
			return
		}
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	// 形状校验在调用适配器之前完成：缺失 / 弱 / 通配 / 形状非法一律 412，且
	// 不触达写锁与磁盘（约束 25）。适配器仍会独立比较一次——那是比较与写入
	// 同一把写锁内的权威判定，本层只是早退。
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}

	files, err := s.opts.AgentAdmin.UpdateContent(r.Context(), name, update, ifMatch, actor)
	if err != nil {
		s.writeAgentContentError(w, err, name, "保存")
		return
	}

	fresh, err := s.opts.AgentAdmin.ReadContent(r.Context(), name)
	if err != nil {
		// 写入已经生效：这里的 500 只表示"新 ETag 没能读出"，操作行保持成功；
		// 客户端可用 GET 重新取 ETag 后继续（**不得**把已生效的写入说成失败）。
		s.logger().Error("内容写成功但新 ETag 读取失败", "agent", name, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	w.Header().Set("ETag", fresh.ETag)
	s.writeOK(w, agentFilesView(files, fresh.ETag))
}

// If-Match 的形状判据统一由 `etag.go` 的 parseIfMatch 提供（US-12 §3.1.8）：
// 单文件与集合 ETag 同语法、不同来源，不得各写一份解析。
// The If-Match shape rule lives in etag.go; this handler only calls it.

// decodeContentUpdate 解析内容写请求体：未知字段拒绝（管理写不接受拼错的键），
// `content` 与 `changes` 恰给一个；`changes.schedules` 的每一项按契约
// `additionalProperties: true` 宽松解析，其余子段仍拒绝未知字段。解析器原文
// 不外传，本层只回固定文案或点名字段的短句。
// decodeContentUpdate rejects unknown keys, enforces exactly one of content and
// changes, and parses schedule items leniently as the contract allows.
func decodeContentUpdate(r *http.Request) (core.AgentContentUpdate, error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body contentUpdateBody
	if err := dec.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return core.AgentContentUpdate{}, err
		}
		return core.AgentContentUpdate{}, errors.New(messageContentBody)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return core.AgentContentUpdate{}, err
		}
		return core.AgentContentUpdate{}, errors.New(messageContentBody)
	}

	switch {
	case body.Content != nil && body.Changes != nil:
		return core.AgentContentUpdate{}, errors.New("content 与 changes 必须恰给一个")
	case body.Content == nil && body.Changes == nil:
		return core.AgentContentUpdate{}, errors.New("必须提供 content 或 changes")
	}
	if body.Changes == nil {
		return core.AgentContentUpdate{Content: *body.Content}, nil
	}
	changes, err := body.Changes.toCore()
	if err != nil {
		return core.AgentContentUpdate{}, err
	}
	return core.AgentContentUpdate{Changes: &changes}, nil
}

// toCore 把线上补丁转换为领域补丁：未给的子段保持 nil（不改），非 nil 不丢字段。
// schedules 逐项要求 JSON 对象（契约的元素类型是 object），字段名按 frontmatter
// 的 id / cron / zone / message 对位，未知附加字段按契约忽略。
// toCore converts the wire patch into the domain patch; schedule items must be
// JSON objects, and their additional properties are ignored as the contract says.
func (b *changesBody) toCore() (core.AgentContentChanges, error) {
	out := core.AgentContentChanges{
		Description:    b.Description,
		DisplayName:    b.DisplayName,
		Body:           b.Body,
		Tools:          b.Tools,
		Skills:         b.Skills,
		MCPServers:     b.MCPServers,
		Bootstrap:      b.Bootstrap,
		NotifyChannels: b.NotifyChannels,
	}
	// notify_mode 只做字面量校验后原样下发：合法性最终仍由装载期规则 10 的候选
	// 校验兜底；这里早退只为给出与契约一致的三种取值提示。
	if b.NotifyMode != nil {
		mode, err := core.ParseNotifyMode(*b.NotifyMode)
		if err != nil {
			return core.AgentContentChanges{}, errors.New("notify_mode 必须是 none / all_eligible / allowlist 之一")
		}
		out.NotifyMode = &mode
	}
	// 绑定元素原样转成领域类型：类别与 UID 的严格解析在补丁应用阶段完成，
	// 非法值按绑定专用哨兵回 400（不在本层按名称补 UID）。
	if b.Bindings != nil {
		items := make([]core.BindingChange, 0, len(*b.Bindings))
		for _, item := range *b.Bindings {
			items = append(items, core.BindingChange{
				ResourceKind: core.BindingKind(item.ResourceKind),
				RefName:      item.RefName,
				ResourceUID:  core.ResourceUID(item.ResourceUID),
			})
		}
		out.Bindings = items
	}
	if b.Identity != nil {
		out.Identity = &core.AgentIdentityChanges{AgentName: b.Identity.AgentName, Prompt: b.Identity.Prompt}
	}
	if b.Provider != nil {
		out.Provider = &core.AgentProviderChanges{
			Name:  b.Provider.Name,
			Model: b.Provider.Model,
		}
		if b.Provider.Temperature.Set {
			out.Provider.Temperature = b.Provider.Temperature.Value
			out.Provider.ClearTemperature = b.Provider.Temperature.Value == nil
		}
	}
	if b.Settings != nil {
		out.Settings = &core.AgentSettingsChanges{
			MaxIterations:   b.Settings.MaxIterations,
			MaxHistoryTurns: b.Settings.MaxHistoryTurns,
		}
	}
	if b.Schedules != nil {
		items := make([]core.Schedule, 0, len(*b.Schedules))
		for i, raw := range *b.Schedules {
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				return core.AgentContentChanges{}, fmt.Errorf("schedules[%d] 必须是 JSON 对象", i)
			}
			var sched core.Schedule
			if err := json.Unmarshal(trimmed, &sched); err != nil {
				return core.AgentContentChanges{}, fmt.Errorf("schedules[%d] 的字段类型不合法", i)
			}
			items = append(items, sched)
		}
		out.Schedules = &items
	}
	return out, nil
}

// writeAgentContentError 把适配器失败按类型映射为固定响应（**不**比对文本，
// EngineeringStandards §1.4）：
//   - *core.AgentNotFoundError / fs.ErrNotExist → 404（点名查不到的名字）
//   - *core.InvalidInputError → 400 BAD_REQUEST（Reason 已由适配器点名且不含路径）
//   - 绑定 / 凭据专用码 → `writeBindingError` / `writeCredentialError`（保存期 400 / 409 / 503）
//   - core.ErrEditUnsupported → 400 EDIT_UNSUPPORTED（去掉哨兵前缀 + 改用整份原文）
//   - core.ErrPreconditionFailed → 412（磁盘零写入、未写管理操作行）
//   - 其余（快照 / 写盘 / 预写失败）→ 500 INTERNAL，底层原因只进日志
//
// writeAgentContentError maps adapter failures by type only; anything else is a
// fixed 500 whose cause stays in the log.
func (s *Server) writeAgentContentError(w http.ResponseWriter, err error, agent, action string) {
	var notFoundErr *core.AgentNotFoundError
	if errors.As(err, &notFoundErr) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+notFoundErr.Name)
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+agent)
		return
	}
	var invalidErr *core.InvalidInputError
	if errors.As(err, &invalidErr) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason)
		return
	}
	// 提交成功后的绑定重建 / 凭据校验失败：专用码排在通用码之前，客户端才能看到
	// 「该修哪一处」。保存面口径与 `writeAgentAdminError` 完全一致。
	if s.writeBindingError(w, err) {
		return
	}
	if s.writeCredentialError(w, err) {
		return
	}
	// 候选定义的装载期拒绝：按类型化分类出中文（SC-033），英文诊断只进日志。
	// A typed candidate rejection maps to Chinese; the English text stays in the log.
	var rejected *core.AgentDefinitionRejectedError
	if errors.As(err, &rejected) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, agentRejectMessage(rejected.Kind, rejected.Detail))
		return
	}
	if errors.Is(err, core.ErrEditUnsupported) {
		s.writeError(w, http.StatusBadRequest, codeEditUnsupported, editUnsupportedMessage(err))
		return
	}
	if errors.Is(err, core.ErrPreconditionFailed) {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageContentConflict)
		return
	}
	s.logger().Error("Agent 内容"+action+"失败", "agent", agent, "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// editUnsupportedMessage 把 EDIT_UNSUPPORTED 的适配器原因整理为对外文案：普通包装
// 错误先去掉哨兵前缀（`AdminError` 的 `Error()` 已是完整文案，前缀天然不命中），
// 再追加"改用整份原文"的下一步；没有原因时用本层兜底。资源写面（块编辑）与 Agent
// 内容写面共用这一出口，**不**各自拼一份。
// editUnsupportedMessage strips the plain sentinel prefix and appends the documented
// next step; both the content and resource write surfaces share this one exit.
func editUnsupportedMessage(err error) string {
	reason := strings.TrimPrefix(err.Error(), core.ErrEditUnsupported.Error()+": ")
	if reason == core.ErrEditUnsupported.Error() {
		reason = ""
	}
	if reason == "" {
		return messageEditUnsupported
	}
	return reason + "；请改用整份原文保存"
}
