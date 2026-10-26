package app

// 本文件是通知渠道写面的回归（颗粒度文档 §3.1.7 / 约束 38，check
// `012-app-notify-author`）：占位校验、空 URL、同名 400、更新留空保留、引用删除闸门
// （活动 + 归档整词、force 与扫描失败）、每次写都留操作行、只回 host 的脱敏边界。
// Regression tests for the notify-channel write surface: placeholder checks, empty
// URLs, duplicate names, keep-url updates, the reference gate, operation rows on every
// write, and the host-only redaction boundary.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// notifyEnv 是通知写面夹具：真实工作区骨架（`agents/` + `archive/`）+ 真实 SQLite +
// 注入的环境变量表 + 只放行两个域名的真实白名单策略。
// The fixture wires a real workspace skeleton, a real database, an injected env table,
// and the real whitelist policy.
type notifyEnv struct {
	t     *testing.T
	root  string
	db    *storage.DB
	store *storage.NotifyAdminStore
	ops   *storage.ManagementStore
	deps  NotifyAdminDeps
	admin *NotifyAdmin
	logs  *bytes.Buffer
}

// notifyTestEnvVars 是夹具可见的环境变量：一个放行域名、一个需授权的飞书域名、
// 一个不在白名单的域名（带查询串凭证，判脱敏用）。
// notifyTestEnvVars holds the fixture's visible variables, including a non-allowlisted
// host carrying a query-string secret for the redaction assertions.
var notifyTestEnvVars = map[string]string{
	"BASALT_TEST_OPS_HOOK":    "https://hooks.example.com/services/ops",
	"BASALT_TEST_FEISHU_HOOK": "https://open.feishu.cn/open-apis/bot/v2/hook/abc",
	"BASALT_TEST_EVIL_HOOK":   "https://evil.example.com/hook?token=super-secret",
	"BASALT_TEST_EMPTY_HOOK":  "",
}

// newNotifyEnv 建夹具；mutate 在构造前改写入参。
// newNotifyEnv builds the fixture; mutate adjusts the deps before construction.
func newNotifyEnv(t *testing.T, mutate ...func(*NotifyAdminDeps)) *notifyEnv {
	t.Helper()

	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(root, "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	logger, logs := newCapturedLogger()
	env := &notifyEnv{
		t:     t,
		root:  root,
		db:    db,
		store: storage.NewNotifyAdminStore(db),
		ops:   storage.NewManagementStore(db),
		logs:  logs,
	}
	env.deps = NotifyAdminDeps{
		Root:       root,
		Store:      env.store,
		Bindings:   storage.NewBindingStore(db),
		RunRefs:    storage.NewNotifyDeliveryStore(db),
		Operations: env.ops,
		Types:      notifyTypeNames(),
		Policy: tool.NewWhitelistPolicy(tool.PolicyOptions{
			Root:           root,
			AllowedDomains: []string{"hooks.example.com", "open.feishu.cn"},
		}),
		LookupEnv: func(name string) (string, bool) {
			value, ok := notifyTestEnvVars[name]
			return value, ok
		},
		Logger: logger,
		Now:    baselineClock,
	}
	for _, m := range mutate {
		m(&env.deps)
	}
	env.admin = NewNotifyAdmin(env.deps)
	return env
}

// create 走真实创建路径。
// create goes through the real create path.
func (e *notifyEnv) create(name, channelType, url, description string) (core.NotifyChannel, error) {
	return e.admin.Create(context.Background(), resourceActor(), NotifyChannelRequest{
		Name: name, Type: channelType, URL: url, URLSet: url != "", Description: description,
	})
}

// update 走真实更新路径。
// update goes through the real update path.
func (e *notifyEnv) update(name string, req NotifyChannelRequest) (core.NotifyChannel, error) {
	return e.admin.Update(context.Background(), resourceActor(), name, req)
}

// rows 取指定动作的全部操作行（新到旧）。
// rows returns every operation row for one action.
func (e *notifyEnv) rows(action string) []core.OperationRecord {
	e.t.Helper()
	records, err := e.ops.ListOperations(context.Background(),
		core.OperationFilter{Action: action, Limit: 100})
	require.NoError(e.t, err)
	return records
}

// operationCount 数全部操作行。
// operationCount counts every operation row.
func (e *notifyEnv) operationCount() int {
	e.t.Helper()
	records, err := e.ops.ListOperations(context.Background(), core.OperationFilter{Limit: 200})
	require.NoError(e.t, err)
	return len(records)
}

// stored 读一行渠道。
// stored reads one channel row.
func (e *notifyEnv) stored(name string) (core.NotifyChannel, error) {
	return e.store.Get(context.Background(), name)
}

// uid 取渠道当前指针的资源身份（删除必须显式携带，缺失 → 409）。
// uid returns the current pointer uid that every delete must carry explicitly.
func (e *notifyEnv) uid(name string) string {
	e.t.Helper()
	stored, err := e.stored(name)
	require.NoError(e.t, err)
	return string(stored.ResourceUID)
}

// 中文验收点：创建 → 发送读面同表可见 → 更新留空保留占位 → 回读是库里的旧值；
// 两次写各留一条 OK 操作行。
// Acceptance point: create, the send path sees the same row, a blank update keeps the
// stored placeholder, and both writes leave an OK operation row.
func TestNotifyAdmin_CreateAndUpdate(t *testing.T) {
	env := newNotifyEnv(t)

	created, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "值班告警")
	require.NoError(t, err)
	// 回显是**存储行**（不是另造的摘要）：四列原样，身份列在库里有值。
	require.Equal(t, "ops-alert", created.Name)
	require.Equal(t, "webhook", created.Type)
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", created.URL,
		"回显必须携带库里的占位，不得是解析后的地址")
	require.Equal(t, "值班告警", created.Description)
	if _, err := core.ParseResourceUID(string(created.ResourceUID)); err != nil {
		t.Fatalf("回显必须带存储行的资源身份: %v", err)
	}
	require.Equal(t, core.CredentialModeEnv, created.CredentialMode)

	sent, err := storage.NewNotifyChannelRepo(env.db).ListNotifyChannels(context.Background())
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", sent[0].URL, "发送读面必须看到写面刚落下的同一行")

	updated, err := env.update("ops-alert", NotifyChannelRequest{
		Type: "feishu", URL: "", Description: "改走飞书",
	})
	require.NoError(t, err)
	require.Equal(t, "feishu", updated.Type)
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", updated.URL, "留空更新必须回读库里的旧占位")
	require.Equal(t, "改走飞书", updated.Description)

	stored, err := env.stored("ops-alert")
	require.NoError(t, err)
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", stored.URL)
	require.Equal(t, "feishu", stored.Type)

	creates := env.rows(core.ActionNotifyChannelCreate)
	require.Len(t, creates, 1)
	require.Equal(t, core.OperationSuccess, creates[0].Status)
	require.Equal(t, lifecycleResultOK, creates[0].ResultCode)
	require.Equal(t, core.TargetKindNotifyChannel, creates[0].TargetKind)
	require.Equal(t, "ops-alert", creates[0].TargetID)
	updates := env.rows(core.ActionNotifyChannelUpdate)
	require.Len(t, updates, 1)
	require.Equal(t, core.OperationSuccess, updates[0].Status)
}

