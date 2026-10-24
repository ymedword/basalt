package web

// 本文件是管理写面的**端口**声明（颗粒度文档 §3.1.8）：`admin_routes` 与各写面
// handler 只依赖这里的五个接口，不触达 `internal/app` 的具体实现。响应形状的归属
// 分两类：`views_admin.go` 能独立投影的（版本行、引用、测试结果、归档记录、Agent
// 文件集合）端口就返回 `core` 类型，由 web 层投影；需要把指针、版本、运行时状态
// 或工作副本正文拼在一起才能成立的（模型连接 / MCP / 技能详情、工作区浏览），
// 端口返回 `views_admin.go` 的视图类型，由 app 层适配器组装——与 US-11 只读面
// `web.ResourceReader` 的既有分工一致，**不**在 web 里复制领域类型。
// This file declares the admin write-side ports. Route registration and handlers depend
// only on these interfaces, never on concrete app services. The web layer owns the
// projection wherever a core record carries enough information; aggregated responses
// that only the app layer can assemble use the view types from views_admin.go.
//
// 线上请求体一律在这里定义（snake_case 与契约逐字一致）：handler 用
// `DisallowUnknownFields` 解码后交给端口；把线上形状转换成 `core` / app 请求类型的
// 工作由适配器完成。`If-Match` 不进请求体——handler 先按 `etag.go` 校验形状，再把
// 原样字符串作为独立参数传入；**不得**在这里做归一化或弱比较。
// Wire request bodies live here too; If-Match is a separate argument and is never
// normalized at this layer.

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// AgentFileSet 是受管文件集合的读取 / 写入结果：文件清单（`core.AgentFiles`）与
// 集合 ETag。集合 ETag 由适配器在**同一把 Agent 写锁内**从同一份清单算出，web 层
// 只把它同时写进响应头与 `data.etag`（契约 D-16），**不**自己读盘重算。
// AgentFileSet pairs the managed file list with its set ETag; the ETag is computed by
// the adapter inside the same write lock and only projected here.
type AgentFileSet struct {
	Files core.AgentFiles
	ETag  string
}

// AgentDraftResult 是作者草稿的端口结果：Agent 名、内存文件与澄清问题。文件与问题
// 互斥（R6.3）——有问题时 Files 为空；web 层只做投影，不补默认值。
// AgentDraftResult is the port-side draft result; files and questions are exclusive.
type AgentDraftResult struct {
	Agent     string
	Files     []core.AuthorFile
	Questions []core.AuthorQuestion
}

// ScheduleDraftResult 是定时候选的端口结果：候选字段（尚未保存）与服务端计算的未来
// 三次触发时间（R6.4）。浏览器不按 cron 自行推导这些时间。
// ScheduleDraftResult carries the unsaved candidate plus server-computed triggers.
type ScheduleDraftResult struct {
	Agent     string
	Candidate core.Schedule
	NextRuns  []time.Time
}

// AgentAdminService 是 Agent 全生命周期写面端口（创建 / 草稿 / 定时候选 / 多文件提交 /
// 复制 / 归档 / 恢复 / 归档列表）。`GenerateFiles` 返回内存草稿或澄清问题，`ScheduleDraft`
// 返回未保存的定时候选——两者都不落盘、不注册；归档记录由 web 层投影为契约视图
// （时间字段在投影处统一格式化）。
// AgentAdminService is the agent lifecycle write port. GenerateFiles and ScheduleDraft
// return in-memory results; archive projections happen in the web layer.
type AgentAdminService interface {
	Create(ctx context.Context, actor core.Actor, spec core.AgentSpec) (AgentFileSet, error)
	GenerateFiles(ctx context.Context, name, description string, answers []core.AuthorAnswer) (AgentDraftResult, error)
	ScheduleDraft(ctx context.Context, name, instruction string) (ScheduleDraftResult, error)
	Files(ctx context.Context, name string) (AgentFileSet, error)
	SubmitFiles(ctx context.Context, actor core.Actor, name, ifMatch string, files []core.AuthorFile) (AgentFileSet, error)
	Copy(ctx context.Context, actor core.Actor, source, target string, mapping []core.ResourceMapping) (AgentFileSet, error)
	Archive(ctx context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error)
	Restore(ctx context.Context, actor core.Actor, archiveID, target string) (AgentFileSet, error)
	Archived(ctx context.Context) ([]core.AgentArchiveRecord, error)
}

