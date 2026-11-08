package app

// 本文件实现 `basalt demo workflow prepare` 的服务面（US-25 §2.3.7 / §3.1.1）：
// 计划**只发 GET**，应用阶段建集合、摄取文档、写只读知识绑定。三条硬边界：
//
//  1. **不连库、不装配运行期**——服务只持一个 HTTP 客户端与工作目录，管理端点自己
//     的审计（集合创建 / 摄取 / `agent.files.update`）就是唯一记录，不另起旁路；
//  2. **ETag 钉住**——写 `AGENT.md` 前先 `GET /files` 取基础集合 ETag、再 `GET
//     /content` 读正文，`If-Match` 始终是这份基础 ETag；412 视为计划过期，停止该
//     Agent 并让用户重跑，**不得**把旧正文配上新 ETag 再提交（那是覆盖他人修改）；
//  3. **凭据只读不铸**——token 来自配置或实例私有 `admin-token`，命令不生成、不打印、
//     不进日志与错误文案。
//
// The service deliberately holds no database handle and no runtime: the management
// endpoints' own audit rows are the only record. The base file-set ETag is read before
// the body and reused verbatim for If-Match; a 412 stops that agent instead of
// resubmitting a stale body against a fresh ETag.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// demoPrepareTimeout 是单次管理面请求的上限：与 `flow_cli.go` 的客户端纪律同一档，
	// 摄取是同步的（可能要调向量模型），因此不能短于一次模型调用。
	// demoPrepareTimeout bounds every management request; ingest is synchronous.
	demoPrepareTimeout = 65 * time.Second

	// demoPrepareMaxResponseBytes 是响应体上限（2 MiB）：文档列表与文件集合都远小于它。
	// demoPrepareMaxResponseBytes caps every response body.
	demoPrepareMaxResponseBytes = 2 << 20

	// demoAdminTokenMaxBytes 是管理面凭据文件的大小上限。
	// demoAdminTokenMaxBytes caps the token file.
	demoAdminTokenMaxBytes = 4096

	// demoWorkflowDirRel 是 `demo workflow install` 安装素材的位置（相对工作区根
	// `.basalt`）；摄取路径按同一个根解析，两边不得各写一份。
	// demoWorkflowDirRel is the installer's destination, relative to the workspace root.
	demoWorkflowDirRel = "samples/bid-readiness"

	// demoCollectionRequest / demoCollectionProduct 是两个知识集合的固定名。
	// The two fixed collection names.
	demoCollectionRequest = "bid-request"
	demoCollectionProduct = "bid-product"

	// 计划 / 结果里的动作字面量：命令面按它们决定是否写，报告按它们逐项输出。
	// The action literals shared by the plan and the per-item report.
	demoActionCreate    = "create"
	demoActionReuse     = "reuse"
	demoActionIngest    = "ingest"
	demoActionUnchanged = "unchanged"
	demoActionBind      = "bind"
	demoActionConflict  = "conflict"
	demoActionApplied   = "applied"
	demoActionStale     = "stale"
)

// errDemoPlanStale 表示读取与提交之间他人改动了 Agent 文件集合（`POST /files` 412）：
// 计划已过期，**只能**重新计划，不得用新 ETag 重提旧正文。
// errDemoPlanStale marks a 412 on submit: the plan must be rebuilt, never resubmitted.
var errDemoPlanStale = errors.New("agent 文件集合已被其他写入修改：本次计划已过期，请重新运行 prepare")

// errDemoAgentMissing 表示管理面回答该 Agent 不存在（未安装模板或未启用）。
// errDemoAgentMissing marks a 404 for an agent that was never installed.
var errDemoAgentMissing = errors.New("管理面找不到该 Agent")

// DemoPrepareDeps 是装配层注入的依赖：工作目录、工作区根、已装载的配置、有界 HTTP
// 客户端与日志。`ConfigErr` 非空表示配置装载失败——未显式给 `--server` 时直接失败，
// 不猜地址。
// DemoPrepareDeps is injected by the assembly layer; a non-nil ConfigErr makes the
// derived endpoint a hard failure.
type DemoPrepareDeps struct {
	WorkDir       string
	WorkspaceRoot string
	Config        *cli.Config
	ConfigErr     error
	HTTPClient    *http.Client
	Logger        *slog.Logger
	Now           func() time.Time
}

// DemoPrepareService 是 `demo workflow prepare` 的端口实现：无数据库句柄、无运行期，
// 只发管理面 HTTP。
// DemoPrepareService holds no database handle and talks to the management endpoints only.
type DemoPrepareService struct {
	workDir       string
	workspaceRoot string
	cfg           *cli.Config
	cfgErr        error
	client        *http.Client
	logger        *slog.Logger
	now           func() time.Time
}