// 中文验收点：空 URL = 渠道暂不可发送——列 NOT NULL 存空串，摘要与发送读面都是空串，
// 不是错误也不是省略。
// Acceptance point: an omitted URL is a legitimate "not sendable yet" channel stored as
// the empty string on both the write and read faces.
func TestNotifyAdmin_CreateWithoutURL(t *testing.T) {
	env := newNotifyEnv(t)

	created, err := env.create("silent", "webhook", "", "还没配地址")
	require.NoError(t, err)
	require.Empty(t, created.URL)

	stored, err := env.stored("silent")
	require.NoError(t, err)
	require.Empty(t, stored.URL, "空地址必须照存（暂不可发送是合法状态）")

	sent, err := storage.NewNotifyChannelRepo(env.db).ListNotifyChannels(context.Background())
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Empty(t, sent[0].URL)
	require.Len(t, env.rows(core.ActionNotifyChannelCreate), 1)
}

// 中文验收点：非空地址必须是**精确** `${VAR}` 占位——明文 URL、前后缀混排、多占位、
// 前导空白一律 400，且零落库零操作行。判据修订（US-12 R4）：值与模式不符按契约码
// `CREDENTIAL_MODE_REQUIRED` 返回，不再复用通用输入错误载体；拒绝面不变。
// Acceptance point: only an exact ${VAR} placeholder is accepted; every other shape is
// the contract's 400 CREDENTIAL_MODE_REQUIRED with zero rows.
func TestNotifyAdmin_PlaceholderOnly(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "明文地址", url: "https://hooks.example.com/services/ops"},
		{name: "前缀混排", url: "Bearer ${BASALT_TEST_OPS_HOOK}"},
		{name: "多占位", url: "${BASALT_TEST_OPS_HOOK}${BASALT_TEST_FEISHU_HOOK}"},
		{name: "前导空白", url: " ${BASALT_TEST_OPS_HOOK}"},
		{name: "未闭合", url: "${BASALT_TEST_OPS_HOOK"},
		{name: "空变量名", url: "${}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifyEnv(t)
			_, err := env.create("ops-alert", "webhook", tc.url, "")
			require.ErrorIs(t, err, core.ErrCredentialModeRequired,
				"非占位地址必须按契约码判为模式错误（400）")
			var adminErr *core.AdminError
			require.ErrorAs(t, err, &adminErr)
			require.Contains(t, adminErr.Message, "占位")

			_, err = env.stored("ops-alert")
			require.ErrorIs(t, err, core.ErrResourceNotFound, "被拒的创建必须零落库")
			require.Zero(t, env.operationCount(), "纯请求校验失败不得留操作行")
		})
	}
}

// 中文验收点：变量未设置或为空 → 400，文案点名变量；零落库零操作行。写面**不**接受
// "先登记、后补变量"——渠道登记即代表当前可发送。
// Acceptance point: an unset or empty variable is a 400 naming the variable only, with
// zero rows and zero operation records.
func TestNotifyAdmin_MissingEnvRejected(t *testing.T) {
	cases := []struct{ name, url, variable string }{
		{name: "未设置", url: "${BASALT_TEST_NO_SUCH_HOOK}", variable: "BASALT_TEST_NO_SUCH_HOOK"},
		{name: "为空", url: "${BASALT_TEST_EMPTY_HOOK}", variable: "BASALT_TEST_EMPTY_HOOK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifyEnv(t)
			_, err := env.create("ops-alert", "webhook", tc.url, "")
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Contains(t, invalid.Reason, tc.variable)
			require.NotContains(t, invalid.Reason, "super-secret")

			_, err = env.stored("ops-alert")
			require.ErrorIs(t, err, core.ErrResourceNotFound)
			require.Zero(t, env.operationCount())
		})
	}
}

// 中文验收点：解析后的地址不在 `http.allowed_domains` → 400；文案只含变量名与
// scheme + host——查询串里的凭证**不得**回显。
// Acceptance point: a resolved host outside the allowlist is a 400 whose text names the
// variable and the scheme+host only, never the query-string secret.
func TestNotifyAdmin_DomainNotAllowed(t *testing.T) {
	env := newNotifyEnv(t)

	_, err := env.create("ops-alert", "webhook", "${BASALT_TEST_EVIL_HOOK}", "")
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid, "白名单预检拒绝在写面是 400，不是沙箱错误")
	require.Contains(t, invalid.Reason, "BASALT_TEST_EVIL_HOOK")
	require.Contains(t, invalid.Reason, "https://evil.example.com")
	require.NotContains(t, invalid.Reason, "super-secret", "凭证值不得进入错误文案")
	require.NotContains(t, invalid.Reason, "/hook", "路径与查询串不得进入错误文案")

	_, err = env.stored("ops-alert")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Zero(t, env.operationCount())
}

