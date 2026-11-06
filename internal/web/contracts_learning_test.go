package web

// 本文件是 US-22「受控自我进化」只读学习查询面的契约对账（颗粒度文档 §3.1.10 /
// FR-22-11，任务 T026）：五条 GET 路径与路由表双向对账、状态字段集逐字等于
// §3.1.10 的 13 个名字、状态 / 失败分类 / 候选类型枚举等于 Go 字面量、真实投影
// 整对象过契约 schema，以及动作字面量闭集**不得**新增 `learning.*`。
// Contract reconciliation for the read-only learning surface: path/route two-way
// check, field-set and enum equality with Go literals, real projections validated
// against the compiled schemas, and no new `learning.*` action literals.

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// learningContractPaths 是学习面契约里的五条相对路径（与路由表 `/api/v1` 前缀对齐）。
// learningContractPaths lists the five relative paths of the learning surface.
var learningContractPaths = []string{
	"/learning/status",
	"/learning/extractions",
	"/learning/extractions/{id}",
	"/learning/drafts",
	"/learning/drafts/{id}",
}

// validLearningExtractionState 与领域状态闭集逐字同源，供枚举比较使用。
// validLearningExtractionState mirrors the domain state set for enum comparison.
func validLearningExtractionState(value string) bool {
	switch core.LearningExtractionState(value) {
	case core.LearningExtractionPending, core.LearningExtractionSuccess,
		core.LearningExtractionFailed, core.LearningExtractionSkipped,
		core.LearningExtractionUnknown:
		return true
	default:
		return false
	}
}

// validLearningFailureClass 与领域失败分类闭集逐字同源。
// validLearningFailureClass mirrors the domain failure-class set.
func validLearningFailureClass(value string) bool {
	switch core.LearningFailureClass(value) {
	case core.LearningFailureModel, core.LearningFailureInvalidOutput,
		core.LearningFailureTimeout, core.LearningFailureAudit,
		core.LearningFailureUnavailable, core.LearningFailureWindowGone:
		return true
	default:
		return false
	}
}

