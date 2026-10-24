//go:build e2e

package e2e

// 本文件是 US-12 管理写面的端到端判卷（模块卡 §4.4 的 `012-e2e`，T065）：真二进制 +
// 临时工作区 + 假模型，覆盖六条产品流程——Agent 创建 → 启用 → 归档 → 恢复、创建 /
// 复制 / 归档后恢复的两条路径、模型连接的版本追加与撤回发布、技能删除的引用闸门、
// 工作区只读浏览与通知渠道 CRUD，以及升级说明的文档断言。
// This file is the end-to-end verdict for the US-12 admin write surface: the real
// binary, a temporary workspace and a fake model drive six product flows, plus a
// documentation assertion over the upgrade notes.
//
// 断言面固定五处：HTTP 信封（状态 / code / message / data）、响应头 ETag、工作区文件
// 系统、生命周期状态文件（generation 的唯一权威载体）与 SQLite 库（runs /
// management_operations）。负向调用一律走 webRequest——postInvoke 在非 OK 信封上直接
// 失败，拿不到状态码与固定文案。
// Assertions read five surfaces only: the HTTP envelope, the ETag header, the
// workspace filesystem, the lifecycle state file (the generation authority) and the
// SQLite database. Negative invokes use webRequest because postInvoke refuses any
// non-OK envelope and would lose the status code.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// —— US-12 端到端判卷视图（逐字段对齐 internal/web/views*.go；未断言的字段省略）——

// adminSkillView 是 SkillView 的判卷副本。只读面**没有** `resource_uid` 载体
// （SkillView 的冻结端口拿不到身份），删除用的 UID 从引用视图取。
// SkillView carries no resource_uid; deletes take the uid from the references view.
type adminSkillView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Path         string   `json:"path"`
	VersionID    int64    `json:"version_id"`
	VersionNo    int      `json:"version_no"`
	ContentHash  string   `json:"content_hash"`
	ETag         string   `json:"etag"`
	FileCount    int      `json:"file_count"`
	ReferencedBy []string `json:"referenced_by"`
}

// adminModelConnectionView 是 ModelConnectionView 的判卷副本。
type adminModelConnectionView struct {
	Name               string   `json:"name"`
	ResourceUID        string   `json:"resource_uid"`
	BaseURL            string   `json:"base_url"`
	APIKeyEnv          string   `json:"api_key_env"`
	DefaultModel       string   `json:"default_model"`
	PublishedVersionID int64    `json:"published_version_id"`
	PublishedVersionNo int      `json:"published_version_no"`
	AppliedVersionID   int64    `json:"applied_version_id"`
	AppliedVersionNo   int      `json:"applied_version_no"`
	ETag               string   `json:"etag"`
	ReferencedBy       []string `json:"referenced_by"`
}

// adminReferencesView 是 ResourceReferencesView 的判卷副本。
type adminReferencesView struct {
	Kind               string   `json:"kind"`
	ID                 string   `json:"id"`
	ResourceUID        string   `json:"resource_uid"`
	PublishedVersionID int64    `json:"published_version_id"`
	Agents             []string `json:"agents"`
	ActiveRuns         []string `json:"active_runs"`
}

// adminNotifyChannelView 是 NotifyChannelView 的判卷副本。
type adminNotifyChannelView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	URLHost     string `json:"url_host"`
}

// adminWorkspaceEntry 是 WorkspaceEntryView 的判卷副本。
type adminWorkspaceEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// adminWorkspaceFile 是 WorkspaceFileView 的判卷副本。
type adminWorkspaceFile struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// adminErrorPayload 是失败信封 `data` 的判卷子集（只取本文件断言的三个白名单键）。
type adminErrorPayload struct {
	Agents      *[]string `json:"agents,omitempty"`
	ActiveRuns  *[]string `json:"active_runs,omitempty"`
	CurrentETag *string   `json:"current_etag,omitempty"`
}

// agentDoc 按管理面写回的同一口径拼 AGENT.md：frontmatter 夹在两条 `---` 之间，
// 正文追加在后（与夹具 writeAgent 的落盘形态一致）。
// agentDoc frames AGENT.md exactly like the fixture writer does.
func agentDoc(frontmatter, body string) string {
	return "---\n" + strings.TrimRight(frontmatter, "\n") + "\n---\n\n" +
		strings.TrimRight(body, "\n") + "\n"
}

// adminInvoke 发一次无状态调用并保留状态码（负向断言必须看到 409 / 404 与固定文案）。
// adminInvoke keeps the status code, which postInvoke would discard.
func adminInvoke(t *testing.T, addr, name, message string) *webResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatalf("编码 invoke 请求体失败: %v", err)
	}
	return webRequest(t, addr, http.MethodPost, "/api/v1/agents/"+name+"/invoke", string(body), nil)
}

// adminVersions 读一个资源的版本列表（新版本在前）。
// adminVersions lists one resource's versions, newest first.
func adminVersions(t *testing.T, addr, kind, id string) []adminVersionRow {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/resources/"+kind+"/"+id+"/versions", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取资源版本")
	var rows []adminVersionRow
	decodeWebData(t, resp, &rows, "读取资源版本")
	return rows
}

