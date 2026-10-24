package web

// 本文件实现公共资源写面的模型连接端点与版本 / 发布端点（颗粒度文档 §3.1.8 /
// §3.7.1）：`POST /model-connections`、`PUT /model-connections/{name}`、
// `POST /model-connections/{name}/test`、`GET /resources/{kind}/{id}/versions` 与
// `POST /resources/{kind}/{id}/publish`。handler 只做四步——解码 → 路径与
// `If-Match` 形状校验 → 委托端口 → 投影视图；块编辑、候选校验、锁序、版本事务、
// 发布指针与回滚全部在适配器内，本层**不得**读盘、补写第二套编排或做乐观更新。
// 删除（T041）、引用（T041）、版本详情（T042）、MCP 端点（T048）与技能端点（T052）
// 也在同一文件落地，共用这里的错误映射与投影出口。
// This file implements the model-connection, MCP, skill, version/publish, delete,
// reference and version-detail endpoints. Handlers decode, validate the path and
// If-Match shape, delegate and project; every write decision stays in the adapter.

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

// 本层自产的固定文案：解码器与适配器的原文一律不外传（约束 43）。可复用
// `handlers_admin_agents.go` 的 `messageAdminTimeout` / `messageInternal`、
// `handlers_agent_content.go` 的 `messageIfMatchRequired` / `messageEditUnsupported`
// 与 `admin_routes.go` 的 `requireIfMatch`，不在这里重复定义。
// Fixed public texts raised by this layer; decoder/adapter wording never leaves it.
const (
	// messageModelConnectionBody 是模型连接请求体不是合法 JSON / 含未知字段时的文案。
	messageModelConnectionBody = "请求体不是合法的模型连接请求"
	// messageMCPServerBody 是 MCP server 请求体不是合法 JSON / 含未知字段时的文案。
	messageMCPServerBody = "请求体不是合法的 MCP server 请求"
	// messageSkillCreateBody 是技能创建请求体不是合法 JSON / 含未知字段时的文案。
	messageSkillCreateBody = "请求体不是合法的技能创建请求"
	// messageSkillImportBody 是技能导入请求体不是合法 JSON / 含未知字段时的文案。
	messageSkillImportBody = "请求体不是合法的技能导入请求"
	// messageSkillUpdateBody 是技能更新 / 校验请求体不合法时的固定文案。
	messageSkillUpdateBody = "请求体不是合法的技能更新请求"
	// messagePublishBody 是发布请求体不合法时的固定文案。
	messagePublishBody = "请求体不是合法的发布请求"
	// messageResourceNameMismatch 点明路径资源名与请求体 name 冲突（条件修改）。
	messageResourceNameMismatch = "路径中的资源名与请求体 name 不一致"
	// messageResourceKind 是未知资源类型的固定文案：只接受三个冻结字面量。
	messageResourceKind = "未知资源类型：只接受 model_connection / mcp_server / skill"
	// messageResourceNotFoundPrefix 是 404 的固定前缀，冒号后只接资源标识。
	messageResourceNotFoundPrefix = "未找到资源："
	// messageResourceConflict 是 412 的固定文案：指向重新读取版本与 ETag。
	messageResourceConflict = "资源已被其他写入修改：请重新读取版本与 ETag 后再试"
	// messageResourceConflictTarget 是 409 在适配器未给文案时的兜底。
	messageResourceConflictTarget = "目标已存在或与当前状态冲突"
	// messageResourceUIDMismatch 是 RESOURCE_UID_MISMATCH 的兜底文案：指向重新读取
	// 当前身份，不按名称猜目标。
	messageResourceUIDMismatch = "请求的 resource_uid 与当前资源身份不一致：请重新读取资源后重试"
	// messageResourceNotConfigured 是 503 在适配器未给文案时的兜底：不点值只点变量。
	messageResourceNotConfigured = "模型连接引用的凭证未配置：请检查对应环境变量"
)

