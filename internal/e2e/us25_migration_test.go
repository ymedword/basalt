//go:build e2e

package e2e

// US-25 存量迁移验收 · M1（SQLite v6 → PostgreSQL v12）九步与跨 schema 拒绝
// （FR-25-24 / FR-25-25 / FR-25-27 的制品拒绝半段）。
//
// 判据：从一份**真** `init` 工作区出发（真 SQLite v6 库、真名册、真内置安装意图），按 M1
// 的九步逐条跑通：
//
//	① 停服（本案省去长期进程：迁移全程无 serve / gateway 在跑）；
//	② `db export --workspace <ws> --out <pkg>`：有未归类文件时只产出受限核查包，
//	   退出码非零且打印 `未归类摘要`；
//	③ 人工逐条处置 `review/`（本案唯一未归类文件 `notes.txt` → include）；
//	④ `db export --seal <pkg> --review-file <review>`：核查状态 sealed、处置计数可复算；
//	⑤ `db verify --dump <pkg>`：包内资产与行摘要通过；
//	⑥ `db import --dump <pkg>`：单事务导入空目标库；
//	⑦ `db verify --dump <pkg> --target`：目标侧逐表读数通过；jsonb 列的行摘要缺口
//	   必须在报告里显式披露；
//	⑧ `db verify --handoff <copy-root>/.basalt`：副本名册 / 记录 / 密钥 / 目标配置核对；
//	⑨ `db switch --dump <pkg> --workspace <ws>`：写切换事实、回退窗口仍开、源端重盘通过，
//	   并**显式**打印交接门禁"未核对"。
//
// 之后按对象核对：runs（内容抽样到归属与执行者）、management_operations（detail_json 的
// jsonb 内容）、llm_calls / tool_invocations、resource_versions / resource_pointers /
// run_resource_refs（版本与内容哈希一致）、agent_lifecycle（valid 布尔化）、sessions /
// messages，以及 storage_cutover 的源谱系版本与回退窗口。
//
// 源库没有的 `flow_*` / `skill_admission_*`（学习审批） / `kb_*` 对象逐组登记：目标库这些
// 表由空库迁移新建，逐表读数必须为 0——写"源无对象、目标新建空表"，不冒充已迁移。
//
// 知识引用单独成例（FR-25-25）：SQLite v6 谱系从未承载知识对象，`agent_resource_bindings`
// 的 knowledge_collection 计数必须为 0，报告写明原因；四个文档面不得出现"知识已迁移"。
//
// M2（单实例 PG → 多 Worker，FR-25-26）同样单独成例：单实例期先由控制面进程领取并
// 执行一次；之后停写、导出种子、在另一个工作区导入并启动 Worker。M2 前后逐项比对
// Agent UID / 生命周期索引 / 生命周期状态文件字节 / 名册字节 / 业务指纹（runs、flows、
// 审计、知识、学习审批）与旧 bundle 树；**种子导入与 Worker 启动本身零业务写入**。
// 之后由 Worker 领取的执行必须准入通过、钉住同一个旧 bundle，且租约归 Worker。
// 非空 Flow 与学习审批对象由夹具预先写入（挂在同一条管理操作上），完整性在 M2 前后
// 逐字比对，不依赖"源库本来就有"。
//
// 跨 schema 拒绝（FR-25-27）：用**旧版本制品**（提交 138382f 构建的 v11 二进制）打开本
// 用例建好的 v12 库，必须拒绝并点名两端版本；拒绝后库版本不变。制品缺失 / 无法构建时
// 跳过并写明理由——不拿当前二进制冒充旧制品。
//
// 逆改点（改坏这里就会红）：
//   - 让导出在存在未归类文件时仍"可交付"（退出码 0 / 缺 `未归类摘要`）：第 ② 步红；
//   - 让封包不核对摘要或跳过处置计数：第 ④ 步红；
//   - 让导入接受未封包 / 未复核的包：第 ⑥ 步红；
//   - 让目标侧复核跳过 jsonb 表却不披露缺口：第 ⑦ 步缺 `行摘要按设计不在目标侧比对`；
//   - 让切换不写 storage_cutover，或把 first_write_at 提前写成非 NULL：第 ⑨ 步与 PG 读数红；
//   - 让切换改写源工作区（资产清单 / 库文件身份漂移）：`源端重盘` 失败或扫码红；
//   - 把源无对象冒充已迁移（目标 kb_* 表出现行 / 文档写"知识已迁移"）：知识例红；
//   - 让种子导入或 Worker 启动改写业务表（含生命周期索引、发布指针、流程、知识、
//     学习审批）：M2 的前后业务指纹不一致；
//   - 让 M2 后的执行钉住"当前工作副本"而不是发布指针里的旧 bundle：bundle 哈希断言红；
//   - 让 Worker 从种子启动时生成新实例 / 覆盖名册：名册与生命周期状态文件字节变化；
//   - 让旧二进制放行新库：跨 schema 例红。
//
// The M1 export/seal/import/verify/switch chain is exercised end to end against a real
// SQLite v6 workspace and a real PostgreSQL target; objects are sampled by content, and
// source-absent object groups are recorded as "no source object, empty target table".
// M2 seeds a second workspace from the single-instance control plane and proves the
// import and worker boot write no business rows: agent identity, lifecycle index,
// roster bytes, the governance fingerprint and the old bundle tree stay byte-identical,
// and the worker's admitted run still pins the old bundle.
// A pinned v11 binary must refuse the v12 database.

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	// us25MigrationBaseMS 是夹具行的固定时间原点：整批行的时间戳可复算，证据里不出现
	// "跑的时候正好几点"这种不可复现的读数。
	us25MigrationBaseMS = int64(1762300000000) // 2026-11-05T00:00:00Z 附近
	// us25MigrationOldCommit 是**旧版本制品**的钉死提交（US-22 收口、schema 支持到 v11），
	// 它是 HEAD 的祖先；跨 schema 拒绝必须由这个制品证明，不用工作区里的当前二进制。
	us25MigrationOldCommit = "138382f7bd6360cc4284416659e1785bbd696820"
	// us25MigrationNotes 是唯一未归类文件的正文；处置为 include。
	us25MigrationNotes = "operator notes\n"
)

var (
	us25MigrationSkillHash    = strings.Repeat("a1", 32)
	us25MigrationConnHash     = strings.Repeat("b2", 32)
	us25MigrationStateHash    = strings.Repeat("c3", 32)
	us25MigrationManifestHash = strings.Repeat("d4", 32)
)

// us25MigrationEnv 把实例私有数据根钉进用例自己的临时目录：`init` 的首次准备会把
// `provisioning.json` 与名册写进 `LOCALAPPDATA`（Windows）/ `XDG_DATA_HOME`（Linux）/
// `HOME`（macOS）下的 `basalt/instances/<id>`——不钉住就会读写真实用户目录，互相污染。
// us25MigrationEnv pins the instance data root into a per-test directory on all platforms.
func us25MigrationEnv(t *testing.T) []string {
	t.Helper()
	dataRoot := filepath.Join(t.TempDir(), "instance-data")
	return []string{
		"LOCALAPPDATA=" + dataRoot,
		"APPDATA=" + dataRoot,
		"HOME=" + dataRoot,
		"XDG_DATA_HOME=" + dataRoot,
	}
}

// us25MigrationSource 是种子完成后的源侧事实：资产盘点之外，源库里有几张表、有没有
// 知识 / Flow / 学习审批对象，都是后面"源无对象"登记的依据。
// us25MigrationSource records what the seeded SQLite lineage actually contains.
type us25MigrationSource struct {
	Workspace       string
	TableCount      int
	FlowTables      int
	KBTables        int
	AdmissionTables int
	Rows            int
}