// adminReferences 读一个资源的引用视图。
// adminReferences reads one resource's reference view.
func adminReferences(t *testing.T, addr, kind, id string) adminReferencesView {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/resources/"+kind+"/"+id+"/references", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取资源引用")
	var view adminReferencesView
	decodeWebData(t, resp, &view, "读取资源引用")
	return view
}

// waitResourceReferences 轮询引用视图直到 ready 成立：引用索引由注册表重载驱动，
// 文件提交成功之后仍有一段去抖窗口。
// waitResourceReferences polls the reference view across the reload debounce window.
func waitResourceReferences(t *testing.T, addr, kind, id, what string, ready func(adminReferencesView) bool) adminReferencesView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last adminReferencesView
	for time.Now().Before(deadline) {
		last = adminReferences(t, addr, kind, id)
		if ready(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时：最后一次引用视图 %+v", what, last)
	return adminReferencesView{}
}

// adminTree 读工作区目录树；path 为空串表示浏览根（此时不带查询参数）。
// adminTree reads one workspace directory; an empty path means the browse root.
func adminTree(t *testing.T, addr, path string) []adminWorkspaceEntry {
	t.Helper()
	target := "/api/v1/workspace/tree"
	if path != "" {
		target += "?path=" + url.QueryEscape(path)
	}
	resp := webRequest(t, addr, http.MethodGet, target, "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "浏览工作区目录")
	var entries []adminWorkspaceEntry
	decodeWebData(t, resp, &entries, "浏览工作区目录")
	return entries
}

// adminReadWorkspaceFile 读一个工作区文件的只读预览。
// adminReadWorkspaceFile reads the read-only preview of one workspace file.
func adminReadWorkspaceFile(t *testing.T, addr, path string) adminWorkspaceFile {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet,
		"/api/v1/workspace/file?path="+url.QueryEscape(path), "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取工作区文件")
	var file adminWorkspaceFile
	decodeWebData(t, resp, &file, "读取工作区文件")
	return file
}

// adminArchived 读归档列表。
// adminArchived lists the archived agents.
func adminArchived(t *testing.T, addr string) []adminArchiveRow {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/archived-agents", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取归档列表")
	var rows []adminArchiveRow
	decodeWebData(t, resp, &rows, "读取归档列表")
	return rows
}

// findArchive 在归档列表里按归档标识找一行。
// findArchive picks one row of the archive listing by archive id.
func findArchive(t *testing.T, rows []adminArchiveRow, archiveID string) adminArchiveRow {
	t.Helper()
	for _, row := range rows {
		if row.ArchiveID == archiveID {
			return row
		}
	}
	t.Fatalf("归档列表缺少 %s（实际 %+v）", archiveID, rows)
	return adminArchiveRow{}
}

// requireAdminState 断言一个生命周期读数处于给定状态（写结果视图可能零值投影代数，
// generation 一律另读状态文件）。
// requireAdminState asserts the lifecycle state of one reading.
func requireAdminState(t *testing.T, view adminLifecycleView, state, label string) {
	t.Helper()
	if view.State == nil || *view.State != state {
		t.Fatalf("%s：生命周期状态 = %v，期望 %q", label, view.State, state)
	}
}

// requireManagedFile 断言受管文件集合里有该文件。
// requireManagedFile asserts the managed file set contains the file.
func requireManagedFile(t *testing.T, files []adminFileView, path, label string) {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return
		}
	}
	t.Fatalf("%s：受管文件集合缺少 %s（实际 %+v）", label, path, files)
}

// requireWorkspaceEntry 断言目录树里有该条目且类型一致。
// requireWorkspaceEntry asserts the entry exists with the expected type.
func requireWorkspaceEntry(t *testing.T, entries []adminWorkspaceEntry, path, kind string) {
	t.Helper()
	for _, entry := range entries {
		if entry.Path == path {
			if entry.Type != kind {
				t.Fatalf("工作区条目 %s 的类型 = %q，期望 %q", path, entry.Type, kind)
			}
			return
		}
	}
	t.Fatalf("工作区缺少条目 %s（实际 %+v）", path, entries)
}

// requireNoResidue 断言目录树里没有 staging / backup 残骸（后缀判定，大小写不敏感）。
// requireNoResidue asserts no staging/backup residue shows up in the tree.
func requireNoResidue(t *testing.T, entries []adminWorkspaceEntry, label string) {
	t.Helper()
	for _, entry := range entries {
		lower := strings.ToLower(entry.Path)
		if strings.HasSuffix(lower, ".staging") || strings.HasSuffix(lower, ".backup") {
			t.Fatalf("%s：目录树不得出现残骸条目 %s", label, entry.Path)
		}
	}
}

// requireNoPath 断言路径不存在（失败路径必须零残余）。
// requireNoPath asserts the path does not exist.
func requireNoPath(t *testing.T, path, label string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s：%s 不应存在（失败必须零残余）", label, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s：检查 %s 失败: %v", label, path, err)
	}
}