// handleAdminCreateModelConnection 处理 `POST /model-connections`：创建即发布，
// 响应是适配器在提交段之后给出的视图（发布与生效版本同一）。空 ifMatch 传端口，
// 表示"创建"而不是条件修改——**不**在这里替适配器决定新旧语义。
// handleAdminCreateModelConnection forwards an empty ifMatch, which the port reads as
// the create path.
func (s *Server) handleAdminCreateModelConnection(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /model-connections")
		return
	}
	var body ModelConnectionWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageModelConnectionBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if s.rejectCredentialRef(w, body.CredentialRef) {
		return
	}
	view, err := s.opts.ResourceAdminService.UpsertModelConnection(r.Context(), actor, body, "")
	if err != nil {
		s.writeResourceAdminError(w, err, body.Name, "新建模型连接")
		return
	}
	s.writeOK(w, view)
}

// handleAdminUpdateModelConnection 处理 `PUT /model-connections/{name}`：路径是目标
// 资源的唯一来源，请求体 `name` 留空或缺省时按路径补齐，非空且不一致时 400
// （纯请求校验，早于任何写动作）。`If-Match` 原样交给适配器，在写锁内与当前发布
// 版本比较；本层**不**归一化、**不**重试、**不**自动修复。
// handleAdminUpdateModelConnection takes the path as the single resource identity and
// forwards If-Match verbatim; stale values are judged inside the adapter's write lock.
func (s *Server) handleAdminUpdateModelConnection(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "PUT /model-connections/{name}")
		return
	}
	var body ModelConnectionWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageModelConnectionBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if s.rejectCredentialRef(w, body.CredentialRef) {
		return
	}
	if body.Name != "" && body.Name != name {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageResourceNameMismatch)
		return
	}
	body.Name = name
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	view, err := s.opts.ResourceAdminService.UpsertModelConnection(r.Context(), actor, body, ifMatch)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "修改模型连接")
		return
	}
	s.writeOK(w, view)
}

// handleAdminDeleteModelConnection 处理 `DELETE /model-connections/{name}`：删除的
// 引用闸门、块移除、候选映射重建与事务都在适配器内（图 G）；本层只把路径名与
// `If-Match` 原样传入，成功回 200 + 空 data。
// handleAdminDeleteModelConnection forwards the path name and the literal If-Match;
// the reference gate, block removal and transaction stay in the adapter.
func (s *Server) handleAdminDeleteModelConnection(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.handleAdminDeleteResource(w, r, actor, core.ResourceModelConnection, "DELETE /model-connections/{name}", "删除模型连接")
}

// handleAdminDeleteMCPServer 处理 `DELETE /mcp-servers/{name}`：与模型连接共用同一
// 删除流程，只有资源类型不同。
// handleAdminDeleteMCPServer shares the delete flow with model connections.
func (s *Server) handleAdminDeleteMCPServer(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.handleAdminDeleteResource(w, r, actor, core.ResourceMCPServer, "DELETE /mcp-servers/{name}", "删除 MCP server")
}

// handleAdminDeleteSkill 处理 `DELETE /skills/{name}`：与模型连接共用同一删除流程，
// 引用闸门扫描的是 Agent `skills:` 声明。
// handleAdminDeleteSkill shares the delete flow; the gate scans Agent skills declarations.
func (s *Server) handleAdminDeleteSkill(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.handleAdminDeleteResource(w, r, actor, core.ResourceSkill, "DELETE /skills/{name}", "删除技能")
}

// handleAdminDeleteResource 是三类资源共用的删除出口：`If-Match` 在本层已由
// `requireIfMatch` 判过形状，这里按请求头重读原值传端口（服务层在写锁内与当前
// 发布版本比较）。`resource_uid` 是本层唯一读取的身份参数，原样传端口——缺失时
// 由服务层回 409 并带上**当前**身份（本层拿不到指针，不能替它填值）。
// 引用闸门的 400 由 `writeResourceAdminError` 携带 `data.agents` /
// `data.active_runs` 投影；本层**不**先查引用、**不**重试。
// handleAdminDeleteResource is the shared delete exit; the 400 with data.agents and
// data.active_runs is projected from the adapter's typed failure.
func (s *Server) handleAdminDeleteResource(w http.ResponseWriter, r *http.Request, actor core.Actor, kind core.ResourceKind, endpoint, action string) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", endpoint)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	resourceUID := r.URL.Query().Get("resource_uid")
	if err := s.opts.ResourceAdminService.Delete(r.Context(), actor, kind, name, ifMatch, resourceUID); err != nil {
		s.writeResourceAdminError(w, err, name, action)
		return
	}
	s.writeOK(w, nil)
}

