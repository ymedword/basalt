//go:build e2e

package e2e

// 本文件是 US-21（学习准入）的端到端验收（颗粒度文档 §4.5、tasks T025）。
//
// 五条主链：
//  1. 全链路：提议 → 评估 → 批准（含 25 小时负例）→ 发布 → Agent 绑定生效 → 撤回；
//  2. 撤回恢复上一版：两个候选先后发布，撤回后者把前者内容作为新版本重新发布；
//  3. 崩溃恢复：伪造 pending 的发布操作行，重命令启动只查证、不重放；
//  4. 并发发布：两个进程同时发布同一候选，恰好一个赢家；
//  5. 只读管理面：列表 / 详情 / 操作查询可用，写方法与候选正文不可达。
//
// 夹具纪律：真实二进制 + 隔离 schema + 回环假模型；不调用真实模型、不发起外部网络、
// 不执行候选脚本。二进制没有时钟注入缝，因此"固定时钟"落在决策文件的**绝对时间**上：
// 每个用例开头取一次 now，批准有效期写 now+1h，24 小时上限负例写 now+25h。
// Fixture discipline: the real binary, an isolated schema and a loopback fake model; no
// real model, no external network, no candidate script execution. The binary has no
// clock seam, so the fixed clock lives in the decision file's absolute timestamps.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	us21SkillName     = "digest-format"
	us21V1Description = "把内容整理成三段摘要"
	us21V2Description = "把内容整理成三段摘要（新版）"
	us21V3Description = "把内容整理成三段摘要（候选二）"
	us21V1Body        = "技能正文哨兵-us21-v1：第一版正文不得进入段一。"
	us21V2Body        = "技能正文哨兵-us21-v2：候选正文不得进入段一。"
	us21V3Body        = "技能正文哨兵-us21-v3：候选二正文不得进入段一。"

	// us21SourceJSON 是严格 JSON 来源声明：未知键会被拒绝，字段名逐字取自
	// `core.ParseAdmissionSource`。`not_applicable` 不要求证据清单。
	// The strict-JSON source declaration; unknown keys are rejected by the parser.
	us21SourceJSON = `{"kind":"manual","ref":"US21 e2e 夹具","task_outcome":"not_applicable",` +
		`"confirmation":"direct","authorization":"declared","redaction":"declared",` +
		`"applicable":true,"evidence":[],"counterexamples":[],"limitations":[]}`
)

// us21CandidateJSON 是 `admission propose --json` 的候选投影。
// us21CandidateJSON mirrors the CLI candidate projection.
type us21CandidateJSON struct {
	CandidateID     string `json:"candidate_id"`
	Skill           string `json:"skill"`
	State           string `json:"state"`
	Revision        int64  `json:"revision"`
	CandidateHash   string `json:"candidate_hash"`
	SourceHash      string `json:"source_hash"`
	RiskTier        string `json:"risk_tier"`
	ProposerSubject string `json:"proposer_subject"`
	ProposerAuth    string `json:"proposer_auth_method"`
	ProposerSource  string `json:"proposer_source"`
	OperationID     string `json:"operation_id"`
}

// us21DimensionJSON 是七维判定的一条。
// us21DimensionJSON is one dimension verdict.
type us21DimensionJSON struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// us21EvaluationJSON 是 `admission evaluate --json` 的评估投影。
// us21EvaluationJSON mirrors the CLI evaluation projection.
type us21EvaluationJSON struct {
	EvaluationID      string              `json:"evaluation_id"`
	CandidateID       string              `json:"candidate_id"`
	CandidateRevision int64               `json:"candidate_revision"`
	CandidateHash     string              `json:"candidate_hash"`
	EvaluationHash    string              `json:"evaluation_hash"`
	PolicyHash        string              `json:"policy_hash"`
	Dimensions        []us21DimensionJSON `json:"dimensions"`
	Scripts           []string            `json:"scripts"`
	Aggregate         string              `json:"aggregate"`
	RiskTier          string              `json:"risk_tier"`
	Referrers         []string            `json:"referrers"`
	BaselineJSON      string              `json:"baseline_json"`
}

// us21PublicationJSON 是 `admission publish|withdraw --json` 的发布记录投影。
// us21PublicationJSON mirrors the CLI publication projection.
type us21PublicationJSON struct {
	Skill             string `json:"skill"`
	ResourceUID       string `json:"resource_uid"`
	VersionID         int64  `json:"version_id"`
	VersionNo         int64  `json:"version_no"`
	ContentHash       string `json:"content_hash"`
	State             string `json:"state"`
	PublishedAt       string `json:"published_at"`
	WithdrawnAt       string `json:"withdrawn_at"`
	WithdrawReason    string `json:"withdraw_reason"`
	WithdrawVersionID int64  `json:"withdraw_version_id"`
}

// us21FileJSON 是候选清单的一条文件摘要（不含正文）。
// us21FileJSON is one frozen manifest digest; bodies never appear in this projection.
type us21FileJSON struct {
	Path string `json:"path"`
}

// us21DecisionJSON 是 `show` 详情里的最新决策投影。
// us21DecisionJSON is the latest decision projection inside `show`.
type us21DecisionJSON struct {
	DecisionID string `json:"decision_id"`
	Decision   string `json:"decision"`
	ExpiresAt  string `json:"expires_at"`
}

