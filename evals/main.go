// Package main 是评测基座运行器（颗粒度文档 §3.1 的 evals 四件之一；FR-026）。
//
// 一条用例的完整生命周期：
//
//	加载 YAML（输入 / 夹具 / 预算 / 可执行断言）
//	  → 仓库**外**的临时工作区里建骨架、写夹具与配置
//	  → app.OpenRuntime 走生产**同一条**重路径装配（锁 → 打开 → 迁移 → 恢复）
//	  → 经唯一运行入口跑一次（默认后端 = 进程内假模型；真模型显式门控）
//	  → 断言逐条判 PASS / FAIL
//	  → 读数（运行标识 / 状态 / 工具序列 / token 用量 / 调用次数 / 计量完整性 / 审计行）
//	    写进 evals/results/<时间戳>.json
//
// 三条纪律：
//
//  1. **零新口径**——读数只从生产同一条审计链取（llm_calls 行 + 会话消息 + 工具审计行），
//     不新增数据表、不另建统计面；结果文件逐例带运行标识，可据此回到审计行；
//  2. **缺一不发**——真模型档要求命令行 `--provider <name>` 与配置里的凭证同时就位；
//     任一缺失即**跳过**该用例，且跳过的用例**不计入通过**（跳过不是通过）；
//  3. **不换算金额**——结果文件只写 token 用量与调用次数；用量缺失 / 行仍悬空（pending）/
//     未知时标「计量不完整」，**不得**把库中默认的零读成零成本。
//
// 结果文件是**运行产物**而非源码：每次运行落一个新时间戳文件，收口时按临时物清除。
// The result files are run artifacts, never sources.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/app"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// 运行器的路径 / 名字字面量（工作区布局与配置文件名与装配层同源；此处按字面量引用，
// 不导出、不反向依赖）。路径一律相对**进程工作目录**——本命令必须在仓库根执行。
// Runner literals. Every path is relative to the process working directory, so the
// command must be run from the repository root.
const (
	defaultCasesDir = "evals/cases"
	resultsDirName  = "evals/results"

	workspaceDirName = ".basalt"
	agentsDirName    = "agents"
	agentFileName    = "AGENT.md"
	configFileName   = "basalt.yaml"

	// probeProviderName 是 fake 档临时配置里的供应商名。用例的 AGENT.md 声明它，
	// 路由查表因此仍是一次真实查表（宪法 III 的显式映射在评测里一步不少）。
	probeProviderName = "probe"
	probeBaseURL      = "http://127.0.0.1:1"
	probeDefaultModel = "eval-probe-model"

	// probeAPIKeyEnv 是 fake 档配置里的凭证**占位名**（配置里只写 ${VAR}，明文不落盘）；
	// probeAPIKeyValue 是进程内自设的哨兵值——它不是凭证，也不会被写到任何地方。
	// The placeholder name goes to disk; the sentinel is set in-process only.
	//
	// 这两行是 G101（硬编码凭证）的**定向豁免**：逐行豁免、只覆盖 G101 一条规则，其余规则
	// 照常判卷。理由是两条字符串都不是凭证——一条是环境变量**名字**，一条是进程内哨兵
	// 值（本仓凭证一律 ${ENV_VAR} 占位，明文不落盘）。豁免经用户裁决，登记在 US-4 验收
	// 报告的例外清单里。
	probeAPIKeyEnv   = "BASALT_EVAL_KEY"             // #nosec G101 -- 环境变量占位**名**，非凭证值
	probeAPIKeyValue = "eval-probe-not-a-credential" // #nosec G101 -- 进程内哨兵值，不落盘

	// evalChannel / evalUserID 是评测运行固定的渠道与用户标识：渠道取值决定 runs.trigger
	// 映射（§6.2 固定口径 8），取 `cli` 即 `<channel>-<unix_millis>-<6 位 base36>` 形态
	// 的运行标识（口径 2）。
	evalChannel = "cli"
	evalUserID  = "eval"

	// caseTimeout 是单条用例的运行上限：进程内假模型瞬间返回，真模型档留足响应时间。
	// 挂死不等于失败——有上限才会把"没返回"如实变成 FAIL 而不是永远挂着。
	// One case never blocks the runner forever.
	caseTimeout = 120 * time.Second
)

// 四类读数（颗粒度文档 §3.1 的 evals 用例集要求：工具选择 / 业务结果 / 越权尝试 / 资源用量）。
// 用例的 kind 只许取这四个字面量——拼错的类别会让"四类各至少一例"变成一句无法复核的话。
// The four reading categories a case's kind may name.
const (
	kindToolSelection = "工具选择"
	kindOutcome       = "业务结果"
	kindInjection     = "越权尝试"
	kindUsage         = "资源用量"
)

// evalKinds 是 kind 的合法取值集（用于装载期校验与汇总输出）。
var evalKinds = []string{kindToolSelection, kindOutcome, kindInjection, kindUsage}

// caseIDPattern / agentNamePattern 限定用例 id 与 Agent 名：两者都会进入会话标识与文件路径
// （`eval-<id>`），只接受小写字母、数字与连字符——路径分隔符与空格的形态在这里先被挡下。
// Case ids and agent names feed session ids and file paths, so the shape is constrained.
var caseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// 退出码：0 = 无 FAIL（跳过不算失败）；1 = 有用例 FAIL；2 = 运行器自身出错（用例装载 /
// 结果文件写不出 / 参数非法）。
const (
	exitOK       = 0
	exitCaseFail = 1
	exitHarness  = 2
)

// ---------------------------------------------------------------------------
// 用例模型（YAML 的逐字映射）
// ---------------------------------------------------------------------------

// evalCase 是一条用例的完整输入。字段与 yaml 键逐字对应——README 里的用例格式说明
// 与本结构是同一份契约（改一处必须改另一处）。
// evalCase mirrors the case YAML verbatim; evals/README.md documents the same contract.
type evalCase struct {
	// ID 是用例标识：同时是结果文件的条目键、会话标识（`eval-<id>`）与排序键。
	ID string `yaml:"id"`

	// Kind 是四类读数之一（见 evalKinds）；结果文件按类别汇总，四类各至少一例。
	Kind string `yaml:"kind"`

	// Provider 为空 = fake 档（进程内假模型 + 脚本回放）；非空 = 真模型档，值必须与
	// 命令行的 `--provider` 逐字相等才执行（缺一不发）。
	Provider string `yaml:"provider"`

	// Description 人读说明（进结果文件，便于核对"这一例在读什么"）。
	Description string `yaml:"description"`

	// Message 是用户消息（一次运行一条）。
	Message string `yaml:"message"`

	// Fixture 是工作区夹具（Agent 目录 + 文件 + 工作区文件）。
	Fixture fixture `yaml:"fixture"`

	// Budget 是执行预算（零值取核心缺省；Profile 只能收紧迭代上限）。
	Budget budgetSpec `yaml:"budget"`

	// Script 是假模型的逐轮回放（**只** fake 档可用；真模型档写了即装载期拒绝——
	// 一条对真模型无效的脚本会让读数看起来是脚本控制的，那是假的确定性）。
	Script []scriptReply `yaml:"script"`

	// Expect 是可执行断言集。
	Expect expectations `yaml:"expect"`
}