// ResourceAdminService 是模型连接 / MCP / 技能三类公共资源的写面端口：创建与修改
// 在适配器内走同一条发布编排（指针与运行时同闸），删除检查引用闸门，发布支持
// "发布最新（幂等）"与"以旧版本内容创建新版本"两种语义。三个 Upsert 的 `ifMatch`
// 为空串表示创建请求（POST），非空表示条件修改（PUT）——handler 已保证 PUT 缺失
// If-Match 时不会走到这里。
// ResourceAdminService is the resource write port. An empty ifMatch means the create
// (POST) path; handlers never let a PUT reach it without an If-Match.
type ResourceAdminService interface {
	UpsertModelConnection(ctx context.Context, actor core.Actor, write ModelConnectionWrite, ifMatch string) (ModelConnectionView, error)
	UpsertMCPServer(ctx context.Context, actor core.Actor, write MCPServerWrite, ifMatch string) (McpServerView, error)

	CreateSkill(ctx context.Context, actor core.Actor, write SkillCreateWrite) (SkillDetailView, error)
	ImportSkill(ctx context.Context, actor core.Actor, write SkillImportWrite) (SkillDetailView, error)
	UpdateSkill(ctx context.Context, actor core.Actor, name string, write SkillUpdateWrite, ifMatch string) (SkillDetailView, error)
	ValidateSkill(ctx context.Context, name string, write SkillUpdateWrite) (core.SkillValidateResult, error)

	// Delete 的 `resourceUID` 是调用方显式给出的目标身份（契约要求删除必须携带），
	// 空串与不匹配都由适配器回 `core.ErrResourceUIDMismatch`（409），不按名猜目标。
	// Delete requires the caller's explicit resource uid; an empty or stale value is
	// a typed 409 from the adapter, never a name-based guess.
	Delete(ctx context.Context, actor core.Actor, kind core.ResourceKind, id, ifMatch, resourceUID string) error
	Publish(ctx context.Context, actor core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error)
	// CurrentETag 返回资源当前的条件 ETag（内容 + 资源 UID + 修订 + 范围 + 凭据状态，
	// FR-081）。第二个值为 false 表示资源不存在 / 已删除 / 没有发布版本——形状级 412
	// 据此决定是否带 `data.current_etag`。实现必须与写路径的比较值同源。
	// CurrentETag resolves the conditional ETag the write paths compare against.
	CurrentETag(ctx context.Context, kind core.ResourceKind, id string) (string, bool)
	Versions(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error)
	VersionDetail(ctx context.Context, versionID int64) (core.ResourceVersionDetail, error)
	References(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error)
	TestModelConnection(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error)
	TestMCPServer(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error)
}

// NotifyAdminService 是通知渠道写面端口：读取仍走 US-11 的只读服务，本接口
// 只承载创建 / 更新 / 删除，**不**另起第二套列表路径。更新时 `write.URL` 为空串
// 表示保留原地址（契约 NotifyChannelUpsert）；是否保留由适配器解释。删除携带
// 请求里的 `resource_uid` 原样下传（契约 DELETE /notify-channels 的必需查询参数，
// 缺失或不等于当前指针 → 409 `RESOURCE_UID_MISMATCH`，由适配器判定）。
// NotifyAdminService carries only channel writes; delete forwards the request's
// resource_uid so the adapter can refuse a stale identity.
type NotifyAdminService interface {
	Create(ctx context.Context, actor core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error)
	Update(ctx context.Context, actor core.Actor, name string, write NotifyChannelWrite) (core.NotifyChannel, error)
	Delete(ctx context.Context, actor core.Actor, name, resourceUID string, force bool) error
	// TestChannel 发出一条固定的连通性测试消息（`core.NotifyConnectivityTestText`），
	// 先写管理操作行再调用；结果三值（success / failed / unknown）与脱敏明细随回执返回。
	// The test sends one fixed connectivity message after the operation pre-write and
	// returns the three-valued outcome.
	TestChannel(ctx context.Context, actor core.Actor, name string) (core.NotifyChannelTestResult, error)
}