// TestUS22_OpenAPIContract 钉住只读学习面的契约（颗粒度文档 §3.1.10）：
//   - 五条 GET 路径与路由表双向对账；任何写方法既没有契约路径，也没有注册行；
//   - 状态 schema 的字段集逐字等于 §3.1.10 的 13 个名字（视图与契约两侧都比）；
//   - `state` 查询枚举与 `LearningExtractionView.status` 同集，等于五个状态字面量；
//   - `failure_class` 枚举等于六个失败分类；`kind` / `state` 与候选记忆视图同集；
//   - `limit` 声明缺省 50、1–200 上下界（与 handler 的校验逐字一致）；
//   - 真实投影（状态、列表、详情、草案列表与详情）整对象过契约 schema。
//
// 逆改点：漏写路径 / 契约多一个写方法 / 状态少一个字段 / 枚举少一个字面量 /
// limit 上下界漂移 / 视图多投影一个字段 → 对应断言红。
// 中文用例名：受控自我进化只读学习面的 openapi 契约对账。
func TestUS22_OpenAPIContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("路径与路由表双向对账", func(t *testing.T) {
		paths, ok := mustDig(t, doc, "paths").(map[string]any)
		require.True(t, ok, "paths 不是映射")
		for _, rel := range learningContractPaths {
			item, ok := paths[rel].(map[string]any)
			require.Truef(t, ok, "openapi.yaml 缺少学习路径 %s", rel)
			_, hasGet := item["get"]
			require.Truef(t, hasGet, "%s 必须声明 GET", rel)
			// 读面只允许 GET：路径项上的其他键只能是元数据，任何第二个方法都红。
			for key := range item {
				switch key {
				case "get", "parameters", "summary", "description", "servers", "$ref":
				default:
					require.Failf(t, "学习路径声明了多余方法", "%s 只允许 GET，发现 %q", rel, key)
				}
			}
		}

		registered := map[string]bool{}
		for _, rt := range newTestServer(t, nil).routeTable() {
			if !strings.HasPrefix(rt.pattern, "/api/v1/learning") {
				continue
			}
			rel := strings.TrimPrefix(rt.pattern, "/api/v1")
			item, ok := paths[rel].(map[string]any)
			require.Truef(t, ok, "路由 %s %s 在契约里没有对应路径", rt.method, rt.pattern)
			_, hasMethod := item[strings.ToLower(rt.method)]
			require.Truef(t, hasMethod, "路由 %s %s 在契约里没有对应方法", rt.method, rt.pattern)
			registered[rt.method+" "+rel] = true
		}
		require.Equal(t, map[string]bool{
			"GET /learning/status":           true,
			"GET /learning/extractions":      true,
			"GET /learning/extractions/{id}": true,
			"GET /learning/drafts":           true,
			"GET /learning/drafts/{id}":      true,
		}, registered, "路由表的学习行必须恰好是契约里的五条 GET")
	})

	t.Run("状态字段集等于 §3.1.10 的 13 个名字", func(t *testing.T) {
		expected := map[string]bool{
			"enabled": true, "agents": true, "unknown_agents": true,
			"rule_version": true, "prompt_version": true,
			"attempts_used": true, "attempt_limit": true, "exhausted": true,
			"budget_window_seconds": true, "pending_extractions": true,
			"unknown_extractions": true, "orphan_drafts": true,
			"last_extraction_at": true,
		}
		require.Equal(t, expected, contractPropertyNames(t, doc, "LearningStatusView"),
			"契约的 LearningStatusView 必须逐字等于 §3.1.10 的 13 个字段")
		require.Equal(t, expected, viewKeys(t, LearningStatusView{}),
			"handler 投影必须与契约字段集完全一致")
	})

	t.Run("枚举等于 Go 字面量", func(t *testing.T) {
		fiveStates := []string{
			string(core.LearningExtractionPending), string(core.LearningExtractionSuccess),
			string(core.LearningExtractionFailed), string(core.LearningExtractionSkipped),
			string(core.LearningExtractionUnknown), "done", "",
		}
		expectedStates := acceptedBy(fiveStates, validLearningExtractionState)
		require.Equal(t, expectedStates,
			contractEnum(t, doc, "LearningExtractionView", "status"),
			"LearningExtractionView.status 必须逐字等于五个状态字面量")

		params, ok := mustDig(t, doc, "paths", "/learning/extractions", "get", "parameters").([]any)
		require.True(t, ok, "/learning/extractions 的 parameters 不是数组")
		queryStates := []string{}
		for _, raw := range params {
			entry, ok := raw.(map[string]any)
			require.True(t, ok, "参数项不是映射")
			if entry["name"] != "state" || entry["in"] != "query" {
				continue
			}
			schema, ok := entry["schema"].(map[string]any)
			require.True(t, ok, "state 的 schema 不是映射")
			values, ok := schema["enum"].([]any)
			require.True(t, ok, "state 的 enum 不是数组")
			for _, value := range values {
				text, ok := value.(string)
				require.True(t, ok, "state 枚举值不是字符串")
				queryStates = append(queryStates, text)
			}
		}
		require.ElementsMatch(t, expectedStates, queryStates,
			"/learning/extractions 的 state 查询枚举必须与五个状态字面量同集")

		classes := []string{
			string(core.LearningFailureModel), string(core.LearningFailureInvalidOutput),
			string(core.LearningFailureTimeout), string(core.LearningFailureAudit),
			string(core.LearningFailureUnavailable), string(core.LearningFailureWindowGone),
			"other", "",
		}
		// 非失败行的 failure_class 是空串，契约枚举因此是六个分类 + 空串。
		require.ElementsMatch(t,
			append(acceptedBy(classes, validLearningFailureClass), ""),
			contractEnum(t, doc, "LearningExtractionView", "failure_class"),
			"failure_class 必须恰为六个失败分类加空串（pending / success / skipped 行没有分类）")

		require.Equal(t, contractEnum(t, doc, "MemoryEntryView", "state"),
			contractEnum(t, doc, "LearningCandidateView", "state"),
			"候选 state 必须与记忆条目视图的六态同集")
		require.Equal(t, contractEnum(t, doc, "MemoryCandidateView", "kind"),
			contractEnum(t, doc, "LearningCandidateView", "kind"),
			"候选 kind 必须与既有记忆候选视图同集（method 只入草案，仍是同一领域闭集）")
	})

	t.Run("limit 声明缺省与上下界", func(t *testing.T) {
		params, ok := mustDig(t, doc, "paths", "/learning/drafts", "get", "parameters").([]any)
		require.True(t, ok, "/learning/drafts 的 parameters 不是数组")
		found := false
		for _, raw := range params {
			entry, ok := raw.(map[string]any)
			require.True(t, ok, "参数项不是映射")
			if entry["name"] != "limit" || entry["in"] != "query" {
				continue
			}
			found = true
			schema, ok := entry["schema"].(map[string]any)
			require.True(t, ok, "limit 的 schema 不是映射")
			require.EqualValues(t, defaultLearningListLimit, schema["default"])
			require.EqualValues(t, 1, schema["minimum"])
			require.EqualValues(t, maxLearningListLimit, schema["maximum"])
		}
		require.True(t, found, "/learning/drafts 必须声明 limit 查询参数")
	})

	t.Run("真实投影过契约 schema", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.statusFn = func(context.Context) (core.LearningStatus, error) {
			return learningTestStatus(), nil
		}
		fake.listExtractionsFn = func(context.Context,
			core.LearningExtractionFilter,
		) ([]core.LearningExtractionView, error) {
			return []core.LearningExtractionView{learningTestExtractionView()}, nil
		}
		fake.loadExtractionFn = func(context.Context, string) (core.LearningExtractionView, error) {
			return learningTestExtractionView(), nil
		}
		fake.listDraftsFn = func(context.Context,
			core.LearningDraftFilter,
		) ([]core.LearningDraftView, error) {
			return []core.LearningDraftView{learningTestExtractionView().Drafts[0]}, nil
		}
		fake.loadDraftFn = func(context.Context, string) (core.LearningDraftView, error) {
			return learningTestExtractionView().Drafts[0], nil
		}

		statusRec := learningCall(t, s, http.MethodGet, "/api/v1/learning/status")
		require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())
		requireViewMatchesSchema(t, doc, "LearningStatusView",
			decodeRunEnvelope[LearningStatusView](t, statusRec).Data)

		listRec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions")
		require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
		requireViewMatchesSchema(t, doc, "LearningExtractionListView",
			decodeRunEnvelope[LearningExtractionListView](t, listRec).Data)

		detailRec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions/lrn_20261105a")
		require.Equal(t, http.StatusOK, detailRec.Code, detailRec.Body.String())
		detail := decodeRunEnvelope[LearningExtractionDetailView](t, detailRec).Data
		requireViewMatchesSchema(t, doc, "LearningExtractionDetailView", detail)
		requireViewMatchesSchema(t, doc, "LearningExtractionView", detail.Extraction)
		require.Len(t, detail.Candidates, 1)
		requireViewMatchesSchema(t, doc, "LearningCandidateView", detail.Candidates[0])
		require.Len(t, detail.Candidates[0].Evidence, 1)
		requireViewMatchesSchema(t, doc, "LearningEvidenceView", detail.Candidates[0].Evidence[0])

		draftListRec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts")
		require.Equal(t, http.StatusOK, draftListRec.Code, draftListRec.Body.String())
		draftList := decodeRunEnvelope[LearningDraftListView](t, draftListRec).Data
		requireViewMatchesSchema(t, doc, "LearningDraftListView", draftList)
		require.Len(t, draftList.Drafts, 1)
		requireViewMatchesSchema(t, doc, "LearningDraftMetaView", draftList.Drafts[0])

		draftRec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts/draft_20261105a")
		require.Equal(t, http.StatusOK, draftRec.Code, draftRec.Body.String())
		draft := decodeRunEnvelope[LearningDraftDetailView](t, draftRec).Data
		requireViewMatchesSchema(t, doc, "LearningDraftDetailView", draft)
		require.Len(t, draft.Evidence, 1)
		requireViewMatchesSchema(t, doc, "LearningEvidenceView", draft.Evidence[0])
	})
}