// us25SeedSource 在全新工作区上跑 `init`，然后直接向 SQLite v6 库写 16 行跨 10 张表的
// 夹具，并把日志模式切回 DELETE：WAL 的 `-wal` / `-shm` 会在导出后被打开时重生，
// `compareDBFiles` 把"存在性 true → false"判成源库漂移，切换随即拒绝。
//
// 16 行覆盖：会话 1、消息 2、Run 2、模型调用 2、工具调用 1、管理操作 2、资源版本 2、
// 资源指针 1、Run 资源引用 2、Agent 生命周期 1——每一行都有内容抽样意义，不是为了凑数。
// us25SeedSource initialises a real workspace, seeds 16 rows across 10 tables and returns
// the SQLite journal to DELETE so the source file identity stays stable across commands.
func us25SeedSource(t *testing.T, h *harness, env []string) us25MigrationSource {
	t.Helper()
	res := h.run(t, env, "init")
	us25MigrationRequireExit(t, h, res, 0, "init 建出 SQLite v6 工作区")

	db := openSQLite(t, h.dbPath())
	// 单连接：PRAGMA 与后续写入落在同一条连接上，切回 DELETE 后关闭即不再留下 WAL。
	db.SetMaxOpenConns(1)

	source := us25MigrationSource{Workspace: filepath.Join(h.Root, ".basalt")}
	source.TableCount = int(us25MigrationScalarInt(t, db, "源库表数",
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`, nil))
	source.FlowTables = int(us25MigrationScalarInt(t, db, "源库 flow_* 表数",
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 5) = 'flow_'`, nil))
	source.KBTables = int(us25MigrationScalarInt(t, db, "源库 kb_* 表数",
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 3) = 'kb_'`, nil))
	source.AdmissionTables = int(us25MigrationScalarInt(t, db, "源库 skill_admission_* 表数",
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 16) = 'skill_admission_'`, nil))

	base := us25MigrationBaseMS
	statements := []struct {
		label string
		query string
		args  []any
	}{
		{
			"管理操作 1（技能发布）", `INSERT INTO management_operations
			(operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
			 status, result_code, detail_json, created_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"op-mig-fixture-0001", "skill.publish", "skill", "demo-skill", "cli:e2e-fixture",
				"cli", "cli", "success", "OK", `{"note":"demo-skill"}`, base + 1000, base + 1200,
			},
		},
		{
			"管理操作 2（Agent 启用）", `INSERT INTO management_operations
			(operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
			 status, result_code, detail_json, created_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"op-mig-fixture-0002", "agent.enable", "agent", "demo-agent", "cli:e2e-fixture",
				"cli", "cli", "success", "OK", `{"note":"demo-agent"}`, base + 2000, base + 2500,
			},
		},
		{
			"会话", `INSERT INTO sessions
			(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"sess-mig-1", "demo-agent", "cli", "local", "active", 2, base, base + 3000},
		},
		{
			"消息 1", `INSERT INTO messages
			(id, session_id, run_id, seq, role, content, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{int64(1), "sess-mig-1", "run-mig-1", 1, "user", "hello", base + 100},
		},
		{
			"消息 2", `INSERT INTO messages
			(id, session_id, run_id, seq, role, content, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{int64(2), "sess-mig-1", "run-mig-1", 2, "assistant", "world", base + 200},
		},
		{
			"Run 1（CLI 成功）", `INSERT INTO runs
			(run_id, session_id, agent, channel, user_id, trigger, status, iterations, tool_calls, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"run-mig-1", "sess-mig-1", "demo-agent", "cli", "local", "user", "completed",
				1, 1, base + 100, base + 900,
			},
		},
		{
			"Run 2（定时失败）", `INSERT INTO runs
			(run_id, session_id, agent, channel, user_id, trigger, status, reason, iterations, tool_calls, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"run-mig-2", nil, "demo-agent", "scheduler", "scheduler", "cron", "failed", "timeout",
				0, 0, base + 5000, base + 6000,
			},
		},
		{
			"模型调用 1（成功）", `INSERT INTO llm_calls
			(id, run_id, session_id, round, provider, model, prompt_tokens, completion_tokens,
			 total_tokens, duration_ms, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				int64(1), "run-mig-1", "sess-mig-1", 1, "stub", "stub-model", 10, 5, 15, 42,
				"success", base + 150,
			},
		},
		{
			"模型调用 2（超时失败）", `INSERT INTO llm_calls
			(id, run_id, session_id, round, provider, model, prompt_tokens, completion_tokens,
			 total_tokens, duration_ms, status, error_message, error_kind, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				int64(2), "run-mig-2", nil, 1, "stub", "stub-model", 0, 0, 0, 30000,
				"failed", "stub timeout", "timeout", base + 5500,
			},
		},
		{
			"工具调用", `INSERT INTO tool_invocations
			(id, run_id, session_id, tool_call_id, tool_name, input_json, result_json, status,
			 duration_ms, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				int64(1), "run-mig-1", "sess-mig-1", "call-1", "echo", `{"text":"hi"}`,
				`{"text":"hi"}`, "success", 7, base + 300, base + 307,
			},
		},
		{
			"资源版本 1（技能）", `INSERT INTO resource_versions
			(id, resource_kind, resource_uid, resource_id, version_no, content_hash,
			 definition_yaml, manifest_json, storage_path, created_at, operation_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				int64(1), "skill", "rs_skill_demo", "demo-skill", 1, us25MigrationSkillHash,
				nil, `[{"path":"SKILL.md","sha256":"` + us25MigrationManifestHash + `","size":12}]`,
				"skills/demo-skill/v1", base + 2000, "op-mig-fixture-0001",
			},
		},
		{
			"资源版本 2（模型连接）", `INSERT INTO resource_versions
			(id, resource_kind, resource_uid, resource_id, version_no, content_hash,
			 definition_yaml, manifest_json, storage_path, created_at, operation_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				int64(2), "model_connection", "rs_conn_demo", "demo-conn", 1, us25MigrationConnHash,
				"name: demo-conn\nprovider: stub\n", nil, nil, base + 2100, "op-mig-fixture-0001",
			},
		},
		{
			"资源指针（技能已发布）", `INSERT INTO resource_pointers
			(resource_kind, resource_id, resource_uid, published_version_id, applied_version_id,
			 scope, owner_agent_uid, revision, updated_at, operation_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"skill", "demo-skill", "rs_skill_demo", int64(1), int64(1), "shared", nil, 1,
				base + 2200, "op-mig-fixture-0001",
			},
		},
		{
			"Run 资源引用 1（技能）", `INSERT INTO run_resource_refs
			(run_id, resource_kind, resource_id, resource_uid, resource_name, version_id, content_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"run-mig-1", "skill", "demo-skill", "rs_skill_demo", "demo-skill",
				int64(1), us25MigrationSkillHash,
			},
		},
		{
			"Run 资源引用 2（模型连接）", `INSERT INTO run_resource_refs
			(run_id, resource_kind, resource_id, resource_uid, resource_name, version_id, content_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"run-mig-1", "model_connection", "demo-conn", "rs_conn_demo", "demo-conn",
				int64(2), us25MigrationConnHash,
			},
		},
		{
			"Agent 生命周期", `INSERT INTO agent_lifecycle
			(agent_name, agent_uid, uid_source, state, generation, state_file_hash, valid,
			 error_message, updated_at, operation_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{
				"demo-agent", "ag_demo", "state_file", "active", 1, us25MigrationStateHash,
				1, nil, base + 2500, "op-mig-fixture-0002",
			},
		},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("写夹具行 %s 失败：%v", statement.label, err)
		}
	}
	source.Rows = len(statements)

	if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
		t.Fatalf("把源库日志模式切回 DELETE 失败：%v", err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("源库日志模式 = %q（%v），要求 delete：WAL 残片会让 db switch 判成源库漂移", mode, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭源库写句柄失败：%v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(h.dbPath() + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("关闭源库后仍存在 %s（err=%v）：导出前的源库文件身份必须稳定", suffix, err)
		}
	}

	// 唯一的未归类文件：导出一律先产出受限核查包，逐条处置后才可封包。
	notesPath := filepath.Join(source.Workspace, "notes.txt")
	if err := os.WriteFile(notesPath, []byte(us25MigrationNotes), 0o600); err != nil {
		t.Fatalf("写未归类文件 notes.txt 失败：%v", err)
	}
	return source
}

// us25MigrationResult 是 M1 九步留下的可核对事实。
// us25MigrationResult carries the facts the nine M1 steps established.
type us25MigrationResult struct {
	Package        string
	EntryCount     int
	Unclassified   int
	ReviewDigest   string
	Included       int
	ManifestSHA256 string
	CutoverID      string
	SwitchOutput   string
}

// us25MigrationRunM1 逐条执行 M1 的第 ②～⑨ 步并对每一步的读数做强断言。
// us25MigrationRunM1 executes M1 steps ②–⑨ and asserts each printed reading.
func us25MigrationRunM1(t *testing.T, h *harness, env []string) us25MigrationResult {
	t.Helper()
	result := us25MigrationResult{Package: filepath.Join(h.Root, "migration-package")}
	reviewPath := filepath.Join(h.Root, "unclassified-review.json")

	// ② 导出：有未归类文件时只能产出受限核查包——退出码必须表达"不可交付"。
	export := h.run(t, env, "db", "export", "--workspace", ".basalt", "--out", result.Package)
	exportOut := h.redact(export.Stdout + export.Stderr)
	if export.ExitCode != 1 {
		t.Fatalf("存在未归类文件时 db export 的退出码 = %d，要求 1（受限核查包不可交付）\n%s",
			export.ExitCode, exportOut)
	}
	counts := us25MigrationMatch(t, `资产条目：(\d+) 条（未归类 (\d+) 条）`, exportOut, "db export")
	result.EntryCount = us25MigrationAtoi(t, counts[1], "资产条目")
	result.Unclassified = us25MigrationAtoi(t, counts[2], "未归类条目")
	if result.Unclassified != 1 {
		t.Fatalf("未归类条目 = %d，要求 1（夹具只多写 notes.txt）\n%s", result.Unclassified, exportOut)
	}
	digest := us25MigrationMatch(t, `未归类摘要：([0-9a-f]{64})`, exportOut, "db export")
	result.ReviewDigest = digest[1]

	// ③ 逐条处置：唯一未归类文件 include。复核人 / 时间写死，摘要由清单钉住。
	review := map[string]any{
		"unclassified_digest": result.ReviewDigest,
		"reviewer":            "e2e-operator",
		"reviewed_at":         "2026-11-08T00:00:00Z",
		"decisions": []map[string]any{
			{"rel_path": "notes.txt", "disposition": "include"},
		},
	}
	rawReview, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("序列化复核文件失败：%v", err)
	}
	if err := os.WriteFile(reviewPath, rawReview, 0o600); err != nil {
		t.Fatalf("写复核文件失败：%v", err)
	}

	// ④ 封包：核查状态 sealed，三类处置计数可复算（本案全部进包）。
	seal := h.run(t, env, "db", "export", "--seal", result.Package, "--review-file", reviewPath)
	sealOut := h.redact(seal.Stdout + seal.Stderr)
	us25MigrationRequireExit(t, h, seal, 0, "db export --seal")
	requireContains(t, sealOut, "核查状态：sealed", "db export --seal")
	sealCounts := us25MigrationMatch(t, `处置统计：进包 (\d+) / 排除 (\d+) / 移交 (\d+)`, sealOut, "db export --seal")
	result.Included = us25MigrationAtoi(t, sealCounts[1], "进包条数")
	if got := us25MigrationAtoi(t, sealCounts[2], "排除条数"); got != 0 {
		t.Fatalf("排除条数 = %d，要求 0（夹具没有处置为 exclude 的条目）\n%s", got, sealOut)
	}
	if got := us25MigrationAtoi(t, sealCounts[3], "移交条数"); got != 0 {
		t.Fatalf("移交条数 = %d，要求 0（夹具没有处置为 handoff 的条目）\n%s", got, sealOut)
	}
	if result.Included != result.EntryCount {
		t.Fatalf("进包 %d 条，条目总数 %d，要求相等（本案全部 include）", result.Included, result.EntryCount)
	}

	// ⑤ 包内复核：资产与逐表行摘要都必须通过。
	verifyDump := h.run(t, env, "db", "verify", "--dump", result.Package)
	verifyDumpOut := h.redact(verifyDump.Stdout + verifyDump.Stderr)
	us25MigrationRequireExit(t, h, verifyDump, 0, "db verify --dump")
	requireContains(t, verifyDumpOut, "复核模式：dump", "db verify --dump")
	requireContains(t, verifyDumpOut, "结论：通过", "db verify --dump")

	// ⑥ 导入前：目标库先迁到 v12。`init` 只建 SQLite 源库，PostgreSQL 谱系的建表是运维
	// 单独一步（`db migrate`）；"空目标库"指业务表零行，不是"没有表"。
	h.ensureStore(t)
	importRes := h.run(t, env, "db", "import", "--dump", result.Package)
	importOut := h.redact(importRes.Stdout + importRes.Stderr)
	us25MigrationRequireExit(t, h, importRes, 0, "db import")
	totals := us25MigrationMatch(t, `合计 (\d+) 表 / (\d+) 行`, importOut, "db import")
	if tables := us25MigrationAtoi(t, totals[1], "导入表数"); tables != 32 {
		t.Fatalf("导入表数 = %d，要求 32（迁移清单的固定表集合）\n%s", tables, importOut)
	}
	if rows := us25MigrationAtoi(t, totals[2], "导入行数"); rows != 16 {
		t.Fatalf("导入行数 = %d，要求 16（夹具逐行可数）\n%s", rows, importOut)
	}

	// ⑦ 目标侧复核：jsonb 表（management_operations.detail_json）行摘要按设计跳过，
	// 但缺口必须在报告里显式披露——"没比"与"比过了"不得读成同一件事。
	verifyTarget := h.run(t, env, "db", "verify", "--dump", result.Package, "--target")
	verifyTargetOut := h.redact(verifyTarget.Stdout + verifyTarget.Stderr)
	us25MigrationRequireExit(t, h, verifyTarget, 0, "db verify --target")
	requireContains(t, verifyTargetOut, "复核模式：target", "db verify --target")
	requireContains(t, verifyTargetOut, "结论：通过", "db verify --target")
	requireContains(t, verifyTargetOut, "行摘要按设计不在目标侧比对", "db verify --target")
	requireContains(t, verifyTargetOut, "detail_json", "db verify --target")

	// ⑧ 交接核对：副本工作区 = `<copy-root>/.basalt`，同级放 basalt.yaml。普通存量实例
	// （无主密钥记录）必须判"交接就绪"，同时**显式**打印"未核对目标配置"。
	copyRoot := filepath.Join(h.Root, "handoff-copy")
	if err := os.MkdirAll(filepath.Join(copyRoot, ".basalt"), 0o700); err != nil {
		t.Fatalf("建交接副本目录失败：%v", err)
	}
	for _, rel := range []string{"basalt.db", "instance.json"} {
		us25MigrationCopyFile(t, filepath.Join(h.Root, ".basalt", rel), filepath.Join(copyRoot, ".basalt", rel))
	}
	us25MigrationCopyFile(t, filepath.Join(h.Root, "basalt.yaml"), filepath.Join(copyRoot, "basalt.yaml"))
	handoff := h.run(t, env, "db", "verify", "--handoff", filepath.Join(copyRoot, ".basalt"))
	handoffOut := h.redact(handoff.Stdout + handoff.Stderr)
	us25MigrationRequireExit(t, h, handoff, 0, "db verify --handoff")
	requireContains(t, handoffOut, "结论：交接就绪", "db verify --handoff")
	requireContains(t, handoffOut, "主密钥记录：**无**", "db verify --handoff")
	requireContains(t, handoffOut, "目标配置核对：**未核对**", "db verify --handoff")

	// ⑨ 切换：写切换事实、回退窗口仍开、源端重盘通过；交接门禁不冒充"已核对"。
	switchRes := h.run(t, env, "db", "switch", "--dump", result.Package, "--workspace", ".basalt")
	switchOut := h.redact(switchRes.Stdout + switchRes.Stderr)
	us25MigrationRequireExit(t, h, switchRes, 0, "db switch")
	requireContains(t, switchOut, "源 schema 版本：v6", "db switch")
	requireContains(t, switchOut, "回退窗口：开放（first_write_at 仍为 NULL）", "db switch")
	requireContains(t, switchOut, "源端重盘：已执行（源工作区与清单一致）", "db switch")
	requireContains(t, switchOut, "交接就绪（handoff）：**未核对**", "db switch")
	cutover := us25MigrationMatch(t, `切换记录：(cut_\d+)`, switchOut, "db switch")
	result.CutoverID = cutover[1]
	manifestSHA := us25MigrationMatch(t, `绑定清单 SHA-256：([0-9a-f]{64})`, switchOut, "db switch")
	result.ManifestSHA256 = manifestSHA[1]
	wantSHA, err := fileSHA256(filepath.Join(result.Package, "manifest.json"))
	if err != nil {
		t.Fatalf("计算包清单摘要失败：%v", err)
	}
	if result.ManifestSHA256 != wantSHA {
		t.Fatalf("切换记录的清单摘要 %s 与包内 manifest.json 的 %s 不一致",
			result.ManifestSHA256, wantSHA)
	}
	result.SwitchOutput = switchOut
	return result
}