var _ cli.DemoPrepare = (*DemoPrepareService)(nil)

// NewDemoPrepareService 构造服务；`WorkDir` / `WorkspaceRoot` 必填，`HTTPClient` 为空
// 时建有界客户端（无代理、不跟随重定向、65s 超时）。
// NewDemoPrepareService requires a work directory and a workspace root, and builds a
// bounded client when none is injected.
func NewDemoPrepareService(deps DemoPrepareDeps) (*DemoPrepareService, error) {
	if strings.TrimSpace(deps.WorkDir) == "" || strings.TrimSpace(deps.WorkspaceRoot) == "" {
		return nil, errors.New("demo 准备端口装配失败：工作目录或工作区根为空")
	}
	service := &DemoPrepareService{
		workDir:       deps.WorkDir,
		workspaceRoot: deps.WorkspaceRoot,
		cfg:           deps.Config,
		cfgErr:        deps.ConfigErr,
		client:        deps.HTTPClient,
		logger:        deps.Logger,
		now:           deps.Now,
	}
	if service.client == nil {
		transport := &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		}
		service.client = &http.Client{
			Transport: transport,
			Timeout:   demoPrepareTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if service.logger == nil {
		service.logger = slog.Default()
	}
	if service.now == nil {
		service.now = time.Now
	}
	return service, nil
}

// newDemoPrepareService 是装配层的构造入口：把配置与工作区根摊平后交给公开构造器。
// newDemoPrepareService is the assembly-layer constructor.
func newDemoPrepareService(cfg *cli.Config, cfgErr error, workDir, wsRoot string, logger *slog.Logger) (*DemoPrepareService, error) {
	return NewDemoPrepareService(DemoPrepareDeps{
		WorkDir:       workDir,
		WorkspaceRoot: wsRoot,
		Config:        cfg,
		ConfigErr:     cfgErr,
		Logger:        logger,
	})
}

// collectionView 是集合列表里本命令需要的字段：稳定 ID、名字与软删时间。
// collectionView decodes only the fields this command needs.
type collectionView struct {
	CollectionID string `json:"collection_id"`
	Name         string `json:"name"`
	DeletedAt    string `json:"deleted_at"`
}

// documentView 是一份已摄取文档的摘要：来源路径与最新版本状态。
// documentView decodes the document summary fields.
type documentView struct {
	SourcePath   string `json:"source_path"`
	LatestStatus string `json:"latest_status"`
	DeletedAt    string `json:"deleted_at"`
}

// ingestView 是一次同步摄取的终态：`status` 只能是 active / failed，`deduplicated`
// 表示服务端按内容哈希复用（**没有**调用向量模型）。
// ingestView is the ingest terminal state; deduplicated means no embedding call.
type ingestView struct {
	DocumentID   string `json:"document_id"`
	Status       string `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	ErrorKind    string `json:"error_kind"`
}

// demoEnvelope 是管理面统一信封：只有 `code == OK` 且 2xx 才算成功。
// demoEnvelope is the management envelope; only 2xx with code OK is success.
type demoEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Plan 只发 GET：列集合、逐集合列文档、读两个 Agent 的基础 ETag 与正文，算出集合 /
// 文档 / 绑定的逐项动作，并给出 embedding 调用与费用提示。零写入。
// Plan sends GETs only and computes every item's action plus the cost warning.
func (s *DemoPrepareService) Plan(ctx context.Context, req cli.DemoPrepareRequest) (cli.DemoPreparePlan, error) {
	started := s.now()
	base, token, err := s.resolveEndpoint(req)
	if err != nil {
		return cli.DemoPreparePlan{}, err
	}
	collections, err := s.listCollections(ctx, base, token)
	if err != nil {
		return cli.DemoPreparePlan{}, err
	}
	plan, err := s.buildPlan(ctx, base, token, collections)
	if err != nil {
		return cli.DemoPreparePlan{}, err
	}
	toIngest := 0
	for _, document := range plan.Documents {
		if document.Action == demoActionIngest {
			toIngest++
		}
	}
	if toIngest > 0 {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("将摄取 %d 个文档：每个新文档会调用一次向量模型（共 %d 次 embedding 调用），按当前模型计费；内容未变的文档由服务端按哈希去重，不调用模型。",
				toIngest, toIngest))
	}
	s.logger.Info("Demo 准备计划完成",
		"server", plan.Server, "collections", len(plan.Collections),
		"documents_to_ingest", toIngest, "bindings", len(plan.Bindings),
		"elapsed", s.now().Sub(started))
	return plan, nil
}

// buildPlan 用已读到的集合列表算出逐项动作：集合 create / reuse，文档 ingest /
// unchanged，绑定 bind / unchanged / conflict。绑定判定读正文（`GET /content`），
// 读之前先取一次基础集合 ETag——与应用阶段的顺序一致，也顺带暴露"Agent 不存在"。
// buildPlan computes every action; the file-set ETag is read before the body, matching
// the apply order and surfacing a missing agent early.
func (s *DemoPrepareService) buildPlan(ctx context.Context, base *url.URL, token string, collections []collectionView) (cli.DemoPreparePlan, error) {
	plan := cli.DemoPreparePlan{
		Server:      base.Scheme + "://" + base.Host,
		WorkflowDir: filepath.Join(s.workspaceRoot, demoWorkflowDirRel),
	}
	active := make(map[string]string, len(collections))
	for _, collection := range collections {
		if collection.DeletedAt != "" {
			continue
		}
		active[collection.Name] = collection.CollectionID
	}
	for _, name := range []string{demoCollectionRequest, demoCollectionProduct} {
		view := cli.DemoCollectionPlan{Name: name, Action: demoActionCreate}
		if id, ok := active[name]; ok {
			view.CollectionID = id
			view.Exists = true
			view.Action = demoActionReuse
		}
		plan.Collections = append(plan.Collections, view)
	}

	// 文档是否已摄取要按集合查一遍清单：计划里写"将摄取"的条目才是费用提示的依据。
	// Whether a document is already active needs one listing per collection.
	documents := make(map[string]map[string]string, len(active))
	for _, view := range demoWorkflowDocuments() {
		if _, loaded := documents[view.Collection]; !loaded {
			// 集合还不存在时清单为空，但**这份文档本身仍要进计划**（它就是要新建的）。
			// A missing collection lists nothing, but the document still belongs to the plan.
			listing := map[string]string{}
			if id, ok := active[view.Collection]; ok {
				listed, err := s.listDocuments(ctx, base, token, id)
				if err != nil {
					return cli.DemoPreparePlan{}, err
				}
				for _, document := range listed {
					if document.DeletedAt != "" {
						continue
					}
					listing[document.SourcePath] = document.LatestStatus
				}
			}
			documents[view.Collection] = listing
		}
		action := demoActionIngest
		if status, ok := documents[view.Collection][view.Path]; ok && status == "active" {
			action = demoActionUnchanged
		}
		view.Action = action
		plan.Documents = append(plan.Documents, view)
	}

	for _, binding := range demoWorkflowBindings() {
		// 先取基础集合 ETag、再读正文（C-21）；计划阶段不保存 ETag，应用阶段重取。
		// Read the base ETag first, then the body; apply re-reads it anyway.
		if _, err := s.readAgentFilesETag(ctx, base, token, binding.Agent); err != nil {
			return cli.DemoPreparePlan{}, err
		}
		content, err := s.readAgentContent(ctx, base, token, binding.Agent)
		if err != nil {
			return cli.DemoPreparePlan{}, err
		}
		existing, err := workspace.ParseKnowledgeBindings([]byte(content))
		if err != nil {
			// 正文里的 knowledge 段结构非法：当作冲突，命令会让用户先修好再跑。
			// A structurally invalid block is reported as a conflict, never "fixed" here.
			binding.Action = demoActionConflict
			plan.Bindings = append(plan.Bindings, binding)
			continue
		}
		binding.Action = demoBindingState(existing, binding.Knowledge)
		plan.Bindings = append(plan.Bindings, binding)
	}
	return plan, nil
}

// Apply 按计划写：先建缺的集合，再摄取要摄取的文档，最后逐个 Agent 绑知识。每个
// Agent 独立成败；任何绑定未完成都置 `Partial`（命令面据此退出 3）。
// Apply creates, ingests and binds; every agent succeeds or fails on its own, and an
// unfinished binding flips Partial.
func (s *DemoPrepareService) Apply(ctx context.Context, req cli.DemoPrepareRequest, plan cli.DemoPreparePlan) (cli.DemoPrepareResult, error) {
	started := s.now()
	base, token, err := s.resolveEndpoint(req)
	if err != nil {
		return cli.DemoPrepareResult{}, err
	}
	var result cli.DemoPrepareResult

	// ── 集合：以**当前**清单为准判定"缺不缺"，计划只决定顺序与名字 ─────────────
	// Re-list at apply time: the plan decides order and names, the live list decides
	// whether a collection still needs creating.
	current, err := s.listCollections(ctx, base, token)
	if err != nil {
		return result, err
	}
	existing := make(map[string]string, len(current))
	for _, collection := range current {
		if collection.DeletedAt == "" {
			existing[collection.Name] = collection.CollectionID
		}
	}
	collectionIDs := make(map[string]string, len(plan.Collections))
	for _, wanted := range plan.Collections {
		if id, ok := existing[wanted.Name]; ok {
			collectionIDs[wanted.Name] = id
			result.Collections = append(result.Collections, cli.DemoCollectionResult{Name: wanted.Name, CollectionID: id})
			continue
		}
		created, err := s.createCollection(ctx, base, token, wanted.Name)
		if err != nil {
			return result, err
		}
		collectionIDs[wanted.Name] = created.CollectionID
		result.Collections = append(result.Collections, cli.DemoCollectionResult{Name: wanted.Name, CollectionID: created.CollectionID, Created: true})
		s.logger.Info("Demo 准备：已创建知识集合", "server", plan.Server, "collection", wanted.Name)
	}

	// ── 文档：只摄取计划里判定为 ingest 的条目 ─────────────────────────────────
	var failed []string
	for _, document := range plan.Documents {
		if document.Action != demoActionIngest {
			result.Documents = append(result.Documents, cli.DemoDocumentResult{
				Collection: document.Collection,
				Path:       document.Path,
				Status:     "active",
			})
			continue
		}
		id, ok := collectionIDs[document.Collection]
		if !ok {
			return result, fmt.Errorf("计划里的集合 %s 没有对应 ID，拒绝摄取 %s", document.Collection, document.Path)
		}
		ingested, err := s.ingestDocument(ctx, base, token, id, document.Path)
		if err != nil {
			return result, err
		}
		result.Documents = append(result.Documents, cli.DemoDocumentResult{
			Collection:   document.Collection,
			Path:         document.Path,
			Status:       ingested.Status,
			Deduplicated: ingested.Deduplicated,
		})
		s.logger.Info("Demo 准备：文档摄取终态",
			"collection", document.Collection, "path", document.Path,
			"status", ingested.Status, "deduplicated", ingested.Deduplicated)
		if ingested.Status != "active" {
			failed = append(failed, fmt.Sprintf("%s（%s）", document.Path, ingested.ErrorKind))
		}
	}

	// ── 绑定：逐 Agent 独立成败；写前重读 ETag 与正文并重判 ────────────────────
	for _, binding := range plan.Bindings {
		status, err := s.applyBinding(ctx, base, token, binding)
		if err != nil {
			return result, err
		}
		result.Bindings = append(result.Bindings, cli.DemoBindingResult{Agent: binding.Agent, Status: status})
		if status != demoActionApplied && status != demoActionUnchanged {
			result.Partial = true
		}
	}
	s.logger.Info("Demo 准备应用完成",
		"server", plan.Server, "collections", len(result.Collections),
		"documents", len(result.Documents), "bindings", len(result.Bindings),
		"partial", result.Partial, "elapsed", s.now().Sub(started))
	if len(failed) > 0 {
		return result, fmt.Errorf("有 %d 个文档摄取失败：%s；已成功的文档保留，重跑只补失败项", len(failed), strings.Join(failed, "、"))
	}
	return result, nil
}

// applyBinding 处理一个 Agent：计划里的 unchanged / conflict 直接照报；bind 则重取
// 基础 ETag → 重读正文 → 重判 → 条件写。412 记 stale 并放弃该 Agent。
// applyBinding re-reads the base ETag, the body and the conflict verdict before the
// conditional write; a 412 becomes stale and stops that agent.
func (s *DemoPrepareService) applyBinding(ctx context.Context, base *url.URL, token string, binding cli.DemoBindingPlan) (string, error) {
	switch binding.Action {
	case demoActionUnchanged:
		return demoActionUnchanged, nil
	case demoActionConflict:
		s.logger.Warn("Demo 准备：跳过冲突绑定", "agent", binding.Agent)
		return demoActionConflict, nil
	}
	// 先取基础集合 ETag，再读正文：If-Match 必须是这一份基础 ETag。
	// The base ETag is read before the body and reused verbatim for If-Match.
	etag, err := s.readAgentFilesETag(ctx, base, token, binding.Agent)
	if err != nil {
		return "", err
	}
	content, err := s.readAgentContent(ctx, base, token, binding.Agent)
	if err != nil {
		return "", err
	}
	existingBindings, err := workspace.ParseKnowledgeBindings([]byte(content))
	if err != nil {
		s.logger.Warn("Demo 准备：正文里的 knowledge 段结构非法，按冲突处理", "agent", binding.Agent)
		return demoActionConflict, nil
	}
	switch demoBindingState(existingBindings, binding.Knowledge) {
	case demoActionUnchanged:
		return demoActionUnchanged, nil
	case demoActionConflict:
		s.logger.Warn("Demo 准备：正文里的绑定已变化，按冲突处理", "agent", binding.Agent)
		return demoActionConflict, nil
	}
	wanted := binding.Knowledge
	next, err := workspace.ApplyChanges([]byte(content), core.AgentContentChanges{Knowledge: &wanted})
	if err != nil {
		return "", fmt.Errorf("无法在 Agent %s 的正文里写入知识绑定（现有写法无法安全保留）：%w", binding.Agent, err)
	}
	if err := s.submitAgentKnowledge(ctx, base, token, binding.Agent, string(next), etag); err != nil {
		if errors.Is(err, errDemoPlanStale) {
			s.logger.Warn("Demo 准备：计划过期，停止该 Agent 的写入", "agent", binding.Agent)
			return demoActionStale, nil
		}
		return "", err
	}
	s.logger.Info("Demo 准备：已写入只读知识绑定", "agent", binding.Agent)
	return demoActionApplied, nil
}

// resolveEndpoint 解析 `--server` 与凭据：`--server` 缺省从 `http.addr` 派生（通配
// 监听改写为同族回环）；配置装载失败且未给 `--server` 直接失败；明文 HTTP 只允许
// localhost / 回环地址。token 只读（`--token-file` → 配置 → 实例私有文件）。
// resolveEndpoint validates the endpoint and reads the token; it never mints one.
func (s *DemoPrepareService) resolveEndpoint(req cli.DemoPrepareRequest) (*url.URL, string, error) {
	raw := strings.TrimSpace(req.Server)
	if raw == "" {
		derived, err := s.deriveServer()
		if err != nil {
			return nil, "", err
		}
		raw = derived
	}
	base, err := url.Parse(raw)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Path != "" && base.Path != "/") || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, "", errors.New("--server 必须是无凭据、查询或路径的 HTTP(S) 实例地址")
	}
	if base.Scheme == "http" && !demoLoopbackHost(base.Hostname()) {
		return nil, "", errors.New("远端实例须使用 HTTPS，以保护凭据")
	}
	base.Path = ""
	base.RawPath = ""
	base.RawQuery = ""
	base.ForceQuery = false
	token, _, err := s.readToken(req)
	if err != nil {
		return nil, "", err
	}
	return base, token, nil
}

// deriveServer 从配置的 `http.addr` 派生管理面地址：通配监听（`0.0.0.0` / `::`）
// 改写为同族回环，其它地址原样使用——远端明文会被 `resolveEndpoint` 拒绝，用户必须
// 显式给 `--server https://...`。
// deriveServer rewrites a wildcard listener to the same-family loopback; a remote
// plaintext address is then refused and must be given explicitly as https.
func (s *DemoPrepareService) deriveServer() (string, error) {
	if s.cfgErr != nil {
		if errors.Is(s.cfgErr, fs.ErrNotExist) {
			return "", fmt.Errorf("未找到配置文件 %s，无法推导管理面地址；请先执行 basalt init，或用 --server 显式指定",
				filepath.Join(s.workDir, configFileName))
		}
		return "", errors.New("配置文件装载失败，无法推导管理面地址；请修复配置或用 --server 显式指定")
	}
	if s.cfg == nil || strings.TrimSpace(s.cfg.HTTP.Addr) == "" {
		return "", errors.New("配置没有 http.addr，无法推导管理面地址；请用 --server 显式指定")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(s.cfg.HTTP.Addr))
	if err != nil || port == "" {
		return "", errors.New("配置项 http.addr 不是可解析的 host:port，无法推导管理面地址；请用 --server 显式指定")
	}
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// readToken 按 `--token-file` → 配置 → 实例私有 `admin-token` 的顺序只读取用凭据。
// readToken reads the token from the explicit file, the config, or the private file.
func (s *DemoPrepareService) readToken(req cli.DemoPrepareRequest) (string, string, error) {
	if path := strings.TrimSpace(req.TokenFile); path != "" {
		raw, err := readFlowCLIFile(path, demoAdminTokenMaxBytes, true)
		if err != nil {
			return "", "", fmt.Errorf("--token-file 不可用：%w", err)
		}
		token, err := demoValidateToken(string(raw))
		if err != nil {
			return "", "", err
		}
		return token, "token-file", nil
	}
	configured := ""
	if s.cfgErr == nil && s.cfg != nil {
		configured = strings.TrimSpace(s.cfg.HTTP.AdminAuth.Token)
	}
	return readAdminTokenForClient(s.workDir, configured)
}

// readAdminTokenForClient 只读取管理凭据：配置值优先，其次实例私有 `admin-token`。
// **不生成**新 token——那会把"命令顺手铸钥"变成新的凭据来源。
// readAdminTokenForClient never mints a token: a command must not become a credential
// source.
func readAdminTokenForClient(workDir, configured string) (token, source string, err error) {
	if configured != "" {
		token, err := demoValidateToken(configured)
		if err != nil {
			return "", "", err
		}
		return token, "config", nil
	}
	path, err := defaultAdminTokenFile(workDir)
	if errors.Is(err, errInstanceIdentityMissing) {
		return "", "", errors.New("实例名册尚未建立，无法定位实例私有 admin-token；请先执行 basalt init / serve，或显式配置 http.admin_auth.token")
	}
	if err != nil {
		return "", "", errors.New("无法定位实例私有 admin-token：请先执行 basalt init / serve")
	}
	token, err = readAdminTokenFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("没有可用的管理面凭据：配置 http.admin_auth.token 为空，实例私有文件 %s 不存在；请先在管理台完成初始化，或显式配置 token", path)
	}
	if err != nil {
		return "", "", err
	}
	token, err = demoValidateToken(token)
	if err != nil {
		return "", "", err
	}
	return token, "file", nil
}

// demoValidateToken 校验凭据形态：去空白后非空、≤4096 字节、不含 NUL / CR / LF /
// 空格与未解析的 `${` 占位。错误文案**不回显**取值。
// demoValidateToken checks the shape without ever echoing the value.
func demoValidateToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" || len(token) > demoAdminTokenMaxBytes ||
		strings.ContainsAny(token, "\x00\r\n\t ") || strings.Contains(token, "${") {
		return "", errors.New("管理面凭据为空或含非法字符：请使用实例私有 admin-token 或配置 http.admin_auth.token")
	}
	return token, nil
}

// demoLoopbackHost 判断主机名是否是回环（`localhost` 或回环 IP）。
// demoLoopbackHost reports whether the host is loopback by name or address.
func demoLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listCollections 读活动集合清单（`GET /api/v1/collections`）。
// listCollections lists the active collections.
func (s *DemoPrepareService) listCollections(ctx context.Context, base *url.URL, token string) ([]collectionView, error) {
	data, status, err := s.call(ctx, http.MethodGet, base, token, "/api/v1/collections", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errors.New("管理面拒绝了集合列表请求")
	}
	var views []collectionView
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &views); err != nil {
			return nil, errors.New("管理面返回的集合列表无法解析")
		}
	}
	return views, nil
}