// 中文验收点：同名创建 → 400 语义（契约只列 200/400，不是 409）；既有行原样保留；
// 操作台账留一条 failed + BAD_REQUEST。
// Acceptance point: a duplicate name is a 400-semantic rejection (not 409), the existing
// row is untouched, and the ledger carries one failed BAD_REQUEST row.
func TestNotifyAdmin_DuplicateName(t *testing.T) {
	env := newNotifyEnv(t)
	_, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "原值")
	require.NoError(t, err)

	_, err = env.create("ops-alert", "feishu", "${BASALT_TEST_FEISHU_HOOK}", "覆盖尝试")
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "ops-alert")
	require.False(t, errors.Is(err, core.ErrConflict), "同名创建不得落成 409 语义")

	stored, err := env.stored("ops-alert")
	require.NoError(t, err)
	require.Equal(t, "webhook", stored.Type, "冲突创建不得改写既有行")
	require.Equal(t, "原值", stored.Description)

	creates := env.rows(core.ActionNotifyChannelCreate)
	require.Len(t, creates, 2)
	failed := 0
	for _, record := range creates {
		if record.Status == core.OperationFailed {
			failed++
			require.Equal(t, agentResultBadRequest, record.ResultCode)
			require.NotEmpty(t, record.ErrorMessage)
		}
	}
	require.Equal(t, 1, failed, "被拒的重复创建必须留一条 failed 操作行")
}

// 中文验收点：每次成功写都恰好一条 OK 操作行（创建 / 更新 / 删除各一）；预写失败时
// 目标零写入，错误带回可查原因。
// Acceptance point: every successful write leaves exactly one OK row, and a failed
// pre-write leaves the target untouched.
func TestNotifyAdmin_EveryWriteHasOperation(t *testing.T) {
	t.Run("成功写各留一行", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)
		_, err = env.update("ops-alert", NotifyChannelRequest{
			Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true, Description: "改说明",
		})
		require.NoError(t, err)
		require.NoError(t, env.admin.Delete(context.Background(), resourceActor(), "ops-alert", env.uid("ops-alert"), false))

		require.Equal(t, 3, env.operationCount())
		for _, record := range []core.OperationRecord{
			env.rows(core.ActionNotifyChannelCreate)[0],
			env.rows(core.ActionNotifyChannelUpdate)[0],
			env.rows(core.ActionNotifyChannelDelete)[0],
		} {
			require.Equal(t, core.OperationSuccess, record.Status)
			require.Equal(t, lifecycleResultOK, record.ResultCode)
			require.Equal(t, core.TargetKindNotifyChannel, record.TargetKind)
			require.Equal(t, "ops-alert", record.TargetID)
			require.True(t, strings.HasPrefix(record.RequestHash, "sha256:"),
				"请求摘要必须是 sha256 前缀的脱敏值")
		}
	})

	t.Run("预写失败零写入", func(t *testing.T) {
		env := newNotifyEnv(t, func(deps *NotifyAdminDeps) {
			deps.Operations = failingBeginOperations{
				OperationStore: deps.Operations,
				err:            errors.New("注入的预写失败"),
			}
		})
		_, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.Error(t, err)
		require.ErrorContains(t, err, "预写操作记录失败")

		_, err = env.stored("ops-alert")
		require.ErrorIs(t, err, core.ErrResourceNotFound, "预写失败必须零写入")
	})
}

// 中文验收点：更新 / 删除不存在的渠道 → not-found 且零操作行；请求体名称与路径不一致
// → 400 且零操作行、行内容不变。
// Acceptance point: missing rows are not-found with zero rows, and a body/path name
// mismatch is a 400 that leaves the stored row untouched.
func TestNotifyAdmin_UpdateGuards(t *testing.T) {
	env := newNotifyEnv(t)

	_, err := env.update("never-existed", NotifyChannelRequest{
		Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
	})
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Zero(t, env.operationCount(), "不存在性校验在写操作行之前")

	_, err = env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "原值")
	require.NoError(t, err)
	before := env.operationCount()

	_, err = env.update("ops-alert", NotifyChannelRequest{
		Name: "renamed", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
	})
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "不一致")
	require.Equal(t, before, env.operationCount(), "名称不一致不得留操作行")

	stored, err := env.stored("ops-alert")
	require.NoError(t, err)
	require.Equal(t, "原值", stored.Description, "被拒的更新不得改动行")

	err = env.admin.Delete(context.Background(), resourceActor(), "never-existed", "", false)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Equal(t, before, env.operationCount(), "删除不存在性校验也不得留操作行")
}