// TestUS25_Migration_V6ToV12VerifiesObjects 是 M1 主路径 + 对象级核对（FR-25-24）。
// TestUS25_Migration_V6ToV12VerifiesObjects covers FR-25-24.
func TestUS25_Migration_V6ToV12VerifiesObjects(t *testing.T) {
	h := newHarness(t)
	env := us25MigrationEnv(t)
	source := us25SeedSource(t, h, env)
	for prefix, count := range map[string]int{
		"flow_":            source.FlowTables,
		"kb_":              source.KBTables,
		"skill_admission_": source.AdmissionTables,
	} {
		if count != 0 {
			t.Fatalf("源库出现 %s 表 %d 张——SQLite v6 谱系不承载该类对象，夹具与假设不符", prefix, count)
		}
	}
	t.Logf("源库：SQLite v6，%d 张表，写入 %d 行；源无 flow_* / skill_admission_* / kb_* 对象",
		source.TableCount, source.Rows)

	result := us25MigrationRunM1(t, h, env)
	t.Logf("M1 完成：包 %s（%d 条资产，未归类 %d 条，进包 %d 条），切换 %s，清单摘要 %s",
		result.Package, result.EntryCount, result.Unclassified, result.Included,
		result.CutoverID, result.ManifestSHA256)

	db := h.openDB(t)
	// 行数与归属：runs 的 PG 专属列由导入侧显式补齐，抽样到每一列的取值。
	if got := us25MigrationCount(t, db, "runs 行数", `SELECT count(*) FROM runs`); got != 2 {
		t.Fatalf("runs 行数 = %d，要求 2", got)
	}
	var runs string
	if err := db.QueryRow(`SELECT string_agg(
			agent || '|' || channel || '|' || status || '|' || caller_id || '|' ||
			authorizing_kind || '|' || actor, '; ' ORDER BY run_id) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("抽样 runs 归属失败：%v", err)
	}
	wantRuns := "demo-agent|cli|completed|local-admin|admin-builtin|admin-cli; " +
		"demo-agent|scheduler|failed|local-admin|admin-builtin|scheduler"
	if runs != wantRuns {
		t.Fatalf("runs 归属抽样 = %q\n要求 %q", runs, wantRuns)
	}
	if got := us25MigrationCount(t, db, "runs 无会话行", `SELECT count(*) FROM runs WHERE session_id IS NULL`); got != 1 {
		t.Fatalf("无会话 Run 行数 = %d，要求 1（定时 Run 允许无会话）", got)
	}
	// 会话 / 消息：无归属会话保留 NULL（只允许管理面读取），消息正文逐条一致。
	if got := us25MigrationCount(t, db, "sessions 行数", `SELECT count(*) FROM sessions`); got != 1 {
		t.Fatalf("sessions 行数 = %d，要求 1", got)
	}
	if got := us25MigrationCount(t, db, "遗留会话无归属", `SELECT count(*) FROM sessions WHERE caller_id IS NULL`); got != 1 {
		t.Fatalf("caller_id IS NULL 的会话 = %d，要求 1（迁移遗留无归属会话）", got)
	}
	if got := us25MigrationCount(t, db, "messages 行数", `SELECT count(*) FROM messages`); got != 2 {
		t.Fatalf("messages 行数 = %d，要求 2", got)
	}
	var lastMessage string
	if err := db.QueryRow(`SELECT content FROM messages WHERE session_id = 'sess-mig-1' AND seq = 2`).Scan(&lastMessage); err != nil {
		t.Fatalf("抽样消息正文失败：%v", err)
	}
	if lastMessage != "world" {
		t.Fatalf("消息正文 = %q，要求 world", lastMessage)
	}
	// 审计：管理操作按 jsonb 内容抽样，模型调用与工具调用逐项核数。
	if got := us25MigrationCount(t, db, "management_operations 行数",
		`SELECT count(*) FROM management_operations`); got != 2 {
		t.Fatalf("management_operations 行数 = %d，要求 2", got)
	}
	for note, want := range map[string]int64{"demo-skill": 1, "demo-agent": 1} {
		if got := us25MigrationCount(t, db, "管理操作 detail_json",
			`SELECT count(*) FROM management_operations WHERE detail_json->>'note' = $1`, note); got != want {
			t.Fatalf("detail_json->>'note' = %q 的行数 = %d，要求 %d", note, got, want)
		}
	}
	if got := us25MigrationCount(t, db, "llm_calls 行数", `SELECT count(*) FROM llm_calls`); got != 2 {
		t.Fatalf("llm_calls 行数 = %d，要求 2", got)
	}
	if got := us25MigrationCount(t, db, "llm_calls purpose 默认值",
		`SELECT count(*) FROM llm_calls WHERE purpose = 'chat'`); got != 2 {
		t.Fatalf("purpose=chat 的调用数 = %d，要求 2（导入补默认值，不臆造新用途）", got)
	}
	if got := us25MigrationCount(t, db, "失败调用错误种类",
		`SELECT count(*) FROM llm_calls WHERE run_id = 'run-mig-2' AND status = 'failed' AND error_kind = 'timeout'`); got != 1 {
		t.Fatalf("失败调用错误种类行数 = %d，要求 1", got)
	}
	if got := us25MigrationCount(t, db, "tool_invocations 行数",
		`SELECT count(*) FROM tool_invocations`); got != 1 {
		t.Fatalf("tool_invocations 行数 = %d，要求 1", got)
	}
	var toolName string
	if err := db.QueryRow(`SELECT tool_name FROM tool_invocations WHERE run_id = 'run-mig-1'`).Scan(&toolName); err != nil {
		t.Fatalf("抽样工具名失败：%v", err)
	}
	if toolName != "echo" {
		t.Fatalf("工具名 = %q，要求 echo", toolName)
	}
	// 资源引用与技能版本：版本存在、指针指同一版本、引用哈希与版本哈希一致——外键只能
	// 证明"存在"，"指对了版本"必须靠内容哈希比对（与 verify 的引用判据同口径）。
	if got := us25MigrationCount(t, db, "resource_versions 行数",
		`SELECT count(*) FROM resource_versions`); got != 2 {
		t.Fatalf("resource_versions 行数 = %d，要求 2", got)
	}
	var skillVersion string
	if err := db.QueryRow(`SELECT resource_kind || '|' || resource_id || '|' || version_no || '|' || content_hash
			FROM resource_versions WHERE id = 1`).Scan(&skillVersion); err != nil {
		t.Fatalf("抽样技能版本失败：%v", err)
	}
	wantSkillVersion := "skill|demo-skill|1|" + us25MigrationSkillHash
	if skillVersion != wantSkillVersion {
		t.Fatalf("技能版本抽样 = %q\n要求 %q", skillVersion, wantSkillVersion)
	}
	if got := us25MigrationCount(t, db, "资源指针",
		`SELECT count(*) FROM resource_pointers WHERE resource_kind = 'skill' AND
			published_version_id = 1 AND applied_version_id = 1`); got != 1 {
		t.Fatalf("技能指针指向版本的读数 = %d，要求 1（发布与应用均指 v1）", got)
	}
	if got := us25MigrationCount(t, db, "run_resource_refs 行数",
		`SELECT count(*) FROM run_resource_refs WHERE run_id = 'run-mig-1'`); got != 2 {
		t.Fatalf("Run 资源引用行数 = %d，要求 2", got)
	}
	if got := us25MigrationCount(t, db, "引用哈希与版本一致",
		`SELECT count(*) FROM run_resource_refs r
			JOIN resource_versions v ON v.id = r.version_id AND v.content_hash = r.content_hash`); got != 2 {
		t.Fatalf("引用与版本哈希一致的行数 = %d，要求 2（改指同存在的另一版本只在这里能发现）", got)
	}
	// Agent 生命周期：INTEGER 0/1 → PostgreSQL boolean，索引与 UID 原样保留。
	if got := us25MigrationCount(t, db, "agent_lifecycle 行数", `SELECT count(*) FROM agent_lifecycle`); got != 1 {
		t.Fatalf("agent_lifecycle 行数 = %d，要求 1", got)
	}
	if got := us25MigrationCount(t, db, "agent_lifecycle 抽样",
		`SELECT count(*) FROM agent_lifecycle WHERE agent_name = 'demo-agent' AND agent_uid = 'ag_demo' AND state = 'active' AND valid`); got != 1 {
		t.Fatalf("生命周期索引抽样行数 = %d，要求 1（UID / 状态 / valid 布尔化）", got)
	}
	// 切换事实：源谱系 v6、清单摘要与包内文件一致、回退窗口仍开、操作者归 CLI。
	if got := us25MigrationCount(t, db, "storage_cutover 行数",
		`SELECT count(*) FROM storage_cutover WHERE cutover_id = $1`, result.CutoverID); got != 1 {
		t.Fatalf("切换记录 %s 的行数 = %d，要求 1", result.CutoverID, got)
	}
	var cutoverFacts string
	if err := db.QueryRow(`SELECT source_schema_version || '|' || manifest_sha256 || '|' ||
			coalesce(first_write_at, -1) || '|' || operator
			FROM storage_cutover WHERE cutover_id = $1`, result.CutoverID).Scan(&cutoverFacts); err != nil {
		t.Fatalf("抽样切换事实失败：%v", err)
	}
	wantCutover := "6|" + result.ManifestSHA256 + "|-1|"
	if !strings.HasPrefix(cutoverFacts, wantCutover) {
		t.Fatalf("切换事实抽样 = %q\n要求前缀 %q（源 v6、清单摘要一致、回退窗口开放）",
			cutoverFacts, wantCutover)
	}
	if !strings.HasSuffix(cutoverFacts, "|cli:e2e") && !strings.Contains(cutoverFacts, "|cli:") {
		t.Fatalf("切换操作者 = %q，要求 cli:<本机用户>", cutoverFacts)
	}

	// 源无对象逐组登记：目标库这些表由**空库迁移**新建，逐表读数必须为 0。表名逐张枚举
	// 并打印——"表存在但没数据"与"表根本不存在"都不是"迁移成功"。
	for _, group := range []struct {
		prefix string
		want   int
		label  string
	}{
		{"flow_", 17, "Flow 持久流程"},
		{"kb_", 7, "知识库"},
		{"skill_admission_", 4, "技能学习审批"},
	} {
		names := us25MigrationPGTables(t, db, group.prefix)
		if len(names) != group.want {
			t.Fatalf("目标库 %s 表数 = %d，要求 %d（v12 冻结集合）", group.prefix, len(names), group.want)
		}
		for _, name := range names {
			if got := us25MigrationCount(t, db, "空表 "+name,
				`SELECT count(*) FROM "`+name+`"`); got != 0 {
				t.Fatalf("目标表 %s 有 %d 行——源库没有该类对象，不得出现「已迁移」的内容", name, got)
			}
		}
		t.Logf("源无对象、目标新建空表（%s，%s*）：%d 张表逐张读数 0 行——不冒充已迁移",
			group.label, group.prefix, len(names))
	}
}

// TestUS25_Migration_KnowledgeAbsentRecorded 覆盖 FR-25-25：源库无 kb_* 表时知识引用
// 计数为 0、原因可读、文档不出现"知识已迁移"。
// TestUS25_Migration_KnowledgeAbsentRecorded covers FR-25-25.
func TestUS25_Migration_KnowledgeAbsentRecorded(t *testing.T) {
	h := newHarness(t)
	env := us25MigrationEnv(t)
	source := us25SeedSource(t, h, env)
	if source.KBTables != 0 {
		t.Fatalf("源库 kb_* 表数 = %d，要求 0", source.KBTables)
	}
	us25MigrationRunM1(t, h, env)

	db := h.openDB(t)
	knowledgeTables := us25MigrationPGTables(t, db, "kb_")
	if len(knowledgeTables) != 7 {
		t.Fatalf("目标库 kb_* 表数 = %d，要求 7（v12 冻结集合）", len(knowledgeTables))
	}
	for _, name := range knowledgeTables {
		if got := us25MigrationCount(t, db, "知识空表 "+name,
			`SELECT count(*) FROM "`+name+`"`); got != 0 {
			t.Fatalf("kb_* 表 %s 有 %d 行——SQLite v6 源谱系没有知识对象，不得出现迁移内容", name, got)
		}
	}
	// 知识引用只认 `agent_resource_bindings` 的 knowledge_collection 行（AGENT.md
	// `knowledge:` 块的投影）；`run_resource_refs` 的闭集只允许三类资源，天然为 0。
	bindings := us25MigrationCount(t, db, "知识引用计数",
		`SELECT count(*) FROM agent_resource_bindings WHERE resource_kind = 'knowledge_collection'`)
	if bindings != 0 {
		t.Fatalf("知识引用计数 = %d，要求 0", bindings)
	}
	if got := us25MigrationCount(t, db, "Run 知识引用闭集",
		`SELECT count(*) FROM run_resource_refs WHERE resource_kind = 'knowledge_collection'`); got != 0 {
		t.Fatalf("run_resource_refs 出现 knowledge_collection 行 %d 条——闭集不该允许", got)
	}
	t.Logf("知识引用计数 0 的原因：SQLite v6 源谱系从未承载 kb_* 知识对象（迁移清单只覆盖 "+
		"32 张源表，源库 kb_* 表数 = %d），目标库 7 张 kb_* 表由空库迁移新建、agent_resource_bindings "+
		"无 knowledge_collection 行；因此不存在「知识已迁移」，文档也不得如此表述",
		source.KBTables)

	// 文档面不得出现"知识已迁移"的正向断言；用拼接构造，避免本意是反例的字面量
	// 在源码里被当成一次"声称"。
	claim := "知识" + "已迁移"
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败：%v", err)
	}
	for _, rel := range []string{
		"README.md",
		"docs/Operations.md",
		"release/package-README.txt",
		"basalt.yaml.example",
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读取文档面 %s 失败：%v", rel, err)
		}
		if bytes.Contains(raw, []byte(claim)) {
			t.Fatalf("文档面 %s 出现「知识已迁移」——源无 kb_* 对象，这是不可证明的声称", rel)
		}
	}
}

// TestUS25_Migration_CrossSchemaRefusal 覆盖 FR-25-27 的制品拒绝半段：旧版本二进制
// （支持到 v11）不得打开 v12 库。
// TestUS25_Migration_CrossSchemaRefusal covers the artifact-side schema gate of FR-25-27.
func TestUS25_Migration_CrossSchemaRefusal(t *testing.T) {
	h := newHarness(t)
	// 旧二进制要经过配置解析才能打开数据库：先写一份只含 DSN 占位的配置。
	// `ensureStore` 会临时覆盖它、迁完再逐字节还原，因此这里先落盘。
	cfgPath := filepath.Join(h.Root, "basalt.yaml")
	if err := os.WriteFile(cfgPath, []byte("database:\n  dsn: ${"+generatedDSNEnv+"}\n"), 0o600); err != nil {
		t.Fatalf("写配置 %s 失败：%v", cfgPath, err)
	}
	h.ensureStore(t)
	var version int
	if err := h.openDB(t).QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("读取目标库版本失败：%v", err)
	}
	if version != 12 {
		t.Fatalf("目标库版本 = %d，要求 12（当前二进制迁移到 v12）", version)
	}

	oldBinary := us25MigrationLegacyBinary(t)
	// 旧二进制读同一张隔离 schema：`db migrate` 是它打开数据库的最短路径。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, oldBinary, "db", "migrate")
	cmd.Dir = h.Root
	cmd.Env = h.environ(nil)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		t.Fatalf("旧版本制品在 v12 库上执行 db migrate 成功——跨 schema 拒绝失效\n%s",
			h.redact(out.String()))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("运行旧版本制品失败（非退出码类）：%v\n%s", err, h.redact(out.String()))
	}
	output := h.redact(out.String())
	if exitErr.ExitCode() != 1 {
		t.Fatalf("旧版本制品的退出码 = %d，要求 1\n%s", exitErr.ExitCode(), output)
	}
	for _, want := range []string{
		"库版本 12 高于二进制支持的 11",
		"旧二进制不得打开新库",
	} {
		requireContains(t, output, want, "旧版本制品拒绝新库")
	}
	if err := h.openDB(t).QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("拒绝后读取目标库版本失败：%v", err)
	}
	if version != 12 {
		t.Fatalf("拒绝后库版本 = %d，要求仍为 12（拒绝不得改动库）", version)
	}
	t.Logf("跨 schema 拒绝：v11 制品（提交 %s）拒绝打开 v12 库，库版本保持 12",
		us25MigrationOldCommit[:7])
}

// us25MigrationScalarInt 读一个整数读数。
// us25MigrationScalarInt reads one integer scalar.
func us25MigrationScalarInt(t *testing.T, db *sql.DB, label, query string, args []any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s：查询失败：%v", label, err)
	}
	return value
}

// us25MigrationCount 读一条 count(*) 读数。
// us25MigrationCount reads one count(*) reading.
func us25MigrationCount(t *testing.T, db *sql.DB, label, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s：查询失败：%v", label, err)
	}
	return value
}

// us25MigrationPGTables 列出当前 schema 里以 prefix 开头的表名（升序）。表名只允许
// 小写字母 / 数字 / 下划线：拼进 SQL 前先过形态校验，不把数据库读面当成注入面。
// us25MigrationPGTables lists the current schema's tables with the given prefix.
func us25MigrationPGTables(t *testing.T, db *sql.DB, prefix string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND starts_with(table_name, $1)
		ORDER BY table_name`, prefix)
	if err != nil {
		t.Fatalf("枚举 %s* 表失败：%v", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读取 %s* 表名失败：%v", prefix, err)
		}
		if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(name) {
			t.Fatalf("表名 %q 形态非法——拒绝拼进 SQL", name)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 %s* 表失败：%v", prefix, err)
	}
	sort.Strings(names)
	return names
}

// us25MigrationMatch 用正则抓一组读数；抓不到即判红并带出完整输出。
// us25MigrationMatch extracts one regex group match or fails with the full output.
func us25MigrationMatch(t *testing.T, pattern, text, label string) []string {
	t.Helper()
	matches := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if matches == nil {
		t.Fatalf("%s：输出里没有匹配 %q 的读数\n%s", label, pattern, text)
	}
	return matches
}

// us25MigrationAtoi 解析命令输出里的十进制读数。
// us25MigrationAtoi parses one decimal reading from the command output.
func us25MigrationAtoi(t *testing.T, raw, label string) int {
	t.Helper()
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		t.Fatalf("解析%s %q 失败：%v", label, raw, err)
	}
	return value
}

