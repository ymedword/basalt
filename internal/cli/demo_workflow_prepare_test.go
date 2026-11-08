package cli

// 本文件判 `demo workflow prepare` 的命令面：计划总是执行、写必须 `--yes`、部分成功
// 退出 3、端口缺失失败关闭。服务侧的 HTTP 行为由 internal/app 的 TestDemoPrepare_* 判，
// 这里只判"命令怎么对待这两段结果"。

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeDemoPrepare 记录调用次数，按注入结果回答——命令面不得自行推断成败。
// fakeDemoPrepare records the calls and replays injected outcomes.
type fakeDemoPrepare struct {
	plan        DemoPreparePlan
	result      DemoPrepareResult
	planErr     error
	applyErr    error
	planCalls   int
	applyCalls  int
	lastRequest DemoPrepareRequest
}

// Plan 记录一次计划调用。
func (f *fakeDemoPrepare) Plan(_ context.Context, req DemoPrepareRequest) (DemoPreparePlan, error) {
	f.planCalls++
	f.lastRequest = req
	return f.plan, f.planErr
}

// Apply 记录一次应用调用。
func (f *fakeDemoPrepare) Apply(_ context.Context, req DemoPrepareRequest, _ DemoPreparePlan) (DemoPrepareResult, error) {
	f.applyCalls++
	f.lastRequest = req
	return f.result, f.applyErr
}

// preparePlanFixture 是一份"全部要新建"的计划。
// preparePlanFixture is an all-create plan.
func preparePlanFixture() DemoPreparePlan {
	return DemoPreparePlan{
		Server:      "http://127.0.0.1:8080",
		WorkflowDir: ".basalt/samples/bid-readiness",
		Collections: []DemoCollectionPlan{
			{Name: "bid-request", Action: "create"},
			{Name: "bid-product", Action: "create"},
		},
		Documents: []DemoDocumentPlan{
			{Collection: "bid-request", Path: "samples/bid-readiness/sources/request-01.md", Action: "ingest"},
			{Collection: "bid-product", Path: "samples/bid-readiness/sources/product-main.md", Action: "ingest"},
		},
		Bindings: []DemoBindingPlan{
			{Agent: "bid-requirements", Action: "bind", Knowledge: []core.KnowledgeBinding{{Name: "bid-request", Access: core.KnowledgeAccessRead}}},
			{Agent: "bid-technical", Action: "bind", Knowledge: []core.KnowledgeBinding{{Name: "bid-product", Access: core.KnowledgeAccessRead}}},
		},
		Warnings: []string{"将摄取 9 个文档，可能产生 embedding 调用费用"},
	}
}

// prepareResultFixture 是"一个成功、一个冲突"的部分成功结果。
// prepareResultFixture is a partial outcome: one applied, one conflicting.
func prepareResultFixture() DemoPrepareResult {
	return DemoPrepareResult{
		Collections: []DemoCollectionResult{{Name: "bid-request", CollectionID: "col_001", Created: true}},
		Documents:   []DemoDocumentResult{{Collection: "bid-request", Path: "samples/bid-readiness/sources/request-01.md", Status: "active"}},
		Bindings: []DemoBindingResult{
			{Agent: "bid-requirements", Status: "applied"},
			{Agent: "bid-technical", Status: "conflict"},
		},
		Partial: true,
	}
}

// executePrepare 以给定 deps 与参数跑一次命令。
// executePrepare runs the command once with the given deps and arguments.
func executePrepare(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	// 必须先执行再取字符串：Go 的返回表达式从左到右求值，写成
	// `return out.String(), cmd.Execute()` 会先读到空缓冲。
	// Execute first: return operands are evaluated left to right.
	err := cmd.Execute()
	return out.String(), err
}

// TestCLI_DemoWorkflowPreparePlanNoWrites 判"无 --yes 只预演"：逐项动作与费用提示
// 都要打印出来，且**不调用** Apply。
// TestCLI_DemoWorkflowPreparePlanNoWrites prints the plan and never applies without --yes.
func TestCLI_DemoWorkflowPreparePlanNoWrites(t *testing.T) {
	fake := &fakeDemoPrepare{plan: preparePlanFixture()}
	output, err := executePrepare(t, Deps{WorkDir: t.TempDir(), DemoPrepare: fake},
		"demo", "workflow", "prepare", "--server", "http://127.0.0.1:8080")
	require.NoError(t, err)
	require.Equal(t, 1, fake.planCalls)
	require.Zero(t, fake.applyCalls, "没有 --yes 时不得调用 Apply")
	require.Contains(t, output, "只读预演")
	require.Contains(t, output, "collection bid-request: create")
	require.Contains(t, output, "document bid-request samples/bid-readiness/sources/request-01.md: ingest")
	require.Contains(t, output, "binding bid-requirements: bind → bid-request(read)")
	require.Contains(t, output, "embedding")
	require.Equal(t, "http://127.0.0.1:8080", fake.lastRequest.Server)
}

// TestCLI_DemoWorkflowPrepareRequiresYesForWrites 判 `--yes` 才写、并按逐项结果收场：
// 部分成功退出 3；硬失败原样上抛；端口缺失失败关闭。
// TestCLI_DemoWorkflowPrepareRequiresYesForWrites applies only with --yes and maps the
// outcome to the documented exit codes.
func TestCLI_DemoWorkflowPrepareRequiresYesForWrites(t *testing.T) {
	fake := &fakeDemoPrepare{plan: preparePlanFixture(), result: prepareResultFixture()}
	output, err := executePrepare(t, Deps{WorkDir: t.TempDir(), DemoPrepare: fake},
		"demo", "workflow", "prepare", "--yes")
	require.Error(t, err)
	require.Equal(t, 3, ExitStatusOf(err), "部分成功必须是退出码 3")
	require.Equal(t, 1, fake.applyCalls)
	require.Contains(t, output, "binding bid-technical: conflict")
	require.Contains(t, output, "partial: true")
	require.NotContains(t, output, "全部完成", "部分成功不得打印整体成功")

	hard := &fakeDemoPrepare{plan: preparePlanFixture(), applyErr: errors.New("管理面拒绝了请求")}
	_, err = executePrepare(t, Deps{WorkDir: t.TempDir(), DemoPrepare: hard}, "demo", "workflow", "prepare", "--yes")
	require.ErrorContains(t, err, "管理面拒绝了请求")
	require.Equal(t, 1, ExitStatusOf(err), "硬失败保持 1")

	nilPort := &fakeDemoPrepare{plan: preparePlanFixture()}
	output, err = executePrepare(t, Deps{WorkDir: t.TempDir()}, "demo", "workflow", "prepare")
	require.ErrorContains(t, err, "demo 准备端口未注入")
	require.NotContains(t, output, "只读预演", "端口缺失时不得打印计划")
	require.NotContains(t, output, "collection ") // cobra 会把错误文案写进同一缓冲，只排除计划内容
	require.Zero(t, nilPort.planCalls)

	planned := &fakeDemoPrepare{plan: preparePlanFixture(), planErr: errors.New("没有可用的管理面凭据")}
	_, err = executePrepare(t, Deps{WorkDir: t.TempDir(), DemoPrepare: planned}, "demo", "workflow", "prepare", "--yes")
	require.ErrorContains(t, err, "没有可用的管理面凭据")
	require.Zero(t, planned.applyCalls, "计划失败时不得进入应用")
}