// WorkspaceService 是只读工作区浏览端口：根目录固定为 `.basalt/agents/`，不跟随
// 软链接，单文件上限 256 KiB（超限报错、**不截断**）。返回视图而不是路径，保证
// 真实路径校验只在适配器里做一次。
// WorkspaceService is the read-only workspace browser port; it returns views, so the
// real-path checks stay in one adapter.
type WorkspaceService interface {
	Tree(ctx context.Context, path string) (WorkspaceTreeView, error)
	File(ctx context.Context, path string) (WorkspaceFileView, error)
}

// ResourceStateService 是两个**资源状态动作**端点的端口：
// `POST /resources/{kind}/{name}/credential/revoke` 与
// `POST /resources/{kind}/{name}/scope`。两者都要条件写、显式 `resource_uid` 与目标
// 存在性，并各落一条管理操作行；`kind` 保持路径原样字符串（只接受
// model_connection / notify_channel），解释与 400 文案由适配器按该动作的字面量给出。
// 身份与 ETag 的比较、pending 预写、执行与终态回填都在适配器**同一把资源锁**内完成，
// web 层只解码、投影视图与设置响应头，**不**自己比较 ETag。
// ResourceStateService covers the revoke and scope actions; the adapter compares the
// identity and the ETag, writes the pending row and records the terminal state inside
// one resource lock, while web only decodes, projects and sets headers.
type ResourceStateService interface {
	// CurrentETag 返回目标当前的条件 ETag（形状级 412 的 `data.current_etag`）。
	// 解析不了（类别非法、资源不存在、端口缺读面）时第二个值为 false。
	CurrentETag(ctx context.Context, kind, name string) (string, bool)
	RevokeCredential(ctx context.Context, actor core.Actor, kind, name string, write CredentialRevokeWrite, ifMatch string) (CredentialRevokeView, error)
	ConvertScope(ctx context.Context, actor core.Actor, kind, name string, write ResourceScopeWrite, ifMatch string) (ResourceScopeView, error)
}

// AgentConfigService 是结构化配置读面端口（`GET /agents/{name}/config`）：一次读取
// 给出字段值、与 `/content` 完全一致的强 `content_etag`，以及每个引用的解析状态。
// 视图由适配器组装——解析状态要同时读定义文件、身份墓碑、发布指针与凭据身份，
// 只有装配层拿得到这几份仓储；web 层不重算 ETag、不按名称猜绑定。
// AgentConfigService reads the structured config plus the per-reference resolution;
// only the assembly layer can reach the file, the tombstones, the pointers and the
// credential identities, so the adapter fills the view.
type AgentConfigService interface {
	Config(ctx context.Context, name string) (AgentConfigView, error)
}

// FileDraftWrite 是契约 FileDraft 的线上形状：相对路径 + UTF-8 正文。多文件提交与
// 技能导入 / 更新共用；路径规范化与边界拒绝由适配器完成，web 层只解码。
// FileDraftWrite is the wire shape of FileDraft, shared by agent submissions and
// skill import/update; path normalization belongs to the adapter.
type FileDraftWrite struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// AgentCreateWrite 是 `POST /agents` 的请求体（`name` 必填，其余可省略）。
// AgentCreateWrite is the create-agent request body.
type AgentCreateWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