// fixture 是一条用例要建出来的工作区形态：Agent 目录名、AGENT.md 正文、Agent 目录内文件、
// 工作区（`.basalt/`）内其他文件（如别的 Agent 的目录——跨 Agent 读取用例的靶子）。
// fixture describes the workspace one case needs.
type fixture struct {
	Agent          string            `yaml:"agent"`
	AgentMD        string            `yaml:"agent_md"`
	Files          map[string]string `yaml:"files"`
	WorkspaceFiles map[string]string `yaml:"workspace_files"`
}

// budgetSpec 是用例的预算三件；零值 = 核心缺省（10 / 50 / 32768）。
type budgetSpec struct {
	MaxIterations int `yaml:"max_iterations"`
	MaxToolCalls  int `yaml:"max_tool_calls"`
	ContextTokens int `yaml:"context_tokens"`
}

// scriptReply 是假模型的一轮回答：正文、工具调用、用量。
// 三条都可省——只回正文即"最终回答"，只回工具调用即"这一轮还没完"。
type scriptReply struct {
	Content   string         `yaml:"content"`
	ToolCalls []toolCallSpec `yaml:"tool_calls"`
	Usage     usageSpec      `yaml:"usage"`
}

// toolCallSpec 是假模型给出的一次工具调用：id 是审计关联键（断言按它定位审计行）。
type toolCallSpec struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	Arguments string `yaml:"arguments"`
}

// usageSpec 是假模型回报的 token 计量；三项全零 = 供应商未回用量（计量不完整的自然来源）。
type usageSpec struct {
	Prompt     int `yaml:"prompt"`
	Completion int `yaml:"completion"`
	Total      int `yaml:"total"`
}

// expectations 是可执行断言集：每条断言只读**已观测到的事实**，不做推断。
// 指针字段区分"没写"与"写了零值"——写 `llm_calls: 0` 是要判零次调用，不写则不判。
// Pointer fields keep "absent" and "zero" apart.
type expectations struct {
	// Status 是期望的终止状态（五值之一；`running` 不是终止态，装载期拒绝）。
	Status string `yaml:"status"`

	// ToolSequence 是期望的工具名序列（逐位相等；写空列表 = 期望一次工具都没调）。
	ToolSequence *[]string `yaml:"tool_sequence"`

	// TextContains 是最终回答里必须出现的片段（逐条判）。
	TextContains []string `yaml:"text_contains"`

	// ToolResultsContain 是**工具结果**里必须出现的片段（四段式拒绝文案的判据落在这里）。
	ToolResultsContain []string `yaml:"tool_results_contain"`

	// Audit 是对工具审计行的断言（按 tool_call_id 定位；行必须存在）。
	Audit []auditSpec `yaml:"audit"`

	// LLMCalls 是期望的模型调用次数（审计行数，也是"调用次数"读数的判据）。
	LLMCalls *int `yaml:"llm_calls"`

	// MinTotalTokens 是 token 总用量的下界（真模型档用它判"确实计量到了"）。
	MinTotalTokens *int `yaml:"min_total_tokens"`

	// UsageComplete 是期望的计量完整性（false 用于断言"这一次读数确实不完整"）。
	UsageComplete *bool `yaml:"usage_complete"`
}

// auditSpec 是对一条工具审计行的断言：`status` 与 `error_kind` 留空即不判该项。
type auditSpec struct {
	ToolCallID string `yaml:"tool_call_id"`
	Status     string `yaml:"status"`
	ErrorKind  string `yaml:"error_kind"`
}

// ---------------------------------------------------------------------------
// 读数与结果模型
// ---------------------------------------------------------------------------

// observation 是一条用例**观测到的全部事实**（判定只读它，不再回库——一次运行一次读数，
// 判定与落盘看到的是同一份快照）。
type observation struct {
	result      core.RunResult
	toolSeq     []string
	toolResults []string
	llmCalls    []core.LLMCallView
	audit       []auditReading
	usage       usageReading
	complete    bool
	usageNote   string
}

