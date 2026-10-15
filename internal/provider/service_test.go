package provider_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧要走裸连接读库（见 auditFixture）：驱动经副作用注册。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
)

// auditFixture 是一次用例的审计判卷环境：真库（temp dir + storage.OpenWritable）+ 仓储 +
// **判卷只读句柄**。
// auditFixture bundles a real database, the repo, and a read handle used for assertions.
//
// 为什么另开只读句柄：`storage.DB` 对外不暴露任意查询（交付面只有 SchemaVersion / TableNames /
// Close），而 provider 是外部测试包、读不到它的内部字段——沿 US-1 在 cli/status_test.go 立下的
// 先例，判卷侧自开一条 `sql.Open("sqlite", …)`；**生产写入路径仍唯一经仓储**。
// The read handle is assertion-side only; production writes still go through the repo alone.
type auditFixture struct {
	db   *storage.DB
	repo *storage.LLMCallRepo
	read *sql.DB
}

// auditRow 是 llm_calls 一行的判卷形态；可空列用 COALESCE 收敛为"空值"（NULL 与空串在
// 本 US 的语义相同：未填）。
// auditRow is the assertion shape of one row; nullable columns are folded to empty.
type auditRow struct {
	Status           core.LLMCallStatus
	ErrorKind        core.ErrorKind
	ErrorMessage     string
	Provider         string
	Model            string
	RunID            string
	Round            int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMS       int64
}

// openTempDB 给每个用例一个独立的临时真库（文档 §4.3 夹具 `openTempDB(t)` 的落地形态：
// 判卷需要读回时用 fx.callRow / fx.countRows，故这里连只读句柄一起返回）。
// openTempDB hands every test its own temporary real database.
func openTempDB(t *testing.T) *auditFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(context.Background(), path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	return &auditFixture{db: db, repo: storage.NewLLMCallRepo(db), read: read}
}