// TestAdminConsoleE2E_FullFlow 覆盖 Agent 的创建 → 启用 → 归档 → 恢复主线：
// 创建默认停用且 generation=1、启用后 HTTP 立即可调用、归档把目录移入归档区并让
// 调用 404、原路恢复回到停用且 generation 递增；四类写操作各留一条 success 操作行。
func TestAdminConsoleE2E_FullFlow(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("reporter 已启用并开始工作"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	startServe(t, h, addr)

	var created adminFilesView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        "reporter",
		"description": "每日汇报",
		"model":       fixtureModel,
	}, nil)
	requireWriteOK(t, resp, &created, "创建 Agent")
	if created.Agent != "reporter" {
		t.Fatalf("创建 Agent：agent = %q，期望 reporter", created.Agent)
	}
	requireAdminState(t, created.Lifecycle, "disabled", "创建 Agent")
	if created.ETag == "" {
		t.Fatalf("创建 Agent：etag 为空")
	}
	requireManagedFile(t, created.Files, "AGENT.md", "创建 Agent")
	state := readLifecycleStateFile(t, h, "reporter")
	if state.State != "disabled" || state.Generation != 1 {
		t.Fatalf("创建后状态文件：state=%q generation=%d，期望 disabled/1",
			state.State, state.Generation)
	}
	requireNoPath(t, filepath.Join(h.Root, ".basalt", "agents", "reporter.staging"), "创建 Agent")

	files := adminAgentFiles(t, addr, "reporter")
	requireAdminState(t, files.Lifecycle, "disabled", "读取 Agent 文件集合")

	// 停用态调用：注册完成之后仍必须被准入闸门拒绝（409 + 固定原因）。
	waitAgentRegistered(t, addr, "reporter")
	resp = adminInvoke(t, addr, "reporter", "请汇报")
	requireWebStatus(t, resp, http.StatusConflict, "AGENT_UNAVAILABLE",
		"Agent「reporter」已停用，请先启用后再执行任务。",
		"停用态调用")

	waitAgentEnabled(t, addr, "reporter", 1)
	state = readLifecycleStateFile(t, h, "reporter")
	if state.State != "active" || state.Generation != 2 {
		t.Fatalf("启用后状态文件：state=%q generation=%d，期望 active/2",
			state.State, state.Generation)
	}
	reading := invokeAgent(t, addr, "reporter", "请汇报")
	if reading.Status != "completed" {
		t.Fatalf("启用后调用状态 = %q，期望 completed（原因 %q）", reading.Status, reading.Reason)
	}
	if reading.FinalText != "reporter 已启用并开始工作" {
		t.Fatalf("启用后调用回复 = %q", reading.FinalText)
	}

	var archived adminArchiveRow
	resp = adminWrite(t, addr, http.MethodDelete, "/api/v1/agents/reporter", nil, nil)
	requireWriteOK(t, resp, &archived, "归档 Agent")
	if archived.Agent != "reporter" {
		t.Fatalf("归档 Agent：agent = %q，期望 reporter", archived.Agent)
	}
	if !strings.HasPrefix(archived.ArchiveID, "reporter-") {
		t.Fatalf("归档标识 = %q，期望以 reporter- 开头（名字 + UTC 时间戳）", archived.ArchiveID)
	}
	if archived.ContentHash == "" {
		t.Fatalf("归档 Agent：content_hash 为空")
	}
	if archived.RestoredAt != "" || archived.RestoredAgentName != "" {
		t.Fatalf("刚归档的记录不应带恢复字段：%+v", archived)
	}
	archiveDir := filepath.Join(h.Root, ".basalt", "archive", archived.ArchiveID)
	requireContains(t, readText(t, filepath.Join(archiveDir, "AGENT.md")), "name: reporter", "归档目录")
	requireNoPath(t, filepath.Join(h.Root, ".basalt", "agents", "reporter"), "归档后")
	waitAgentGone(t, addr, "reporter")

	resp = adminInvoke(t, addr, "reporter", "请汇报")
	requireWebStatus(t, resp, http.StatusNotFound, "NOT_FOUND",
		"Agent 请求名不存在或未注册：reporter", "归档后调用")

	row := findArchive(t, adminArchived(t, addr), archived.ArchiveID)
	if row.RestoredAt != "" || row.RestoredAgentName != "" {
		t.Fatalf("归档列表里的新记录不应带恢复字段：%+v", row)
	}

	// 原路恢复：空请求体合法（不换名），回到停用且 generation 递增（条件迁移）。
	var restored adminFilesView
	resp = adminWrite(t, addr, http.MethodPost,
		"/api/v1/archived-agents/"+archived.ArchiveID+"/restore", nil, nil)
	requireWriteOK(t, resp, &restored, "恢复 Agent")
	if restored.Agent != "reporter" {
		t.Fatalf("恢复 Agent：agent = %q，期望 reporter", restored.Agent)
	}
	requireAdminState(t, restored.Lifecycle, "disabled", "恢复 Agent")
	state = readLifecycleStateFile(t, h, "reporter")
	if state.State != "disabled" || state.Generation != 4 {
		t.Fatalf("恢复后状态文件：state=%q generation=%d，期望 disabled/4",
			state.State, state.Generation)
	}
	waitAgentRegistered(t, addr, "reporter")
	resp = adminInvoke(t, addr, "reporter", "请汇报")
	requireWebStatus(t, resp, http.StatusConflict, "AGENT_UNAVAILABLE",
		"Agent「reporter」已停用，请先启用后再执行任务。",
		"恢复后调用")

	row = findArchive(t, adminArchived(t, addr), archived.ArchiveID)
	if row.RestoredAt == "" {
		t.Fatalf("恢复后归档记录缺少 restored_at：%+v", row)
	}
	if row.RestoredAgentName != "reporter" {
		t.Fatalf("恢复后 restored_agent_name = %q，期望 reporter", row.RestoredAgentName)
	}

	db := openDB(t, h.dbPath())
	if got := scalarInt(t, db, "SELECT COUNT(*) FROM runs WHERE agent = 'reporter' AND status = 'completed'"); got != 1 {
		t.Fatalf("已完成的 reporter 运行数 = %d，期望 1", got)
	}
	for _, action := range []string{"agent.create", "agent.enable", "agent.archive", "agent.restore"} {
		if got := scalarInt(t, db,
			"SELECT COUNT(*) FROM management_operations WHERE action = ? AND target_id = 'reporter' AND status = 'success'",
			action); got != 1 {
			t.Fatalf("动作 %s 的 success 操作行数 = %d，期望 1", action, got)
		}
	}
}