// listDocuments 读一个集合的文档清单（`GET /api/v1/collections/{id}/documents`）。
// listDocuments lists one collection's documents.
func (s *DemoPrepareService) listDocuments(ctx context.Context, base *url.URL, token, collectionID string) ([]documentView, error) {
	data, status, err := s.call(ctx, http.MethodGet, base, token,
		"/api/v1/collections/"+url.PathEscape(collectionID)+"/documents", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errors.New("管理面拒绝了文档列表请求")
	}
	var views []documentView
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &views); err != nil {
			return nil, errors.New("管理面返回的文档列表无法解析")
		}
	}
	return views, nil
}

// createCollection 建同名集合；已存在（409）时重读清单复用现有 ID，不把冲突当失败。
// createCollection re-reads the list on 409 and reuses the existing id.
func (s *DemoPrepareService) createCollection(ctx context.Context, base *url.URL, token, name string) (collectionView, error) {
	data, status, err := s.call(ctx, http.MethodPost, base, token, "/api/v1/collections", map[string]string{"name": name}, "")
	if err == nil && status == http.StatusOK {
		var view collectionView
		if err := json.Unmarshal(data, &view); err != nil || view.CollectionID == "" {
			return collectionView{}, errors.New("管理面返回的集合结果无法解析")
		}
		return view, nil
	}
	if status != http.StatusConflict {
		if err != nil {
			return collectionView{}, err
		}
		return collectionView{}, fmt.Errorf("创建知识集合 %s 被拒绝（HTTP %d）", name, status)
	}
	listed, listErr := s.listCollections(ctx, base, token)
	if listErr != nil {
		return collectionView{}, listErr
	}
	for _, view := range listed {
		if view.Name == name && view.DeletedAt == "" {
			return view, nil
		}
	}
	return collectionView{}, fmt.Errorf("知识集合 %s 报冲突但清单里找不到活动集合；请刷新后重试", name)
}

