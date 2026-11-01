package compaction_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 **G1-25 的结构性判卷**（T093）：用确定性桩摘要器跑"近期历史 / 单份摘要 /
// 分层视图＋回查"三臂，产出机器读数，并证明"没有真实测量就不得出现收益数字"。
// This file is the G1-25 structural verdict: three arms over a frozen task set, a machine
// reading, and a guard that forbids benefit claims without a real measurement.
//
// 三条口径**必须先读**（越界即等于编造结果）：
//  1. **不是真实模型评测**——摘要器是确定性桩（`evalStubModel`），正文不含任何语义内容；
//     本文件所有读数只能用于"三组配置确实不同、报告结构合规"，**不得**被引用为收益证据；
//  2. **词面对齐，不做语义判定**——`must_keep` 的判据是"该要点原文是否逐字出现在拼装后的
//     上下文里"，它检验的是**保留与否**，不是"模型还能不能答对"；
//  3. **先冻结后测量**——任务集哈希与各臂聚合读数先冻结成常量，再比对；读数变了必须
//     显式改常量，**不得**让"跑出来多少就算多少"滑过去。
//
// 三臂取值逐字取自设计卡 §2.10.5 与 G1-25，**不得**改名：
//
//	A = 近期历史     现状：完整工具组整组丢弃，无摘要（基线）
//	B = 单份摘要     滚动摘要 + 最近完整组
//	C = 分层视图＋回查 结构化状态 + 滚动摘要 + 最近组 + 按需回查
//
// C 在本 US **尚未实现、尚未测量**（`SummaryInput.StateBlock` 恒为空串、按需回查未交付），
// 因此它只声明配置、不带任何读数——这正是"未真实测量时写未测量"的落点。
//
// 本文件产出的 `evalCompactionReport` 是**本用例的机器读数**，不是收口产物
// `docs/us/us16-eval-report.md` 的替代品。

const (
	// evalRoot 是压缩对照任务集的目录（相对包目录）。
	evalRoot = "testdata/compaction-eval"
	// evalDatasetFile 是任务集文件名。
	evalDatasetFile = "cases.json"
	// evalSession 是全部用例共用的会话标识（用例之间各自新建夹具，不会串）。
	evalSession = "s1"
	// evalStubModel 是桩摘要器的模型版本字：前缀 `stub:` 是"这不是真实模型"的机器判据。
	evalStubModel = "stub:deterministic-summarizer-v1"
	// evalStubPrefix 判定"模型版本是否桩"的前缀。
	evalStubPrefix = "stub:"
	// evalQualityClaim 声明本用例**不主张**任何质量结论。
	evalQualityClaim = "none"
	// evalNotMeasured 是"未测量"的唯一字面量（G1-25 冻结）。
	evalNotMeasured = "未测量"
	// evalLabelStatus 如实标记标注来源：AI 依 2026-10-27 用户授权判定，不是人工标注。
	evalLabelStatus = "ai_adjudicated_not_human"
	// evalSummaryRole 是摘要文本估算用的角色字，与引擎内部 `roleSummarySystem` 同值——
	// 引擎的 `estimateTextTokens` 未导出，这里复制**字面量**而不是复制实现。
	evalSummaryRole = "system"

	// 三臂共用的预算与保留参数（三臂必须同预算，否则对照不成立）。
	evalContextTokens    = 2000
	evalOutputReserve    = 50
	evalFixedTotal       = 200
	evalKeepRecent       = 3
	evalSummaryMaxTokens = 100
	// evalTriggerRatio 把比例线压到 `fixed` 之下，使**每条用例的每次压缩都必然触发**：
	// 触发与否不该由某条用例的正文长短决定，否则改一条字数就会让读数漂移。
	// The ratio line sits below the fixed overhead so every case always compacts.
	evalTriggerRatio = 0.01
)

// 冻结读数：数据集摘要与"任务集 + 实现"共同决定的聚合读数。
// `PENDING-FIRST-RUN` 是首轮守卫：没有冻结过的报告**不得**被当成基线。
const (
	evalPendingDigest       = "PENDING-FIRST-RUN"
	evalFrozenDatasetSHA256 = "a5378b876f052b9b677ab28eb235b169ba5f8012c544b51849519bbfb0844b44"
	evalFrozenReadings      = "A:calls=0,ctx=399,sum=0,kept=8,lost=10,leak=0 | B:calls=9,ctx=679,sum=280,kept=8,lost=10,leak=0 | C=未执行"
)

// —— 任务集结构 ——

// evalDataset 是任务集文件的整体形态。
type evalDataset struct {
	Dataset     string     `json:"dataset"`
	Version     int        `json:"version"`
	LabelStatus string     `json:"label_status"`
	Source      string     `json:"source"`
	Cases       []evalCase `json:"cases"`
}