// 中文验收点：被 Agent（活动文档）整词引用的渠道 → 400 点名引用者、行保留、操作
// failed BAD_REQUEST 且 `detail_json` 带引用清单；告警日志同样点名。
// Acceptance point: a channel referenced as a whole word by an agent definition is a 400
// naming the referrer, the row survives, and the failed row carries the referrer list.
func TestNotifyAdmin_DeleteReferenced(t *testing.T) {
	env := newNotifyEnv(t)
	_, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	writeAgentDir(t, env.root, "billing-agent", `---
name: billing-agent
provider:
  name: deepseek
  model: deepseek-chat
---

对账结束后把摘要发到 ops-alert 渠道。
`)

	err = env.admin.Delete(context.Background(), resourceActor(), "ops-alert", env.uid("ops-alert"), false)
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.ErrorContains(t, err, "billing-agent")
	var invalid *core.InvalidInputError
	require.ErrorAs(t, adminErr.Kind, &invalid, "引用闸门拒绝是 400 语义")

	agents, ok := adminErr.Data["agents"].([]string)
	require.True(t, ok, "错误 data 必须携带引用清单")
	require.Equal(t, []string{"billing-agent"}, agents)

	_, err = env.stored("ops-alert")
	require.NoError(t, err, "被引用的渠道不得被删除")

	deletes := env.rows(core.ActionNotifyChannelDelete)
	require.Len(t, deletes, 1)
	require.Equal(t, core.OperationFailed, deletes[0].Status)
	require.Equal(t, agentResultBadRequest, deletes[0].ResultCode)
	var detail map[string][]string
	require.NoError(t, json.Unmarshal(deletes[0].DetailJSON, &detail))
	require.Equal(t, []string{"billing-agent"}, detail["agents"])

	require.Contains(t, env.logs.String(), "billing-agent", "拒绝必须进 WARN 日志")
	require.NotContains(t, env.logs.String(), "hooks.example.com", "日志不得回显解析后的地址")
}

// writeArchivedAgentDoc 在归档区写一份定义（删除闸门的归档扫描面）。
// writeArchivedAgentDoc writes one archived definition for the delete gate.
func writeArchivedAgentDoc(t *testing.T, root, archiveID, body string) {
	t.Helper()
	archiveDir := filepath.Join(root, "archive", archiveID)
	require.NoError(t, os.MkdirAll(archiveDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "AGENT.md"), []byte(body), 0o640))
}

// 中文验收点（删除闸门的扫描判据，锚点 `TestNotifyAdmin_ReferenceGate`）：非整词与
// 大小写不同不算命中；归档文档按 `archive:<id>` 点名；扫描失败（工作区没初始化）时
// 拒绝删除且不进入写路径。
// Acceptance point: non-whole-word and case-different text never blocks, archived
// documents are named archive:<id>, and a failed scan refuses before any delete.
func TestNotifyAdmin_ReferenceGate(t *testing.T) {
	t.Run("非整词不命中", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)
		writeAgentDir(t, env.root, "noise-agent", `---
name: noise-agent
---

myops 与 ops2 都不是渠道名，OPS 大小写也不同。
`)
		require.NoError(t, env.admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), false))
		_, err = env.stored("ops")
		require.ErrorIs(t, err, core.ErrResourceNotFound)
	})

	t.Run("归档文档点名", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)
		const archiveID = "legacy-agent-20261022-080000"
		writeArchivedAgentDoc(t, env.root, archiveID,
			"---\nname: legacy-agent\n---\n\n把告警发到 ops 渠道\n")

		err = env.admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), false)
		require.ErrorContains(t, err, "archive:"+archiveID)
		_, err = env.stored("ops")
		require.NoError(t, err, "被归档文档引用的渠道不得被删除")
	})

	t.Run("扫描失败不放行", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		// 换一个没有 agents/ 的根：扫描证明不了"没人引用"，必须 fail-closed。
		bare := t.TempDir()
		deps := env.deps
		deps.Root = bare
		admin := NewNotifyAdmin(deps)

		err = admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true)
		require.ErrorContains(t, err, "无法扫描")
		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.NotEmpty(t, adminErr.Data["operation_id"], "失败必须可查操作标识")

		_, err = env.stored("ops")
		require.NoError(t, err, "扫描失败必须保留渠道")

		deletes := env.rows(core.ActionNotifyChannelDelete)
		require.Len(t, deletes, 1)
		require.Equal(t, core.OperationFailed, deletes[0].Status)
		require.Equal(t, lifecycleResultInternal, deletes[0].ResultCode)
	})
}

// 中文验收点（锚点 `TestNotifyAdmin_ForceDelete`）：命中引用时只有 `force=true` 才删；
// 强删成功后操作行 `detail_json` 记 `forced=true`；强删仍走同一条扫描（归档文档同样
// 命中），不是"force 即跳过证明"。
// Acceptance point: only force bypasses a hit, a forced delete records forced=true, and
// it still runs the same scan.
func TestNotifyAdmin_ForceDelete(t *testing.T) {
	env := newNotifyEnv(t)
	_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)
	const archiveID = "legacy-agent-20261022-080000"
	writeArchivedAgentDoc(t, env.root, archiveID,
		"---\nname: legacy-agent\n---\n\n把告警发到 ops 渠道\n")

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true),
		"force=true 才跳过命中")

	_, err = env.stored("ops")
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	deletes := env.rows(core.ActionNotifyChannelDelete)
	require.Len(t, deletes, 1)
	require.Equal(t, core.OperationSuccess, deletes[0].Status)
	var detail map[string]bool
	require.NoError(t, json.Unmarshal(deletes[0].DetailJSON, &detail))
	require.True(t, detail["forced"], "强删必须在 detail_json 记 forced=true")
}