// ingestDocument 同步摄取一份文档：`status` 是服务端的 active / failed 终态，HTTP 200
// 只表示"已被处理"，**不**等于成功。
// ingestDocument returns the server's terminal state; HTTP 200 alone is never success.
func (s *DemoPrepareService) ingestDocument(ctx context.Context, base *url.URL, token, collectionID, path string) (ingestView, error) {
	data, status, err := s.call(ctx, http.MethodPost, base, token,
		"/api/v1/collections/"+url.PathEscape(collectionID)+"/documents", map[string]string{"path": path}, "")
	if err != nil {
		return ingestView{}, err
	}
	if status != http.StatusOK {
		return ingestView{}, fmt.Errorf("摄取文档 %s 被拒绝（HTTP %d）", path, status)
	}
	var view ingestView
	if err := json.Unmarshal(data, &view); err != nil || view.Status == "" {
		return ingestView{}, fmt.Errorf("管理面返回的摄取结果无法解析（%s）", path)
	}
	return view, nil
}

// readAgentFilesETag 读受管文件集合的强 ETag（`GET /agents/{name}/files`）：它是
// `POST /files` 的 If-Match 基础值，**必须**在读取正文之前取。
// readAgentFilesETag reads the base file-set ETag before the body.
func (s *DemoPrepareService) readAgentFilesETag(ctx context.Context, base *url.URL, token, agent string) (string, error) {
	data, status, err := s.call(ctx, http.MethodGet, base, token,
		"/api/v1/agents/"+url.PathEscape(agent)+"/files", nil, "")
	// 404 必须**先于**通用错误判定：`call` 对任何非 2xx 都返回错误，先看 err 会让
	// "未安装该角色"退化成一句通用拒绝文案。
	// The 404 check must precede the generic error: call() errors on every non-2xx.
	if status == http.StatusNotFound {
		return "", fmt.Errorf("%w：%s（请先 basalt demo workflow install 并在管理台启用该角色）", errDemoAgentMissing, agent)
	}
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("读取 Agent %s 的文件集合被拒绝（HTTP %d）", agent, status)
	}
	var view struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(data, &view); err != nil || view.ETag == "" {
		return "", fmt.Errorf("管理面返回的 Agent %s 文件集合缺少 ETag", agent)
	}
	return view.ETag, nil
}