// AgentGenerateWrite 是 `POST /agents/{name}/generate-files` 的请求体：一句描述，
// 外加可选的问答答案（R6.3）。答案的条数 / 长度 / 非空由适配器裁决（400 点名）；
// 草稿边界（文件数 / 大小 / UTF-8）同样归适配器。
// AgentGenerateWrite is the draft request body: the description plus optional answers.
type AgentGenerateWrite struct {
	Description string             `json:"description"`
	Answers     []AgentAnswerWrite `json:"answers"`
}

// AgentAnswerWrite 是问答补全的一条答案（R6.3）：`id` 对位模型给出的问题，`question`
// 原样回传，`answer` 是用户输入。答案不落盘、不进日志，只随本次请求进入提示词。
// AgentAnswerWrite is one Q&A answer; it never touches disk or logs.
type AgentAnswerWrite struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// ScheduleDraftWrite 是 `POST /agents/{name}/schedule-draft` 的请求体（R6.4）：一句话
// 指令，去空白后非空且 ≤ 1000 字符，超限由适配器按 400 点名。
// ScheduleDraftWrite is the one-line schedule request body.
type ScheduleDraftWrite struct {
	Instruction string `json:"instruction"`
}

// AgentCopyWrite 是 `POST /agents/{name}/copy` 的请求体（`new_name` 必填）。
// `resource_mapping` 为每一条**源专属**绑定提供显式替代；缺失时整次复制以
// `AGENT_COPY_BLOCKED`（409）拒绝，不产生副本——映射从不自动推导。
// AgentCopyWrite is the copy request body; every source-exclusive binding needs an
// explicit mapping entry and the adapter never infers one.
type AgentCopyWrite struct {
	NewName         string                  `json:"new_name"`
	ResourceMapping []AgentCopyMappingWrite `json:"resource_mapping"`
}

// AgentCopyMappingWrite 是契约 `AgentCopyRequest.resource_mapping[]` 的线上形状：
// `resource_kind` 只接受 model_connection / notify_channel（适配器逐字解析，未知值
// 400）；两个 UID 都是不可变资源身份，适配器校验存在性、资格与"不沿用源 UID"。
// AgentCopyMappingWrite is one explicit mapping entry; the adapter validates the kind
// literal, both uids, eligibility and the no-reuse rule.
type AgentCopyMappingWrite struct {
	ResourceKind      string `json:"resource_kind"`
	SourceResourceUID string `json:"source_resource_uid"`
	TargetResourceUID string `json:"target_resource_uid"`
}

// AgentRestoreWrite 是 `POST /archived-agents/{archive_id}/restore` 的请求体：
// `new_name` 留空 = 恢复原目录名。
// AgentRestoreWrite is the restore request body; an empty new_name means the
// original directory name.
type AgentRestoreWrite struct {
	NewName string `json:"new_name"`
}

// AgentFilesWrite 是 `POST /agents/{name}/files` 的请求体：未列出的受管文件保留。
// AgentFilesWrite is the multi-file submission body; unlisted files are kept.
type AgentFilesWrite struct {
	Files []FileDraftWrite `json:"files"`
}

// ModelConnectionWrite 是契约 ModelConnectionUpsert 的线上形状。`api_key` 是指针：
// 字段省略 = 保留（指针为 nil），出现即按 `credential_mode` 解释形态——env 要精确
// `${ENV_VAR}` 占位，managed 是 **write-only** 完整新值；空串是 400，既不当保留也不
// 当清除。`credential_ref` 只读：出现在请求体里就是 400，解码成指针只为识别它。
// ModelConnectionWrite is the upsert body. APIKey is a pointer so "omitted" (keep) is
// distinguishable from an explicit value; CredentialRef exists only to reject it.
type ModelConnectionWrite struct {
	Name           string  `json:"name"`
	BaseURL        string  `json:"base_url"`
	APIKey         *string `json:"api_key"`
	DefaultModel   string  `json:"default_model"`
	CredentialMode string  `json:"credential_mode"`
	CredentialRef  *string `json:"credential_ref"`
	Scope          string  `json:"scope"`
	OwnerAgent     string  `json:"owner_agent"`
}