// us21DetailJSON 是 `admission show --json` 的详情投影。
// us21DetailJSON mirrors the CLI detail projection.
type us21DetailJSON struct {
	Candidate        us21CandidateJSON    `json:"candidate"`
	Evaluation       *us21EvaluationJSON  `json:"evaluation"`
	Decision         *us21DecisionJSON    `json:"decision"`
	Publication      *us21PublicationJSON `json:"publication"`
	Files            []us21FileJSON       `json:"files"`
	DecisionTemplate string               `json:"decision_template"`
}

// us21AdmissionView 是只读管理面列表行。
// us21AdmissionView mirrors the read-only list row.
type us21AdmissionView struct {
	CandidateID     string `json:"candidate_id"`
	SkillName       string `json:"skill_name"`
	State           string `json:"state"`
	RiskTier        string `json:"risk_tier"`
	ProposerSubject string `json:"proposer_subject"`
}

// us21AdmissionDetailView 是只读管理面详情（子记录缺失时为 null）。
// us21AdmissionDetailView mirrors the read-only detail view.
type us21AdmissionDetailView struct {
	us21AdmissionView
	Source struct {
		Kind string `json:"kind"`
	} `json:"source"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
	Evaluation *struct {
		Aggregate string `json:"aggregate"`
	} `json:"evaluation"`
	Publication *struct {
		State string `json:"state"`
	} `json:"publication"`
}

// us21OperationView 是操作查询的一行；本用例只判目标类型与动作。
// us21OperationView carries the two fields this case reads from an operation row.
type us21OperationView struct {
	Action     string `json:"action"`
	TargetKind string `json:"target_kind"`
}

// us21Setup 建夹具：独立工作目录 + 已初始化工作区，并登记"恢复只读目录可写"的清理。
// 冻结的候选目录是只读的，Windows 上 t.TempDir 的 RemoveAll 需要先去掉只读位；该清理
// 注册晚于 TempDir 清理，按 LIFO 先执行。
// us21Setup builds the fixture and registers the writability restore that must run
// before the TempDir sweep (frozen candidate trees are read-only).
func us21Setup(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	t.Cleanup(func() { us21MakeTreeWritable(t, h.Root) })
	initWorkspace(t, h)
	return h
}

// us21MakeTreeWritable 把工作区整树的权限恢复为可写（目录 0755 / 文件 0644）。
// us21MakeTreeWritable restores writable modes across the workspace tree.
func us21MakeTreeWritable(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
	if err != nil {
		t.Logf("恢复 %s 的可写权限失败: %v", root, err)
	}
}

// us21Bootstrap 落配置并迁移运行期 schema；调用方随后写技能、跑基线预热。
// us21Bootstrap writes the config and migrates the runtime schema.
func us21Bootstrap(t *testing.T, h *harness, baseURL string) {
	t.Helper()
	h.writeConfig(t, configWithProvider(baseURL))
	h.ensureStore(t)
}

// us21WriteSource 写候选来源目录（根 SKILL.md + references/notes.md）并返回目录路径。
// us21WriteSource writes the candidate source tree and returns its directory.
func us21WriteSource(t *testing.T, h *harness, description, body string) string {
	t.Helper()
	dir := filepath.Join(h.Root, "us21-source")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatalf("建候选来源目录失败: %v", err)
	}
	skill := "---\nname: " + us21SkillName + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatalf("写候选 SKILL.md 失败: %v", err)
	}
	notes := "参考笔记：三段摘要的结构说明。\n"
	if err := os.WriteFile(filepath.Join(dir, "references", "notes.md"), []byte(notes), 0o644); err != nil {
		t.Fatalf("写候选参考文件失败: %v", err)
	}
	return dir
}

// us21WriteSourceFile 写来源声明文件并返回路径。
// us21WriteSourceFile writes the source declaration and returns its path.
func us21WriteSourceFile(t *testing.T, h *harness) string {
	t.Helper()
	path := filepath.Join(h.Root, "us21-source.json")
	if err := os.WriteFile(path, []byte(us21SourceJSON), 0o600); err != nil {
		t.Fatalf("写来源声明失败: %v", err)
	}
	return path
}

// us21RunJSON 跑一条 `--json` 命令并要求退出码 0 且标准输出是预期 JSON。
// us21RunJSON runs one --json command and decodes its stdout.
func us21RunJSON(t *testing.T, h *harness, dst any, args ...string) {
	t.Helper()
	res := h.run(t, fixtureEnv(), args...)
	if res.ExitCode != 0 {
		t.Fatalf("%v：退出码 = %d\nstdout:\n%s\nstderr:\n%s",
			args, res.ExitCode, res.Stdout, res.Stderr)
	}
	if err := json.Unmarshal([]byte(res.Stdout), dst); err != nil {
		t.Fatalf("%v：输出不是预期 JSON：%v\n%s", args, err, res.Stdout)
	}
}

// us21Propose 提议一个候选并断言冻结结果的基本形态。
// us21Propose freezes one candidate and asserts the basic shape.
func us21Propose(t *testing.T, h *harness, dir, sourcePath string) us21CandidateJSON {
	t.Helper()
	var candidate us21CandidateJSON
	us21RunJSON(t, h, &candidate, "admission", "propose", "--name", us21SkillName,
		"--from-dir", dir, "--source-file", sourcePath, "--json")
	if candidate.CandidateID == "" || !strings.HasPrefix(candidate.CandidateID, "adm_") {
		t.Fatalf("候选标识形态不对：%q", candidate.CandidateID)
	}
	if candidate.State != "proposed" {
		t.Fatalf("新候选状态 = %q，期望 proposed", candidate.State)
	}
	if candidate.Skill != us21SkillName {
		t.Fatalf("候选技能 = %q，期望 %q", candidate.Skill, us21SkillName)
	}
	// 身份三层的第一层：主体必须是 cli:<非空>，且绝不是 cli:unknown。
	if !strings.HasPrefix(candidate.ProposerSubject, "cli:") || candidate.ProposerSubject == "cli:unknown" {
		t.Fatalf("提出者主体 = %q，期望 cli:<本机用户>", candidate.ProposerSubject)
	}
	if candidate.ProposerAuth != "cli" || candidate.ProposerSource != "cli" {
		t.Fatalf("提出者 Method / Source = %q / %q，期望 cli / cli",
			candidate.ProposerAuth, candidate.ProposerSource)
	}
	return candidate
}

// us21Evaluate 评估候选并断言七维的固定读数：四维 passed、effect / cost 恒为
// not_evaluable，聚合必须是 incomplete（不可评估**不**等于通过），风险档 normal
// （无脚本、无引用者）。regressionStatus 由调用方给出：技能已在库中时是
// not_evaluable（本 US 不做行为回归），不在库中时按"无回归面"记 passed。
// us21Evaluate runs the deterministic grader and pins the seven-dimension reading.
func us21Evaluate(t *testing.T, h *harness, candidateID, regressionStatus string) us21EvaluationJSON {
	t.Helper()
	var evaluation us21EvaluationJSON
	us21RunJSON(t, h, &evaluation, "admission", "evaluate", candidateID, "--json")
	status := make(map[string]string, len(evaluation.Dimensions))
	for _, dimension := range evaluation.Dimensions {
		status[dimension.Name] = dimension.Status
	}
	for _, name := range []string{"content", "source", "permissions", "safety"} {
		if status[name] != "passed" {
			t.Fatalf("维度 %s = %q，期望 passed（%+v）", name, status[name], evaluation.Dimensions)
		}
	}
	if status["regression"] != regressionStatus {
		t.Fatalf("维度 regression = %q，期望 %q（%+v）",
			status["regression"], regressionStatus, evaluation.Dimensions)
	}
	for _, name := range []string{"effect", "cost"} {
		if status[name] != "not_evaluable" {
			t.Fatalf("维度 %s = %q，期望 not_evaluable（本 US 无该维度证据）", name, status[name])
		}
	}
	if evaluation.Aggregate != "incomplete" {
		t.Fatalf("聚合 = %q，期望 incomplete（not_evaluable 不得计为通过）", evaluation.Aggregate)
	}
	if evaluation.RiskTier != "normal" {
		t.Fatalf("风险档 = %q，期望 normal（无脚本、无引用者）", evaluation.RiskTier)
	}
	if len(evaluation.Scripts) != 0 {
		t.Fatalf("候选不应含脚本，实际 %v", evaluation.Scripts)
	}
	return evaluation
}

// us21Template 取决策模板并解码成可变对象（模板已预填绑定字段，只缺 reason / expires_at）。
// us21Template fetches the prefilled decision template as a mutable object.
func us21Template(t *testing.T, h *harness, candidateID string) map[string]any {
	t.Helper()
	var detail us21DetailJSON
	us21RunJSON(t, h, &detail, "admission", "show", candidateID, "--decision-template", "--json")
	if detail.DecisionTemplate == "" {
		t.Fatalf("show --decision-template 未返回模板：%+v", detail)
	}
	var template map[string]any
	if err := json.Unmarshal([]byte(detail.DecisionTemplate), &template); err != nil {
		t.Fatalf("决策模板不是 JSON 对象: %v\n%s", err, detail.DecisionTemplate)
	}
	return template
}

// us21WriteDecision 把填好的决策写成 0600 的文件并返回路径。
// us21WriteDecision writes the decision file with owner-only permissions.
func us21WriteDecision(t *testing.T, h *harness, name string, template map[string]any) string {
	t.Helper()
	raw, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		t.Fatalf("序列化决策文件失败: %v", err)
	}
	path := filepath.Join(h.Root, name)
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("写决策文件失败: %v", err)
	}
	return path
}

// us21Approve 用 now+1h 的有效期批准候选。
// us21Approve approves the candidate with a one-hour window.
func us21Approve(t *testing.T, h *harness, candidateID string, expiresAt time.Time) {
	t.Helper()
	template := us21Template(t, h, candidateID)
	template["reason"] = "US21 e2e：确定性与来源材料已人工核对"
	template["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	path := us21WriteDecision(t, h, "us21-decision.json", template)
	res := h.run(t, fixtureEnv(), "admission", "approve", candidateID, "--decision-file", path)
	if res.ExitCode != 0 {
		t.Fatalf("批准失败：退出码 = %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// us21ApproveBeyondWindow 用 now+25h 的有效期批准，必须被 24 小时上限拒绝。
// us21ApproveBeyondWindow must be refused by the 24-hour ceiling.
func us21ApproveBeyondWindow(t *testing.T, h *harness, candidateID string, now time.Time) {
	t.Helper()
	template := us21Template(t, h, candidateID)
	template["reason"] = "US21 e2e：24 小时上限负例"
	template["expires_at"] = now.Add(25 * time.Hour).UTC().Format(time.RFC3339)
	path := us21WriteDecision(t, h, "us21-decision-beyond.json", template)
	res := h.run(t, fixtureEnv(), "admission", "approve", candidateID, "--decision-file", path)
	if res.ExitCode == 0 {
		t.Fatalf("超出 24 小时窗口的批准不应成功\nstdout:\n%s", res.Stdout)
	}
}

// us21Publish 发布候选并断言发布记录已生效。
// us21Publish publishes the candidate and asserts the active publication.
func us21Publish(t *testing.T, h *harness, candidateID string) us21PublicationJSON {
	t.Helper()
	var publication us21PublicationJSON
	us21RunJSON(t, h, &publication, "admission", "publish", candidateID, "--json")
	if publication.State != "active" {
		t.Fatalf("发布记录状态 = %q，期望 active", publication.State)
	}
	if !strings.HasPrefix(publication.ResourceUID, "rs_") {
		t.Fatalf("发布记录的资源身份形态不对：%q", publication.ResourceUID)
	}
	return publication
}

// us21Withdraw 撤回候选并返回发布记录投影。
// us21Withdraw withdraws the candidate and returns the publication projection.
func us21Withdraw(t *testing.T, h *harness, candidateID, reason string) us21PublicationJSON {
	t.Helper()
	var publication us21PublicationJSON
	us21RunJSON(t, h, &publication, "admission", "withdraw", candidateID, "--reason", reason, "--json")
	return publication
}

// us21Show 读取候选详情（只读，不取实例锁）。
// us21Show reads the candidate detail.
func us21Show(t *testing.T, h *harness, candidateID string) us21DetailJSON {
	t.Helper()
	var detail us21DetailJSON
	us21RunJSON(t, h, &detail, "admission", "show", candidateID, "--json")
	return detail
}

// us21RequireCard 断言段一里的技能卡片逐字指向某个发布版本：名称 / 描述 / 路径前缀
// 逐字相等，身份段等于发布记录里的 resource_uid，版本号等于发布版本号。
// us21RequireCard pins the L1 card to one published version.
func us21RequireCard(t *testing.T, system, description string, publication us21PublicationJSON) {
	t.Helper()
	prefix := "- " + us21SkillName + "：" + description + "（.basalt/versions/skills/" + us21SkillName + "/"
	requireContains(t, system, prefix, "L1 卡片三要素逐字")
	rest := system[strings.Index(system, prefix)+len(prefix):]
	suffix := fmt.Sprintf("/%d/SKILL.md）", publication.VersionNo)
	uid, _, ok := strings.Cut(rest, suffix)
	if !ok {
		t.Fatalf("L1 卡片路径必须以 %s 收尾，实际：%q", suffix, rest[:min(len(rest), 80)])
	}
	if uid != publication.ResourceUID {
		t.Fatalf("L1 卡片里的资源身份 = %q，期望发布记录里的 %q", uid, publication.ResourceUID)
	}
}

// us21VersionCount 统计某个技能名下已落库的版本行数。
// us21VersionCount counts the published version rows of one skill name.
func us21VersionCount(t *testing.T, h *harness, db *sql.DB) int {
	t.Helper()
	return scalarInt(t, db,
		`SELECT count(*) FROM resource_versions WHERE resource_kind = 'skill' AND resource_id = $1`,
		us21SkillName)
}

// TestUS21_E2E_FullAdmissionFlow 走完整链路：冻结 → 评估 → 批准（含负例）→ 发布 →
// Agent 绑定新版本 → 撤回恢复上一版，并逐段核对段一卡片与数据库行。
// TestUS21_E2E_FullAdmissionFlow drives the whole admission chain end to end.
func TestUS21_E2E_FullAdmissionFlow(t *testing.T) {
	now := time.Now().UTC()
	h := us21Setup(t)
	fake := newFakeModel(t, textResponse("第一轮：按技能格式回答"), textResponse("第二轮：已回到旧版技能"))
	us21Bootstrap(t, h, fake.URL())

	// 工作副本 v1 + 基线预热：预热既给技能建 v1（撤回的"上一版"来源），也补上
	// evaluate / approve 需要的策略快照行。
	// The v1 work copy plus the heavy warm-up seeds both the skill v1 and the policy rows.
	h.writeSkill(t, us21SkillName,
		"name: "+us21SkillName+"\ndescription: "+us21V1Description+"\n", us21V1Body)
	h.runBaselineWarmup(t, fixtureEnv())

	sourcePath := us21WriteSourceFile(t, h)
	candidate := us21Propose(t, h, us21WriteSource(t, h, us21V2Description, us21V2Body), sourcePath)
	us21Evaluate(t, h, candidate.CandidateID, "not_evaluable")

	db := h.openDB(t)
	// 24 小时上限负例：拒绝且零决策行（拒绝发生在预写操作之前，连操作行也不留）。
	// The >24h approval is refused before any row is written.
	us21ApproveBeyondWindow(t, h, candidate.CandidateID, now)
	if got := scalarInt(t, db,
		`SELECT count(*) FROM skill_admission_decisions WHERE candidate_id = $1`,
		candidate.CandidateID); got != 0 {
		t.Fatalf("超出窗口的批准留下了 %d 条决策行", got)
	}

	us21Approve(t, h, candidate.CandidateID, now.Add(time.Hour))
	publication := us21Publish(t, h, candidate.CandidateID)
	if publication.VersionNo != 2 {
		t.Fatalf("v1 已存在时候选发布版本号 = %d，期望 2", publication.VersionNo)
	}
	if got := us21VersionCount(t, h, db); got != 2 {
		t.Fatalf("发布后版本行 = %d，期望 2（基线 v1 + 候选 v2）", got)
	}

	detail := us21Show(t, h, candidate.CandidateID)
	if detail.Candidate.State != "published" {
		t.Fatalf("发布后候选状态 = %q，期望 published", detail.Candidate.State)
	}
	if detail.Publication == nil || detail.Publication.VersionID != publication.VersionID {
		t.Fatalf("发布记录与数据库不一致：%+v", detail.Publication)
	}

	// Agent 声明即绑定：发布后的新 Run 拿到 v2 的卡片，正文不预载。
	front := agentFrontmatter("demo", nil) + "skills:\n  - " + us21SkillName + "\n"
	h.writeAgent(t, "demo", front, "US21 准入后的技能绑定演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "整理一下")
	requireExit(t, res, 0, "发布后对话")
	requireContains(t, res.Stdout, "第一轮：按技能格式回答", "发布后对话")
	requests := fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("发布后对话应产生 1 次模型请求，实际 %d 次", len(requests))
	}
	system := systemPrompt(t, requests[0])
	us21RequireCard(t, system, us21V2Description, publication)
	requireAbsent(t, system, us21V2Body, "候选正文不得预载进段一")

	// 撤回：指针仍指向本次发布 → 上一版内容作为**新版本**重新发布。
	withdrawn := us21Withdraw(t, h, candidate.CandidateID, "US21 e2e：撤回候选内容")
	if withdrawn.State != "withdrawn" {
		t.Fatalf("撤回后发布记录状态 = %q，期望 withdrawn", withdrawn.State)
	}
	if withdrawn.WithdrawReason != "restored_previous" {
		t.Fatalf("撤回分类 = %q，期望 restored_previous", withdrawn.WithdrawReason)
	}
	if withdrawn.WithdrawVersionID == 0 {
		t.Fatal("restored_previous 必须记录恢复版本 id")
	}
	if got := us21VersionCount(t, h, db); got != 3 {
		t.Fatalf("撤回后版本行 = %d，期望 3（v3 = 上一版内容）", got)
	}
	workCopy, err := os.ReadFile(filepath.Join(h.Root, ".basalt", "skills", us21SkillName, "SKILL.md"))
	if err != nil {
		t.Fatalf("读撤回后的工作副本失败: %v", err)
	}
	requireContains(t, string(workCopy), us21V1Description, "撤回后工作副本回到上一版")
	requireAbsent(t, string(workCopy), us21V2Body, "撤回后工作副本不含候选正文")

	// 第二个 Run：卡片回到 v1 的描述与 v3 的路径。
	res = h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "再整理一下")
	requireExit(t, res, 0, "撤回后对话")
	requireContains(t, res.Stdout, "第二轮：已回到旧版技能", "撤回后对话")
	requests = fake.Requests()
	if len(requests) != 2 {
		t.Fatalf("撤回后对话累计应有 2 次模型请求，实际 %d 次", len(requests))
	}
	system = systemPrompt(t, requests[1])
	restored := withdrawn
	restored.VersionNo = 0
	var restoredNo int64
	if err := db.QueryRow(`SELECT version_no FROM resource_versions WHERE id = $1`,
		withdrawn.WithdrawVersionID).Scan(&restoredNo); err != nil {
		t.Fatalf("读恢复版本行失败: %v", err)
	}
	if restoredNo != 3 {
		t.Fatalf("恢复版本号 = %d，期望 3", restoredNo)
	}
	restored.VersionNo = restoredNo
	us21RequireCard(t, system, us21V1Description, restored)
	requireAbsent(t, system, "（新版）", "撤回后卡片不得再出现候选描述")
}

// TestUS21_E2E_WithdrawRestoresPrevious 先把候选一发布出去，再发布候选二并撤回：
// 撤回必须把**候选一**的内容作为新版本重新发布，而不是回到基线工作副本。
// TestUS21_E2E_WithdrawRestoresPrevious proves the restore target is the previous
// publication's content, not whatever the baseline happened to hold.
func TestUS21_E2E_WithdrawRestoresPrevious(t *testing.T) {
	now := time.Now().UTC()
	h := us21Setup(t)
	fake := newFakeModel(t)
	us21Bootstrap(t, h, fake.URL())
	h.writeSkill(t, us21SkillName,
		"name: "+us21SkillName+"\ndescription: "+us21V1Description+"\n", us21V1Body)
	h.runBaselineWarmup(t, fixtureEnv())

	sourcePath := us21WriteSourceFile(t, h)

	first := us21Propose(t, h, us21WriteSource(t, h, us21V2Description, us21V2Body), sourcePath)
	us21Evaluate(t, h, first.CandidateID, "not_evaluable")
	us21Approve(t, h, first.CandidateID, now.Add(time.Hour))
	publishedFirst := us21Publish(t, h, first.CandidateID)
	if publishedFirst.VersionNo != 2 {
		t.Fatalf("候选一版本号 = %d，期望 2", publishedFirst.VersionNo)
	}

	second := us21Propose(t, h, us21WriteSource(t, h, us21V3Description, us21V3Body), sourcePath)
	us21Evaluate(t, h, second.CandidateID, "not_evaluable")
	us21Approve(t, h, second.CandidateID, now.Add(time.Hour))
	publishedSecond := us21Publish(t, h, second.CandidateID)
	if publishedSecond.VersionNo != 3 {
		t.Fatalf("候选二版本号 = %d，期望 3", publishedSecond.VersionNo)
	}

	withdrawn := us21Withdraw(t, h, second.CandidateID, "US21 e2e：候选二回退")
	if withdrawn.WithdrawReason != "restored_previous" {
		t.Fatalf("撤回分类 = %q，期望 restored_previous", withdrawn.WithdrawReason)
	}
	if withdrawn.VersionNo != publishedSecond.VersionNo {
		t.Fatalf("撤回记录版本号 = %d，期望被撤回的 %d", withdrawn.VersionNo, publishedSecond.VersionNo)
	}

	db := h.openDB(t)
	if got := us21VersionCount(t, h, db); got != 4 {
		t.Fatalf("版本行 = %d，期望 4（v1 基线、v2 候选一、v3 候选二、v4 恢复）", got)
	}
	var restoredNo int64
	if err := db.QueryRow(`SELECT version_no FROM resource_versions WHERE id = $1`,
		withdrawn.WithdrawVersionID).Scan(&restoredNo); err != nil {
		t.Fatalf("读恢复版本行失败: %v", err)
	}
	if restoredNo != 4 {
		t.Fatalf("恢复版本号 = %d，期望 4", restoredNo)
	}

	workCopy, err := os.ReadFile(filepath.Join(h.Root, ".basalt", "skills", us21SkillName, "SKILL.md"))
	if err != nil {
		t.Fatalf("读撤回后的工作副本失败: %v", err)
	}
	requireContains(t, string(workCopy), us21V2Description, "撤回恢复到候选一的内容")
	requireAbsent(t, string(workCopy), us21V3Description, "撤回后工作副本不得保留候选二的内容")

	if got := us21Show(t, h, first.CandidateID).Candidate.State; got != "published" {
		t.Fatalf("候选一状态 = %q，期望仍是 published（撤回只影响候选二）", got)
	}
	secondDetail := us21Show(t, h, second.CandidateID)
	if secondDetail.Candidate.State != "withdrawn" {
		t.Fatalf("候选二状态 = %q，期望 withdrawn", secondDetail.Candidate.State)
	}
	if secondDetail.Publication == nil || secondDetail.Publication.State != "withdrawn" {
		t.Fatalf("候选二的发布记录应为 withdrawn：%+v", secondDetail.Publication)
	}
}

// TestUS21_E2E_RecoveryAfterCrash 伪造一条"发布预写了 pending 但进程没走完"的操作行，
// 重命令启动必须把它查证为 not_applied → failed，并且**不重放**：没有发布记录、
// 没有新版本、候选保持 approved、修订不变。
// TestUS21_E2E_RecoveryAfterCrash proves startup recovery verifies without replaying.
func TestUS21_E2E_RecoveryAfterCrash(t *testing.T) {
	now := time.Now().UTC()
	h := us21Setup(t)
	fake := newFakeModel(t)
	us21Bootstrap(t, h, fake.URL())
	h.writeSkill(t, us21SkillName,
		"name: "+us21SkillName+"\ndescription: "+us21V1Description+"\n", us21V1Body)
	h.runBaselineWarmup(t, fixtureEnv())

	sourcePath := us21WriteSourceFile(t, h)
	candidate := us21Propose(t, h, us21WriteSource(t, h, us21V2Description, us21V2Body), sourcePath)
	us21Evaluate(t, h, candidate.CandidateID, "not_evaluable")
	us21Approve(t, h, candidate.CandidateID, now.Add(time.Hour))

	db := h.openDB(t)
	operationID := fmt.Sprintf("op-%d-us21e2e", time.Now().UnixMilli())
	if _, err := db.Exec(
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, detail_json, created_at)
		 VALUES ($1, 'skill.admission.publish', 'skill_admission', $2, 'cli:e2e-fixture', 'cli',
		         'cli', 'pending', '{}'::jsonb, $3)`,
		operationID, candidate.CandidateID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("插入 pending 发布操作失败: %v", err)
	}

	// 修订号基线取在批准之后：提议 / 评估 / 批准各自推进修订属正常流程，
	// 这里要判的是"恢复路径不再推进"。
	// The revision baseline is taken after approval; recovery must not advance it.
	before := us21Show(t, h, candidate.CandidateID)
	if before.Candidate.State != "approved" {
		t.Fatalf("恢复前候选状态 = %q，期望 approved", before.Candidate.State)
	}

	res := h.run(t, fixtureEnv(), "tool", "list")
	requireExit(t, res, 0, "恢复后重命令启动")

	var status, verification sql.NullString
	var recoveredAt sql.NullInt64
	if err := db.QueryRow(
		`SELECT status, verification, recovered_at FROM management_operations WHERE operation_id = $1`,
		operationID).Scan(&status, &verification, &recoveredAt); err != nil {
		t.Fatalf("读恢复后的操作行失败: %v", err)
	}
	if status.String != "failed" {
		t.Fatalf("恢复后操作状态 = %q，期望 failed（not_applied → failed）", status.String)
	}
	if verification.String != "not_applied" {
		t.Fatalf("恢复查证 = %q，期望 not_applied", verification.String)
	}
	if !recoveredAt.Valid {
		t.Fatal("恢复后必须记录 recovered_at")
	}

	if got := scalarInt(t, db,
		`SELECT count(*) FROM skill_admission_publications WHERE candidate_id = $1`,
		candidate.CandidateID); got != 0 {
		t.Fatalf("恢复路径不得重放发布，实际写了 %d 条发布记录", got)
	}
	if got := us21VersionCount(t, h, db); got != 1 {
		t.Fatalf("恢复路径不得建新版本，版本行 = %d，期望 1", got)
	}
	detail := us21Show(t, h, candidate.CandidateID)
	if detail.Candidate.State != "approved" {
		t.Fatalf("候选状态 = %q，期望 approved", detail.Candidate.State)
	}
	if detail.Candidate.Revision != before.Candidate.Revision {
		t.Fatalf("候选修订 = %d，期望未被恢复路径改动 %d",
			detail.Candidate.Revision, before.Candidate.Revision)
	}
	if detail.Publication != nil {
		t.Fatalf("恢复路径不得写发布记录：%+v", detail.Publication)
	}
	if detail.Evaluation == nil || detail.Decision == nil {
		t.Fatal("恢复不应影响已落库的评估与决策行")
	}
}