// auditReading 是一条工具审计行的读数（未命中 = Found false，仍是一条读数）。
type auditReading struct {
	ToolCallID string `json:"tool_call_id"`
	Found      bool   `json:"found"`
	Status     string `json:"status,omitempty"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

// usageReading 是 token 用量的读数：三列平铺（与审计表列同构），**没有**金额字段。
type usageReading struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// checkResult 是一条断言的判定结果（Detail 只在失败时给——成功也写字会让结果文件变噪声）。
type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// caseResult 是一条用例的完整结果条目：读数在前、判定在后、跳过与错误各有一栏。
type caseResult struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"`
	Description string       `json:"description,omitempty"`
	Backend     string       `json:"backend"`
	SessionID   string       `json:"session_id"`
	RunID       string       `json:"run_id,omitempty"`
	Status      string       `json:"status,omitempty"`
	Iterations  int          `json:"iterations"`
	ToolCalls   int          `json:"tool_calls"`
	ToolSeq     []string     `json:"tool_sequence"`
	LLMCalls    int          `json:"llm_calls"`
	Usage       usageReading `json:"usage"`
	// UsageComplete 是「计量完整性」标记；UsageNote 说明为什么判不完整。
	UsageComplete bool           `json:"usage_complete"`
	UsageNote     string         `json:"usage_note,omitempty"`
	Audit         []auditReading `json:"audit,omitempty"`
	Checks        []checkResult  `json:"checks"`
	Passed        bool           `json:"passed"`
	Skipped       bool           `json:"skipped"`
	SkipReason    string         `json:"skip_reason,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// report 是结果文件的顶层形态：口径三项（估算器版本 / 余量 / 门控）在前，读数在后。
// 顶层**没有**任何金额字段——资源用量只以 token 与次数表达（FR-026）。
type report struct {
	GeneratedAt  string `json:"generated_at"`
	CasesDir     string `json:"cases_dir"`
	ProviderGate string `json:"provider_gate"`

	// EstimateVersion / ReserveTokens 是上下文估算器的版本与准入余量（§6.2 固定口径 9）：
	// 读数要能回答"这次用量是在哪一版估算口径下观测的"。
	EstimateVersion string `json:"estimate_version"`
	ReserveTokens   int    `json:"reserve_tokens"`

	Totals totals         `json:"totals"`
	Kinds  map[string]int `json:"kinds"`
	Cases  []caseResult   `json:"cases"`
}

// totals 是三个计数的汇总；skipped 与 passed 分开计——跳过**不计入通过**。
type totals struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

// mainRun 解析参数并跑一轮评测，返回进程退出码（便于以真实参数做端到端复核）。
// mainRun parses the arguments, runs one evaluation round and returns the exit code.
func mainRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	casesDir := fs.String("cases", defaultCasesDir, "用例目录（其中的 *.yaml 逐例执行）")
	provider := fs.String("provider", "", "真模型门控：仅执行声明了同名 provider 的用例（凭证走环境变量）")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "用法：go run ./evals [--cases <目录>] [--provider <名称>]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "参数错误：不认识的位置参数 %q\n", fs.Args())
		fs.Usage()
		return exitHarness
	}

	// 时间在这里取一次并显式向下传：用例名、结果文件名与报告时刻同源，跨零点也自洽。
	if code := runEvals(*casesDir, *provider, time.Now(), stdout, stderr); code != exitOK {
		return code
	}
	return exitOK
}

// runEvals 跑一轮评测：装载 → 逐例执行（跳过 / 运行 / 判定）→ 落结果文件 → 打印汇总。
// 返回进程退出码。
// runEvals loads the cases, executes them one by one, writes the result file and prints
// the summary.
func runEvals(casesDir, gate string, startedAt time.Time, stdout, stderr io.Writer) int {
	cases, err := loadCases(casesDir)
	if err != nil {
		fmt.Fprintf(stderr, "用例装载失败：%v\n", err)
		return exitHarness
	}

	// 探针凭证的哨兵在开跑前一次性设好：fake 档的配置装载要求该变量存在（值不是凭证，
	// 也不落盘）。整批只有真模型用例时**不**设——那一档的凭证由运维经环境变量提供。
	if slices.ContainsFunc(cases, func(ec evalCase) bool { return ec.Provider == "" }) {
		if err := ensureProbeKey(); err != nil {
			fmt.Fprintf(stderr, "环境准备失败：%v\n", err)
			return exitHarness
		}
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rpt := &report{
		GeneratedAt:     startedAt.Format(time.RFC3339),
		CasesDir:        casesDir,
		ProviderGate:    gate,
		EstimateVersion: core.EstimateVersion,
		ReserveTokens:   core.ReserveTokens,
		Kinds:           map[string]int{},
		Cases:           make([]caseResult, 0, len(cases)),
	}

	for _, ec := range cases {
		cr := executeCase(context.Background(), ec, gate, logger)
		rpt.Cases = append(rpt.Cases, cr)
		rpt.Kinds[cr.Kind]++
	}

	for _, cr := range rpt.Cases {
		rpt.Totals.Total++
		switch {
		case cr.Skipped:
			rpt.Totals.Skipped++
		case cr.Passed:
			rpt.Totals.Passed++
		default:
			rpt.Totals.Failed++
		}
	}

	writeCaseLines(stdout, rpt)
	path, err := writeReport(resultsDirName, rpt, startedAt)
	if err != nil {
		fmt.Fprintf(stderr, "结果文件写入失败：%v\n", err)
		return exitHarness
	}
	fmt.Fprintf(stdout, "\n用例 %d：通过 %d / 失败 %d / 跳过 %d（跳过不计入通过）\n",
		rpt.Totals.Total, rpt.Totals.Passed, rpt.Totals.Failed, rpt.Totals.Skipped)
	if incomplete := countIncomplete(rpt.Cases); incomplete > 0 {
		fmt.Fprintf(stdout, "计量不完整 %d 例：%s\n",
			incomplete, strings.Join(incompleteIDs(rpt.Cases), "、"))
	}
	fmt.Fprintf(stdout, "四类读数：%s\n", kindsLine(rpt.Kinds))
	fmt.Fprintf(stdout, "结果文件：%s（token 用量与调用次数，不换算金额）\n", filepath.ToSlash(path))

	if rpt.Totals.Failed > 0 {
		return exitCaseFail
	}
	return exitOK
}

// writeCaseLines 逐例打一行收场（PASS / FAIL / SKIP 三种形态各一行）。
// writeCaseLines prints one line per case.
func writeCaseLines(stdout io.Writer, rpt *report) {
	for _, cr := range rpt.Cases {
		switch {
		case cr.Skipped:
			fmt.Fprintf(stdout, "[SKIP] %s（%s）%s\n", cr.ID, cr.Kind, cr.SkipReason)
		case cr.Passed:
			fmt.Fprintf(stdout, "[PASS] %s（%s）run=%s 状态=%s 工具序列=%s 模型调用=%d token=%d/%d/%d 计量=%s\n",
				cr.ID, cr.Kind, cr.RunID, cr.Status, toolSeqText(cr.ToolSeq), cr.LLMCalls,
				cr.Usage.Prompt, cr.Usage.Completion, cr.Usage.Total, completenessText(cr.UsageComplete))
		default:
			fmt.Fprintf(stdout, "[FAIL] %s（%s）run=%s 状态=%s\n", cr.ID, cr.Kind, cr.RunID, cr.Status)
			if cr.Error != "" {
				fmt.Fprintf(stdout, "      运行错误：%s\n", cr.Error)
			}
			for _, c := range cr.Checks {
				if !c.OK {
					fmt.Fprintf(stdout, "      断言未过 %s：%s\n", c.Name, c.Detail)
				}
			}
		}
	}
}

// toolSeqText 渲染工具序列（空序列写成 `[]`——"一次都没调"是要能一眼看出的读数）。
func toolSeqText(seq []string) string { return "[" + strings.Join(seq, " ") + "]" }

// completenessText 渲染计量完整性标记。
func completenessText(complete bool) string {
	if complete {
		return "完整"
	}
	return "不完整"
}

// countIncomplete 数计量不完整的用例（跳过的不算——它们根本没有读数）。
func countIncomplete(cases []caseResult) int {
	n := 0
	for _, cr := range cases {
		if !cr.Skipped && !cr.UsageComplete {
			n++
		}
	}
	return n
}

// incompleteIDs 列出计量不完整的用例 id（升序，同装载序）。
func incompleteIDs(cases []caseResult) []string {
	var ids []string
	for _, cr := range cases {
		if !cr.Skipped && !cr.UsageComplete {
			ids = append(ids, cr.ID)
		}
	}
	return ids
}

// kindsLine 按四类固定次序渲染计数行（**不**遍历映射表——顺序必须是确定的）。
func kindsLine(kinds map[string]int) string {
	parts := make([]string, 0, len(evalKinds))
	for _, k := range evalKinds {
		parts = append(parts, fmt.Sprintf("%s %d", k, kinds[k]))
	}
	return strings.Join(parts, " / ")
}

// ---------------------------------------------------------------------------
// 装载与校验
// ---------------------------------------------------------------------------

// loadCases 读目录下的全部 *.yaml 并按文件名排序装载：排序是**显式**的，用例的执行序
// 因此可复现（Go 的目录读取序不是契约）。任何一条用例不合法即整批拒绝——半套用例跑出来的
// 读数比没有读数更危险。
// loadCases loads every *.yaml in the directory in explicit filename order. One invalid
// case rejects the whole batch: a partial round of readings is worse than none.
func loadCases(dir string) ([]evalCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取用例目录 %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("用例目录 %s 下没有 *.yaml 用例", dir)
	}

	handle, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("打开用例目录根 %s: %w", dir, err)
	}
	cases, loadErr := loadCasesWithin(handle, dir, names)
	closeErr := handle.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		return nil, err
	}
	return cases, nil
}

// loadCasesWithin 逐条读装用例：每个文件按**相对用例目录根的裸名**读取，由句柄自己拒绝
// 逃逸——不依赖"用例文件恰好来自仓库"这条假设，目录树在列目录与读取之间被换掉也兜得住。
// Every case is read by its bare name through the directory handle, so nothing leans on
// the assumption that case files happen to come from the repository.
func loadCasesWithin(handle *os.Root, dir string, names []string) ([]evalCase, error) {
	seen := make(map[string]string, len(names))
	cases := make([]evalCase, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := handle.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("读取用例 %s: %w", path, err)
		}
		ec, err := parseCase(path, data)
		if err != nil {
			return nil, err
		}
		if first, dup := seen[ec.ID]; dup {
			return nil, fmt.Errorf("用例 id 重复：%s 与 %s 都用了 %q", first, path, ec.ID)
		}
		seen[ec.ID] = path
		cases = append(cases, ec)
	}
	return cases, nil
}

// parseCase 解析并校验一条用例。
func parseCase(path string, data []byte) (evalCase, error) {
	var ec evalCase
	// 未知键一律拒绝：用例里写错的键名（如 max_iteration）如果只是被忽略，用例就会
	// "看起来配了断言"而实际没判——装载期拒绝是唯一能保证断言真被读到的位置。
	dec := yaml.NewDecoder(strings.NewReader(string(data)), yaml.DisallowUnknownField())
	if err := dec.Decode(&ec); err != nil {
		return ec, fmt.Errorf("用例 %s 解析失败：%s", path, yamlErrorSummary(err))
	}
	if err := validateCase(ec); err != nil {
		return ec, fmt.Errorf("用例 %s 不合法：%w", path, err)
	}
	return ec, nil
}

// yamlErrorSummary 把 YAML 库错误压成类别（不回显原文——用例正文里可能有长文本，
// 库的默认错误会把它当源码摘录带出来）。
// yamlErrorSummary keeps library messages (which quote the source) out of the output.
func yamlErrorSummary(err error) string {
	var syntaxErr *yaml.SyntaxError
	switch {
	case errors.As(err, &syntaxErr):
		return "YAML 语法错误"
	default:
		return "解析失败（类别：" + fmt.Sprintf("%T", err) + "）"
	}
}

// validateCase 校验一条用例的必备项与形态；**装载期**拒绝胜过运行时炸在半路。
// validateCase rejects a case at load time rather than halfway through a run.
func validateCase(ec evalCase) error {
	if !caseIDPattern.MatchString(ec.ID) {
		return fmt.Errorf("id %q 不合法——只允许小写字母、数字与连字符（它会进入会话标识与文件路径）", ec.ID)
	}
	if !slices.Contains(evalKinds, ec.Kind) {
		return fmt.Errorf("kind %q 不在四类读数内（%s）", ec.Kind, strings.Join(evalKinds, " / "))
	}
	if strings.TrimSpace(ec.Message) == "" {
		return errors.New("message 必填——一次运行要有一条用户消息")
	}
	if !caseIDPattern.MatchString(ec.Fixture.Agent) {
		return fmt.Errorf("fixture.agent %q 不合法——只允许小写字母、数字与连字符", ec.Fixture.Agent)
	}
	if strings.TrimSpace(ec.Fixture.AgentMD) == "" {
		return errors.New("fixture.agent_md 必填——Agent 目录里必须有 AGENT.md")
	}
	if err := validateRelPaths("fixture.files", ec.Fixture.Files); err != nil {
		return err
	}
	if err := validateRelPaths("fixture.workspace_files", ec.Fixture.WorkspaceFiles); err != nil {
		return err
	}
	if ec.Budget.MaxIterations < 0 || ec.Budget.MaxToolCalls < 0 || ec.Budget.ContextTokens < 0 {
		return errors.New("budget 三项不得为负——预算只能收紧，不能取负值")
	}

	// 后端分档：fake 档必须有脚本（没有脚本的假模型无话可说），真模型档**不得**有脚本。
	if ec.Provider == "" {
		if len(ec.Script) == 0 {
			return errors.New("fake 档必须给 script（至少一条回答）——否则第一次模型调用就只能报「脚本已用尽」")
		}
	} else if len(ec.Script) > 0 {
		return fmt.Errorf("真模型档（provider: %s）不得写 script——真模型的回答不是脚本回放，"+
			"留着脚本会让读数看起来是脚本控制的", ec.Provider)
	}

	return validateExpectations(ec)
}

// validateExpectations 校验断言集：状态字面量、审计断言的定位键、序列里工具名的形态。
func validateExpectations(ec evalCase) error {
	switch ec.Expect.Status {
	case string(core.StatusCompleted), string(core.StatusLimitReached),
		string(core.StatusCancelled), string(core.StatusFailed):
	case string(core.StatusRunning):
		return errors.New("expect.status 不得写 running——它不是终止态，运行入口只会返回四种终止态之一")
	case "":
		return errors.New("expect.status 必填——一条用例至少要判终止状态")
	default:
		return fmt.Errorf("expect.status %q 不是运行状态五值之一", ec.Expect.Status)
	}

	scriptIDs := make(map[string]struct{})
	for _, reply := range ec.Script {
		for _, tc := range reply.ToolCalls {
			if tc.ID == "" {
				return errors.New("script 的工具调用必须给 id——审计断言按它定位审计行")
			}
			if tc.Name == "" {
				return fmt.Errorf("script 的工具调用 %s 缺 name", tc.ID)
			}
			scriptIDs[tc.ID] = struct{}{}
		}
	}
	for _, spec := range ec.Expect.Audit {
		if spec.ToolCallID == "" {
			return errors.New("expect.audit 必须给 tool_call_id")
		}
		if _, ok := scriptIDs[spec.ToolCallID]; !ok {
			return fmt.Errorf("expect.audit 引用了脚本里不存在的 tool_call_id %q——"+
				"断言一条不会发生的调用等于让它空过", spec.ToolCallID)
		}
		if spec.Status != "" && !slices.Contains([]string{
			string(core.LLMCallPending), string(core.LLMCallSuccess),
			string(core.LLMCallFailed), string(core.LLMCallUnknown),
		}, spec.Status) {
			return fmt.Errorf("expect.audit.status %q 不是审计状态四值之一", spec.Status)
		}
		if spec.ErrorKind != "" && !slices.Contains([]string{
			string(core.ErrorKindSandboxDenied), string(core.ErrorKindTimeout),
			string(core.ErrorKindNotFound), string(core.ErrorKindInvalidInput),
			string(core.ErrorKindExternalError), string(core.ErrorKindRuntimeError),
			string(core.ErrorKindUnknown),
		}, spec.ErrorKind) {
			return fmt.Errorf("expect.audit.error_kind %q 不是失败分类七值之一", spec.ErrorKind)
		}
	}
	if ec.Expect.LLMCalls != nil && *ec.Expect.LLMCalls < 0 {
		return errors.New("expect.llm_calls 不得为负")
	}
	if ec.Expect.MinTotalTokens != nil && *ec.Expect.MinTotalTokens < 0 {
		return errors.New("expect.min_total_tokens 不得为负")
	}
	return nil
}

// validateRelPaths 校验夹具路径：一律是**干净的相对路径**——绝对路径与 `..` 逃逸会把夹具
// 写到临时工作区之外（夹具越界等于运行器自己先破坏了"探针建在仓库外"的纪律）。
// Fixture paths must stay relative and inside the temp workspace.
func validateRelPaths(field string, files map[string]string) error {
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if rel == "" {
			return fmt.Errorf("%s 含空路径", field)
		}
		if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
			return fmt.Errorf("%s 的路径 %q 是绝对路径——夹具路径一律相对工作区", field, rel)
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("%s 的路径 %q 逃出工作区（含 ..）", field, rel)
		}
		if clean != filepath.ToSlash(rel) {
			return fmt.Errorf("%s 的路径 %q 不是规整形态（规整后为 %q）", field, rel, clean)
		}
	}
	return nil
}

// coreBudget 把用例预算映成运行请求的预算（零值取核心缺省——mergeBudget 负责填）。
func (b budgetSpec) coreBudget() core.Budget {
	return core.Budget{
		MaxIterations: b.MaxIterations,
		MaxToolCalls:  b.MaxToolCalls,
		ContextTokens: b.ContextTokens,
	}
}

// sessionID 由用例 id 派生会话标识：前缀 `eval-` 与命令面的派生形态（渠道前缀 + 哈希）
// 明确区分开——评测的会话不该伪装成某个用户经 chat 建的会话。
// **不**复用命令面的派生函数：那是"渠道 + 用户 + 画像"三元组的派生，评测没有用户。
// The eval session id is derived from the case id and deliberately not the command
// surface's tuple-derived form: an eval session must not masquerade as a user's.
func sessionID(caseID string) string { return "eval-" + caseID }

// ---------------------------------------------------------------------------
// 真模型门控（缺一不发）
// ---------------------------------------------------------------------------

// gateSkip 判定该用例本次是否跳过；返回非空字符串即跳过原因。
// **缺一不发**：门控开关、目标供应商、配置文件、凭证环境变量——四样缺任何一样都不跑，
// 且跳过**不计入通过**（把"没跑"读成"过了"是评测最坏的失效形态）。
// gateSkip reports why a case is skipped. A skipped case never counts as a pass.
func gateSkip(ec evalCase, gate string) string {
	if ec.Provider == "" {
		return "" // fake 档恒执行（进程内假模型，无外部依赖）
	}
	if gate == "" {
		return fmt.Sprintf("未门控：用例声明 provider=%s，本次未给 --provider（真模型档缺一不发）", ec.Provider)
	}
	if gate != ec.Provider {
		return fmt.Sprintf("未门控：用例声明 provider=%s，本次门控的是 %s", ec.Provider, gate)
	}
	raw, err := os.ReadFile(configFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("门控不成立：工作目录没有 %s（真模型的端点与凭证占位从这里来）", configFileName)
		}
		return fmt.Sprintf("门控不成立：读取 %s 失败：%v", configFileName, err)
	}
	if !configHasProvider(raw, gate) {
		return fmt.Sprintf("门控不成立：%s 里没有名为 %s 的供应商条目", configFileName, gate)
	}
	return ""
}

// configHasProvider 在配置文件里找同名供应商条目（原始文本的键值级查找——这一步只判
// "条目存在吗"，凭证解析仍归 LoadConfig，明文凭证在这里不接触）。
// configHasProvider only answers "does an entry with this name exist".
func configHasProvider(raw []byte, name string) bool {
	var shape struct {
		Providers []struct {
			Name string `yaml:"name"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &shape); err != nil {
		return false
	}
	for _, p := range shape.Providers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 单例执行