// handleAdminTestModelConnection 处理 `POST /model-connections/{name}/test`：按已
// 发布版本发一次最小协议调用，结果脱敏后投影；**不**创建 Run、**不**换运行时映射。
// 超时（504）与凭证缺失（503）由适配器按类型给出，连接失败仍返回 200 + `ok=false`
// ——"调用确实发生过"与"结论是失败"是两件事。
// handleAdminTestModelConnection projects the redacted result; a completed call with an
// error stays 200 with ok=false, while timeout and missing credentials are errors.
func (s *Server) handleAdminTestModelConnection(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /model-connections/{name}/test")
		return
	}
	result, err := s.opts.ResourceAdminService.TestModelConnection(r.Context(), actor, name)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "测试模型连接")
		return
	}
	s.writeOK(w, modelConnectionTestResultView(result))
}

// handleAdminCreateMCPServer 处理 `POST /mcp-servers`：创建即发布，空 ifMatch 传端口
// 表示"创建"而不是条件修改。候选校验（传输分支、args 与 command 分离、env 精确占位）
// 全在适配器内，本层只解码与投影。
// handleAdminCreateMCPServer forwards an empty ifMatch, which the port reads as the
// create path; the transport-specific candidate validation stays in the adapter.
func (s *Server) handleAdminCreateMCPServer(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /mcp-servers")
		return
	}
	var body MCPServerWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageMCPServerBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	view, err := s.opts.ResourceAdminService.UpsertMCPServer(r.Context(), actor, body, "")
	if err != nil {
		s.writeResourceAdminError(w, err, body.Name, "新建 MCP server")
		return
	}
	s.writeOK(w, view)
}

// handleAdminUpdateMCPServer 处理 `PUT /mcp-servers/{name}`：路径是唯一目标，请求体
// `name` 留空按路径补齐，非空且不一致 → 400（纯请求校验，早于任何写动作）。`If-Match`
// 原样交给适配器，在写锁内与当前发布版本比较；本层不归一化、不重试。
// handleAdminUpdateMCPServer takes the path as the single identity and forwards
// If-Match verbatim; the stale check runs inside the adapter's write lock.
func (s *Server) handleAdminUpdateMCPServer(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "PUT /mcp-servers/{name}")
		return
	}
	var body MCPServerWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageMCPServerBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Name != "" && body.Name != name {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageResourceNameMismatch)
		return
	}
	body.Name = name
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	view, err := s.opts.ResourceAdminService.UpsertMCPServer(r.Context(), actor, body, ifMatch)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "修改 MCP server")
		return
	}
	s.writeOK(w, view)
}

// handleAdminTestMCPServer 处理 `POST /mcp-servers/{name}/test`：按已发布版本建立一次
// 临时连接并返回工具发现结果；超时（504）与策略拒绝的区分由适配器给出——策略拒绝是
// "调用被拒绝"的确定结论，仍返回 200 + `ok=false`，与模型连接测试同一口径。
// handleAdminTestMCPServer projects the redacted result; a policy denial stays 200 with
// ok=false while a timeout is an error.
func (s *Server) handleAdminTestMCPServer(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /mcp-servers/{name}/test")
		return
	}
	result, err := s.opts.ResourceAdminService.TestMCPServer(r.Context(), actor, name)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "测试 MCP server")
		return
	}
	s.writeOK(w, mcpServerTestResultView(result))
}

// handleAdminCreateSkill 处理 `POST /skills`：创建骨架并发布 version 1。响应按契约
// 只回 `SkillView`（`openapi.yaml` 的 data 是 SkillView，文件清单只在详情端点给）；
// 名字规则、骨架正文与同名 409 都在适配器内判定，本层不落盘、不补写编排。
// handleAdminCreateSkill projects the SkillView the contract declares for POST /skills;
// the skeleton body and the duplicate-name conflict stay in the adapter.
func (s *Server) handleAdminCreateSkill(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /skills")
		return
	}
	var body SkillCreateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageSkillCreateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	view, err := s.opts.ResourceAdminService.CreateSkill(r.Context(), actor, body)
	if err != nil {
		s.writeResourceAdminError(w, err, body.Name, "新建技能")
		return
	}
	s.writeOK(w, view.SkillView)
}