// readAgentContent 读 AGENT.md 原文（`GET /agents/{name}/content`）。只读，不写管理
// 操作行。
// readAgentContent reads the raw AGENT.md.
func (s *DemoPrepareService) readAgentContent(ctx context.Context, base *url.URL, token, agent string) (string, error) {
	data, status, err := s.call(ctx, http.MethodGet, base, token,
		"/api/v1/agents/"+url.PathEscape(agent)+"/content", nil, "")
	// 同 readAgentFilesETag：404 先判，再判通用错误。
	// Same as readAgentFilesETag: the 404 check precedes the generic error.
	if status == http.StatusNotFound {
		return "", fmt.Errorf("%w：%s（请先 basalt demo workflow install 并在管理台启用该角色）", errDemoAgentMissing, agent)
	}
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("读取 Agent %s 正文被拒绝（HTTP %d）", agent, status)
	}
	var view struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &view); err != nil || strings.TrimSpace(view.Content) == "" {
		return "", fmt.Errorf("管理面返回的 Agent %s 正文为空或无法解析", agent)
	}
	return view.Content, nil
}

// submitAgentKnowledge 条件写 AGENT.md：只提交一个文件，`If-Match` 必须是基础集合
// ETag。412 上抛为计划过期——**不**重试、**不**换 ETag、**不**改用 `PUT /content`。
// submitAgentKnowledge submits only AGENT.md against the base ETag; a 412 is stale.
func (s *DemoPrepareService) submitAgentKnowledge(ctx context.Context, base *url.URL, token, agent, content, ifMatch string) error {
	body := map[string]any{"files": []map[string]string{{"path": "AGENT.md", "content": content}}}
	_, status, err := s.call(ctx, http.MethodPost, base, token,
		"/api/v1/agents/"+url.PathEscape(agent)+"/files", body, ifMatch)
	if status == http.StatusPreconditionFailed {
		return errDemoPlanStale
	}
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("提交 Agent %s 文件集合被拒绝（HTTP %d）", agent, status)
	}
	return nil
}