// us25MigrationRequireExit 断言退出码，失败时打印**脱敏后**的两路输出。
// us25MigrationRequireExit asserts the exit code and prints redacted streams.
func us25MigrationRequireExit(t *testing.T, h *harness, res runResult, want int, label string) {
	t.Helper()
	if res.ExitCode != want {
		t.Fatalf("%s：退出码 = %d，期望 %d\nstdout:\n%s\nstderr:\n%s",
			label, res.ExitCode, want, h.redact(res.Stdout), h.redact(res.Stderr))
	}
}

// us25MigrationCopyFile 逐字节复制一个文件（交接副本只镜像布局，不改内容）。
// us25MigrationCopyFile copies one file byte for byte.
func us25MigrationCopyFile(t *testing.T, source, target string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", source, err)
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatalf("写 %s 失败：%v", target, err)
	}
}

// us25MigrationLegacyBinary 从钉死提交构建旧版本制品（v11）。缺 git / 缺提交 / 构建
// 环境不可用时**跳过并写明理由**——绝不拿当前二进制冒充旧制品，也不伪造通过。
// us25MigrationLegacyBinary builds the pinned v11 artifact; a missing commit or build
// environment skips with a written reason instead of faking the gate.
func us25MigrationLegacyBinary(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Skipf("旧版本制品无法构建：定位仓库根失败（%v）——登记为未验证", err)
	}
	if out, err := exec.Command("git", "-C", root, "cat-file", "-e",
		us25MigrationOldCommit+"^{commit}").CombinedOutput(); err != nil {
		t.Skipf("旧版本制品无法构建：本地仓库没有提交 %s（%v）——登记为未验证\n%s",
			us25MigrationOldCommit, err, strings.TrimSpace(string(out)))
	}

	workDir := t.TempDir()
	archive := filepath.Join(workDir, "legacy.tar")
	if out, err := exec.Command("git", "-C", root, "archive", "--format=tar", "-o", archive,
		us25MigrationOldCommit).CombinedOutput(); err != nil {
		t.Skipf("旧版本制品无法构建：git archive 失败（%v）——登记为未验证\n%s",
			err, strings.TrimSpace(string(out)))
	}
	extractDir := filepath.Join(workDir, "src")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		t.Skipf("旧版本制品无法构建：建解包目录失败（%v）", err)
	}
	us25MigrationExtractTar(t, archive, extractDir)

	name := "basalt-old"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(workDir, name)
	buildCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-trimpath",
		"-ldflags", "-X main.version=us25-old-"+us25MigrationOldCommit[:7],
		"-o", target, "./cmd/basalt")
	build.Dir = extractDir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=https://goproxy.cn")
	var buildOut bytes.Buffer
	build.Stdout, build.Stderr = &buildOut, &buildOut
	if err := build.Run(); err != nil {
		t.Skipf("旧版本制品无法构建（%v）——登记为未验证，不拿当前二进制冒充\n%s",
			err, buildOut.String())
	}
	return target
}