// ---------------------------------------------------------------------------

// executeCase 跑一条用例并判定（跳过 / 运行 / 装配失败三档收场）。
// 每个用例一份**独立的临时工作区 + 独立的运行期**：跨用例不共享库、不共享会话、不共享假模型
// 的回放游标——一个用例再怎么坏也污染不到下一个。
// Each case gets its own temp workspace and runtime, so no case can contaminate another.
func executeCase(ctx context.Context, ec evalCase, gate string, logger *slog.Logger) caseResult {
	cr := caseResult{ID: ec.ID, Kind: ec.Kind, Description: ec.Description}

	if reason := gateSkip(ec, gate); reason != "" {
		cr.Skipped = true
		cr.SkipReason = reason
		cr.Backend = "provider:" + ec.Provider
		return cr
	}
	if ec.Provider == "" {
		cr.Backend = "fake"
	} else {
		cr.Backend = "provider:" + ec.Provider
	}
	cr.SessionID = sessionID(ec.ID)

	workDir, err := os.MkdirTemp("", "basalt-eval-"+ec.ID+"-*")
	if err != nil {
		cr.Error = fmt.Sprintf("建临时工作区失败：%v", err)
		return cr
	}
	defer func() {
		// 探针工作区用完即删：临时物不进仓库、不攒到最后。
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			logger.Warn("评测临时工作区清理失败", "case", ec.ID, "work_dir", workDir, "detail", rmErr)
		}
	}()

	if err := buildWorkspace(workDir, ec); err != nil {
		cr.Error = err.Error()
		return cr
	}

	opts := app.Options{
		WorkDir:                workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 logger,
	}
	if ec.Provider == "" {
		opts.ProviderClient = newScriptedClient(ec.Script)
	}
	rt, err := app.OpenRuntime(ctx, opts)
	if err != nil {
		if ec.Provider == "" {
			// fake 档没有任何"缺条件"的借口：装配失败就是运行器或夹具的问题，如实登记。
			cr.Error = fmt.Sprintf("装配失败：%v", err)
			return cr
		}
		// 真模型档：装配失败最常见的原因就是凭证缺失（LoadConfig fail-closed 拒绝）——
		// 这正是「凭证环境变量缺一不发」的那一不发，按跳过处理，不计入通过。
		cr.Skipped = true
		cr.SkipReason = fmt.Sprintf("门控不成立：装配失败（凭证 / 配置未就位）：%v", err)
		return cr
	}
	defer func() {
		if closeErr := rt.Close(); closeErr != nil {
			logger.Warn("评测运行期释放失败", "case", ec.ID, "detail", closeErr)
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()

	req := core.RunRequest{
		Agent:     ec.Fixture.Agent,
		SessionID: cr.SessionID,
		Message:   ec.Message,
		Channel:   evalChannel,
		UserID:    evalUserID,
		Budget:    ec.Budget.coreBudget(),
	}
	res, runErr := rt.RunService().Run(runCtx, req)
	if runErr != nil {
		// 函数级 error 只表达调用方问题（Agent 不存在 / 同会话忙 / 请求非法）——
		// 这些都是夹具或运行器的错，如实判 FAIL 并留下原文。
		cr.Error = fmt.Sprintf("运行入口拒绝执行：%v", runErr)
		return cr
	}
	cr.RunID = res.RunID
	cr.Status = string(res.Status)
	cr.Iterations = res.Iterations
	cr.ToolCalls = res.ToolCalls

	readers := caseReaders{
		sessions:    rt.Sessions(),
		invocations: rt.Invocations(),
		calls:       rt.LLMCalls(),
	}
	obs, err := observe(runCtx, readers, ec, res)
	if err != nil {
		cr.Error = fmt.Sprintf("读数失败：%v", err)
		return cr
	}

	cr.ToolSeq = obs.toolSeq
	cr.LLMCalls = len(obs.llmCalls)
	cr.Usage = obs.usage
	cr.UsageComplete = obs.complete
	cr.UsageNote = obs.usageNote
	cr.Audit = obs.audit
	cr.Checks = judge(ec, obs)
	cr.Passed = allChecksPassed(cr.Checks)
	return cr
}

// caseReaders 是读数需要的三个只读面（接口形态取自消费方声明——包外拿到的是能力）。
// caseReaders groups the three read-only surfaces the observation needs.
type caseReaders struct {
	sessions    cli.SessionLister
	invocations core.ToolInvocationReader
	calls       core.LLMCallReader
}

// observe 把一次运行的读数取全：会话消息（工具序列与工具结果）、模型调用审计行
// （用量与计量完整性）、工具审计行（按脚本给出的 tool_call_id 逐条定位）。
// observe collects every reading of one run from the production read surfaces.
func observe(ctx context.Context, rd caseReaders, ec evalCase, res core.RunResult) (observation, error) {
	obs := observation{result: res}

	msgs, err := rd.sessions.Messages(ctx, sessionID(ec.ID))
	if err != nil {
		return obs, fmt.Errorf("读取会话消息: %w", err)
	}
	obs.toolSeq = toolSequence(msgs)
	obs.toolResults = toolResultTexts(msgs)

	views, err := rd.calls.CallsByRun(ctx, res.RunID)
	if err != nil {
		return obs, fmt.Errorf("读取模型调用审计行: %w", err)
	}
	obs.llmCalls = views
	obs.usage, obs.complete, obs.usageNote = assessUsage(views)

	obs.audit, err = collectAudit(ctx, rd.invocations, res.RunID, scriptToolCallIDs(ec))
	if err != nil {
		return obs, err
	}
	return obs, nil
}

// scriptToolCallIDs 按脚本顺序列出工具调用 id（审计读数的定位键）。
func scriptToolCallIDs(ec evalCase) []string {
	var ids []string
	for _, reply := range ec.Script {
		for _, tc := range reply.ToolCalls {
			ids = append(ids, tc.ID)
		}
	}
	return ids
}

// collectAudit 逐 id 取工具审计行；未命中记一条 Found=false 的读数（**不是**错误——
// "这次调用没有审计行"正是运行时归属校验要判的事实）。
func collectAudit(ctx context.Context, reader core.ToolInvocationReader, runID string, ids []string) ([]auditReading, error) {
	readings := make([]auditReading, 0, len(ids))
	for _, id := range ids {
		view, ok, err := reader.Lookup(ctx, runID, id)
		if err != nil {
			return nil, fmt.Errorf("读取工具审计行 %s: %w", id, err)
		}
		reading := auditReading{ToolCallID: id, Found: ok}
		if ok {
			reading.Status = string(view.Status)
			reading.ErrorKind = string(view.ErrorKind)
		}
		readings = append(readings, reading)
	}
	return readings, nil
}

// toolSequence 按消息序取工具名序列：序列来自**助手消息里的工具调用**（顺序即执行顺序——
// 工具严格串行，循环逐条执行并按同一顺序回填结果），不依赖任何第二处记账。
// The sequence comes from the assistant messages' tool calls, the same order the loop
// executed them in; there is no second ledger to disagree with.
func toolSequence(msgs []core.Message) []string {
	var seq []string
	for _, m := range msgs {
		if m.Role != core.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			seq = append(seq, tc.Name)
		}
	}
	return seq
}