// handleAdminImportSkill 处理 `POST /skills/import`：浏览器目录读取后的文本文件
// 集合，导入边界（越界 / 绝对路径 / 隐藏目录 / 软链接 / 非 UTF-8 / 超限）与同名
// 409 全在适配器内；响应按契约只回 `SkillView`。
// handleAdminImportSkill projects the contract's SkillView; the import boundary and the
// duplicate-name conflict stay in the adapter.
func (s *Server) handleAdminImportSkill(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /skills/import")
		return
	}
	var body SkillImportWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageSkillImportBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	view, err := s.opts.ResourceAdminService.ImportSkill(r.Context(), actor, body)
	if err != nil {
		s.writeResourceAdminError(w, err, body.Name, "导入技能")
		return
	}
	s.writeOK(w, view.SkillView)
}

// handleAdminUpdateSkill 处理 `PUT /skills/{name}`：全量替换（未列出文件被删除），
// `If-Match` 必须等于当前发布版本的强 ETag。条件写中间件已判形状，这里仍按请求头
// 重读原值传端口——服务层在写锁内比较。成功回 `SkillDetail` 并在响应头带新 ETag；
// `files` 由 `nonNilSkillDetail` 保证是数组而不是 null。
// handleAdminUpdateSkill forwards If-Match verbatim into the adapter's write lock and
// returns the SkillDetail with the fresh ETag in the response header.
func (s *Server) handleAdminUpdateSkill(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "PUT /skills/{name}")
		return
	}
	var body SkillUpdateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageSkillUpdateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	view, err := s.opts.ResourceAdminService.UpdateSkill(r.Context(), actor, name, body, ifMatch)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "修改技能")
		return
	}
	view = nonNilSkillDetail(view)
	w.Header().Set("ETag", view.ETag)
	s.writeOK(w, view)
}

// handleAdminValidateSkill 处理 `POST /skills/{name}/validate`：只做导入边界与
// `SKILL.md` 校验，**不落盘、不写管理操作行、不执行脚本**（约束 26）。端口签名
// 没有 actor 也没有 If-Match——校验不是写动作，失败分类与导入共用同一映射。
// handleAdminValidateSkill runs the shared import gate without a write path: the port
// takes no actor and no If-Match, because validation changes nothing.
func (s *Server) handleAdminValidateSkill(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /skills/{name}/validate")
		return
	}
	var body SkillUpdateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageSkillUpdateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	result, err := s.opts.ResourceAdminService.ValidateSkill(r.Context(), name, body)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "校验技能")
		return
	}
	s.writeOK(w, skillValidateResultView(result))
}

// handleAdminResourceVersions 处理 `GET /resources/{kind}/{id}/versions`：只读观测，
// 不写管理操作行。未知资源类型与不存在的资源同映射 404——不让状态码成为枚举工具。
// 版本行自身不带指针状态，`published` / `applied` / 撤回来源由适配器在同一读取
// 快照里给出，本层只投影。
// handleAdminResourceVersions maps an unknown kind and a missing resource alike to 404;
// the pointer-derived flags come from the adapter's read snapshot.
func (s *Server) handleAdminResourceVersions(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	kind, ok := s.resourceKindParam(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "GET /resources/{kind}/{id}/versions")
		return
	}
	details, err := s.opts.ResourceAdminService.Versions(r.Context(), kind, id)
	if err != nil {
		s.writeResourceAdminError(w, err, id, "读取版本列表")
		return
	}
	views := make([]ResourceVersionView, len(details))
	for i, detail := range details {
		views[i] = resourceVersionView(detail.Version, detail.Published, detail.Applied,
			detail.RevertedFromVersionID)
	}
	s.writeOK(w, views)
}