// us25MigrationExtractTar 把 git archive 的 tar 解到目录；绝对路径 / 越界路径一律拒绝。
// us25MigrationExtractTar extracts a git archive tar and rejects escaping entries.
func us25MigrationExtractTar(t *testing.T, archive, dir string) {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Skipf("旧版本制品无法构建：打开归档失败（%v）", err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Skipf("旧版本制品无法构建：解包归档失败（%v）", err)
		}
		name := filepath.FromSlash(header.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			t.Fatalf("归档条目越界：%q", header.Name)
		}
		target := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// `git archive` 会在最前面写一条 pax 全局头（记录提交号），它不是文件。
			// pax global headers carry metadata, not files.
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("建归档目录 %s 失败：%v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatalf("建归档父目录失败：%v", err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("读取归档条目 %s 失败：%v", header.Name, err)
			}
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatalf("写归档条目 %s 失败：%v", target, err)
			}
		default:
			t.Fatalf("归档条目 %q 类型 %d 不受支持", header.Name, header.Typeflag)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// M2：单实例 PostgreSQL → 多 Worker（FR-25-26）
// ─────────────────────────────────────────────────────────────────────────────

// M2 夹具对象的固定标识与哈希：一条管理操作串起技能版本、完整的学习审批链、一个
// 非空 Flow（定义 + 指针）与一个知识集合 + 文档。读数可复算，且与探针 Agent 的真实
// 对象互不干扰。
// The M2 fixture identifiers and hashes are fixed so every reading is reproducible.
const (
	us25M2OperationID    = "op-m2-fixture"
	us25M2CandidateID    = "adm_0123456789ABCDEFGHJKMNPQRS"
	us25M2EvaluationID   = "eval_0123456789ABCDEFGHJKMNPQRS"
	us25M2DecisionID     = "dec_0123456789ABCDEFGHJKMNPQRS"
	us25M2SkillUID       = "rs_0123456789ABCDEFGHJKMNPQRS"
	us25M2SkillName      = "m2-skill"
	us25M2SkillVersionID = 900001
	us25M2FlowUID        = "flow-m2-demo"
	us25M2FlowName       = "M2 演示流程"
	us25M2CollectionID   = "col-m2-knowledge"
	us25M2CollectionName = "m2-knowledge"
	us25M2DocumentID     = "doc-m2-knowledge-1"
)