// toolResultTexts 取工具结果文本（role=tool 的消息正文，按消息序）。
func toolResultTexts(msgs []core.Message) []string {
	var texts []string
	for _, m := range msgs {
		if m.Role == core.RoleTool {
			texts = append(texts, m.Content)
		}
	}
	return texts
}

// assessUsage 汇总量并判「计量完整性」：三列求和（所有行）在前，完整性判定在后。
//
// 判不完整的三类（FR-026 逐字）：
//   - **无行**：一次运行连一条审计行都没有（读数无从谈起）；
//   - **悬空 / 未知**：任何一行仍是 `pending`（预写后没等到终态回填）或 `unknown`（恢复扫描归一）；
//   - **成功行三列全零**：调用成功但供应商未回用量——**不得**把库中默认的零读成零成本。
//
// 失败行三列为零**不**判不完整：调用没成功就没有用量，那是事实而不是缺失。
// A failed row with zero tokens is a fact, not a missing reading.
func assessUsage(views []core.LLMCallView) (usageReading, bool, string) {
	var usage usageReading
	var dangling []int
	var zeroSuccess []int
	for _, v := range views {
		usage.Prompt += v.PromptTokens
		usage.Completion += v.CompletionTokens
		usage.Total += v.TotalTokens
		switch v.Status {
		case core.LLMCallPending, core.LLMCallUnknown:
			dangling = append(dangling, v.Round)
		case core.LLMCallSuccess:
			if v.PromptTokens == 0 && v.CompletionTokens == 0 && v.TotalTokens == 0 {
				zeroSuccess = append(zeroSuccess, v.Round)
			}
		}
	}

	switch {
	case len(views) == 0:
		return usage, false, "计量不完整：本次运行没有任何模型调用审计行"
	case len(dangling) > 0:
		return usage, false, fmt.Sprintf("计量不完整：第 %s 轮审计行仍未落终态（pending / unknown），用量无从确定",
			roundsText(dangling))
	case len(zeroSuccess) > 0:
		return usage, false, fmt.Sprintf("计量不完整：第 %s 轮调用成功但三列 token 均为零（供应商未回用量）",
			roundsText(zeroSuccess))
	default:
		return usage, true, ""
	}
}