// call 发一次管理面请求并解信封：无代理、不跟随重定向、65s 超时、响应 ≤ 2 MiB。
// 连接失败的错误文案只回 scheme + host，不回显路径或凭据。
// call issues one management request with the fixed client discipline; connection
// errors name only the scheme and host.
func (s *DemoPrepareService) call(ctx context.Context, method string, base *url.URL, token, path string, body any, ifMatch string) (json.RawMessage, int, error) {
	target := *base
	target.Path = path
	var raw []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, errors.New("无法编码 Demo 准备请求")
		}
		raw = encoded
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, 0, errors.New("无法创建 Demo 准备请求")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("管理面请求未完成（%s %s://%s）：请确认实例已启动且可达",
			method, base.Scheme, base.Host)
	}
	defer func() { _ = response.Body.Close() }()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, demoPrepareMaxResponseBytes+1))
	if readErr != nil || len(payload) > demoPrepareMaxResponseBytes {
		return nil, response.StatusCode, errors.New("管理面响应读取失败或超过 2 MiB 上限")
	}
	var envelope demoEnvelope
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, response.StatusCode, errors.New("管理面返回了无效响应")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || envelope.Code != "OK" {
		if message := strings.TrimSpace(envelope.Message); message != "" {
			return envelope.Data, response.StatusCode, fmt.Errorf("管理面拒绝了请求（HTTP %d）：%s", response.StatusCode, message)
		}
		return envelope.Data, response.StatusCode, fmt.Errorf("管理面拒绝了请求（HTTP %d）", response.StatusCode)
	}
	return envelope.Data, response.StatusCode, nil
}