var (
	us25M2CandidateHash = strings.Repeat("e5", 32)
	us25M2SourceHash    = strings.Repeat("f6", 32)
	us25M2PolicyHash    = strings.Repeat("39", 32)
	us25M2FlowHash      = strings.Repeat("17", 32)
	us25M2DocumentHash  = strings.Repeat("28", 32)
)

// TestUS25_Migration_SingleInstanceToMultiworker 覆盖 FR-25-26 的 M2 半段。
// TestUS25_Migration_SingleInstanceToMultiworker covers the M2 half of FR-25-26.
func TestUS25_Migration_SingleInstanceToMultiworker(t *testing.T) {
	s := us25NewStack(t, "m2")
	s.addProbeEnvOverlay(t)
	controlVolume := s.prepareControlForProbe(t, us25ProbeRefusedURL)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	s.startControl(t)
	s.installProbeAssets(t)
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)
	us25M2InsertFixtures(t, s)
	// M2 的完整性判定要有东西可判：非空 Flow 与已发布的学习审批先自证一遍。
	if got := s.psql(t, "SELECT name || '|' || current_version::text FROM flow_pointers "+
		"WHERE definition_uid = '"+us25M2FlowUID+"'"); got != us25M2FlowName+"|1" {
		t.Fatalf("M2 夹具的流程指针读数 = %q，要求 %q（非空 Flow 未就位）", got, us25M2FlowName+"|1")
	}
	if got := s.psql(t, "SELECT state FROM skill_admission_publications "+
		"WHERE candidate_id = '"+us25M2CandidateID+"'"); got != "active" {
		t.Fatalf("M2 夹具的学习审批发布态 = %q，要求 active", got)
	}

	// ── 单实例期：控制面进程领取并执行一次 ───────────────────────────────────
	controlWorker := s.waitLiveWorkerIDs(t, 1, 2*time.Minute)[0]
	s.armProbe(t, us25ProbeArmDelay)
	first := s.waitProbeExecution(t, 1, 90*time.Second)
	if first[3] != controlWorker {
		t.Fatalf("单实例期的执行行 owner 是 %q，要求控制面进程 %q", first[3], controlWorker)
	}
	firstExecution, firstPlanned := first[0], first[1]
	firstRun := s.probeRunID(t, firstExecution)
	if status := s.waitExecutionStatus(t, firstExecution, 3*time.Minute); status != "failed" {
		t.Fatalf("单实例期的执行行收尾状态 = %q，要求 failed（拒绝的回环地址）", status)
	}
	s.waitForQuery(t, 2*time.Minute, "单实例期的 Run 收尾",
		"SELECT status FROM runs WHERE run_id = '"+firstRun+"'",
		func(v string) bool { return v != "" && v != "running" })
	s.waitForQuery(t, time.Minute, "单实例期没有未确认的调用",
		"SELECT count(*)::text FROM llm_calls WHERE run_id = '"+firstRun+"' AND status = 'pending'",
		func(v string) bool { return v == "0" })
	bundleA := s.probeRunBundleHash(t, firstRun)
	if published := s.probePublishedBundleHash(t); published != bundleA {
		t.Fatalf("执行钉住的 bundle 是 %s，发布指针是 %s（准入与指针必须同一份）", bundleA, published)
	}
	s.assertSingleExecution(t, firstPlanned)

	// ── M2 之前的状态：身份、文件字节、业务指纹、旧 bundle 树 ───────────────
	before := s.stableBusinessFingerprint(t, 10*time.Second)
	beforeIdentity := s.probeIdentitySnapshot(t)
	beforeLifecycle := us25VolumeText(t, s, ctx, controlVolume, us25VolumeLifecycleRel)
	beforeRoster := us25VolumeText(t, s, ctx, controlVolume, us25VolumeRosterRel)
	beforeBundle := us25VolumeTreeText(t, s, ctx, controlVolume, us25BundleRel(bundleA))
	rosterInstance := s.instanceID(t, beforeRoster)

	// ── M2：A 停写 → 种子 B → 启动 B ────────────────────────────────────────
	_, seedPath := s.exportControlAssets(t)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.prepareWorkerForProbe(t, workerVolume, us25ProbeRefusedURL)
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	workerID := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)[0]
	if workerID == controlWorker {
		t.Fatalf("Worker 的 InstanceID 是 %q，与单实例期控制面进程相同（要求两个不同进程身份）", workerID)
	}
	if !us25WorkerIDPattern.MatchString(workerID) {
		t.Fatalf("Worker 的 InstanceID %q 不符合 wrk_ 形态", workerID)
	}

	// M2 本身零业务写入：种子导入与 Worker 启动不新增 runs / flows / 审计 / 知识 /
	// 学习审批行，也不改写发布指针与生命周期索引。
	if after := s.stableBusinessFingerprint(t, 10*time.Second); !maps.Equal(after, before) {
		t.Fatalf("种子导入或 Worker 启动改写了业务表：%s", us25FingerprintDiff(before, after))
	}
	if got := s.probeIdentitySnapshot(t); got != beforeIdentity {
		t.Fatalf("M2 后生命周期索引读数 = %q，要求与 M2 前一致（%q）", got, beforeIdentity)
	}
	if got := us25VolumeText(t, s, ctx, workerVolume, us25VolumeLifecycleRel); got != beforeLifecycle {
		t.Fatalf("Worker 工作区的生命周期状态文件与源工作区不一致（Agent UID / 代数必须原样保留）")
	}
	workerRoster := us25VolumeText(t, s, ctx, workerVolume, us25VolumeRosterRel)
	if workerRoster != beforeRoster {
		t.Fatalf("Worker 工作区的名册与源工作区不一致（Worker 不得改写 instance.json）")
	}
	if got := s.instanceID(t, workerRoster); got != rosterInstance {
		t.Fatalf("Worker 工作区名册的 instance_id = %q，要求 %q", got, rosterInstance)
	}
	if got := us25VolumeTreeText(t, s, ctx, workerVolume, us25BundleRel(bundleA)); got != beforeBundle {
		t.Fatalf("Worker 工作区的旧 bundle 树与源工作区不一致（种子必须原样搬运已发布资产）")
	}
	if s.volumeHas(ctx, workerVolume, "/.basalt/basalt.db") {
		t.Fatalf("Worker 工作区出现 SQLite 库（Worker 不得建库）")
	}
	if raw := s.workerListJSON(t, ctx); !strings.Contains(raw, `"`+workerID+`"`) {
		t.Fatalf("workers list 没有列出 Worker %q（读数：%s）", workerID, us18Tail(raw, 2000))
	}

	// ── M2 之后：Worker 领取执行、旧 bundle 仍可读 ──────────────────────────
	s.armProbe(t, us25ProbeArmDelay)
	second := s.waitProbeExecution(t, 2, 3*time.Minute)
	if second[3] != workerID {
		t.Fatalf("M2 后的执行行 owner 是 %q，要求 Worker %q", second[3], workerID)
	}
	if second[1] == firstPlanned {
		t.Fatalf("M2 后的执行行复用了计划时刻 %s（每轮到期必须产生新的 planned_at）", firstPlanned)
	}
	secondRun := s.probeRunID(t, second[0])
	if status := s.waitExecutionStatus(t, second[0], 3*time.Minute); status != "failed" {
		t.Fatalf("M2 后的执行行收尾状态 = %q，要求 failed（拒绝的回环地址）", status)
	}
	s.waitForQuery(t, 2*time.Minute, "M2 后的 Run 收尾",
		"SELECT status FROM runs WHERE run_id = '"+secondRun+"'",
		func(v string) bool { return v != "" && v != "running" })
	if got := s.probeRunBundleHash(t, secondRun); got != bundleA {
		t.Fatalf("M2 后的执行钉住 bundle %s，要求旧 bundle %s（Worker 不得安装或重建资产）", got, bundleA)
	}
	if lease := s.probeLease(t, second[0]); lease[0] != workerID || lease[2] == "-" {
		t.Fatalf("M2 后的执行租约读数 = %v，要求 owner=%s 且已释放", lease, workerID)
	}

	// 计数：runs / task_executions 各 +1；流程、审计、知识、学习审批与资源指针逐字不变。
	after := s.stableBusinessFingerprint(t, 10*time.Second)
	us25RequireGovernanceUnchanged(t, before, after)
	for _, table := range []string{"runs", "task_executions"} {
		if got := us25FingerprintCount(t, after[table]); got != us25FingerprintCount(t, before[table])+1 {
			t.Fatalf("%s 行数 %d，要求 M2 前 %d + 1（读数：%s）", table, got,
				us25FingerprintCount(t, before[table]), after[table])
		}
	}
	if got := s.psql(t, "SELECT name || '|' || current_version::text FROM flow_pointers "+
		"WHERE definition_uid = '"+us25M2FlowUID+"'"); got != us25M2FlowName+"|1" {
		t.Fatalf("M2 后流程指针读数 = %q，要求 %q", got, us25M2FlowName+"|1")
	}
	if got := s.psql(t, "SELECT state || '|' || version_no::text || '|' || skill_uid "+
		"FROM skill_admission_publications WHERE candidate_id = '"+us25M2CandidateID+"'"); got != "active|1|"+us25M2SkillUID {
		t.Fatalf("M2 后学习审批发布读数 = %q，要求 active|1|%s", got, us25M2SkillUID)
	}
	if got := s.psql(t, "SELECT count(*)::text FROM kb_documents WHERE collection_id = '"+us25M2CollectionID+"'"); got != "1" {
		t.Fatalf("M2 后知识文档数 = %s，要求 1", got)
	}

	// ── 收尾：控制面回到同一张库上，两个进程在线且同制品 ────────────────────
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("M2 后拉起控制面失败：%v", err)
	}
	both := s.waitLiveWorkerIDs(t, 2, 3*time.Minute)
	if !us25ContainsLine(both, workerID) {
		t.Fatalf("M2 收尾的在线 Worker 集合 %v 不含 %q", both, workerID)
	}
	if got, want := s.liveArtifactFingerprint(t, ctx), us18DefaultVersion+"|12"; got != want {
		t.Fatalf("在线制品牌本 = %q，要求 %q（两节点必须同版本、同 schema v12）", got, want)
	}
	if again := s.stableBusinessFingerprint(t, 10*time.Second); !maps.Equal(again, after) {
		t.Fatalf("控制面重启改写了业务表：%s", us25FingerprintDiff(after, again))
	}
	t.Logf("M2：控制面 %s → Worker %s；执行 #1 与 #2 都准入并钉住 bundle %s；"+
		"种子导入与 Worker 启动零业务写入；流程 / 审计 / 知识 / 学习审批逐字保留",
		controlWorker, workerID, bundleA)
}