// 中文验收点（脱敏边界，锚点 `TestNotifyAdmin_OnlyHost`）：返回值只携带占位，解析后
// 的真实地址不进入摘要、操作摘要或成功日志；错误文案最多出现 scheme + host。
// Acceptance point: resolved addresses never enter the summary, the operation digest or
// the success log; an error text may carry the scheme+host at most.
func TestNotifyAdmin_OnlyHost(t *testing.T) {
	env := newNotifyEnv(t)
	summary, err := env.create("ops-alert", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	resolved := notifyTestEnvVars["BASALT_TEST_OPS_HOOK"]
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", summary.URL)
	for _, field := range []string{summary.URL, summary.Description} {
		require.NotContains(t, field, "hooks.example.com", "摘要不得携带解析后的地址")
		require.NotContains(t, field, "services/ops")
	}
	events := env.rows(core.ActionNotifyChannelCreate)
	require.Len(t, events, 1)
	require.NotContains(t, string(events[0].DetailJSON), "hooks.example.com")
	require.NotContains(t, events[0].ErrorMessage, resolved)

	_, err = env.create("evil", "webhook", "${BASALT_TEST_EVIL_HOOK}", "")
	require.ErrorContains(t, err, "https://evil.example.com")
	require.NotContains(t, err.Error(), "super-secret")
	require.NotContains(t, err.Error(), "evil.example.com/hook")
}

// TestNotifyAdmin_ConcurrentDelete：同名并发删除由 DELETE 的 RowsAffected 唯一裁决——
// 恰好一个成功，其余按 404 失败并如实回填 failed；每次预写都有终态行，不留 pending
// （012-race 必跑）。
// TestNotifyAdmin_ConcurrentDelete pins the single-winner delete: one success row and
// 404 for every loser, with no pending rows left behind.
func TestNotifyAdmin_ConcurrentDelete(t *testing.T) {
	env := newNotifyEnv(t)
	_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	const deleters = 6
	// 身份在并发开始前取一次：goroutine 里不再读库或让 require 失败。
	uid := env.uid("ops")
	start := make(chan struct{})
	errs := make([]error, deleters)
	var wg sync.WaitGroup
	for i := 0; i < deleters; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = env.admin.Delete(context.Background(), resourceActor(), "ops", uid, true)
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for i := 0; i < deleters; i++ {
		if errs[i] == nil {
			successes++
			continue
		}
		require.ErrorIs(t, errs[i], core.ErrResourceNotFound, "deleter %d", i)
	}
	require.Equal(t, 1, successes, "同名并发删除只能有一个成功")

	successRows := 0
	for _, row := range env.rows(core.ActionNotifyChannelDelete) {
		require.NotEqual(t, core.OperationPending, row.Status)
		if row.Status == core.OperationSuccess {
			successRows++
		}
	}
	require.Equal(t, 1, successRows)

	all, listErr := env.ops.ListOperations(context.Background(), core.OperationFilter{Limit: 200})
	require.NoError(t, listErr)
	for _, row := range all {
		require.NotEqual(t, core.OperationPending, row.Status, "并发删除后不得留下 pending 行")
	}

	_, err = env.stored("ops")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "删除后渠道必须不存在")
}

// freezeNotifyRef 造一条活动运行冻结：先写运行行（外键要求），再写引用。
// freezeNotifyRef inserts one run row (the FK target) and then the frozen reference.
func freezeNotifyRef(t *testing.T, env *notifyEnv, runID string, agentUID core.AgentUID,
	channel core.NotifyChannel,
) {
	t.Helper()
	startedAt := baselineClock()
	require.NoError(t, storage.NewRunStore(env.db).StartRun(context.Background(),
		core.RunRecord{
			RunID: runID, Agent: "billing-agent", Channel: "cli", UserID: "tester",
			StartedAt: startedAt,
		}, core.RunResourceManifest{}))
	require.NoError(t, storage.NewNotifyDeliveryStore(env.db).FreezeRef(context.Background(),
		core.RunNotifyRef{
			RunID: runID, AgentUID: agentUID,
			ChannelUID: channel.ResourceUID, ChannelName: channel.Name,
		}))
}

// TestNotifyDelete_NamesReferrers —— R4.7 / FR-073：删除闸门按 UID 合并三类证据——
// 绑定投影（含归档 Agent）、未释放的活动 Run 冻结、`AGENT.md` 整词引用；400 文案与
// `data` / `detail_json` 都点名引用者，且行与冻结都不动。
// The delete gate merges binding, run-ref and document evidence; the 400 names every
// referrer in the message, the error data and the operation detail.
func TestNotifyDelete_NamesReferrers(t *testing.T) {
	env := newNotifyEnv(t)
	_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)
	channel, err := env.stored("ops")
	require.NoError(t, err)

	// 证据一：绑定投影（不写 AGENT.md 正文，证明 UID 投影自身就能拦住删除）。
	boundAgentUID, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, storage.NewBindingStore(env.db).Rebuild(context.Background(),
		boundAgentUID, "billing-agent", "sha256:fixture",
		core.ResourceBindings{NotifyChannels: []core.ResourceBinding{
			{Name: channel.Name, UID: channel.ResourceUID},
		}}, baselineClock()))

	// 证据二：未释放的活动运行冻结。
	const pendingRun = "cli-1761200000000-abc123"
	freezeNotifyRef(t, env, pendingRun, boundAgentUID, channel)

	// 证据三：另一份定义文件的整词引用。
	writeAgentDir(t, env.root, "digest-agent", `---
name: digest-agent
---

把摘要发到 ops 渠道。
`)

	err = env.admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), false)
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.ErrorContains(t, err, "billing-agent", "文案点名绑定者")
	require.ErrorContains(t, err, "digest-agent", "文案点名文档引用者")
	require.ErrorContains(t, err, pendingRun, "文案点名活动运行")

	agents, ok := adminErr.Data["agents"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"billing-agent", "digest-agent"}, agents,
		"两类 Agent 证据去重排序后合并")
	runIDs, ok := adminErr.Data["run_ids"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{pendingRun}, runIDs)
	require.NotEmpty(t, adminErr.Data["operation_id"], "失败必须可查操作标识")

	deletes := env.rows(core.ActionNotifyChannelDelete)
	require.Len(t, deletes, 1)
	require.Equal(t, core.OperationFailed, deletes[0].Status)
	require.Equal(t, agentResultBadRequest, deletes[0].ResultCode)
	var detail struct {
		Agents []string `json:"agents"`
		RunIDs []string `json:"run_ids"`
	}
	require.NoError(t, json.Unmarshal(deletes[0].DetailJSON, &detail))
	require.Equal(t, []string{"billing-agent", "digest-agent"}, detail.Agents)
	require.Equal(t, []string{pendingRun}, detail.RunIDs)

	_, err = env.stored("ops")
	require.NoError(t, err, "被引用的渠道不得被删除")
	refs, err := storage.NewNotifyDeliveryStore(env.db).UnreleasedRunsByChannel(
		context.Background(), channel.ResourceUID)
	require.NoError(t, err)
	require.Equal(t, []string{pendingRun}, refs, "拒绝删除不得释放冻结")
}