// evalCase 是一条用例：一段会话历史 + 必须保持的条目（ground truth）。
type evalCase struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Rationale   string            `json:"rationale"`
	Compactions int               `json:"compactions"`
	ResumeAt    int               `json:"resume_at"`
	Messages    []evalCaseMessage `json:"messages"`
	Revoke      []evalRef         `json:"revoke"`
	MustKeep    []evalPoint       `json:"must_keep"`
}

// evalCaseMessage 是一条消息；`source_refs` 用于撤权用例，其余用例留空。
type evalCaseMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []evalToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
	Truncated  bool           `json:"truncated"`
	SourceRefs []evalRef      `json:"source_refs"`
}

// evalToolCall 是一条工具调用声明。
type evalToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// evalRef 是一条来源引用（种类固定 knowledge，与测试夹具同一口径）。
type evalRef struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
}

// evalPoint 是一条必须保持的要点：`point` 必须是 `message` 那条消息正文的**逐字子串**。
type evalPoint struct {
	Message int    `json:"message"`
	Point   string `json:"point"`
}

// evalLoadDataset 读取并解析任务集。
func evalLoadDataset(t *testing.T) (evalDataset, []byte) {
	t.Helper()
	path := filepath.Join(evalRoot, evalDatasetFile)
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "读取任务集 %s", path)
	var ds evalDataset
	require.NoErrorf(t, json.Unmarshal(raw, &ds), "解析任务集 %s", path)
	return ds, raw
}

// evalDatasetDigest 按"排序后的 相对路径 + 内容哈希"规范字节算 sha256：
// 文件重排、换行风格或内容任一变化都会改变摘要，冻结值才对得上号。
func evalDatasetDigest(t *testing.T, raw []byte) string {
	t.Helper()
	inner := sha256.Sum256(raw)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", filepath.ToSlash(filepath.Join(evalRoot, evalDatasetFile)),
		hex.EncodeToString(inner[:]))
	return hex.EncodeToString(h.Sum(nil))
}