// demoBindingState 把正文里已声明的绑定与目标绑定判成三种动作：没声明 → bind；
// 等价 → unchanged；非空且不等价 → conflict（用户已有别的绑定，命令不得覆盖）。
// demoBindingState classifies the declared bindings against the wanted ones.
func demoBindingState(existing, want []core.KnowledgeBinding) string {
	if len(existing) == 0 {
		return demoActionBind
	}
	if demoBindingsEquivalent(existing, want) {
		return demoActionUnchanged
	}
	return demoActionConflict
}

// bindingConflicts 是写前的冲突重判：正文里的绑定非空且不等价即冲突（结构非法也按
// 冲突处理——命令不替用户"修好"再写）。
// bindingConflicts is the pre-write conflict verdict; an invalid block is a conflict.
func bindingConflicts(content string, want []core.KnowledgeBinding) (bool, error) {
	existing, err := workspace.ParseKnowledgeBindings([]byte(content))
	if err != nil {
		return true, nil
	}
	return demoBindingState(existing, want) == demoActionConflict, nil
}

// demoBindingsEquivalent 判断两组绑定是否等价（顺序无关；名字 + 权限逐项相同）。
// demoBindingsEquivalent compares two binding sets order-independently.
func demoBindingsEquivalent(left, right []core.KnowledgeBinding) bool {
	if len(left) != len(right) {
		return false
	}
	wanted := make(map[string]core.KnowledgeAccess, len(right))
	for _, binding := range right {
		wanted[binding.Name] = binding.Access
	}
	for _, binding := range left {
		access, ok := wanted[binding.Name]
		if !ok || access != binding.Access {
			return false
		}
	}
	return true
}