// MCPServerWrite 是契约 McpServerUpsert 的线上形状：`transport` 必须是显式的
// stdio / http；stdio 用 command + args，http 用 url；env 值只接受精确 `${VAR}`。
// MCPServerWrite is the MCP upsert body; transport is explicit and env values are
// exact ${VAR} references.
type MCPServerWrite struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	URL       string            `json:"url"`
	Env       map[string]string `json:"env"`
}

// SkillCreateWrite 是契约 SkillCreate 的线上形状：名字 + 一句话描述。
// SkillCreateWrite is the skill skeleton creation body.
type SkillCreateWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SkillImportWrite 是契约 SkillImport 的线上形状：浏览器目录读取后的文本文件集合
// （不得含二进制；文件数与大小上限由适配器裁决）。
// SkillImportWrite carries the text files read by the browser directory picker.
type SkillImportWrite struct {
	Name  string           `json:"name"`
	Files []FileDraftWrite `json:"files"`
}

// SkillUpdateWrite 是契约 SkillUpdate 的线上形状：全量替换（未列出的文件被删除）。
// SkillUpdateWrite is a full replacement; unlisted files are removed.
type SkillUpdateWrite struct {
	Files []FileDraftWrite `json:"files"`
}

// PublishWrite 是契约 PublishRequest 的线上形状：`version_id` 缺省 = 发布最新；
// 给旧版本则先创建同内容新版本再发布。0 不是合法版本号，由适配器拒绝。
// PublishWrite is the publish body; an absent version_id publishes the latest.
type PublishWrite struct {
	VersionID *int64 `json:"version_id"`
}

// NotifyChannelWrite 是契约 NotifyChannelUpsert 的线上形状。`url` 是指针：字段省略 =
// 保留原值（managed 渠道上它是密文，没有可写回的值），出现即按 `credential_mode`
// 解释——env 要精确 `${ENV_VAR}` 占位，managed 是完整新地址（只进密文，只回 host）；
// 空串是 400。`credential_ref` 只读，出现即 400。
// NotifyChannelWrite is the channel upsert body. URL is a pointer so "omitted" (keep)
// is distinguishable from an explicit value; CredentialRef exists only to reject it.
type NotifyChannelWrite struct {
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	URL            *string `json:"url"`
	Description    string  `json:"description"`
	CredentialMode string  `json:"credential_mode"`
	CredentialRef  *string `json:"credential_ref"`
	Scope          string  `json:"scope"`
	OwnerAgent     string  `json:"owner_agent"`
}

// CredentialRevokeWrite 是契约 CredentialRevokeRequest 的线上形状：`resource_uid`
// 必填，`confirm` 必须显式为 `true`（`false` 与缺省同样 400，在 handler 调端口
// **之前**判定——它不是目标状态，不该留操作行）。缺少 `resource_uid` 的合法性
// 由适配器按动作字面量判 400，不按名猜目标。
// CredentialRevokeWrite is the revoke body; confirm must be literally true and the
// handler rejects anything else before the port is reached.
type CredentialRevokeWrite struct {
	ResourceUID string `json:"resource_uid"`
	Confirm     bool   `json:"confirm"`
}

// ResourceScopeWrite 是契约 ResourceScopeRequest 的线上形状：`scope` 字面量与
// `owner_agent` 的组合规则（exclusive 必须带、shared 不得带）由适配器判定并映射
// 400；本篇只解码，不做范围语义裁决。
// ResourceScopeWrite is the scope body; the adapter owns the literal and owner rules.
type ResourceScopeWrite struct {
	ResourceUID string `json:"resource_uid"`
	Scope       string `json:"scope"`
	OwnerAgent  string `json:"owner_agent"`
}