// TestNotifyDelete_ForceStillScans —— force 只跳过「命中」，**不得**跳过「证明不了
// 没人引用」：扫描失败、绑定反查失败、运行冻结反查失败三类证据故障在 force 下同样
// 拒绝；命中且 force 才删除，并在 `detail_json` 记 forced。
// Force only bypasses a hit, never an unproven absence: scan, binding and run-ref
// failures refuse even with force; a forced delete records forced=true.
func TestNotifyDelete_ForceStillScans(t *testing.T) {
	t.Run("扫描失败时 force 仍拒绝", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		deps := env.deps
		deps.Root = t.TempDir() // 没有 agents/：扫描证明不了"没人引用"
		admin := NewNotifyAdmin(deps)

		err = admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true)
		require.ErrorContains(t, err, "无法扫描")
		_, err = env.stored("ops")
		require.NoError(t, err, "扫描失败必须保留渠道")
	})

	t.Run("绑定反查失败时 force 仍拒绝", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		deps := env.deps
		deps.Bindings = failingBindingReferrer{}
		admin := NewNotifyAdmin(deps)

		err = admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true)
		require.ErrorContains(t, err, "无法核对资源绑定")
		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.NotEmpty(t, adminErr.Data["operation_id"], "失败必须可查操作标识")
		_, err = env.stored("ops")
		require.NoError(t, err, "反查失败必须保留渠道")
	})

	t.Run("运行冻结反查失败时 force 仍拒绝", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		deps := env.deps
		deps.RunRefs = failingRunRefReferrer{}
		admin := NewNotifyAdmin(deps)

		err = admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true)
		require.ErrorContains(t, err, "无法核对活动运行引用")
		_, err = env.stored("ops")
		require.NoError(t, err, "反查失败必须保留渠道")
	})

	t.Run("命中时 force 删除并记 forced", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		writeAgentDir(t, env.root, "digest-agent", `---
name: digest-agent
---

把摘要发到 ops 渠道。
`)

		require.NoError(t, env.admin.Delete(context.Background(), resourceActor(), "ops", env.uid("ops"), true),
			"force 跳过命中后删除")

		deletes := env.rows(core.ActionNotifyChannelDelete)
		require.Len(t, deletes, 1)
		require.Equal(t, core.OperationSuccess, deletes[0].Status)
		require.Contains(t, string(deletes[0].DetailJSON), `"forced":true`)
	})
}

// failingBindingReferrer 模拟绑定投影读取故障（删除闸门必须失败关闭）。
// failingBindingReferrer simulates a binding lookup failure.
type failingBindingReferrer struct{}

// ReferencingAgents 恒返回错误。
// ReferencingAgents always fails.
func (failingBindingReferrer) ReferencingAgents(context.Context, core.ResourceUID) ([]core.AgentResourceBinding, error) {
	return nil, errors.New("夹具模拟绑定投影故障")
}

// failingRunRefReferrer 模拟活动运行冻结读取故障。
// failingRunRefReferrer simulates a run-ref lookup failure.
type failingRunRefReferrer struct{}

// UnreleasedRunsByChannel 恒返回错误。
// UnreleasedRunsByChannel always fails.
func (failingRunRefReferrer) UnreleasedRunsByChannel(context.Context, core.ResourceUID) ([]string, error) {
	return nil, errors.New("夹具模拟运行冻结故障")
}

// ============================================================
// 托管凭据保存面（R4.8 / T141 app 侧）
// ============================================================
//
// 托管地址只进凭据密文：渠道行的 `url` 恒为空、只留 scheme + host，完整地址交给
// `CredentialSaveWriter`；`managed → env` 在同一次保存里落新占位并退役身份。下列用例
// 用替身记录写入材料，断言编排顺序（预检 → 写行 → 写凭据）与失败时的零写入。
// The managed save path keeps the address in ciphertext only; these tests pin the
// ordering (preflight, row, credential) and the zero-write failure paths.

// credentialRecorder 是 `CredentialSaveWriter` 的替身：记录托管写入就绪检查的次数与
// 每次写入的类别 / 名称 / UID / 模式 / 明文副本；就绪检查可注入失败。
// credentialRecorder records what the save path hands to the credential writer and can
// fail the readiness check on demand.
type credentialRecorder struct {
	preflight    int
	preflightErr error
	writes       []CredentialWriteRequest
}

// PrepareManagedWrite 记录一次就绪检查并返回注入的错误。
// PrepareManagedWrite counts one readiness check and returns the injected error.
func (r *credentialRecorder) PrepareManagedWrite(context.Context, core.Actor) error {
	r.preflight++
	return r.preflightErr
}

// Write 记录一次凭据写入（明文先复制，避免被测实现清零后断言失真）。
// Write records one credential write; the plaintext is copied before it can be zeroed.
func (r *credentialRecorder) Write(_ context.Context, req CredentialWriteRequest) (CredentialWriteResult, error) {
	recorded := req
	recorded.Value = append([]byte(nil), req.Value...)
	r.writes = append(r.writes, recorded)
	return CredentialWriteResult{Changed: true, State: core.CredentialIdentityActive}, nil
}

// newManagedNotifyEnv 在网络夹具之上接一个凭据写面替身。
// newManagedNotifyEnv wires the fixture with a recording credential writer.
func newManagedNotifyEnv(t *testing.T) (*notifyEnv, *credentialRecorder) {
	t.Helper()
	recorder := &credentialRecorder{}
	env := newNotifyEnv(t, func(deps *NotifyAdminDeps) { deps.CredentialWriter = recorder })
	return env, recorder
}