// callRow 按 id 读回审计行。
// callRow reads one audit row back by id.
func (f *auditFixture) callRow(t *testing.T, id int64) auditRow {
	t.Helper()
	var row auditRow
	err := f.read.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(error_kind, ''), COALESCE(error_message, ''),
		        provider, model, run_id, round,
		        prompt_tokens, completion_tokens, total_tokens, duration_ms
		   FROM llm_calls WHERE id = ?`, id).
		Scan(&row.Status, &row.ErrorKind, &row.ErrorMessage, &row.Provider, &row.Model,
			&row.RunID, &row.Round, &row.PromptTokens, &row.CompletionTokens,
			&row.TotalTokens, &row.DurationMS)
	require.NoError(t, err)
	return row
}

// lastRow 读回最近一次调用留下的行（单调用用例的读出口；无行即失败）。
// lastRow reads the row left by the most recent call.
func (f *auditFixture) lastRow(t *testing.T) auditRow {
	t.Helper()
	var id int64
	require.NoError(t, f.read.QueryRowContext(context.Background(),
		`SELECT max(id) FROM llm_calls`).Scan(&id))
	require.Positive(t, id, "该用例应当至少留下一条审计行")
	return f.callRow(t, id)
}

// countRows 跑一条计数查询——「无行 = 确定没执行」的判据出口。
// countRows runs one counting query, the exit for the "no row means never executed" rule.
func (f *auditFixture) countRows(t *testing.T, query string) int {
	t.Helper()
	var n int
	require.NoError(t, f.read.QueryRowContext(context.Background(), query).Scan(&n))
	return n
}

// testService 组装被测服务；logs 为 nil 时日志丢弃（只有判日志轨的用例才收集）。
// testService builds the service under test; a nil buffer discards logs.
func testService(clients map[string]core.ProviderClient, models map[string]string,
	store core.LLMCallStore, logs *bytes.Buffer,
) *provider.ProviderService {
	var w io.Writer = io.Discard
	if logs != nil {
		w = logs
	}
	return provider.NewProviderService(clients, models, store, slog.New(slog.NewTextHandler(w, nil)))
}

// newServiceWithRepo 是"单家假客户端 + 真库"的标准装配（文档 §4.3 的 `newServiceWithRepo`）。
// newServiceWithRepo is the standard single-fake setup used by most service cases.
func newServiceWithRepo(t *testing.T, steps ...testutil.FakeStep) (*provider.ProviderService, *auditFixture, *testutil.FakeProvider) {
	t.Helper()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(steps...)
	svc := testService(map[string]core.ProviderClient{"fake": fakeP},
		map[string]string{"fake": "m"}, fx.repo, nil)
	return svc, fx, fakeP
}

// TestProviderService_RoutesByExplicitMap —— 宪法 III：路由**只查表**。
// 判据：请求名指向哪家，就只有哪家被调用；同名键的两条路径不因 map 遍历顺序而漂移。
func TestProviderService_RoutesByExplicitMap(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	first := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "first"}})
	second := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "second"}})
	svc := testService(map[string]core.ProviderClient{"alpha": first, "beta": second},
		map[string]string{"alpha": "m-alpha", "beta": "m-beta"}, fx.repo, nil)

	resp, err := svc.Chat(ctx, core.ChatRequest{Provider: "beta", RunID: "run-1", Round: 1})
	require.NoError(t, err)
	require.Equal(t, "second", resp.Content)

	require.Equal(t, 0, first.CallCount(), "未被点名的供应商不得收到调用")
	require.Equal(t, 1, second.CallCount())
	require.Equal(t, "m-beta", fx.lastRow(t).Model, "缺省模型同样按表取——不跨供应商借用")
}

// TestProviderService_UnknownProviderRejectedWithoutAuditRow —— 「无行 = 确定没执行」。
// 查表不中 → 点名错误、**零调用**、**零审计行**；日志轨收到 WARN（FR-013：路由失败记日志）。
func TestProviderService_UnknownProviderRejectedWithoutAuditRow(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "never"}})
	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, &logs)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "ghost", RunID: "run-1", Round: 1})

	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrProviderNotConfigured, "必须用哨兵表达'查表不中'")
	require.Contains(t, err.Error(), "ghost", "必须点名 provider")
	require.Equal(t, 0, fakeP.CallCount(), "查表不中不得发出调用")
	require.Equal(t, 0, fx.countRows(t, `SELECT count(*) FROM llm_calls`), "未发出的调用不得有审计行")
	require.Contains(t, logs.String(), "level=WARN", "路由失败记 WARN")
	require.Contains(t, logs.String(), "provider=ghost", "日志字段键固定（provider）")
}

// TestProviderService_RejectsMissingRunBinding —— 审计关联标识（易漏维度 / 约束 4）。
// run_id 为空 / round < 1 → 拒绝发出、零审计行（用法错误，不是调用失败）。
func TestProviderService_RejectsMissingRunBinding(t *testing.T) {
	cases := []struct {
		name string
		req  core.ChatRequest
	}{
		{name: "run_id 为空", req: core.ChatRequest{Provider: "fake", Round: 1}},
		{name: "round 为零", req: core.ChatRequest{Provider: "fake", RunID: "run-1"}},
		{name: "round 为负", req: core.ChatRequest{Provider: "fake", RunID: "run-1", Round: -3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := openTempDB(t)
			fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "never"}})
			var logs bytes.Buffer
			svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, &logs)

			_, err := svc.Chat(context.Background(), tc.req)

			require.Error(t, err)
			require.Contains(t, err.Error(), "run_id", "必须点名字段")
			require.Equal(t, 0, fakeP.CallCount())
			require.Equal(t, 0, fx.countRows(t, `SELECT count(*) FROM llm_calls`), "用法错误不得留审计行")
			require.Contains(t, logs.String(), "level=WARN")
		})
	}
}

// TestProviderService_UnresolvableModelRejectedWithoutAuditRow —— 模型解析的第三道拒绝。
// 请求模型与供应商缺省模型都空 → 点名 provider 拒绝、零调用、零审计行。
func TestProviderService_UnresolvableModelRejectedWithoutAuditRow(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "never"}})
	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": ""}, fx.repo, &logs)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-1", Round: 1})

	require.Error(t, err)
	require.Contains(t, err.Error(), "fake", "必须点名 provider")
	require.Contains(t, err.Error(), "模型", "必须点明是模型解析不了")
	require.Equal(t, 0, fakeP.CallCount())
	require.Equal(t, 0, fx.countRows(t, `SELECT count(*) FROM llm_calls`))
	require.Contains(t, logs.String(), "level=WARN")
}

// TestProviderService_DefaultModelFromMapAndOverride —— 模型解析顺序：请求模型 > 供应商缺省；
// 生效模型落 `llm_calls.model`（审计要能回答"这次调用用的哪个模型"）。
func TestProviderService_DefaultModelFromMapAndOverride(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(
		testutil.FakeStep{Response: core.ChatResponse{Content: "a"}},
		testutil.FakeStep{Response: core.ChatResponse{Content: "b"}},
	)
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "default-m"}, fx.repo, nil)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-default", Round: 1})
	require.NoError(t, err)
	require.Equal(t, "default-m", fx.lastRow(t).Model, "请求未指定 → 用供应商缺省模型")

	_, err = svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-override", Round: 2, Model: "ask-m"})
	require.NoError(t, err)
	row := fx.lastRow(t)
	require.Equal(t, "ask-m", row.Model, "请求指定 → 覆盖供应商缺省")
	require.Equal(t, "ask-m", fakeP.Requests()[1].Model, "生效模型同时是发给客户端的模型")
}

// TestProviderService_AuditPendingBeforeCall —— 留痕顺序（契约 §7.2：预写先于发出）。
// 判据不靠时序猜测：假 Provider 在 **Chat 内部**当场查库，此时该次调用的 pending 行
// 必须已经存在——"先发出、后补记"的实现会在这里判红。
func TestProviderService_AuditPendingBeforeCall(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)

	var pendingRows int
	probe := fakeProviderFunc(func(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
		// 此刻调用尚未返回；预写必须已经落库。
		pendingRows = fx.countRows(t, "SELECT count(*) FROM llm_calls WHERE status = 'pending'")
		return core.ChatResponse{Content: "ok"}, nil
	})

	svc := testService(map[string]core.ProviderClient{"fake": probe}, map[string]string{"fake": "m"},
		fx.repo, nil)

	_, err := svc.Chat(ctx, core.ChatRequest{
		Provider: "fake", RunID: "run-test", Round: 1, Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, pendingRows, "发出之前必须已存在 pending 行（fail-closed 预写）")
}

// fakeProviderFunc 把函数适配成 core.ProviderClient——判"留痕顺序"需要在 Chat **内部**查库，
// 固定脚本的 testutil.FakeProvider 给不了这个探针位。其余用例一律用 testutil.FakeProvider。
type fakeProviderFunc func(context.Context, core.ChatRequest) (core.ChatResponse, error)

func (f fakeProviderFunc) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	return f(ctx, req)
}

// TestProviderService_WritesSuccessRowWithUsage —— 成功行：状态 + 三 token 列 + 耗时 + 类别空。
func TestProviderService_WritesSuccessRowWithUsage(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{
		Content: "ok",
		Usage:   core.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, nil)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-1", SessionID: "sess-1", Round: 4})
	require.NoError(t, err)

	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallSuccess, row.Status)
	require.Equal(t, 11, row.PromptTokens)
	require.Equal(t, 7, row.CompletionTokens)
	require.Equal(t, 18, row.TotalTokens)
	require.GreaterOrEqual(t, row.DurationMS, int64(0), "耗时落毫秒列（不设下界断言——本机可快到 0ms）")
	require.Equal(t, core.ErrorKind(""), row.ErrorKind, "成功行 error_kind 为 NULL")
	require.Equal(t, "", row.ErrorMessage, "成功行 error_message 为 NULL")
	require.Equal(t, "run-1", row.RunID)
	require.Equal(t, 4, row.Round)
}

// TestProviderService_UsageAbsentIsZero —— usage 可空兜底（FR-006）：三列 0、不判失败、不 panic。
func TestProviderService_UsageAbsentIsZero(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "ok"}})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, nil)

	resp, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-1", Round: 1})
	require.NoError(t, err, "供应商未返回 usage 不判失败")
	require.Equal(t, "ok", resp.Content)

	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallSuccess, row.Status)
	require.Zero(t, row.PromptTokens)
	require.Zero(t, row.CompletionTokens)
	require.Zero(t, row.TotalTokens)
	require.Equal(t, core.ErrorKind(""), row.ErrorKind)
}

// TestProviderService_ErrorResponseRecordedWithKind —— **错误模型响应**（走真客户端 + 假服务）：
// 供应商 5xx → 行 failed + external_error + 失败原文；日志轨收到 WARN（FR-013）。
func TestProviderService_ErrorResponseRecordedWithKind(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		StatusCode: http.StatusBadGateway,
		Body:       `{"error":{"message":"upstream boom","type":"server_error"}}`,
	})
	client, err := provider.NewOpenAIClient(provider.Options{Name: "boom", BaseURL: srv.URL(), APIKey: "k"})
	require.NoError(t, err)

	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"boom": client}, map[string]string{"boom": "m"}, fx.repo, &logs)

	_, chatErr := svc.Chat(ctx, core.ChatRequest{
		Provider: "boom", RunID: "run-1", Round: 1,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})

	require.Error(t, chatErr, "失败不得被吞")
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status)
	require.Equal(t, core.ErrorKindExternalError, row.ErrorKind)
	require.Contains(t, row.ErrorMessage, "upstream boom", "服务器给的诊断信息要留")
	require.LessOrEqual(t, len(row.ErrorMessage), 4*1024, "失败原文截断上界（落库责任在 storage）")
	require.Contains(t, logs.String(), "level=WARN", "调用失败记 WARN")
	require.Contains(t, logs.String(), "error_kind=external_error", "日志字段键固定（error_kind）")
}

// TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs —— 凭证三路（约束 8）。
// 假服务把 Authorization 里的 key **原样回显**进错误正文（现实里供应商确实会这么干）。
// 客户端必须在出口前抹掉它——判据是"三处都不含"，不是"我们记得别打印"。
func TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs(t *testing.T) {
	const key = "sk-synthetic-echo-probe" // 合成值：非真凭证，也不触发仓库的密钥扫描模式
	ctx := context.Background()
	fx := openTempDB(t)

	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		StatusCode: http.StatusUnauthorized,
		Body:       `{"error":{"message":"invalid key: ` + key + `","type":"invalid_request_error"}}`,
	})
	client, err := provider.NewOpenAIClient(provider.Options{Name: "echo", BaseURL: srv.URL(), APIKey: key})
	require.NoError(t, err)

	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"echo": client},
		map[string]string{"echo": "m"}, fx.repo, &logs)

	_, chatErr := svc.Chat(ctx, core.ChatRequest{
		Provider: "echo", RunID: "run-test", Round: 1,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})

	require.Error(t, chatErr, "401 必须失败")
	require.NotContains(t, chatErr.Error(), key, "返回错误不得含 key")
	require.NotContains(t, logs.String(), key, "日志缓冲不得含 key")
	require.NotContains(t, fx.lastRow(t).ErrorMessage, key, "审计 error_message 不得含 key")
}

// TestProviderService_RefusesCallWhenAuditPreWriteFails —— 审计写入失败（宪法 V 的可执行判据）。
// 断言的是**客户端未被调用**：审计不可用时"拒绝发出"，而不是"照发再抱怨"。
func TestProviderService_RefusesCallWhenAuditPreWriteFails(t *testing.T) {
	ctx := context.Background()
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "never"}})
	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"},
		failingStore{}, &logs)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

	require.Error(t, err, "预写失败必须返回错误")
	require.Equal(t, 0, fakeP.CallCount(), "预写失败时**不得**发出调用（fail-closed）")
	require.Contains(t, logs.String(), "level=ERROR", "预写失败 = 服务降级，记 ERROR")
}

// failingStore 是 core.LLMCallStore 的失败注入实现：预写必失败。
type failingStore struct{}

func (failingStore) BeginCall(context.Context, core.LLMCall) (int64, error) {
	return 0, errors.New("audit unavailable")
}

func (failingStore) FinishCall(context.Context, int64, core.LLMCallResult) error { return nil }

// TestProviderService_TerminalWriteFailureKeepsPendingRowAndLogsERROR —— 残余窗口。
// 终态写失败：行**留 pending**（不伪造终态，待 US-4 启动恢复标 unknown）、ERROR 日志；
// 调用结果照常返回（终态写失败不改变"这次调用成功了"这一事实）。
func TestProviderService_TerminalWriteFailureKeepsPendingRowAndLogsERROR(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	var logs bytes.Buffer
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "ok"}})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"},
		finishFailingStore{fx.repo}, &logs)

	resp, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

	require.NoError(t, err, "终态写失败不得改写调用结果")
	require.Equal(t, "ok", resp.Content)
	require.Contains(t, logs.String(), "审计终态回填失败", "残余窗口必须落 ERROR 日志")
	require.Contains(t, logs.String(), "level=ERROR")
	require.Equal(t, core.LLMCallPending, fx.lastRow(t).Status, "行留在 pending，待启动恢复标 unknown")
}

// TestProviderService_CanceledCallTerminalWriteFailurePreservesCancelChain —— 取消 + 终态写失败。
// 两条纪律一次判完：① 行留 pending（不伪造终态）；② 返回的**仍是取消错误**——终态写失败
// 只记 ERROR，不得覆盖调用结果（约束 7；契约 §4.2 第二句）。
func TestProviderService_CanceledCallTerminalWriteFailurePreservesCancelChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := openTempDB(t)
	var logs bytes.Buffer
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Block: true})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"},
		finishFailingStore{fx.repo}, // 预写真写、终态必失败
		&logs)

	go func() { <-fakeP.Started(1); cancel() }()
	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

	require.ErrorIs(t, err, context.Canceled, "终态写失败不得覆盖取消错误链")
	require.Contains(t, logs.String(), "审计终态回填失败", "残余窗口必须落 ERROR 日志")
	require.Equal(t, core.LLMCallPending, fx.lastRow(t).Status, "行留在 pending，待启动恢复标 unknown")
}

// finishFailingStore 包一层真仓储：预写真写、终态必失败——残余窗口的注入点。
// 嵌入接口只覆盖 FinishCall，其余方法原样转发（注入面保持最小）。
type finishFailingStore struct{ core.LLMCallStore }

func (finishFailingStore) FinishCall(context.Context, int64, core.LLMCallResult) error {
	return errors.New("audit write failed")
}

// TestProviderService_CancelDuringCallRecordsTimeoutKind —— 取消路径（契约 §4.2 + §7.3）。
// 同步方式：假 Provider 进入 Chat 后**关信号通道**，测试收到信号才取消——不用 time.Sleep
// 猜时序。Sleep 的坏处是具体的：取消可能落在预写完成之前，用例会走成 fail-closed 拒发那条
// 分支（判的就不是取消路径），而且时紧时松。
func TestProviderService_CancelDuringCallRecordsTimeoutKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, fx, fakeP := newServiceWithRepo(t, testutil.FakeStep{Block: true})

	go func() {
		<-fakeP.Started(1) // 调用确已进入（此时预写必然已完成）
		cancel()
	}()
	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

	require.ErrorIs(t, err, context.Canceled, "取消错误链必须保留（US-4 据此判 cancelled）")
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status, "取消也不得留下 pending 行")
	require.Equal(t, core.ErrorKindTimeout, row.ErrorKind)
	require.Equal(t, 1, fakeP.CallCount())
}

// TestProviderService_PreCanceledContextStillAuditsFailedCall —— 调用前 ctx 已取消（§2.4 注 8）。
// 预写与取消解耦：行照落 pending → 发出（客户端立刻以 context.Canceled 失败）→ 终态
// failed + timeout。另一条路（预写用原 ctx）会让"取消"伪装成"审计不可用"的 fail-closed
// 拒发——与契约 §4.2 首句的理由（被取消的调用也必须落审计）相反。
func TestProviderService_PreCanceledContextStillAuditsFailedCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 调用前就取消
	svc, fx, fakeP := newServiceWithRepo(t, testutil.FakeStep{Err: context.Canceled})

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

	require.ErrorIs(t, err, context.Canceled)
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status, "预写已解耦——取消不改变'这次调用存在过'")
	require.Equal(t, core.ErrorKindTimeout, row.ErrorKind)
	require.Equal(t, 1, fakeP.CallCount(), "已取消不改变调用路径：仍走'预写→发出→终态'")
}

// TestProviderService_ConcurrentCallsAreRaceFree —— 并发（约束 14）。
// 一半脚本步阻塞、一半立即返回：等全部调用**进入**后一并取消，于是"成功"与
// "取消"两条路径在同一进程内并发走完；`-race` 判竞态，行数与终态判"没有半截活"。
func TestProviderService_ConcurrentCallsAreRaceFree(t *testing.T) {
	const total = 8
	const warmup = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := openTempDB(t)

	// 第一步留给热身。连接池的常驻 goroutine 在**首次使用**时才起（属数据库驱动，不属被测
	// 路径），先把它们热起来再取基线，否则"泄漏"读数会把驱动的池启动算在被测代码账上。
	steps := make([]testutil.FakeStep, 0, warmup+total)
	steps = append(steps, testutil.FakeStep{Response: core.ChatResponse{Content: "warmup"}})
	for i := range total {
		if i%2 == 0 {
			steps = append(steps, testutil.FakeStep{Block: true}) // 会被取消
		} else {
			steps = append(steps, testutil.FakeStep{Response: core.ChatResponse{Content: "ok"}})
		}
	}
	fakeP := testutil.NewFakeProvider(steps...)
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, nil)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-warmup", Round: 1})
	require.NoError(t, err)
	require.Equal(t, 1, fx.countRows(t, `SELECT count(*) FROM llm_calls`), "热身留下且只留下一行")

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, total)
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Chat(ctx, core.ChatRequest{
				Provider: "fake", RunID: "run-" + string(rune('a'+i)), Round: 1,
			})
			errs[i] = err
		}()
	}

	// 全部调用进入后再取消：已返回的调用不受影响，阻塞的那几条走取消路径。
	require.Eventually(t, func() bool { return fakeP.CallCount() == warmup+total },
		2*time.Second, time.Millisecond, "全部调用都进入后才有确定的取消面")
	cancel()
	wg.Wait()

	require.Equal(t, warmup+total, fakeP.CallCount())
	require.Equal(t, warmup+total, fx.countRows(t, `SELECT count(*) FROM llm_calls`), "每次发出的调用一行")
	require.Equal(t, 0, fx.countRows(t, `SELECT count(*) FROM llm_calls WHERE status = 'pending'`),
		"返回的调用不得留下半截行")

	// 计数的两条出口（并发下调用与脚本步的配对顺序不定，故按"各走了几条"判）。
	require.Equal(t, warmup+total/2, fx.countRows(t, `SELECT count(*) FROM llm_calls WHERE status = 'success'`))
	require.Equal(t, total/2, fx.countRows(t, `SELECT count(*) FROM llm_calls WHERE status = 'failed' AND error_kind = 'timeout'`))

	// 取消的那几条：错误链保留取消；其余成功。
	for i, err := range errs {
		if err == nil {
			continue
		}
		require.ErrorIsf(t, err, context.Canceled, "第 %d 次调用的错误", i)
	}

	// 泄漏判据：轮询在**测试自身的 goroutine** 里做。不能用 require.Eventually——它把条件
	// 放进新起的 goroutine 求值，那一条就把读数抬到基线之上，条件永远不成立（实测踩坑：
	// 直接读数已回到基线，Eventually 却始终报 "Condition never satisfied"）。
	// The poll must run on the test goroutine: testify's Eventually evaluates the
	// condition in a fresh goroutine, which itself pushes the count above baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), baseline,
		"调用结束后不得留下 goroutine（基线与实测都在测试 goroutine 内读）")
}

// TestProviderService_ResponseFieldsPassThrough —— 透传（契约：工具调用只透传不执行；
// 思考内容不得丢）：客户端给回什么，调用方就拿到什么。
func TestProviderService_ResponseFieldsPassThrough(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	want := core.ChatResponse{
		Content:          "",
		ToolCalls:        []core.ToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		ReasoningContent: "thinking…",
		ProviderExtra:    map[string]any{"vendor_sig": "sig-9"},
		Usage:            core.Usage{TotalTokens: 3},
	}
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: want})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, nil)

	got, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-1", Round: 1})

	require.NoError(t, err, "带工具调用且 content 为空：只透传，不判失败")
	require.Equal(t, want, got, "响应字段两端保真（含空 content / tool_calls / 思考内容 / 透传位）")
	require.Equal(t, core.LLMCallSuccess, fx.lastRow(t).Status)
}

// TestProviderService_UnclassifiedErrorRecordedAsUnknownKind —— 「未知」兜底（FR-008 第三类 / 契约 §7.3）：
// 假客户端抛回**裸错误**（非 CallError、错误链上无 ctx 错误）→ 行 failed + unknown。
// 该值**不是**恢复路径专属（恢复扫描以 status=unknown 判定，与本兜底同值不同因）。
func TestProviderService_UnclassifiedErrorRecordedAsUnknownKind(t *testing.T) {
	ctx := context.Background()
	fx := openTempDB(t)
	fakeP := testutil.NewFakeProvider(testutil.FakeStep{Err: errors.New("vendor blew up")})
	svc := testService(map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"}, fx.repo, nil)

	_, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-1", Round: 1})

	require.Error(t, err)
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status, "兜底不等于恢复路径的 unknown 状态")
	require.Equal(t, core.ErrorKindUnknown, row.ErrorKind)
	require.Contains(t, row.ErrorMessage, "vendor blew up", "原文照记（脱敏责任在客户端边界）")
}

// TestProviderService_CancelDuringResponseReadRecordsTimeoutKind —— P1 回归（审计面）：
// 取消落在**响应头已返回之后**（客户端已越过 Do、阻塞在读取正文）时，终态必须是
// failed + timeout，且失败原文点名读取阶段（「未取完响应」）——这一阶段曾被统一记成
// external_error（客户端类别与终态回填类别同时错）。
// 触发点用 readPhaseTrigger（余量，非同步手段）：响应头毫秒级到达，取消必然落在读取中。
func TestProviderService_CancelDuringResponseReadRecordsTimeoutKind(t *testing.T) {
	fx := openTempDB(t)
	client, err := provider.NewOpenAIClient(provider.Options{
		Name: "slow", BaseURL: stalledResponseServer(t), APIKey: "k",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(readPhaseTrigger, cancel)
	defer func() { timer.Stop(); cancel() }()

	var logs bytes.Buffer
	svc := testService(map[string]core.ProviderClient{"slow": client}, map[string]string{"slow": "m"}, fx.repo, &logs)

	_, chatErr := svc.Chat(ctx, core.ChatRequest{
		Provider: "slow", RunID: "run-read-cancel", Round: 1,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})

	require.ErrorIs(t, chatErr, context.Canceled, "取消错误链必须保留")
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status, "取消不得留下 pending 行")
	require.Equal(t, core.ErrorKindTimeout, row.ErrorKind, "读取阶段的取消归 timeout——曾误记 external_error")
	require.Contains(t, row.ErrorMessage, "未取完响应", "失败来自读取阶段（响应头已返回）")
	require.Contains(t, logs.String(), "error_kind=timeout", "日志字段键固定（error_kind）")
}

// TestProviderService_MalformedResponseRecordsFailedAudit —— P2 回归（审计面）：200 但首条
// choice 缺 message（`{"choices":[{}]}`）→ 客户端判 external_error → 行 failed + 类别落地，
// **不得**按零值响应记 success（那会把坏响应当成空回答写进审计，事后无从分辨）。
func TestProviderService_MalformedResponseRecordsFailedAudit(t *testing.T) {
	fx := openTempDB(t)
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{Body: `{"choices":[{}]}`})
	client, err := provider.NewOpenAIClient(provider.Options{Name: "bad", BaseURL: srv.URL(), APIKey: "k"})
	require.NoError(t, err)

	svc := testService(map[string]core.ProviderClient{"bad": client}, map[string]string{"bad": "m"}, fx.repo, nil)

	_, chatErr := svc.Chat(context.Background(), core.ChatRequest{
		Provider: "bad", RunID: "run-bad-response", Round: 1,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})

	require.Error(t, chatErr, "坏响应不得被吞")
	row := fx.lastRow(t)
	require.Equal(t, core.LLMCallFailed, row.Status, "坏响应不得记 success")
	require.Equal(t, core.ErrorKindExternalError, row.ErrorKind, "响应不可解析归 external_error")
}