// handleAdminResourceVersionDetail 处理 `GET /resource-versions/{version_id}`（H2）：
// 只读观测，不写管理操作行。路径参数只接受 ASCII 十进制正整数——非数字、符号、
// 溢出与 ≤0 一律 404，与"版本不存在"同一状态码（不做 400 区分，避免按状态码枚举）。
// `definition_yaml` 的脱敏与 fail-closed 在适配器内完成，走到这里的内容已经过凭证
// 扫描；本层只投影 `ResourceVersionDetailView`。
// handleAdminResourceVersionDetail maps every malformed id to the same 404 as a missing
// version; redaction has already happened inside the adapter.
func (s *Server) handleAdminResourceVersionDetail(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	raw := r.PathValue("version_id")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "GET /resource-versions/{version_id}")
		return
	}
	versionID, ok := parseVersionID(raw)
	if !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound, messageResourceNotFoundPrefix+raw)
		return
	}
	detail, err := s.opts.ResourceAdminService.VersionDetail(r.Context(), versionID)
	if err != nil {
		s.writeResourceAdminError(w, err, raw, "读取资源版本详情")
		return
	}
	s.writeOK(w, resourceVersionDetailView(detail))
}

// handleAdminResourceReferences 处理 `GET /resources/{kind}/{id}/references`：只读
// 观测，不写管理操作行。未知资源类型与不存在的资源同映射 404（与版本列表同一口
// 径）；已删除的资源仍可查引用——删除后运行引用与 Agent 快照仍是排障依据，由
// 适配器读取含 `deleted_at` 的指针行。
// handleAdminResourceReferences is read-only; a deleted resource stays answerable
// because the adapter reads pointers including deleted rows.
func (s *Server) handleAdminResourceReferences(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	kind, ok := s.resourceKindParam(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "GET /resources/{kind}/{id}/references")
		return
	}
	refs, err := s.opts.ResourceAdminService.References(r.Context(), kind, id)
	if err != nil {
		s.writeResourceAdminError(w, err, id, "读取资源引用")
		return
	}
	s.writeOK(w, resourceReferencesView(refs))
}