// TestAdminConsoleE2E_CreateCopyArchivedDefaults 覆盖创建失败面与复制 / 异名恢复：
// 同名创建 400 且零覆盖、非法目录名 400；复制改写 name 并 Bootstrap 为停用；
// 异名恢复同样 Bootstrap 为停用，操作行按新名字点名。
func TestAdminConsoleE2E_CreateCopyArchivedDefaults(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("不应被调用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	startServe(t, h, addr)

	var created adminFilesView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        "origin",
		"description": "源 Agent",
		"model":       fixtureModel,
	}, nil)
	requireWriteOK(t, resp, &created, "创建 Agent")

	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        "origin",
		"description": "重复创建",
		"model":       fixtureModel,
	}, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"同名 Agent 目录已存在，拒绝覆盖：origin", "同名创建")

	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        "a/b",
		"description": "非法名",
		"model":       fixtureModel,
	}, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		`目录名不得含路径分隔符或 NUL："a/b"`, "非法目录名创建")

	var copied adminFilesView
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/origin/copy", map[string]any{
		"new_name": "copycat",
	}, nil)
	requireWriteOK(t, resp, &copied, "复制 Agent")
	if copied.Agent != "copycat" {
		t.Fatalf("复制 Agent：agent = %q，期望 copycat", copied.Agent)
	}
	requireAdminState(t, copied.Lifecycle, "disabled", "复制 Agent")
	copiedState := readLifecycleStateFile(t, h, "copycat")
	if copiedState.State != "disabled" || copiedState.Generation != 1 {
		t.Fatalf("复制后状态文件：state=%q generation=%d，期望 disabled/1",
			copiedState.State, copiedState.Generation)
	}
	requireNoPath(t, filepath.Join(h.Root, ".basalt", "agents", "copycat.staging"), "复制 Agent")
	copiedDoc := readText(t, filepath.Join(h.Root, ".basalt", "agents", "copycat", "AGENT.md"))
	requireContains(t, copiedDoc, "name: copycat", "复制后的 AGENT.md")
	// 正文按原样复制（仍写着源 Agent 的名字），复制只改写 frontmatter 的 name。
	requireAbsent(t, copiedDoc, "name: origin", "复制后的 AGENT.md")

	var archived adminArchiveRow
	resp = adminWrite(t, addr, http.MethodDelete, "/api/v1/agents/origin", nil, nil)
	requireWriteOK(t, resp, &archived, "归档 Agent")

	var restored adminFilesView
	resp = adminWrite(t, addr, http.MethodPost,
		"/api/v1/archived-agents/"+archived.ArchiveID+"/restore",
		map[string]any{"new_name": "revived"}, nil)
	requireWriteOK(t, resp, &restored, "异名恢复 Agent")
	if restored.Agent != "revived" {
		t.Fatalf("异名恢复：agent = %q，期望 revived", restored.Agent)
	}
	requireAdminState(t, restored.Lifecycle, "disabled", "异名恢复 Agent")
	revivedState := readLifecycleStateFile(t, h, "revived")
	if revivedState.State != "disabled" || revivedState.Generation != 1 {
		t.Fatalf("异名恢复后状态文件：state=%q generation=%d，期望 disabled/1（Bootstrap）",
			revivedState.State, revivedState.Generation)
	}
	row := findArchive(t, adminArchived(t, addr), archived.ArchiveID)
	if row.RestoredAgentName != "revived" {
		t.Fatalf("异名恢复后 restored_agent_name = %q，期望 revived", row.RestoredAgentName)
	}

	db := openDB(t, h.dbPath())
	if got := scalarInt(t, db,
		"SELECT COUNT(*) FROM management_operations WHERE action = 'agent.copy' AND target_id = 'copycat' AND status = 'success'"); got != 1 {
		t.Fatalf("agent.copy 的 success 操作行数 = %d，期望 1", got)
	}
	if got := scalarInt(t, db,
		"SELECT COUNT(*) FROM management_operations WHERE action = 'agent.restore' AND target_id = 'revived' AND status = 'success'"); got != 1 {
		t.Fatalf("agent.restore 的 success 操作行数 = %d，期望 1", got)
	}
}