// evalStoredMessages 把任务集里的消息转成引擎视图（不分配标识——由夹具按落库顺序给）。
func evalStoredMessages(t *testing.T, in []evalCaseMessage) []compaction.StoredMessage {
	t.Helper()
	out := make([]compaction.StoredMessage, 0, len(in))
	for i, m := range in {
		require.NotEmptyf(t, m.Role, "第 %d 条消息缺少 role", i)
		sm := compaction.StoredMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Truncated:  m.Truncated,
		}
		for _, tc := range m.ToolCalls {
			sm.ToolCalls = append(sm.ToolCalls, core.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		for _, r := range m.SourceRefs {
			sm.SourceRefs = append(sm.SourceRefs, ref(r.ID, r.VersionID))
		}
		out = append(out, sm)
	}
	return out
}

// evalRunBound 是一段"一次 Run 新增的消息"：连续压缩用例据此切成两次 Run。
type evalRunBound struct {
	from int
	to   int
	run  string
}

// evalRunBounds 按 `compactions` / `resume_at` 切分 Run 边界。
func evalRunBounds(t *testing.T, c evalCase, total int) []evalRunBound {
	t.Helper()
	resume := c.ResumeAt
	if resume <= 0 || resume > total {
		resume = total
	}
	bounds := []evalRunBound{{from: 0, to: resume, run: "r1"}, {from: resume, to: total, run: "r2"}}
	require.Truef(t, c.Compactions >= 1 && c.Compactions <= len(bounds),
		"[%s] 压缩次数 %d 超出可切分的 Run 段数", c.ID, c.Compactions)
	return bounds[:c.Compactions]
}

// evalHarness 建一台三臂同口径的夹具：预算 / 保留组数 / 摘要上限对三臂完全一致。
func evalHarness(t *testing.T) *compactionHarness {
	t.Helper()
	h := newCompactionHarness(t)
	// 比例线必须在 `setLimits` 之前改：策略是构造时冻结的，`setLimits` 会重建引擎。
	h.policy.TriggerRatio = evalTriggerRatio
	h.setLimits(evalContextTokens, evalKeepRecent, evalSummaryMaxTokens)
	h.budget(evalContextTokens, evalOutputReserve)
	h.fixed(evalFixedTotal)
	return h
}

// evalIndexOf 返回一条消息在会话顺序里的下标。
func evalIndexOf(t *testing.T, msgs []compaction.StoredMessage, id string) int {
	t.Helper()
	for i, m := range msgs {
		if m.MessageID == id {
			return i
		}
	}
	t.Fatalf("保留起点 %s 不在消息表里", id)
	return -1
}

// evalAssembleText 把投影段还原成"模型会看到的正文"，供逐字子串判卷使用。
// `recent` 段的 Content 按设计恒为空（正文由提示层从消息渲染），故单独拼接。
func evalAssembleText(segments []compaction.ContextSegment, recent []compaction.StoredMessage) string {
	var b strings.Builder
	for _, s := range segments {
		if s.ID == compaction.SegRecent {
			for _, m := range recent {
				b.WriteString(m.Content)
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteString(s.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// evalSegmentIDs 取段序（三臂配置是否真的不同的机器判据）。
func evalSegmentIDs(segments []compaction.ContextSegment) []string {
	ids := make([]string, 0, len(segments))
	for _, s := range segments {
		ids = append(ids, s.ID)
	}
	return ids
}

// evalTextTokens 估算一段纯文本的 token 数：与引擎 `estimateTextTokens` 同一公式
// （`core.EstimateMessage` + `system` 角色字），此处只复制字面量、不复制实现。
func evalTextTokens(text string) int {
	if text == "" {
		return 0
	}
	return core.EstimateMessage(core.Message{Role: evalSummaryRole, Content: text})
}

// —— 桩摘要器 ——

// evalStubSummarizer 是**确定性**摘要器：正文只含覆盖条数与首末标识，
// **不含任何语义内容**。它存在的意义是让"调用了几次、输入是什么、输出多大"可判定，
// 而不是替代模型——因此任何据它得出的"保住了几条要点"都只能读成未测量。
// evalStubSummarizer is deterministic and semantically empty: it makes calls, inputs and
// sizes observable, and nothing else.
type evalStubSummarizer struct {
	mu     sync.Mutex
	inputs []compaction.SummaryInput
}

// Summarize 实现 `compaction.Summarizer`。
func (s *evalStubSummarizer) Summarize(_ context.Context, in compaction.SummaryInput) (compaction.SummaryOutput, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	s.mu.Unlock()
	return compaction.SummaryOutput{
		Text:  evalStubText(in),
		Usage: core.Usage{},
		Model: in.Model,
	}, nil
}

// recorded 返回每次调用的完整输入。
func (s *evalStubSummarizer) recorded() []compaction.SummaryInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]compaction.SummaryInput{}, s.inputs...)
}

// evalStubText 渲染桩摘要正文：只写条数与首末标识，**不写**任何要点。
// 正文刻意不带 `## 已完成` 段——那一节要求逐条带证据标识，桩没有可引用的证据。
func evalStubText(in compaction.SummaryInput) string {
	first, last := "", ""
	if len(in.Covered) > 0 {
		first = in.Covered[0].MessageID
		last = in.Covered[len(in.Covered)-1].MessageID
	}
	return fmt.Sprintf("【确定性桩摘要】覆盖 %d 条消息（%s → %s）。不含语义内容，未测量。",
		len(in.Covered), first, last)
}

// —— 三臂执行 ——

// evalCaseReading 是**一个用例 × 一个臂**的当场读数。
type evalCaseReading struct {
	ContextTokens int
	SummaryTokens int
	SummaryCalls  int
	Retained      int
	Lost          int
	LostPoints    []string
	SegmentIDs    []string
	Leakage       int
}

// evalScorePoints 逐条判"要点原文是否逐字留在上下文里"，并登记丢失样例。
func evalScorePoints(t *testing.T, c evalCase, arm, context string) (retained, lost int, samples []string) {
	t.Helper()
	for _, p := range c.MustKeep {
		if strings.Contains(context, p.Point) {
			retained++
			continue
		}
		lost++
		samples = append(samples, fmt.Sprintf("%s/%s 丢失要点（%s#%d）：%s", c.ID, arm, c.Kind, p.Message, p.Point))
	}
	return retained, lost, samples
}

// evalRunArmA 跑基线臂：完整工具组整组丢弃，无摘要。它不调摘要器，零外部调用。
func evalRunArmA(t *testing.T, c evalCase) evalCaseReading {
	t.Helper()
	h := evalHarness(t)
	// 基线臂不做来源过滤，撤权只作为配置声明存在——三臂的输入必须同一份。
	for _, r := range c.Revoke {
		h.validity.revoke(ref(r.ID, r.VersionID))
	}
	h.seedRun(t, evalSession, "r1", evalStoredMessages(t, c.Messages)...)

	all := compaction.AssignToolGroups(h.msgs)
	cut, ok := compaction.FindCut(all, evalKeepRecent)
	require.Truef(t, ok, "[%s/A] 任务集必须留出可丢弃的完整工具组", c.ID)
	recent := all[cut:]

	segments := compaction.Projection{}.Assemble("", "", "", nil, recent)
	context := evalAssembleText(segments, recent)
	reading := evalCaseReading{
		ContextTokens: compaction.MessageEstimator{}.Estimate(recent),
		SegmentIDs:    evalSegmentIDs(segments),
	}
	reading.Retained, reading.Lost, reading.LostPoints = evalScorePoints(t, c, "A", context)
	return reading
}

// evalRunArmB 跑"单份摘要"臂：真跑引擎（真切点、真过滤、真追加），摘要器是桩。
func evalRunArmB(t *testing.T, c evalCase) evalCaseReading {
	t.Helper()
	h := evalHarness(t)
	stub := &evalStubSummarizer{}
	// `setLimits` 会用默认摘要器重建引擎，故必须在它之后换成桩。
	h.compactor = compaction.NewCompactor(compaction.CompactorDeps{
		Messages:   h.reader,
		Store:      h.store,
		Summarizer: stub,
		Validator:  h.validity,
		Estimator:  compaction.MessageEstimator{},
		Clock:      func() time.Time { return time.Unix(1760000000, 0).UTC() },
		Policy:     h.policy,
	})
	for _, r := range c.Revoke {
		h.validity.revoke(ref(r.ID, r.VersionID))
	}

	msgs := evalStoredMessages(t, c.Messages)
	for i, b := range evalRunBounds(t, c, len(msgs)) {
		h.seedRun(t, evalSession, b.run, msgs[b.from:b.to]...)
		res, err := h.compactor.Compact(h.ctx, h.request(evalSession, b.run))
		require.NoErrorf(t, err, "[%s/B] 第 %d 次压缩报错", c.ID, i+1)
		require.Falsef(t, res.Degraded, "[%s/B] 第 %d 次压缩降级：%s", c.ID, i+1, res.ReasonCode)
		require.NotNilf(t, res.Entry, "[%s/B] 第 %d 次压缩没有给出条目", c.ID, i+1)
	}
	require.Equalf(t, c.Compactions, h.store.count(t), "[%s/B] 每次压缩都应写入一条新条目", c.ID)

	chain, err := h.store.LatestCommittedChain(h.ctx, compaction.Scope{SessionID: evalSession})
	require.NoErrorf(t, err, "[%s/B] 读已提交链", c.ID)
	require.NotEmptyf(t, chain, "[%s/B] 压缩后必须留下可注入的链", c.ID)
	head := chain[len(chain)-1]

	all := compaction.AssignToolGroups(h.msgs)
	recent := all[evalIndexOf(t, all, head.FirstKeptMessageID):]
	segments := compaction.Projection{}.Assemble("", "", "", chain, recent)
	context := evalAssembleText(segments, recent)

	reading := evalCaseReading{
		ContextTokens: compaction.MessageEstimator{}.Estimate(recent) + evalTextTokens(head.Summary),
		SummaryTokens: evalTextTokens(head.Summary),
		SummaryCalls:  len(stub.recorded()),
		SegmentIDs:    evalSegmentIDs(segments),
	}
	reading.Retained, reading.Lost, reading.LostPoints = evalScorePoints(t, c, "B", context)
	reading.Leakage = evalLeakage(t, c, h, stub)
	return reading
}

// evalLeakage 校验撤权红线：挂失效引用的消息**必须**在摘要输入里被整条替换，
// 正文**不得**进入摘要器。返回命中的次数（红线为 0）。
// evalLeakage returns how many times a revoked body reached the summarizer; the red line is 0.
func evalLeakage(t *testing.T, c evalCase, h *compactionHarness, stub *evalStubSummarizer) int {
	t.Helper()
	if len(c.Revoke) == 0 {
		return 0
	}
	revoked := make(map[evalRef]struct{}, len(c.Revoke))
	for _, r := range c.Revoke {
		revoked[r] = struct{}{}
	}
	hits := 0
	for i, m := range c.Messages {
		if m.Content == "" || !evalHasRef(m, revoked) {
			continue
		}
		id := h.msgs[i].MessageID
		for _, in := range stub.recorded() {
			if strings.Contains(renderSummaryInput(in), m.Content) {
				hits++
			}
			for _, sm := range in.Covered {
				if sm.MessageID == id {
					require.Truef(t, sm.Redacted,
						"[%s] 撤权消息 %s 在摘要输入里未被标记为不可注入", c.ID, id)
				}
			}
		}
	}
	return hits
}

// evalHasRef 报告一条消息是否挂着任一被撤权的引用。
func evalHasRef(m evalCaseMessage, revoked map[evalRef]struct{}) bool {
	for _, r := range m.SourceRefs {
		if _, ok := revoked[r]; ok {
			return true
		}
	}
	return false
}

// —— 报告（只在测试里存在的类型，不新增生产导出概念）——

// evalCompactionReport 是三臂对照的机器读数。
type evalCompactionReport struct {
	Dataset        string
	DatasetSHA256  string
	Model          string // 桩摘要器的模型版本字
	Estimator      string // Token 估算口径版本
	LabelStatus    string
	QualityClaim   string
	Benefit        string // 未测量时**必须**逐字写 `未测量`
	Measurement    *evalMeasurement
	Cases          int
	ByKind         map[string]int
	Arms           []evalArmReport
	FailureSamples []string
	Notes          []string
}

// evalArmReport 是一条臂的配置与聚合读数。
type evalArmReport struct {
	Arm              string // A / B / C
	Name             string // 逐字取自 G1-25
	Summary          bool
	StateBlock       bool
	Recall           bool
	KeepRecentGroups int
	Implemented      bool // 生产代码里存在这个配置
	Executed         bool // 本轮真的跑过
	Unmeasured       bool // 未跑 / 跑了也不得给收益
	SummaryCalls     int
	ContextTokens    int
	SummaryTokens    int
	RetainedPoints   int
	LostPoints       int
	Leakage          int
}

// evalMeasurement 是一次**真实**测量的证据；没有它就不许写收益数字。
type evalMeasurement struct {
	Model      string // 真实模型版本（**不得**是 `stub:` 前缀）
	Runs       int    // 至少两轮才有可比性
	MeasuredAt string // YYYY-MM-DD
	Evidence   string // 读数落点（报告路径 / 段落）
}

// evalArmsConfig 返回三臂的**配置声明**——与是否跑过无关，三组配置必须两两不同。
func evalArmsConfig() []evalArmReport {
	return []evalArmReport{
		{Arm: "A", Name: "近期历史", Implemented: true, KeepRecentGroups: evalKeepRecent},
		{Arm: "B", Name: "单份摘要", Summary: true, Implemented: true, KeepRecentGroups: evalKeepRecent},
		{
			Arm: "C", Name: "分层视图＋回查", Summary: true, StateBlock: true, Recall: true,
			Implemented: false, KeepRecentGroups: evalKeepRecent, Unmeasured: true,
		},
	}
}

// validate 判卷报告是否合规：数据可追溯、三臂配置确实不同、未实现的臂没有读数、
// **没有真实测量就不得出现收益数字**。返回 nil 表示合规。
func (r evalCompactionReport) validate() error {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if r.Dataset == "" {
		fail("数据集名缺失")
	}
	if r.DatasetSHA256 == "" || r.DatasetSHA256 == evalPendingDigest {
		fail("数据集哈希未冻结（%q）：报告必须绑定一份可复核的任务集", r.DatasetSHA256)
	}
	if r.Estimator == "" {
		fail("估算器版本缺失")
	}
	if len(r.Arms) != 3 {
		fail("必须恰好三臂，实得 %d", len(r.Arms))
	}

	seen := make(map[string]string, len(r.Arms))
	for _, a := range r.Arms {
		key := fmt.Sprintf("%t/%t/%t/%d", a.Summary, a.StateBlock, a.Recall, a.KeepRecentGroups)
		if prev, dup := seen[key]; dup {
			fail("%s 与 %s 的配置完全相同：三臂必须互不相同", a.Arm, prev)
		}
		seen[key] = a.Arm
		if !a.Implemented {
			if a.Executed {
				fail("%s 尚未实现却被标记为已执行", a.Arm)
			}
			if a.SummaryCalls != 0 || a.ContextTokens != 0 || a.SummaryTokens != 0 ||
				a.RetainedPoints != 0 || a.LostPoints != 0 || a.Leakage != 0 {
				fail("%s 尚未实现却带非零读数：未实现的能力不得有测量值", a.Arm)
			}
			if !a.Unmeasured {
				fail("%s 尚未实现却没有标注未测量", a.Arm)
			}
		}
	}

	// 收益守卫：未测量时只能写 `未测量`；写了数字就必须拿得出真实测量的证据。
	if r.Benefit != evalNotMeasured {
		m := r.Measurement
		switch {
		case m == nil:
			fail("报告给出了收益（%q）却没有测量记录：未真实测量时只能写 %q", r.Benefit, evalNotMeasured)
		case strings.HasPrefix(m.Model, evalStubPrefix):
			fail("收益（%q）来自桩模型（%q）：确定性桩不含语义，不得据此给出收益", r.Benefit, m.Model)
		case m.Runs < 2:
			fail("测量轮次 %d 不足：少于两轮不可比", m.Runs)
		case m.MeasuredAt == "":
			fail("测量记录缺少 MeasuredAt")
		case m.Evidence == "":
			fail("测量记录缺少 Evidence：读数必须能指回落点")
		}
	}
	if r.Measurement == nil && !strings.HasPrefix(r.Model, evalStubPrefix) {
		fail("未测量的报告必须标明桩模型（%s 前缀），实得 %q", evalStubPrefix, r.Model)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("压缩对照报告不合规：\n- %s", strings.Join(problems, "\n- "))
}

// evalReadingsKey 把各臂聚合读数压成一行规范串，用作冻结比对。
func evalReadingsKey(arms []evalArmReport) string {
	parts := make([]string, 0, len(arms))
	for _, a := range arms {
		if !a.Executed {
			parts = append(parts, a.Arm+"=未执行")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:calls=%d,ctx=%d,sum=%d,kept=%d,lost=%d,leak=%d",
			a.Arm, a.SummaryCalls, a.ContextTokens, a.SummaryTokens,
			a.RetainedPoints, a.LostPoints, a.Leakage))
	}
	return strings.Join(parts, " | ")
}

// evalBuildReport 逐用例跑 A / B 两臂并汇总成报告。
func evalBuildReport(t *testing.T, ds evalDataset, digest string) evalCompactionReport {
	t.Helper()
	byKind := make(map[string]int, len(ds.Cases))
	totals := map[string]*evalArmReport{}
	for _, a := range evalArmsConfig() {
		arm := a
		totals[arm.Arm] = &arm
	}

	var samples []string
	for _, c := range ds.Cases {
		byKind[c.Kind]++

		aReading := evalRunArmA(t, c)
		require.Equalf(t, []string{compaction.SegRecent}, aReading.SegmentIDs,
			"[%s/A] 基线臂只有近期历史段", c.ID)
		evalAccumulate(totals["A"], aReading)
		samples = append(samples, aReading.LostPoints...)

		bReading := evalRunArmB(t, c)
		require.Equalf(t, []string{compaction.SegSummary, compaction.SegRecent}, bReading.SegmentIDs,
			"[%s/B] 单份摘要臂必须是 摘要 → 近期 两段", c.ID)
		evalAccumulate(totals["B"], bReading)
		samples = append(samples, bReading.LostPoints...)
	}

	arms := make([]evalArmReport, 0, 3)
	for _, a := range evalArmsConfig() {
		arm := *totals[a.Arm]
		arm.Name, arm.Summary, arm.StateBlock, arm.Recall = a.Name, a.Summary, a.StateBlock, a.Recall
		arm.Implemented, arm.KeepRecentGroups, arm.Unmeasured = a.Implemented, a.KeepRecentGroups, a.Unmeasured
		arm.Executed = arm.Implemented
		arms = append(arms, arm)
	}

	return evalCompactionReport{
		Dataset:        ds.Dataset,
		DatasetSHA256:  digest,
		Model:          evalStubModel,
		Estimator:      core.EstimateVersion,
		LabelStatus:    ds.LabelStatus,
		QualityClaim:   evalQualityClaim,
		Benefit:        evalNotMeasured,
		Cases:          len(ds.Cases),
		ByKind:         byKind,
		Arms:           arms,
		FailureSamples: samples,
		Notes: []string{
			"摘要器是确定性桩：正文只含覆盖条数与首末标识，不含语义内容——任何'保持率'读数均不代表模型能力。",
			"C 臂（分层视图＋回查）在本 US 尚未实现（StateBlock 恒为空、按需回查未交付），本轮不跑、不给读数。",
			"延迟、任务成功率、回查次数与回查 Token 未测量：桩摘要器无网络与模型耗时，测出的延迟不具代表性。",
			"权限泄露红线只在摘要输入路径上判定（只有 B 臂有摘要输入）；注入时的来源闸门由 SourceGate 用例覆盖，不在本评测内。",
		},
	}
}

// evalAccumulate 把一次用例读数累加进臂聚合。
func evalAccumulate(arm *evalArmReport, r evalCaseReading) {
	arm.SummaryCalls += r.SummaryCalls
	arm.ContextTokens += r.ContextTokens
	arm.SummaryTokens += r.SummaryTokens
	arm.RetainedPoints += r.Retained
	arm.LostPoints += r.Lost
	arm.Leakage += r.Leakage
}

// evalDumpReport 把报告打成一行 JSON（失败时贴进日志，便于对账）。
func evalDumpReport(t *testing.T, r evalCompactionReport) string {
	t.Helper()
	raw, err := json.Marshal(r)
	require.NoError(t, err, "序列化压缩对照报告")
	return string(raw)
}

// TestEval_CompactionArms 是 G1-25 的结构性判卷：三组配置存在且互不相同、
// 报告字段齐全、未测量的地方逐字写"未测量"、读数与冻结值逐字一致。
// 「逆改点」：删掉 B 臂的摘要段 → 段序断言红；把 `Benefit` 改成任何数字 →
// `validate()` 红；把 C 臂填上任一读数 → `validate()` 红；把桩摘要改成带语义的文本 →
// `retained_points` 变化，冻结读数红。
func TestEval_CompactionArms(t *testing.T) {
	ds, raw := evalLoadDataset(t)
	digest := evalDatasetDigest(t, raw)

	t.Run("dataset", func(t *testing.T) {
		require.Equalf(t, "us16-compaction-arms", ds.Dataset, "任务集名与报告口径不符")
		require.Equal(t, 1, ds.Version, "任务集版本")
		require.Equalf(t, evalLabelStatus, ds.LabelStatus,
			"标注来源必须如实标记（AI 依 2026-10-27 授权判定，非人工标注）")

		// 八类用例必须齐全——少一类就等于少一个失败模式。
		wantKinds := []string{
			"goal", "constraint_negation", "number_unit", "open_items",
			"version_change", "revoked_source", "long_tool_output", "sequential_compaction",
		}
		gotKinds := make(map[string]int, len(ds.Cases))
		ids := make(map[string]struct{}, len(ds.Cases))
		for _, c := range ds.Cases {
			require.NotEmptyf(t, c.ID, "用例缺少 id")
			_, dup := ids[c.ID]
			require.Falsef(t, dup, "用例 id 重复：%s", c.ID)
			ids[c.ID] = struct{}{}
			gotKinds[c.Kind]++

			require.Containsf(t, wantKinds, c.Kind, "[%s] 未归类的用例类型", c.ID)
			require.NotEmptyf(t, c.Rationale, "[%s] 缺少判定理由", c.ID)
			require.NotEmptyf(t, c.MustKeep, "[%s] 没有声明必须保持的条目", c.ID)
			require.Containsf(t, []int{1, 2}, c.Compactions, "[%s] 压缩次数只能是 1 或 2", c.ID)
			if c.Compactions > 1 {
				require.Truef(t, c.ResumeAt > 0 && c.ResumeAt < len(c.Messages),
					"[%s] 连续压缩必须给出可切分的 resume_at", c.ID)
			}

			// ground truth 可追溯：每条要点必须逐字落在它指认的那条消息里。
			for _, p := range c.MustKeep {
				require.Truef(t, p.Message >= 0 && p.Message < len(c.Messages),
					"[%s] 要点指认的消息下标越界：%d", c.ID, p.Message)
				require.Containsf(t, c.Messages[p.Message].Content, p.Point,
					"[%s] 要点 %q 不是消息 #%d 的逐字子串", c.ID, p.Point, p.Message)
			}
		}
		for _, kind := range wantKinds {
			require.Positivef(t, gotKinds[kind], "任务集缺少用例类型 %q", kind)
		}
	})

	t.Run("arms", func(t *testing.T) {
		arms := evalArmsConfig()
		require.Len(t, arms, 3, "必须恰好三臂")
		require.Equal(t, "近期历史", arms[0].Name, "A 臂名逐字取自 G1-25")
		require.Equal(t, "单份摘要", arms[1].Name, "B 臂名逐字取自 G1-25")
		require.Equal(t, "分层视图＋回查", arms[2].Name, "C 臂名逐字取自 G1-25")

		seen := make(map[string]struct{}, len(arms))
		for _, a := range arms {
			key := fmt.Sprintf("%t/%t/%t/%d", a.Summary, a.StateBlock, a.Recall, a.KeepRecentGroups)
			_, dup := seen[key]
			require.Falsef(t, dup, "%s 臂配置与已有臂完全相同，对照不成立", a.Arm)
			seen[key] = struct{}{}
		}
		require.False(t, arms[2].Implemented, "C 臂本 US 未实现，不得写成已交付")
	})

	t.Run("run", func(t *testing.T) {
		report := evalBuildReport(t, ds, digest)

		require.NoErrorf(t, report.validate(), "报告不合规：%s", evalDumpReport(t, report))
		require.Equal(t, evalNotMeasured, report.Benefit, "未真实测量时收益只能写未测量")
		require.Nil(t, report.Measurement, "本轮没有真实测量记录")
		require.True(t, strings.HasPrefix(report.Model, evalStubPrefix), "模型版本必须标明桩")
		require.Equal(t, core.EstimateVersion, report.Estimator, "Token 估算口径版本")

		// C 臂：只声明、不测量。
		c := report.Arms[2]
		require.False(t, c.Executed, "C 臂本轮不执行")
		require.True(t, c.Unmeasured, "C 臂本轮标注未测量")
		require.Zero(t, c.ContextTokens, "C 臂不得有 Token 读数")

		// 撤权红线：0 次命中。
		for _, a := range report.Arms {
			require.Zerof(t, a.Leakage, "%s 臂把撤权正文送进了摘要输入", a.Arm)
		}

		// 失败样例必须保留（未测量不等于"没有失败"）。
		require.NotEmptyf(t, report.FailureSamples, "桩摘要器下必然丢要点，失败样例不得为空：%s",
			evalDumpReport(t, report))

		// 冻结读数：先冻结、后测量。读数一律先打进日志——它是本用例的现场证据。
		got := evalReadingsKey(report.Arms)
		t.Logf("压缩三臂读数：%s", got)
		require.NotEqualf(t, evalPendingDigest, evalFrozenReadings, "冻结读数尚未回填")
		require.Equalf(t, evalFrozenReadings, got,
			"三臂读数与冻结值不一致——读数变了必须先确认原因再改冻结常量\n报告：%s", evalDumpReport(t, report))

		// 可复现：同一任务集连跑两次，读数逐字相同。
		again := evalBuildReport(t, ds, digest)
		require.Equalf(t, got, evalReadingsKey(again.Arms), "两次运行读数不一致：桩必须确定性")
	})

	t.Run("digest", func(t *testing.T) {
		t.Logf("数据集摘要：%s", digest)
		require.NotEqualf(t, evalPendingDigest, evalFrozenDatasetSHA256, "数据集哈希尚未冻结")
		require.Equalf(t, evalFrozenDatasetSHA256, digest,
			"任务集内容已变化——改任务集必须重新冻结哈希并重跑读数")
	})
}

// TestEval_CompactionReportRejectsBenefitWithoutMeasurement 证明收益守卫**有判别力**：
// 它拒绝"没有测量的收益"与"桩测量冒充收益"，但不一刀切拒绝合规的真实测量。
// 「逆改点」：把 `validate()` 里的收益守卫删掉 → 前三个子用例翻红；把条件写成
// 恒真（一律拒绝）→ 最后一个子用例翻红。
func TestEval_CompactionReportRejectsBenefitWithoutMeasurement(t *testing.T) {
	baseline := func() evalCompactionReport {
		return evalCompactionReport{
			Dataset:       "us16-compaction-arms",
			DatasetSHA256: "9f1c0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d0f0d",
			Model:         evalStubModel,
			Estimator:     core.EstimateVersion,
			LabelStatus:   evalLabelStatus,
			QualityClaim:  evalQualityClaim,
			Benefit:       evalNotMeasured,
			Cases:         8,
			Arms:          evalArmsConfig(),
		}
	}

	t.Run("未测量时合规", func(t *testing.T) {
		require.NoError(t, baseline().validate())
	})

	t.Run("无测量记录的收益被拒", func(t *testing.T) {
		r := baseline()
		r.Benefit = "输入 token 比 A 组低 12%"
		err := r.validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "测量", "错误文案必须点明缺的是测量")
	})

	t.Run("桩模型测量不能证明收益", func(t *testing.T) {
		r := baseline()
		r.Benefit = "输入 token 比 A 组低 12%"
		r.Measurement = &evalMeasurement{Model: evalStubModel, Runs: 3, MeasuredAt: "2026-10-29", Evidence: "x"}
		err := r.validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "桩", "错误文案必须点明桩模型不含语义")
	})

	t.Run("轮次不足被拒", func(t *testing.T) {
		r := baseline()
		r.Model = "qwen-max"
		r.Benefit = "输入 token 比 A 组低 12%"
		r.Measurement = &evalMeasurement{Model: "qwen-max", Runs: 1, MeasuredAt: "2026-10-29", Evidence: "x"}
		require.ErrorContains(t, r.validate(), "少于两轮")
	})

	t.Run("缺测量时间被拒", func(t *testing.T) {
		r := baseline()
		r.Model = "qwen-max"
		r.Benefit = "输入 token 比 A 组低 12%"
		r.Measurement = &evalMeasurement{Model: "qwen-max", Runs: 2, Evidence: "x"}
		require.ErrorContains(t, r.validate(), "MeasuredAt")
	})

	t.Run("缺证据落点被拒", func(t *testing.T) {
		r := baseline()
		r.Model = "qwen-max"
		r.Benefit = "输入 token 比 A 组低 12%"
		r.Measurement = &evalMeasurement{Model: "qwen-max", Runs: 2, MeasuredAt: "2026-10-29"}
		require.ErrorContains(t, r.validate(), "Evidence")
	})

	t.Run("完整的真实测量放行", func(t *testing.T) {
		r := baseline()
		r.Model = "qwen-max"
		r.Benefit = "输入 token 比 A 组低 12%"
		r.Measurement = &evalMeasurement{
			Model: "qwen-max", Runs: 2, MeasuredAt: "2026-10-29",
			Evidence: "docs/us/us16-eval-report.md#压缩章节",
		}
		require.NoError(t, r.validate(), "守卫不是一刀切拒绝：合规的真实测量必须放行")
	})

	t.Run("未实现臂带读数被拒", func(t *testing.T) {
		r := baseline()
		r.Arms[2].ContextTokens = 123
		require.ErrorContains(t, r.validate(), "尚未实现却带非零读数")
	})

	t.Run("三臂配置重复被拒", func(t *testing.T) {
		r := baseline()
		r.Arms[2].Summary = false
		r.Arms[2].StateBlock = false
		r.Arms[2].Recall = false
		require.ErrorContains(t, r.validate(), "三臂必须互不相同")
	})
}