// TestUS21_E2E_ConcurrentPublish 让两个进程同时发布同一候选：实例锁 + 状态 CAS 保证
// 恰好一个赢家，版本行只多一条，发布记录只有一条。
// TestUS21_E2E_ConcurrentPublish proves exactly one publisher wins.
func TestUS21_E2E_ConcurrentPublish(t *testing.T) {
	now := time.Now().UTC()
	h := us21Setup(t)
	fake := newFakeModel(t)
	us21Bootstrap(t, h, fake.URL())
	h.writeSkill(t, us21SkillName,
		"name: "+us21SkillName+"\ndescription: "+us21V1Description+"\n", us21V1Body)
	h.runBaselineWarmup(t, fixtureEnv())

	sourcePath := us21WriteSourceFile(t, h)
	candidate := us21Propose(t, h, us21WriteSource(t, h, us21V2Description, us21V2Body), sourcePath)
	us21Evaluate(t, h, candidate.CandidateID, "not_evaluable")
	us21Approve(t, h, candidate.CandidateID, now.Add(time.Hour))

	first := h.start(t, fixtureEnv(), "admission", "publish", candidate.CandidateID, "--json")
	second := h.start(t, fixtureEnv(), "admission", "publish", candidate.CandidateID, "--json")
	codeFirst := first.waitExit(t, 90*time.Second)
	codeSecond := second.waitExit(t, 90*time.Second)

	successes := 0
	for _, code := range []int{codeFirst, codeSecond} {
		if code == 0 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("并发发布应恰好一个赢家，实际成功 %d 个\n进程一:\n%s\n进程二:\n%s",
			successes, first.output(), second.output())
	}

	db := h.openDB(t)
	if got := scalarInt(t, db,
		`SELECT count(*) FROM skill_admission_publications WHERE candidate_id = $1`,
		candidate.CandidateID); got != 1 {
		t.Fatalf("发布记录 = %d，期望 1", got)
	}
	if got := us21VersionCount(t, h, db); got != 2 {
		t.Fatalf("版本行 = %d，期望 2（只多出一条候选版本）", got)
	}
	if got := us21Show(t, h, candidate.CandidateID).Candidate.State; got != "published" {
		t.Fatalf("并发发布后候选状态 = %q，期望 published", got)
	}
}