// TestAdminConsoleE2E_ModelConnectionPublishRevert 覆盖模型连接的写入语义：创建即
// 发布并生效、条件写 412 带当前 ETag（缺失 / 弱 / 过期三种）、版本只追加、空体发布
// 幂等、撤回用旧内容创建新版本；候选非法时 400 且磁盘零写入。
func TestAdminConsoleE2E_ModelConnectionPublishRevert(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("不应被调用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	startServe(t, h, addr)

	var created adminModelConnectionView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":          "aux",
		"base_url":      "https://api.example.com/v1",
		"api_key":       "${" + fixtureKeyEnv + "}",
		"default_model": "m-aux",
	}, nil)
	requireWriteOK(t, resp, &created, "创建模型连接")
	if created.APIKeyEnv != fixtureKeyEnv {
		t.Fatalf("创建模型连接：api_key_env = %q，期望 %q（只回引用名）", created.APIKeyEnv, fixtureKeyEnv)
	}
	if created.PublishedVersionNo != 1 || created.AppliedVersionNo != 1 {
		t.Fatalf("创建模型连接：已发布 / 已应用版本 = %d / %d，期望 1 / 1",
			created.PublishedVersionNo, created.AppliedVersionNo)
	}
	if created.ETag == "" {
		t.Fatalf("创建模型连接：etag 为空")
	}

	// 更新时**省略** api_key 表示沿用已发布占位（R4：空串是 400，省略才是保留）。
	// R4: omitting api_key keeps the current value; an empty string is a 400.
	var updated adminModelConnectionView
	resp = adminWrite(t, addr, http.MethodPut, "/api/v1/model-connections/aux", map[string]any{
		"base_url":      "https://api.example.com/v1",
		"default_model": "m-aux-2",
	}, map[string]string{"If-Match": created.ETag})
	requireWriteOK(t, resp, &updated, "更新模型连接")
	if updated.APIKeyEnv != fixtureKeyEnv {
		t.Fatalf("更新模型连接：api_key_env = %q，期望沿用 %q", updated.APIKeyEnv, fixtureKeyEnv)
	}
	if updated.PublishedVersionNo != 2 || updated.DefaultModel != "m-aux-2" {
		t.Fatalf("更新模型连接：版本 = %d、默认模型 = %q，期望 2 / m-aux-2",
			updated.PublishedVersionNo, updated.DefaultModel)
	}

	// 过期 ETag：412 + data.current_etag（条件写让页面重新取 ETag）。
	resp = adminWrite(t, addr, http.MethodPut, "/api/v1/model-connections/aux", map[string]any{
		"base_url":      "https://api.example.com/v1",
		"default_model": "m-aux-3",
	}, map[string]string{"If-Match": created.ETag})
	requireWebStatus(t, resp, http.StatusPreconditionFailed, "PRECONDITION_FAILED",
		"预条件失败：If-Match 与当前发布版本不一致", "过期 ETag 更新")
	var stale adminErrorPayload
	decodeWebData(t, resp, &stale, "过期 ETag 更新")
	if stale.CurrentETag == nil || *stale.CurrentETag != updated.ETag {
		t.Fatalf("过期 ETag 更新：current_etag = %v，期望 %q", stale.CurrentETag, updated.ETag)
	}

	// 缺失 / 弱 ETag 在中间件就被拒绝（412，固定文案），不产生操作行。
	for _, header := range []map[string]string{nil, {"If-Match": "W/" + updated.ETag}} {
		resp = adminWrite(t, addr, http.MethodPut, "/api/v1/model-connections/aux", map[string]any{
			"base_url":      "https://api.example.com/v1",
			"default_model": "m-aux-4",
		}, header)
		requireWebStatus(t, resp, http.StatusPreconditionFailed, "PRECONDITION_FAILED",
			"条件写要求 If-Match 为当前 ETag 的逐字强 ETag", "缺失 / 弱 ETag 更新")
	}

	rows := adminVersions(t, addr, "model_connection", "aux")
	if len(rows) != 2 {
		t.Fatalf("更新后版本数 = %d，期望 2（版本只追加）", len(rows))
	}
	if rows[0].VersionNo != 2 || !rows[0].Published || !rows[0].Applied {
		t.Fatalf("最新版本行 = %+v，期望 v2 且已发布 / 已应用", rows[0])
	}
	if rows[1].VersionNo != 1 || rows[1].Published || rows[1].Applied {
		t.Fatalf("历史版本行 = %+v，期望 v1 且未发布 / 未应用", rows[1])
	}

	// 空体发布 = 发布最新：已发布即幂等，不产生新版本。
	var idempotent adminPublishResult
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/resources/model_connection/aux/publish", nil,
		map[string]string{"If-Match": updated.ETag})
	requireWriteOK(t, resp, &idempotent, "幂等发布")
	if idempotent.PendingRestart {
		t.Fatalf("幂等发布：pending_restart = true，期望 false")
	}
	if idempotent.Version.VersionNo != 2 {
		t.Fatalf("幂等发布：版本号 = %d，期望 2（不追加）", idempotent.Version.VersionNo)
	}
	if got := len(adminVersions(t, addr, "model_connection", "aux")); got != 2 {
		t.Fatalf("幂等发布后版本数 = %d，期望仍为 2", got)
	}

	// 撤回：用旧版本内容创建新版本（历史不被改写）。
	var reverted adminPublishResult
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/resources/model_connection/aux/publish",
		map[string]any{"version_id": rows[1].VersionID},
		map[string]string{"If-Match": updated.ETag})
	requireWriteOK(t, resp, &reverted, "撤回发布")
	if reverted.Version.VersionNo != 3 {
		t.Fatalf("撤回发布：版本号 = %d，期望 3（只追加）", reverted.Version.VersionNo)
	}
	if reverted.Version.RevertedFromVersionID == nil || *reverted.Version.RevertedFromVersionID != rows[1].VersionID {
		t.Fatalf("撤回发布：reverted_from_version_id = %v，期望 %d",
			reverted.Version.RevertedFromVersionID, rows[1].VersionID)
	}
	if !reverted.Version.Applied || reverted.PendingRestart {
		t.Fatalf("撤回发布：applied = %v、pending_restart = %v，期望 true / false",
			reverted.Version.Applied, reverted.PendingRestart)
	}

	// 候选非法：明文凭证与非法协议都被拒绝，且磁盘逐字节不变（名字不落盘）。
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":          "bad-key",
		"base_url":      "https://api.example.com/v1",
		"api_key":       "sk-plaintext",
		"default_model": "m",
	}, nil)
	// R4：明文凭证按凭据模式错误码拒绝（同一个修复入口，不再落到泛化 BAD_REQUEST）。
	// R4 rejects plaintext credentials with the credential-mode code.
	requireWebStatus(t, resp, http.StatusBadRequest, "CREDENTIAL_MODE_REQUIRED",
		"模型连接 bad-key: env 模式的凭据字段必须是精确 ${VAR} 占位",
		"明文凭证创建")

	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":          "bad-url",
		"base_url":      "ftp://api.example.com",
		"api_key":       "${" + fixtureKeyEnv + "}",
		"default_model": "m",
	}, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"invalid definition: 字段 base_url 只接受 http / https", "非法协议创建")

	config := readText(t, filepath.Join(h.Root, "basalt.yaml"))
	requireAbsent(t, config, "bad-key", "候选校验失败的定义不得落盘")
	requireAbsent(t, config, "bad-url", "候选校验失败的定义不得落盘")
}