// TestUS22_Contracts_NoNewActionLiterals 钉住审计动作字面量闭集（颗粒度文档 §3.1.8 /
// FR-22-11 的依赖边界）：`docs/contracts/run-and-errors.md` 的 `action` 字面量行里，
// `memory.*` 恰为既有的四个动作、`skill.admission.propose` 仍在，且**不得**出现任何
// `learning.*` 动作——抽取与草案不是管理面变更动作，确认 / 删除复用记忆动作。
// 逆改点：给抽取或晋升新增 `learning.*` 动作字面量 / 悄悄删掉既有 memory 动作 →
// 对应断言红。
// 中文用例名：受控自我进化不得新增动作字面量。
func TestUS22_Contracts_NoNewActionLiterals(t *testing.T) {
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile("run-and-errors.md")
	require.NoError(t, err)

	actionLine := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "`action` 字面量") {
			actionLine = line
			break
		}
	}
	require.NotEmpty(t, actionLine, "run-and-errors.md 缺少 `action` 字面量行")

	actionPattern := regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$`)
	literals := []string{}
	for i, token := range strings.Split(actionLine, "`") {
		if i%2 == 0 || !actionPattern.MatchString(token) {
			continue
		}
		literals = append(literals, token)
	}
	require.NotEmpty(t, literals, "动作字面量行没有解析出任何字面量")

	memoryActions := []string{}
	learningActions := []string{}
	for _, literal := range literals {
		switch {
		case strings.HasPrefix(literal, "memory."):
			memoryActions = append(memoryActions, literal)
		case strings.HasPrefix(literal, "learning."):
			learningActions = append(learningActions, literal)
		}
	}
	require.ElementsMatch(t,
		[]string{"memory.propose", "memory.confirm", "memory.correct", "memory.delete"},
		memoryActions, "memory.* 必须恰为既有四个动作；抽取不是管理面变更动作")
	require.Empty(t, learningActions,
		"不得新增 learning.* 动作字面量：确认 / 删除复用记忆管理动作")
	require.Contains(t, literals, "skill.admission.propose",
		"草案 handoff 只调既有技能准入提议入口，其动作字面量必须仍在")
}