// handleAdminResourcePublish 处理 `POST /resources/{kind}/{id}/publish`：请求体可省略
// （契约 required:false），缺省即"发布最新"；带旧版本号时由适配器用该版本内容创建
// 新版本再发布。`If-Match` 原样传入，适配器在写锁内比较当前发布版本。
// handleAdminResourcePublish accepts an omitted body (publish the latest) and forwards
// If-Match verbatim into the adapter's write lock.
func (s *Server) handleAdminResourcePublish(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	kind, ok := s.resourceKindParam(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if s.opts.ResourceAdminService == nil {
		s.writeAdminPortMissing(w, "ResourceAdminService", "POST /resources/{kind}/{id}/publish")
		return
	}
	var body PublishWrite
	if err := s.decodeOptionalAdminWriteBody(w, r, &body, messagePublishBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	req := core.ResourcePublishRequest{
		Kind:         kind,
		ID:           id,
		VersionID:    body.VersionID,
		ExpectedETag: ifMatch,
	}
	result, err := s.opts.ResourceAdminService.Publish(r.Context(), actor, req)
	if err != nil {
		s.writeResourceAdminError(w, err, id, "发布资源版本")
		return
	}
	s.writeOK(w, resourcePublishResultView(req, result))
}

// resourceKindParam 解析路径里的资源类型：只接受三个冻结字面量，其余一律 404
// NOT_FOUND（契约在这两个端点上声明 404；与"资源不存在"共用同一状态码，避免按
// 类型拼写枚举资源面）。拒绝发生在端口调用之前，**不**产生任何写动作。
// resourceKindParam rejects every non-frozen kind spelling with a fixed 404 before the
// port is reached.
func (s *Server) resourceKindParam(w http.ResponseWriter, r *http.Request) (core.ResourceKind, bool) {
	kind, err := core.ParseResourceKind(r.PathValue("kind"))
	if err != nil {
		s.writeError(w, http.StatusNotFound, codeNotFound, messageResourceKind)
		return "", false
	}
	return kind, true
}

// decodeOptionalAdminWriteBody 解析**可省略**请求体的写端点（契约 required:false）：
// 空请求体等同全部字段缺省；非空时与 `decodeAdminWriteBody` 同一套上限、未知字段与
// 尾随内容规则。只给"省略即默认"的端点使用，**不**放宽必填端点的解码。
// decodeOptionalAdminWriteBody treats an empty body as all-zero fields and otherwise
// applies the same bounds and strictness as decodeAdminWriteBody.
func (s *Server) decodeOptionalAdminWriteBody(w http.ResponseWriter, r *http.Request, dst any, message string) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
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

// messageCredentialRefReadOnly 是只读凭据字段的固定文案：说明引用由服务端生成。
// messageCredentialRefReadOnly explains that the reference is server-issued.
// #nosec G101 -- 固定用户文案，值不是凭据（变量名含 credential 命中的误报）。
const messageCredentialRefReadOnly = "credential_ref 是服务端生成的只读凭据引用，不能在请求体里指定"

// rejectCredentialRef 在调端口**之前**拒绝请求体里出现的只读 `credential_ref`：
// 接受它会让调用方误以为自己能指定凭据身份（契约 400 `CREDENTIAL_REF_READ_ONLY`）。
// 返回 true 表示已写响应、调用方立即返回。
// rejectCredentialRef refuses a client-supplied credential reference before the port.
func (s *Server) rejectCredentialRef(w http.ResponseWriter, ref *string) bool {
	if ref == nil {
		return false
	}
	s.writeError(w, http.StatusBadRequest, codeCredentialRefReadOnly, messageCredentialRefReadOnly)
	return true
}

// writeCredentialError 是凭据与使用范围专用错误码的统一出口（保存期 400 / 409 / 503）。
// 判定一律按 sentinel，**不**按文案；命中即写响应并返回 true，调用方不再落通用码。
// `credential_ref` 只读没有领域错误载体，由处理器在调端口之前单独拒绝。
// writeCredentialError maps the credential and scope sentinels to their contract codes.
func (s *Server) writeCredentialError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, core.ErrCredentialModeRequired):
		s.writeAdminDataError(w, http.StatusBadRequest, codeCredentialModeRequired,
			adminMessageOr(err, "凭据值与模式不符：请显式给出 credential_mode 与对应的值"), err)
	case errors.Is(err, core.ErrCredentialRevoked):
		s.writeAdminDataError(w, http.StatusBadRequest, codeCredentialRevoked,
			adminMessageOr(err, "绑定的凭据已撤销，请重新录入新值"), err)
	case errors.Is(err, core.ErrResourceScopeInUse):
		s.writeAdminDataError(w, http.StatusConflict, codeResourceScopeInUse,
			adminMessageOr(err, "共享转专属被拒绝：仍被其他 Agent 或活动 Run 引用"), err)
	case errors.Is(err, core.ErrCredentialOwnerMismatch):
		s.writeAdminDataError(w, http.StatusConflict, codeCredentialOwnerMismatch,
			adminMessageOr(err, "凭据不属于目标资源"), err)
	case errors.Is(err, core.ErrCredentialUnavailable):
		s.writeAdminDataError(w, http.StatusServiceUnavailable, codeCredentialUnavailable,
			adminMessageOr(err, "托管凭据在当前主密钥下不可用"), err)
	default:
		return false
	}
	return true
}

// writeBindingError 是 Agent 保存期绑定错误码的统一出口（契约 §8 保存面，四条都是
// 400）：缺绑定 / 绑定非法 / 被引用资源不可用 / 资源对该 Agent 无资格。码区分「该去
// 修什么」，文案由适配器点名 `{kind, name}`；准入面同条件走 409，由运行准入的出口映射。
// writeBindingError maps the four save-time binding sentinels to their contract codes.
func (s *Server) writeBindingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, core.ErrAgentResourceBindingMissing):
		s.writeAdminDataError(w, http.StatusBadRequest, codeAgentResourceBindingMissing,
			adminMessageOr(err, messageBindingMissing), err)
	case errors.Is(err, core.ErrAgentResourceBindingInvalid):
		s.writeAdminDataError(w, http.StatusBadRequest, codeAgentResourceBindingInvalid,
			adminMessageOr(err, messageBindingInvalid), err)
	case errors.Is(err, core.ErrAgentResourceUnavailable):
		s.writeAdminDataError(w, http.StatusBadRequest, codeAgentResourceUnavailable,
			adminMessageOr(err, messageBindingUnavailable), err)
	case errors.Is(err, core.ErrResourceNotEligible):
		s.writeAdminDataError(w, http.StatusBadRequest, codeResourceNotEligible,
			adminMessageOr(err, messageNotEligible), err)
	default:
		return false
	}
	return true
}