// roundsText 把轮号列成 `1、3` 形态。
func roundsText(rounds []int) string {
	parts := make([]string, 0, len(rounds))
	for _, r := range rounds {
		parts = append(parts, fmt.Sprint(r))
	}
	return strings.Join(parts, "、")
}

// ---------------------------------------------------------------------------
// 判定
// ---------------------------------------------------------------------------

// judge 逐条判断言：只读观测到的事实，缺什么就点名什么（失败的 Detail 一律给"期望 vs 实际"）。
// judge evaluates every expectation against the observation, naming both sides on failure.
func judge(ec evalCase, obs observation) []checkResult {
	checks := []checkResult{checkStatus(ec.Expect.Status, obs.result)}

	if ec.Expect.ToolSequence != nil {
		checks = append(checks, checkToolSequence(*ec.Expect.ToolSequence, obs.toolSeq))
	}
	for _, want := range ec.Expect.TextContains {
		checks = append(checks, checkContains("text_contains", want, []string{obs.result.FinalText}))
	}
	for _, want := range ec.Expect.ToolResultsContain {
		checks = append(checks, checkContains("tool_results_contain", want, obs.toolResults))
	}
	for _, spec := range ec.Expect.Audit {
		checks = append(checks, checkAudit(spec, obs.audit))
	}
	if ec.Expect.LLMCalls != nil {
		checks = append(checks, checkInt("llm_calls", *ec.Expect.LLMCalls, len(obs.llmCalls)))
	}
	if ec.Expect.MinTotalTokens != nil {
		got := obs.usage.Total
		ok := got >= *ec.Expect.MinTotalTokens
		checks = append(checks, checkResult{
			Name: "min_total_tokens",
			OK:   ok,
			Detail: detailUnless(ok, fmt.Sprintf("期望总用量 ≥ %d，实际 %d（本项判 token 用量，不换算金额）",
				*ec.Expect.MinTotalTokens, got)),
		})
	}
	if ec.Expect.UsageComplete != nil {
		ok := obs.complete == *ec.Expect.UsageComplete
		note := obs.usageNote
		if ok && !obs.complete {
			note = obs.usageNote // 期望不完整且确实不完整：把原因留在读数里，不算失败
		}
		checks = append(checks, checkResult{
			Name:   "usage_complete",
			OK:     ok,
			Detail: detailUnless(ok, fmt.Sprintf("期望计量完整性 %t，实际 %t（%s）", *ec.Expect.UsageComplete, obs.complete, note)),
		})
	}
	return checks
}