// us25M2InsertFixtures 写入 M2 要判完整性的存量对象：一条管理操作、一个技能版本与
// 完整的学习审批链（候选 → 评估 → 批准 → 发布）、一个非空 Flow（定义 + 指针）与一个
// 知识集合 + 文档。全部挂在同一操作上，时间戳取固定原点，读数可复算。
// us25M2InsertFixtures writes the pre-existing objects M2 must preserve: one management
// operation, a skill version with a full learning-approval chain, one non-empty flow and
// one knowledge collection with a document.
func us25M2InsertFixtures(t *testing.T, s *us25Stack) {
	t.Helper()
	base := fmt.Sprintf("%d", us25MigrationBaseMS)
	expires := fmt.Sprintf("%d", us25MigrationBaseMS+30*24*60*60*1000)
	statements := []string{
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, result_code, detail_json, created_at, finished_at)
		 VALUES ('` + us25M2OperationID + `', 'm2.fixture', 'fixture', 'm2', 'cli:m2-fixture',
		         'cli', 'cli', 'success', 'fixture', '{}'::jsonb, ` + base + `, ` + base + `)`,
		`INSERT INTO resource_versions
		   (id, resource_kind, resource_uid, resource_id, version_no, content_hash,
		    manifest_json, storage_path, created_at, operation_id)
		 VALUES (` + fmt.Sprintf("%d", us25M2SkillVersionID) + `, 'skill', '` + us25M2SkillUID + `',
		         '` + us25M2SkillName + `', 1, '` + us25M2SourceHash + `', '{}',
		         '.basalt/skills/` + us25M2SkillName + `/1', ` + base + `, '` + us25M2OperationID + `')`,
		`INSERT INTO skill_admission_candidates
		   (candidate_id, skill_name, state, revision, candidate_hash, source_hash,
		    manifest_json, source_json, baseline_json, proposer_subject, proposer_auth_method,
		    proposer_source, risk_tier, operation_id, created_at, updated_at)
		 VALUES ('` + us25M2CandidateID + `', '` + us25M2SkillName + `', 'published', 1,
		         '` + us25M2CandidateHash + `', '` + us25M2SourceHash + `', '{}', '{}', '{}',
		         'cli:m2-fixture', 'cli', 'cli', 'normal', '` + us25M2OperationID + `',
		         ` + base + `, ` + base + `)`,
		`INSERT INTO skill_admission_evaluations
		   (evaluation_id, candidate_id, candidate_revision, candidate_hash, evaluation_hash,
		    grader_version, policy_hash, dimensions_json, scripts_json, aggregate, risk_tier,
		    referrers_json, baseline_json, evaluated_at, operation_id)
		 VALUES ('` + us25M2EvaluationID + `', '` + us25M2CandidateID + `', 1,
		         '` + us25M2CandidateHash + `', '` + us25M2SourceHash + `', 'm2-grader',
		         '` + us25M2PolicyHash + `', '{}', '{}', 'passed', 'normal', '{}', '{}',
		         ` + base + `, '` + us25M2OperationID + `')`,
		`INSERT INTO skill_admission_decisions
		   (decision_id, candidate_id, candidate_revision, candidate_hash, evaluation_id,
		    evaluation_hash, decision, reason, acknowledged_json, approved_scope_json,
		    script_review_json, baseline_json, risk_tier, approver_subject, approver_auth_method,
		    approver_source, expires_at, created_at, operation_id)
		 VALUES ('` + us25M2DecisionID + `', '` + us25M2CandidateID + `', 1,
		         '` + us25M2CandidateHash + `', '` + us25M2EvaluationID + `',
		         '` + us25M2SourceHash + `', 'approve', 'M2 夹具批准', '{}', '{}', '{}', '{}',
		         'normal', 'cli:m2-fixture', 'cli', 'cli', ` + expires + `, ` + base + `,
		         '` + us25M2OperationID + `')`,
		`INSERT INTO skill_admission_publications
		   (candidate_id, decision_id, skill_name, skill_uid, version_id, version_no,
		    content_hash, baseline_json, state, published_at, operation_id)
		 VALUES ('` + us25M2CandidateID + `', '` + us25M2DecisionID + `', '` + us25M2SkillName + `',
		         '` + us25M2SkillUID + `', ` + fmt.Sprintf("%d", us25M2SkillVersionID) + `, 1,
		         '` + us25M2SourceHash + `', '{}', 'active', ` + base + `, '` + us25M2OperationID + `')`,
		`INSERT INTO flow_definitions
		   (definition_uid, version, name, content_hash, definition_content, contract_content,
		    created_at, operation_id)
		 VALUES ('` + us25M2FlowUID + `', 1, '` + us25M2FlowName + `', '` + us25M2FlowHash + `',
		         '{"format_version":"1"}', '{}', ` + base + `, '` + us25M2OperationID + `')`,
		`INSERT INTO flow_pointers
		   (definition_uid, name, current_version, retired, updated_at, operation_id)
		 VALUES ('` + us25M2FlowUID + `', '` + us25M2FlowName + `', 1, false, ` + base + `,
		         '` + us25M2OperationID + `')`,
		`INSERT INTO kb_collections (collection_id, name, project, description, created_at)
		 VALUES ('` + us25M2CollectionID + `', '` + us25M2CollectionName + `', '', '', ` + base + `)`,
		`INSERT INTO kb_documents
		   (document_id, collection_id, source_path, source_kind, content_hash, created_at, updated_at)
		 VALUES ('` + us25M2DocumentID + `', '` + us25M2CollectionID + `', 'fixtures/m2.md',
		         'file', '` + us25M2DocumentHash + `', ` + base + `, ` + base + `)`,
	}
	for _, statement := range statements {
		if tag := s.psql(t, statement); tag != "INSERT 0 1" {
			t.Fatalf("M2 夹具写入未命中一行：读数是 %q（语句：%s）", tag, statement)
		}
	}
}