// TestUS21_E2E_ReadOnlyConsole 验证只读管理面：列表 / 详情 / 操作查询可用且不出正文，
// 写方法一律不可达且零写入，管理台静态壳与打包产物可读。
// TestUS21_E2E_ReadOnlyConsole covers the read-only admin surface end to end.
func TestUS21_E2E_ReadOnlyConsole(t *testing.T) {
	h := us21Setup(t)
	fake := newFakeModel(t)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.ensureStore(t)
	h.runBaselineWarmup(t, fixtureEnv())

	sourcePath := us21WriteSourceFile(t, h)
	candidate := us21Propose(t, h, us21WriteSource(t, h, us21V2Description, us21V2Body), sourcePath)
	// 本用例不落 v1 工作副本，因此 regression 按"无回归面"记 passed。
	us21Evaluate(t, h, candidate.CandidateID, "passed")

	startServe(t, h, addr)
	db := h.openDB(t)

	resp := webRequest(t, addr, http.MethodGet, "/api/v1/admissions", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "准入列表")
	var list []us21AdmissionView
	decodeWebData(t, resp, &list, "准入列表")
	if len(list) != 1 {
		t.Fatalf("准入列表应有 1 条，实际 %d 条（%s）", len(list), resp.Raw)
	}
	if list[0].CandidateID != candidate.CandidateID || list[0].State != "evaluated" {
		t.Fatalf("列表行与候选不一致：%+v", list[0])
	}
	if !strings.HasPrefix(list[0].ProposerSubject, "cli:") {
		t.Fatalf("列表提出的主体 = %q，期望 cli:<本机用户>", list[0].ProposerSubject)
	}
	requireAbsent(t, resp.Raw, us21V2Body, "列表不得返回候选文件正文")

	resp = webRequest(t, addr, http.MethodGet, "/api/v1/admissions/"+candidate.CandidateID, "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "准入详情")
	var detail us21AdmissionDetailView
	decodeWebData(t, resp, &detail, "准入详情")
	if detail.Source.Kind != "manual" {
		t.Fatalf("详情来源类型 = %q，期望 manual", detail.Source.Kind)
	}
	if len(detail.Files) != 2 {
		t.Fatalf("详情清单应有 2 个文件，实际 %d 个：%+v", len(detail.Files), detail.Files)
	}
	if detail.Evaluation == nil || detail.Evaluation.Aggregate != "incomplete" {
		t.Fatalf("详情评估投影不对：%+v", detail.Evaluation)
	}
	if detail.Publication != nil {
		t.Fatalf("尚未发布时详情发布记录必须为 null：%+v", detail.Publication)
	}
	requireAbsent(t, resp.Raw, us21V2Body, "详情不得返回候选文件正文")
	requireAbsent(t, resp.Raw, "参考笔记", "详情不得返回候选参考文件正文")

	resp = webRequest(t, addr, http.MethodGet, "/api/v1/operations?target_kind=skill_admission", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "操作查询")
	var operations []us21OperationView
	decodeWebData(t, resp, &operations, "操作查询")
	actions := make(map[string]bool, len(operations))
	for _, operation := range operations {
		if operation.TargetKind != "skill_admission" {
			t.Fatalf("操作查询返回了别的目标类型：%+v", operation)
		}
		actions[operation.Action] = true
	}
	if !actions["skill.admission.propose"] || !actions["skill.admission.evaluate"] {
		t.Fatalf("操作查询缺少提议 / 评估行：%v", actions)
	}

	// 写方法一律不可达；全部探针前后零写入（候选数、操作数都不变）。
	// Every write probe is refused and leaves both counters untouched.
	candidatesBefore := scalarInt(t, db, `SELECT count(*) FROM skill_admission_candidates`)
	operationsBefore := scalarInt(t, db, `SELECT count(*) FROM management_operations`)
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		for _, path := range []string{
			"/api/v1/admissions",
			"/api/v1/admissions/" + candidate.CandidateID,
		} {
			probe := webRequest(t, addr, method, path, "{}", nil)
			if probe.Status >= 200 && probe.Status < 300 {
				t.Fatalf("%s %s 不应成功，实际 HTTP %d", method, path, probe.Status)
			}
		}
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM skill_admission_candidates`); got != candidatesBefore {
		t.Fatalf("写探针改动了候选行：%d → %d", candidatesBefore, got)
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM management_operations`); got != operationsBefore {
		t.Fatalf("写探针写入了操作行：%d → %d", operationsBefore, got)
	}

	// 管理台静态壳 + 打包产物 + SPA 回退：页面可读且含准入页面文案。
	// The console shell, its bundle and the SPA fallback all stay readable.
	shell := webRequest(t, addr, http.MethodGet, "/console/", "", nil)
	if shell.Status != http.StatusOK {
		t.Fatalf("管理台入口 HTTP = %d，期望 200", shell.Status)
	}
	asset := regexp.MustCompile(`/console/assets/index-[A-Za-z0-9_-]+\.js`).FindString(shell.Raw)
	if asset == "" {
		t.Fatalf("管理台入口未引用打包产物：%s", shell.Raw[:min(len(shell.Raw), 400)])
	}
	bundle := webRequest(t, addr, http.MethodGet, asset, "", nil)
	if bundle.Status != http.StatusOK {
		t.Fatalf("打包产物 %s HTTP = %d，期望 200", asset, bundle.Status)
	}
	requireContains(t, bundle.Raw, "学习准入", "管理台打包产物含准入页面")

	deepLink := webRequest(t, addr, http.MethodGet, "/console/admissions", "", nil)
	if deepLink.Status != http.StatusOK {
		t.Fatalf("SPA 回退 HTTP = %d，期望 200", deepLink.Status)
	}
	requireContains(t, deepLink.Raw, "/console/assets/", "SPA 回退返回同一入口")
}