// TestAdminConsoleE2E_SkillReferenceGate 覆盖技能删除的引用闸门：被 Agent 声明时
// 400 并点名引用者；解除声明后可删除；删除后历史版本仍可读；R4 起同名重建生成新
// `resource_uid`、版本号从 1 重新计（旧 UID 历史不再进按名列表）。
func TestAdminConsoleE2E_SkillReferenceGate(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("不应被调用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	h.writeAgent(t, "writer", agentFrontmatter("writer", []string{"save_memory", "recall_memory"}), "写日报")
	startServe(t, h, addr)

	var created adminSkillView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/skills", map[string]any{
		"name":        "daily-report",
		"description": "每天生成日报",
	}, nil)
	requireWriteOK(t, resp, &created, "新建技能")
	if created.VersionNo != 1 || created.ETag == "" {
		t.Fatalf("新建技能：版本号 = %d、etag = %q，期望 1 且非空", created.VersionNo, created.ETag)
	}
	if created.Path != ".basalt/skills/daily-report" {
		t.Fatalf("新建技能：path = %q", created.Path)
	}
	if created.FileCount != 1 {
		t.Fatalf("新建技能：file_count = %d，期望 1（骨架只有 SKILL.md）", created.FileCount)
	}

	before := adminAgentFiles(t, addr, "writer")
	// R4：提交正文必须自带 `resource_bindings`（服务端不按名补绑），UID 取自发布指针。
	// R4: the submission carries its own bindings; the uid comes from the published pointer.
	binding := bindingsBlock("model_connection", fixtureProvider,
		adminModelConnection(t, addr, fixtureProvider).ResourceUID)
	declared := agentDoc(
		agentFrontmatter("writer", []string{"save_memory", "recall_memory"})+
			"skills:\n  - daily-report\n"+binding,
		"写日报")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/writer/files", map[string]any{
		"files": []map[string]string{{"path": "AGENT.md", "content": declared}},
	}, map[string]string{"If-Match": before.ETag})
	requireWriteOK(t, resp, nil, "提交 AGENT.md（声明技能）")

	refs := waitResourceReferences(t, addr, "skill", "daily-report", "引用视图看见 writer",
		func(view adminReferencesView) bool {
			return len(view.Agents) == 1 && view.Agents[0] == "writer"
		})
	if refs.Kind != "skill" || refs.ID != "daily-report" {
		t.Fatalf("引用视图 = %+v，期望 skill / daily-report", refs)
	}
	if refs.ResourceUID == "" {
		t.Fatalf("引用视图 = %+v，期望带资源身份（删除必须以它为条件）", refs)
	}

	// 只读面：ETag 与响应头一致，description 没有可达来源（run-state I-60）。
	resp = webRequest(t, addr, http.MethodGet, "/api/v1/skills/daily-report", "", nil)
	var read adminSkillView
	requireWriteOK(t, resp, &read, "读取技能")
	if read.ETag == "" || resp.Header.Get("ETag") != read.ETag {
		t.Fatalf("读取技能：响应头 ETag = %q、data.etag = %q", resp.Header.Get("ETag"), read.ETag)
	}
	if read.Path != ".basalt/skills/daily-report" || read.FileCount != 1 || read.VersionNo != 1 {
		t.Fatalf("读取技能：%+v", read)
	}

	// 删除闸门：400 点名引用者，且操作行按失败终态落库。R4 起 DELETE 必须在查询
	// 串携带 `resource_uid`（同名重建不自动接管），缺失时先按 409 身份不符拒绝。
	// R4 requires the delete query to name the current pointer's uid.
	resp = adminWrite(t, addr, http.MethodDelete,
		"/api/v1/skills/daily-report?resource_uid="+refs.ResourceUID, nil,
		map[string]string{"If-Match": read.ETag})
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"skill daily-report 正在被引用，解除引用后才能删除（Agents: writer）", "被引用时删除技能")
	var gate adminErrorPayload
	decodeWebData(t, resp, &gate, "被引用时删除技能")
	if gate.Agents == nil || len(*gate.Agents) != 1 || (*gate.Agents)[0] != "writer" {
		t.Fatalf("删除闸门的 data.agents = %v，期望 [writer]", gate.Agents)
	}

	// 解除声明后引用索引清空，删除成功。
	after := adminAgentFiles(t, addr, "writer")
	cleared := agentDoc(agentFrontmatter("writer", []string{"save_memory", "recall_memory"})+
		binding, "写日报")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/writer/files", map[string]any{
		"files": []map[string]string{{"path": "AGENT.md", "content": cleared}},
	}, map[string]string{"If-Match": after.ETag})
	requireWriteOK(t, resp, nil, "提交 AGENT.md（解除声明）")
	waitResourceReferences(t, addr, "skill", "daily-report", "引用视图清空 writer",
		func(view adminReferencesView) bool { return len(view.Agents) == 0 })

	resp = adminWrite(t, addr, http.MethodDelete,
		"/api/v1/skills/daily-report?resource_uid="+refs.ResourceUID, nil,
		map[string]string{"If-Match": read.ETag})
	requireWriteOK(t, resp, nil, "删除技能")

	// 删除后历史版本仍可读（版本只追加、不随之删除）。
	versions := adminVersions(t, addr, "skill", "daily-report")
	if len(versions) != 1 || versions[0].VersionNo != 1 {
		t.Fatalf("删除后版本列表 = %+v，期望仍能读到 v1", versions)
	}

	// 同名重建：R4 起是**新资源身份**——版本号从 1 重新计，旧 UID 的版本历史仍留在
	// 存储里，但按名列表只回当前指针身份（颗粒度文档 R4.5 / 注 17 的修订口径）。
	// Recreation mints a new uid; per-name reads only ever cover the current pointer.
	var recreated adminSkillView
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/skills", map[string]any{
		"name":        "daily-report",
		"description": "重建",
	}, nil)
	requireWriteOK(t, resp, &recreated, "同名重建技能")
	if recreated.VersionNo != 1 {
		t.Fatalf("同名重建：版本号 = %d，期望 1（新身份从 v1 起）", recreated.VersionNo)
	}
	refreshed := adminReferences(t, addr, "skill", "daily-report")
	if refreshed.ResourceUID == "" || refreshed.ResourceUID == refs.ResourceUID {
		t.Fatalf("同名重建：resource_uid = %q、旧 = %q，期望新身份", refreshed.ResourceUID, refs.ResourceUID)
	}
}