// allChecksPassed 判定一条用例是否全过；**零条断言不算通过**（没有断言的用例只会产生
// "看起来跑过了"的假读数，装载期已拦住无 status 的用例，这里再兜一层）。
// An empty assertion set never counts as a pass.
func allChecksPassed(checks []checkResult) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// checkStatus 判终止状态。
func checkStatus(want string, res core.RunResult) checkResult {
	ok := want == string(res.Status)
	detail := detailUnless(ok, fmt.Sprintf("期望状态 %s，实际 %s", want, res.Status))
	if !ok && res.Reason != "" {
		detail += "；运行给出的原因：" + preview(res.Reason)
	}
	return checkResult{Name: "status", OK: ok, Detail: detail}
}

// checkToolSequence 判工具名序列：逐位相等（长度不同即不等——"少调了一次"与"顺序不同"
// 都是要能看出来的读数）。
func checkToolSequence(want, got []string) checkResult {
	ok := slices.Equal(want, got)
	return checkResult{
		Name: "tool_sequence",
		OK:   ok,
		Detail: detailUnless(ok, fmt.Sprintf("期望序列 %s，实际序列 %s（按执行顺序逐位比较）",
			toolSeqText(want), toolSeqText(got))),
	}
}

// checkContains 判某片段是否出现在给定文本集合的连接体里；多条集合共用一条断言名，
// 片段本身进 Detail（否则逐条断言的显示名会无法区分）。
func checkContains(name, want string, haystack []string) checkResult {
	joined := strings.Join(haystack, "\n")
	ok := strings.Contains(joined, want)
	return checkResult{
		Name: name,
		OK:   ok,
		Detail: detailUnless(ok, fmt.Sprintf("期望命中 %q，实际未命中（观测到 %d 段文本，合计 %d 字符）",
			preview(want), len(haystack), len([]rune(joined)))),
	}
}

// checkAudit 判一条工具审计行：行**必须存在**（未命中即失败），随后判状态与失败分类。
func checkAudit(spec auditSpec, readings []auditReading) checkResult {
	name := "audit:" + spec.ToolCallID
	var found *auditReading
	for i := range readings {
		if readings[i].ToolCallID == spec.ToolCallID {
			found = &readings[i]
			break
		}
	}
	if found == nil {
		// 脚本里的 id 装不进读数列表 = 运行器自己的缺陷（装载期已保证 id 存在）。
		return checkResult{Name: name, OK: false, Detail: "运行器未为该 tool_call_id 取读数"}
	}
	if !found.Found {
		return checkResult{Name: name, OK: false, Detail: "该工具调用没有审计行（预写未发生）"}
	}
	if spec.Status != "" && found.Status != spec.Status {
		return checkResult{
			Name: name, OK: false,
			Detail: fmt.Sprintf("期望审计状态 %s，实际 %s", spec.Status, found.Status),
		}
	}
	if spec.ErrorKind != "" && found.ErrorKind != spec.ErrorKind {
		return checkResult{
			Name: name, OK: false,
			Detail: fmt.Sprintf("期望失败分类 %s，实际 %s（状态 %s）", spec.ErrorKind, found.ErrorKind, found.Status),
		}
	}
	return checkResult{Name: name, OK: true}
}

// checkInt 判一个整数读数。
func checkInt(name string, want, got int) checkResult {
	ok := want == got
	return checkResult{
		Name: name, OK: ok,
		Detail: detailUnless(ok, fmt.Sprintf("期望 %d，实际 %d", want, got)),
	}
}

// detailUnless 在判定通过时返回空 Detail（成功也写字会让结果文件淹没在噪声里）。
func detailUnless(ok bool, detail string) string {
	if ok {
		return ""
	}
	return detail
}

// preview 截断一段文本供 Detail 使用：**不回显全文**（观测到的正文可能是整份文件内容，
// 结果文件不该成为第二个文件副本）。截断语不用核心的截断标记字面量——那个标记的含义是
// "工具输出触顶"，与这里的"显示层截断"是两回事。
// preview caps a fragment for the report; the core truncation marker means something else
// and must not be reused here.
func preview(s string) string {
	const limit = 200
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + fmt.Sprintf("…（后面还有 %d 字符，未回显）", len(runes)-limit)
}

// ---------------------------------------------------------------------------
// 夹具装配
// ---------------------------------------------------------------------------

// buildWorkspace 在临时目录里建出一份可跑的工作区：骨架 → Agent 目录与文件 → 工作区文件
// → 配置（fake 档写探针配置，真模型档原样复制仓库配置）。落盘一律经**临时工作区根句柄**：
// 用例给的相对路径装载期已校验过（绝对路径 / `..` 直接拒绝），句柄是那之外的第二道、也是
// 真正生效的那道——校验与写入之间目录树可以被换掉，"先检查路径、随后普通 open"兜不住。
// buildWorkspace lays out the skeleton, the fixture files and the configuration; every
// write goes through a handle on the temporary workspace root.
func buildWorkspace(workDir string, ec evalCase) error {
	wsRoot := filepath.Join(workDir, workspaceDirName)
	// 骨架（六个目录 + 五个引导文件）由生产代码建——夹具不自己拼 .basalt/ 的结构，
	// 否则评测跑的就不是生产那条"先有骨架才有重路径"的顺序。
	if _, err := workspace.Init(wsRoot); err != nil {
		return fmt.Errorf("建工作区骨架：%w", err)
	}

	// agents/ 目录骨架**不**预建（与生产一致：谁用谁建），Agent 目录在这里落盘。
	// 建目录一律 0o750，与生产工作区目录同口径（internal/workspace.go 的 dirPerm）。
	agentRel := filepath.Join(workspaceDirName, agentsDirName, ec.Fixture.Agent)
	if err := os.MkdirAll(filepath.Join(workDir, agentRel), 0o750); err != nil {
		return fmt.Errorf("建 Agent 目录：%w", err)
	}

	handle, err := os.OpenRoot(workDir)
	if err != nil {
		return fmt.Errorf("打开夹具根 %s：%w", workDir, err)
	}
	buildErr := buildWorkspaceWithin(handle, agentRel, ec)
	closeErr := handle.Close()
	return errors.Join(buildErr, closeErr)
}