// seedManagedChannel 用真实仓储行造一条已托管渠道（凭据身份列有值），供退役路径读取
// 真实的存储行；操作行也走真实预写，不裸写 SQL。
// seedManagedChannel writes one managed channel row with a credential identity through
// the real stores, so the retire path reads a genuine stored row.
func seedManagedChannel(t *testing.T, env *notifyEnv, name, credentialID string) core.NotifyChannel {
	t.Helper()
	ctx := context.Background()
	opID := core.NewOperationID(baselineClock())
	require.NoError(t, env.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionNotifyChannelCreate,
		TargetKind:   core.TargetKindNotifyChannel,
		TargetID:     name,
		ActorSubject: resourceActor().Subject,
		AuthMethod:   resourceActor().Method,
		Source:       resourceActor().Source,
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	channel := core.NotifyChannel{
		ResourceUID:    uid,
		Name:           name,
		Type:           "webhook",
		URLHost:        "https://hooks.example.com",
		CredentialMode: core.CredentialModeManaged,
		Scope:          core.ScopeShared,
		CredentialID:   credentialID,
		OperationID:    opID,
	}
	require.NoError(t, env.store.Create(ctx, channel))
	return channel
}

// TestNotifyAdmin_ManagedCreateSealsAddress —— 托管创建：行上 url 为空、只留 scheme +
// host；落行前预检一次主密钥；完整地址只出现在凭据写入请求里，不进操作详情与日志。
// 托管模式收到 `${VAR}` 占位按契约码拒绝，零落库零操作行零预检。
func TestNotifyAdmin_ManagedCreateSealsAddress(t *testing.T) {
	ctx := context.Background()
	env, recorder := newManagedNotifyEnv(t)
	const address = "https://hooks.example.com/services/ops?token=inline-secret"

	created, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
		Name: "ops-alert", Type: "webhook", URL: address, URLSet: true,
		CredentialMode: "managed", Description: "托管地址",
	})
	require.NoError(t, err)
	require.Equal(t, core.CredentialModeManaged, created.CredentialMode)
	require.Empty(t, created.URL, "托管地址不得写进 url 列")
	require.Equal(t, "https://hooks.example.com", created.URLHost)

	stored, err := env.stored("ops-alert")
	require.NoError(t, err)
	require.Empty(t, stored.URL)
	require.Equal(t, "https://hooks.example.com", stored.URLHost)

	require.Equal(t, 1, recorder.preflight, "落新密文之前必须预检主密钥")
	require.Len(t, recorder.writes, 1)
	write := recorder.writes[0]
	require.Equal(t, core.BindingNotifyChannel, write.Kind)
	require.Equal(t, "ops-alert", write.Name)
	require.Equal(t, created.ResourceUID, write.UID)
	require.Equal(t, core.CredentialModeManaged, write.Mode)
	require.Equal(t, address, string(write.Value), "完整地址只进凭据写面")

	creates := env.rows(core.ActionNotifyChannelCreate)
	require.Len(t, creates, 1)
	require.Equal(t, core.OperationSuccess, creates[0].Status)
	require.NotContains(t, string(creates[0].DetailJSON), "inline-secret",
		"操作详情不得携带地址或查询串")
	require.NotContains(t, env.logs.String(), "inline-secret", "日志不得携带托管地址")

	t.Run("托管模式拒绝占位", func(t *testing.T) {
		env, recorder := newManagedNotifyEnv(t)
		_, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
			Name: "ops-alert", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}",
			URLSet: true, CredentialMode: "managed",
		})
		require.ErrorIs(t, err, core.ErrCredentialModeRequired)
		_, err = env.stored("ops-alert")
		require.ErrorIs(t, err, core.ErrResourceNotFound, "被拒的创建必须零落库")
		require.Zero(t, env.operationCount(), "形态校验在预写之前")
		require.Zero(t, recorder.preflight)
		require.Empty(t, recorder.writes)
	})
}

// TestNotifyAdmin_ManagedUpdateKeepsAddressSealed —— 省略地址只改元数据、不重写凭据；
// 重新给地址换的只是密文：行上 url 仍为空，host 换成新主机，凭据写面收到新地址。
func TestNotifyAdmin_ManagedUpdateKeepsAddressSealed(t *testing.T) {
	ctx := context.Background()
	env, recorder := newManagedNotifyEnv(t)
	const first = "https://hooks.example.com/services/ops"
	const second = "https://open.feishu.cn/open-apis/bot/v2/hook/new"

	created, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
		Name: "ops-alert", Type: "webhook", URL: first, URLSet: true, CredentialMode: "managed",
	})
	require.NoError(t, err)
	require.Len(t, recorder.writes, 1)

	t.Run("省略地址只改元数据", func(t *testing.T) {
		updated, err := env.update("ops-alert", NotifyChannelRequest{Type: "feishu", Description: "换类型"})
		require.NoError(t, err)
		require.Equal(t, "feishu", updated.Type)
		require.Empty(t, updated.URL)
		require.Equal(t, "https://hooks.example.com", updated.URLHost)
		require.Len(t, recorder.writes, 1, "省略地址不得触发凭据重写")
		require.Equal(t, 1, recorder.preflight)
	})

	t.Run("重新给地址只换密文", func(t *testing.T) {
		updated, err := env.update("ops-alert", NotifyChannelRequest{
			Type: "feishu", URL: second, URLSet: true, CredentialMode: "managed",
		})
		require.NoError(t, err)
		require.Empty(t, updated.URL)
		require.Equal(t, "https://open.feishu.cn", updated.URLHost)
		require.Equal(t, created.ResourceUID, updated.ResourceUID)
		require.Len(t, recorder.writes, 2)
		require.Equal(t, second, string(recorder.writes[1].Value))
		require.Equal(t, 2, recorder.preflight)
	})
}