// writeResourceAdminError 把资源写面失败按类型映射为固定响应（约束 43；只认
// errors.Is / errors.As，**不**比对文本）：
//   - core.ErrResourceNotFound / fs.ErrNotExist → 404 NOT_FOUND（点名资源标识）
//   - *core.InvalidInputError → 400 BAD_REQUEST（Reason 已由适配器点名且不含路径）
//   - 凭据与使用范围专用码 → `writeCredentialError`（400 / 409 / 503，排在通用码之前）
//   - core.ErrImportRejected → 400 IMPORT_REJECTED（技能导入边界，T052 起使用）
//   - core.ErrEditUnsupported → 400 EDIT_UNSUPPORTED（去掉哨兵前缀 + 改用整份原文）
//   - core.ErrPreconditionFailed → 412 PRECONDITION_FAILED + `data.current_etag`
//   - core.ErrResourceUIDMismatch → 409 RESOURCE_UID_MISMATCH + `data.resource_uid`
//     （当前指针身份；缺失或与当前指针不一致都走这里，不按名称猜目标）
//   - core.ErrConflict → 409 CONFLICT（同名资源、文件分歧、状态冲突）
//   - core.ErrProviderNotConfigured → 503 NOT_CONFIGURED（点名变量名，不回显值）
//   - context.DeadlineExceeded → 504 TIMEOUT
//   - 其余（发布事务失败、磁盘错误等）→ 500 INTERNAL，底层原因只进日志
//
// 每个分支都经 `writeAdminDataError` 出站：`pending` 之后的失败带 `operation_id`
// （约束 1a），白名单没有字段时它与 `writeError` 完全等价。
// writeResourceAdminError maps adapter failures by type only; every branch goes
// through writeAdminDataError so a post-pending operation id reaches the client.
func (s *Server) writeResourceAdminError(w http.ResponseWriter, err error, resource, action string) {
	if errors.Is(err, core.ErrResourceNotFound) || errors.Is(err, fs.ErrNotExist) {
		s.writeAdminDataError(w, http.StatusNotFound, codeNotFound,
			messageResourceNotFoundPrefix+resource, err)
		return
	}
	var invalidErr *core.InvalidInputError
	if errors.As(err, &invalidErr) {
		// 删除引用闸门把点名结果放在 `AdminError.Data`（agents / active_runs）；
		// 无白名单字段时 `writeAdminDataError` 自动退回不带 data 的信封。
		s.writeAdminDataError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason, err)
		return
	}
	if s.writeCredentialError(w, err) {
		return
	}
	if errors.Is(err, core.ErrImportRejected) {
		s.writeAdminDataError(w, http.StatusBadRequest, codeImportRejected,
			adminMessageOr(err, "导入内容未通过安全边界校验"), err)
		return
	}
	if errors.Is(err, core.ErrEditUnsupported) {
		s.writeAdminDataError(w, http.StatusBadRequest, codeEditUnsupported, editUnsupportedMessage(err), err)
		return
	}
	if errors.Is(err, core.ErrPreconditionFailed) {
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			adminMessageOr(err, messageResourceConflict), err)
		return
	}
	// 身份不一致要**排在** ErrConflict 之前：AdminError 的 Kind 是单个哨兵，但语义上
	// 它比通用冲突更具体——先给出专门的码，客户端才知道该重新读取 resource_uid。
	if errors.Is(err, core.ErrResourceUIDMismatch) {
		s.writeAdminDataError(w, http.StatusConflict, codeResourceUIDMismatch,
			adminMessageOr(err, messageResourceUIDMismatch), err)
		return
	}
	if errors.Is(err, core.ErrConflict) {
		s.writeAdminDataError(w, http.StatusConflict, codeConflict,
			adminMessageOr(err, messageResourceConflictTarget), err)
		return
	}
	if errors.Is(err, core.ErrProviderNotConfigured) {
		s.writeAdminDataError(w, http.StatusServiceUnavailable, codeNotConfigured,
			adminMessageOr(err, messageResourceNotConfigured), err)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.writeAdminDataError(w, http.StatusGatewayTimeout, codeTimeout, messageAdminTimeout, err)
		return
	}
	s.logger().Error("资源管理写失败", "resource", resource, "action", action, "error", err)
	s.writeAdminDataError(w, http.StatusInternalServerError, codeInternal, messageInternal, err)
}