// buildWorkspaceWithin 在夹具根句柄内落盘全部夹具文件（路径一律**相对夹具根**：
// `.basalt/agents/<agent>/…` / `.basalt/…` / `basalt.yaml`）。
// Every fixture path is rooted at the temporary workspace and written through the handle.
func buildWorkspaceWithin(handle *os.Root, agentRel string, ec evalCase) error {
	if err := writeWithin(handle, filepath.Join(agentRel, agentFileName), ec.Fixture.AgentMD); err != nil {
		return err
	}
	if err := writeRelFiles(handle, agentRel, ec.Fixture.Files); err != nil {
		return err
	}
	if err := writeRelFiles(handle, workspaceDirName, ec.Fixture.WorkspaceFiles); err != nil {
		return err
	}
	return writeConfig(handle, ec)
}

// writeConfig 落配置：fake 档写探针条目（凭证只写 ${VAR} 占位）；真模型档把仓库根的
// basalt.yaml **原样复制**——真模型档读的就是运维写的那份配置，评测不替它改编。
// The fake path writes a probe entry; the gated path copies the operator's own file
// verbatim.
func writeConfig(handle *os.Root, ec evalCase) error {
	if ec.Provider != "" {
		raw, err := os.ReadFile(configFileName)
		if err != nil {
			return fmt.Errorf("读取 %s（真模型档原样复制）：%w", configFileName, err)
		}
		return writeWithin(handle, configFileName, string(raw))
	}
	cfg := fmt.Sprintf(`# 评测夹具配置（fake 档）：探针条目只为让路由查表与审计链走完整路径。
# 凭证写占位形态，值由进程内自设的哨兵提供——明文不落盘。
providers:
  - name: %s
    base_url: %s
    api_key: ${%s}
    default_model: %s
`, probeProviderName, probeBaseURL, probeAPIKeyEnv, probeDefaultModel)
	return writeWithin(handle, configFileName, cfg)
}

// writeRelFiles 按**排序后的键序**逐条落文件（顺序确定，失败时点到同一个文件）；
// 中间目录自动创建——夹具里的深层路径不该要求用例逐层声明目录。base 是这些相对路径
// 在夹具根里的落点（Agent 目录或工作区根），**不经**任何绝对路径拼接。
func writeRelFiles(handle *os.Root, base string, files map[string]string) error {
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		target := filepath.Join(base, filepath.FromSlash(rel))
		if dir := filepath.Dir(target); dir != "." {
			if err := handle.MkdirAll(dir, 0o750); err != nil {
				return fmt.Errorf("建目录 %s：%w", dir, err)
			}
		}
		if err := writeWithin(handle, target, files[rel]); err != nil {
			return err
		}
	}
	return nil
}

// writeWithin 经根句柄写一个文件（夹具根 / 结果目录根），权限 **0o600**：夹具与结果文件
// 都是评测运行产物，没有"要让别的用户读得到"的验收要求，不刻意复制生产的文件权限
// （目录仍是 0o750，与工作区同口径）。
// Runner artifacts get 0600 instead of the production file mode; nothing requires them to
// be readable by other users.
func writeWithin(handle *os.Root, rel, content string) error {
	if err := handle.WriteFile(rel, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写文件 %s：%w", rel, err)
	}
	return nil
}

// ensureProbeKey 把探针凭证的哨兵值设进进程环境（fake 档的配置装载要求该变量存在；
// 值不是凭证、也不落盘）。真模型档**不**设任何东西——那里的凭证由运维经环境变量提供。
// ensureProbeKey installs the in-process sentinel the fake path's config resolution needs.
func ensureProbeKey() error {
	if _, ok := os.LookupEnv(probeAPIKeyEnv); ok {
		return nil
	}
	if err := os.Setenv(probeAPIKeyEnv, probeAPIKeyValue); err != nil {
		return fmt.Errorf("设置探针凭证环境变量 %s：%w", probeAPIKeyEnv, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 假模型（默认后端）
// ---------------------------------------------------------------------------

// scriptedClient 是默认后端：按用例脚本**逐条**回答的进程内假模型。
//
// 它实现的是核心的 `ProviderClient`，装配处把它**塞进路由与两阶段审计之内**（替身路照建全表）——
// 评测因此跑的是与生产同一条审计链，用量读数不是另一套统计口径。
// 脚本用尽即**报错**（不静默重放最后一条）：用例写漏了回答就该红，而不是拿到一份
// "看起来跑通了"的读数。
// The scripted client errors on script exhaustion instead of silently replaying the last
// reply: a case that promises more rounds than it wrote must turn red.
type scriptedClient struct {
	mu      sync.Mutex
	replies []scriptReply
	used    int
}

// newScriptedClient 造一个回放器；脚本在构造时拷一份（用例定义不可被运行过程改写）。
func newScriptedClient(replies []scriptReply) *scriptedClient {
	return &scriptedClient{replies: slices.Clone(replies)}
}

// 编译期断言：假模型实现的是核心的协议调用接口。
var _ core.ProviderClient = (*scriptedClient)(nil)

// Chat 回放一条脚本回答；先查取消（与真实客户端一致——取消优先级高于一切）。
func (c *scriptedClient) Chat(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return core.ChatResponse{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used >= len(c.replies) {
		return core.ChatResponse{}, fmt.Errorf(
			"脚本已用尽：模型被叫了 %d 次，用例脚本只写了 %d 条回答", c.used+1, len(c.replies))
	}
	reply := c.replies[c.used]
	c.used++

	resp := core.ChatResponse{
		Content: reply.Content,
		Usage: core.Usage{
			PromptTokens:     reply.Usage.Prompt,
			CompletionTokens: reply.Usage.Completion,
			TotalTokens:      reply.Usage.Total,
		},
	}
	for _, tc := range reply.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	if resp.Usage.TotalTokens == 0 && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0) {
		// 用例只写提示 / 补全两项时合成总量——供应商的总量列缺省由调用方补齐是既有惯例。
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	return resp, nil
}

// callCount 返回已被调用的次数（测试与排障用；报告里的调用次数取自审计行读数）。
func (c *scriptedClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// ---------------------------------------------------------------------------
// 结果文件
// ---------------------------------------------------------------------------

// writeReport 把结果落成 evals/results/<时间戳>.json；同一秒内的第二次运行追加 `-2`、`-3`…
// ——连跑两次的读数要能同时留下（"连跑两次一致"这条复核依赖两份文件都在）。
// writeReport never overwrites an earlier result of the same second.
func writeReport(dir string, rpt *report, startedAt time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("建结果目录 %s: %w", dir, err)
	}
	path, err := newResultPath(dir, startedAt)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rpt, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化结果: %w", err)
	}
	data = append(data, '\n')
	handle, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("打开结果目录根 %s: %w", dir, err)
	}
	writeErr := writeWithin(handle, filepath.Base(path), string(data))
	closeErr := handle.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	return path, nil
}

// newResultPath 取一个尚未占用的结果文件路径（时间戳来自显式传入的时刻）。
func newResultPath(dir string, startedAt time.Time) (string, error) {
	stamp := startedAt.Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		name := stamp + ".json"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.json", stamp, i)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
	}
	return "", fmt.Errorf("结果目录 %s 下 %s 的时间戳文件已占满（同一秒内跑了一百次？）", dir, stamp)
}