// demoWorkflowDocuments 返回固定的 9 个文档：`bid-request` × 8 与 `bid-product` × 1。
// **不扫描目录**——素材集合由模板冻结，扫描会把用户后来放进来的文件也算进去。
// demoWorkflowDocuments is the fixed nine; it never scans the directory.
func demoWorkflowDocuments() []cli.DemoDocumentPlan {
	documents := make([]cli.DemoDocumentPlan, 0, 9)
	for index := 1; index <= 8; index++ {
		documents = append(documents, cli.DemoDocumentPlan{
			Collection: demoCollectionRequest,
			Path:       fmt.Sprintf("%s/sources/request-%02d.md", demoWorkflowDirRel, index),
		})
	}
	return append(documents, cli.DemoDocumentPlan{
		Collection: demoCollectionProduct,
		Path:       demoWorkflowDirRel + "/sources/product-main.md",
	})
}

// demoWorkflowBindings 返回两个 Agent 的目标绑定：`bid-requirements` → bid-request；
// `bid-technical` → bid-product + bid-request；均只读。
// demoWorkflowBindings is the fixed binding set; both agents are read-only.
func demoWorkflowBindings() []cli.DemoBindingPlan {
	read := core.KnowledgeBinding{Access: core.KnowledgeAccessRead}
	request := read
	request.Name = demoCollectionRequest
	product := read
	product.Name = demoCollectionProduct
	return []cli.DemoBindingPlan{
		{Agent: "bid-requirements", Knowledge: []core.KnowledgeBinding{request}},
		{Agent: "bid-technical", Knowledge: []core.KnowledgeBinding{product, request}},
	}
}