// TestNotifyAdmin_ManagedToEnvRetiresIdentity —— managed → env：同一次保存落新的
// `${VAR}` 占位并退役旧身份；行上 credential_id 清空、模式转 env；退役只改状态，
// 不装载主密钥、不写新密文。
func TestNotifyAdmin_ManagedToEnvRetiresIdentity(t *testing.T) {
	const credentialID = "cred_0000000000000000000000000A"

	env, recorder := newManagedNotifyEnv(t)
	seed := seedManagedChannel(t, env, "ops-alert", credentialID)

	retired, err := env.update("ops-alert", NotifyChannelRequest{
		Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true, CredentialMode: "env",
	})
	require.NoError(t, err)
	require.Equal(t, core.CredentialModeEnv, retired.CredentialMode)
	require.Equal(t, "${BASALT_TEST_OPS_HOOK}", retired.URL)
	require.Equal(t, "https://hooks.example.com", retired.URLHost)
	require.Empty(t, retired.CredentialID, "退役后行上不得再挂凭据身份")
	require.Equal(t, seed.ResourceUID, retired.ResourceUID)

	require.Zero(t, recorder.preflight, "退役只改状态，不需要主密钥")
	require.Len(t, recorder.writes, 1)
	write := recorder.writes[0]
	require.Equal(t, core.CredentialModeEnv, write.Mode)
	require.Nil(t, write.Value, "退役不写新密文")
	require.Equal(t, seed.ResourceUID, write.UID)
}

// TestNotifyAdmin_ManagedWriteFailClosed —— 保存路径的 503 零写入：没有凭据写面与
// 主密钥预检失败都在预写之前拦下，不留渠道行、不留操作行、不留密文写入。
func TestNotifyAdmin_ManagedWriteFailClosed(t *testing.T) {
	ctx := context.Background()
	request := NotifyChannelRequest{
		Name: "ops-alert", Type: "webhook",
		URL: "https://hooks.example.com/services/ops", URLSet: true, CredentialMode: "managed",
	}

	t.Run("没有凭据写面", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.admin.Create(ctx, resourceActor(), request)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		_, err = env.stored("ops-alert")
		require.ErrorIs(t, err, core.ErrResourceNotFound, "503 必须零落库")
		require.Zero(t, env.operationCount(), "预检在预写之前：不得留操作行")
	})

	t.Run("主密钥预检失败", func(t *testing.T) {
		env, recorder := newManagedNotifyEnv(t)
		recorder.preflightErr = credentialUnavailableError("夹具：主密钥不可读")
		_, err := env.admin.Create(ctx, resourceActor(), request)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		_, err = env.stored("ops-alert")
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Zero(t, env.operationCount())
		require.Empty(t, recorder.writes)
	})
}

// TestNotifyAdmin_CreateExclusiveScope —— 创建时声明专属：属主在写行之前解析（存在、
// 可证明身份、未归档），行上带 scope 与 owner_agent_uid；属主不合格或被拒时零残余；
// 装配缺范围服务时拒绝专属创建（fail-closed）。
func TestNotifyAdmin_CreateExclusiveScope(t *testing.T) {
	ctx := context.Background()
	env := newNotifyEnv(t)
	versions := storage.NewResourceStore(env.db)
	env.deps.Scopes = &ScopeService{
		Resources: versions, Agents: versions, WorkspaceRoot: env.root, Now: baselineClock,
	}
	env.admin = NewNotifyAdmin(env.deps)

	seedOp := core.NewOperationID(baselineClock())
	require.NoError(t, env.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  seedOp,
		Action:       "system.baseline",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))
	ownerUID, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, versions.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 2, Agent: "ops-bot", AgentUID: ownerUID, State: core.AgentDisabled,
		Generation: 1, UpdatedAt: baselineClock().UnixMilli(), OperationID: seedOp,
	}, true, ""))
	archivedUID, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, versions.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 2, Agent: "old-bot", AgentUID: archivedUID, State: core.AgentArchived,
		Generation: 1, UpdatedAt: baselineClock().UnixMilli(), OperationID: seedOp,
	}, true, ""))

	t.Run("声明专属写入范围列", func(t *testing.T) {
		created, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
			Name: "ops-secret", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
			Scope: "exclusive", OwnerAgent: "ops-bot",
		})
		require.NoError(t, err)
		require.Equal(t, core.ScopeExclusive, created.Scope)
		require.Equal(t, ownerUID, created.OwnerAgentUID)
		stored, err := env.stored("ops-secret")
		require.NoError(t, err)
		require.Equal(t, core.ScopeExclusive, stored.Scope)
		require.Equal(t, ownerUID, stored.OwnerAgentUID)
	})

	t.Run("属主不存在零残余", func(t *testing.T) {
		before := env.operationCount()
		_, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
			Name: "nobody", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
			Scope: "exclusive", OwnerAgent: "ghost",
		})
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		_, err = env.stored("nobody")
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Equal(t, before, env.operationCount(), "属主解析在预写之前")
	})

	t.Run("归档属主拒绝", func(t *testing.T) {
		before := env.operationCount()
		_, err := env.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
			Name: "old", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
			Scope: "exclusive", OwnerAgent: "old-bot",
		})
		require.ErrorIs(t, err, core.ErrConflict)
		_, err = env.stored("old")
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Equal(t, before, env.operationCount())
	})

	t.Run("没有范围服务时拒绝专属创建", func(t *testing.T) {
		plain := newNotifyEnv(t)
		_, err := plain.admin.Create(ctx, resourceActor(), NotifyChannelRequest{
			Name: "ops-secret", Type: "webhook", URL: "${BASALT_TEST_OPS_HOOK}", URLSet: true,
			Scope: "exclusive", OwnerAgent: "ops-bot",
		})
		require.ErrorContains(t, err, "范围转换")
		require.Zero(t, plain.operationCount())
	})
}