// TestAdminConsoleE2E_WorkspaceAndNotify 覆盖只读浏览的边界（残骸隐藏、越界 400、
// 超限 400 不截断）与通知渠道 CRUD 的凭证纪律（只存 ${VAR}、只回 host、force 严格）。
func TestAdminConsoleE2E_WorkspaceAndNotify(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("不应被调用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithProvider(model.URL())+
		"http:\n  addr: "+addr+"\n  allowed_domains:\n    - hooks.example.com\n"+adminAuthSection())
	h.writeAgent(t, "browser", agentFrontmatter("browser", nil), "浏览器夹具")
	startServeWithEnv(t, h, addr,
		append(fixtureEnv(), "BASALT_E2E_WEBHOOK_URL=https://hooks.example.com/services/e2e"))

	// 残骸与超限文件在 serve 启动**之后**落盘：启动对账不得先把它们清掉，
	// 这样浏览端点看到的才是真实的残留面。
	agentsRoot := filepath.Join(h.Root, ".basalt", "agents")
	writeFile := func(rel, content string) {
		t.Helper()
		path := filepath.Join(agentsRoot, filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", rel, err)
		}
	}
	writeFile("browser.staging", "残骸")
	writeFile("browser/draft.staging", "残骸")
	writeFile("browser/old.backup", "残骸")
	writeFile("browser/big.txt", strings.Repeat("a", 262145))

	root := adminTree(t, addr, "")
	requireWorkspaceEntry(t, root, "browser", "dir")
	requireNoResidue(t, root, "浏览根")

	tree := adminTree(t, addr, "browser")
	requireWorkspaceEntry(t, tree, "browser/AGENT.md", "file")
	requireNoResidue(t, tree, "browser 目录")

	file := adminReadWorkspaceFile(t, addr, "browser/AGENT.md")
	requireContains(t, file.Content, "name: browser", "读取工作区文件")
	if file.Path != "browser/AGENT.md" || file.Truncated {
		t.Fatalf("读取工作区文件：path = %q、truncated = %v", file.Path, file.Truncated)
	}

	resp := webRequest(t, addr, http.MethodGet, "/api/v1/workspace/file", "", nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"path 参数必填：请给出相对工作区根的文件路径", "缺少 path")

	resp = webRequest(t, addr, http.MethodGet,
		"/api/v1/workspace/file?path="+url.QueryEscape("../basalt.yaml"), "", nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", "路径不得含 ..", "越界路径")

	resp = webRequest(t, addr, http.MethodGet,
		"/api/v1/workspace/file?path="+url.QueryEscape("browser.staging"), "", nil)
	requireWebStatus(t, resp, http.StatusNotFound, "NOT_FOUND",
		"工作区路径不存在：browser.staging", "残骸路径")

	resp = webRequest(t, addr, http.MethodGet,
		"/api/v1/workspace/file?path="+url.QueryEscape("browser/big.txt"), "", nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"文件超过 256 KiB 上限，拒绝读取（不截断）", "超限文件")

	// 通知渠道：只存 ${VAR} 引用、只回 host；同名 400；明文地址 400；force 严格。
	var channel adminNotifyChannelView
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/notify-channels", map[string]any{
		"name":        "ops",
		"type":        "webhook",
		"url":         "${BASALT_E2E_WEBHOOK_URL}",
		"description": "每日巡检通知",
	}, nil)
	requireWriteOK(t, resp, &channel, "创建通知渠道")
	if channel.Name != "ops" || channel.Type != "webhook" {
		t.Fatalf("创建通知渠道：%+v", channel)
	}
	if channel.URLHost != "" {
		t.Fatalf("创建通知渠道：url_host = %q，期望空串（占位不解析成地址）", channel.URLHost)
	}
	requireAbsent(t, resp.Raw, "hooks.example.com", "通知渠道响应不得回出解析后的地址")
	requireAbsent(t, resp.Raw, "/services/", "通知渠道响应不得回出解析后的路径")

	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/notify-channels", map[string]any{
		"name":        "ops",
		"type":        "webhook",
		"url":         "${BASALT_E2E_WEBHOOK_URL}",
		"description": "重复创建",
	}, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"通知渠道 ops 已存在，未执行创建", "同名创建通知渠道")

	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/notify-channels", map[string]any{
		"name":        "leaky",
		"type":        "webhook",
		"url":         "https://hooks.example.com/hook",
		"description": "明文地址",
	}, nil)
	// R4：明文地址不再落到泛化的 BAD_REQUEST，而是凭据模式的固定错误码——语义相同
	// （凭证不得明文入库），修复入口更明确。
	// R4 names the failure with the credential-mode code; the rejection is unchanged.
	requireWebStatus(t, resp, http.StatusBadRequest, "CREDENTIAL_MODE_REQUIRED",
		"通知渠道 leaky: env 模式的凭据字段必须是精确 ${VAR} 占位", "明文地址创建")

	resp = adminWrite(t, addr, http.MethodDelete, "/api/v1/notify-channels/ops?force=maybe", nil, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"force 只接受字面 true", "非法 force")
}

// TestUpgradeNotes_Documented 断言升级说明留在运维手册里：MCP 连接前的白名单判定、
// 旧格式多词 command 的兼容与迁移提示、以及改写示例（command + args）。
func TestUpgradeNotes_Documented(t *testing.T) {
	root := mustRepoRoot(t)
	notes := readText(t, filepath.Join(root, "docs", "Operations.md"))
	for _, want := range []string{
		"shell.allowed_commands",
		"http.allowed_domains",
		"旧格式的多词",
		"command: npx",
		"args:",
		`- "@modelcontextprotocol/server-filesystem"`,
		`- "/data"`,
		"改用 `args` 字段",
	} {
		requireContains(t, notes, want, "升级说明必须覆盖 MCP 白名单与旧格式迁移")
	}
}
