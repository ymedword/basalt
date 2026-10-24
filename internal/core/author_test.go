package core_test

// 本文件是作者模型领域面的验收测试（颗粒度文档 §3.2.1：`internal/core/author_test.go`，
// 六例：提示词稳定、草稿解析、非法路径；R6 追加问答分支、答案上限与定时候选）。
// 命名族固定为 `TestAuthor_*`（§4.4）。这里只判纯函数行为：不发网络请求、不落盘、
// 不注册——调用与审计归 internal/app。
// This file covers the author domain surface: prompt stability, draft parsing, path rules
// and the R6 Q&A / schedule branches. Test names follow module card §4.4 (TestAuthor_*),
// and nothing here touches the network or the filesystem.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestAuthor_PromptStable：同一入参两次构建逐字节相同，且名称列表按传入顺序出现。
// TestAuthor_PromptStable pins byte-for-byte stability and slice-order rendering.
func TestAuthor_PromptStable(t *testing.T) {
	t.Parallel()

	refs := core.AuthorReferences{
		Providers: []string{"openai-main", "backup-provider"},
		Skills:    []string{"writer", "reporter"},
		MCP:       []string{"search", "fetch"},
		Template:  "---\nname: demo\n---\n正文\n",
	}

	first := core.BuildAuthorPrompt("demo", "整理每日运维日报", refs)
	second := core.BuildAuthorPrompt("demo", "整理每日运维日报", refs)

	require.Equal(t, first, second)
	require.Contains(t, first, "demo")
	require.Contains(t, first, "整理每日运维日报")
	for _, name := range []string{"openai-main", "backup-provider", "writer", "reporter", "search", "fetch"} {
		require.Contains(t, first, name)
	}
	require.Contains(t, first, refs.Template)
	require.Less(t, strings.Index(first, "openai-main"), strings.Index(first, "backup-provider"))
	require.Less(t, strings.Index(first, "writer"), strings.Index(first, "reporter"))
	require.Less(t, strings.Index(first, "search"), strings.Index(first, "fetch"))
}

// TestAuthor_PromptMarksEmptyReferences：空列表写可见占位符，空模板不渲染模板段。
// TestAuthor_PromptMarksEmptyReferences renders a visible placeholder for empty lists
// and omits the template section when no template is supplied.
func TestAuthor_PromptMarksEmptyReferences(t *testing.T) {
	t.Parallel()

	prompt := core.BuildAuthorPrompt("demo", "描述", core.AuthorReferences{})

	require.Equal(t, 3, strings.Count(prompt, "（无）"))
	require.NotContains(t, prompt, "\nAGENT.md 模板：")
}

// TestAuthor_DraftParsesFiles：合法数组按模型顺序返回，反斜杠统一为 `/`；
// 外层代码围栏只剥一层，不改变接受的形态。
// TestAuthor_DraftParsesFiles preserves file order and normalizes separators; a single
// surrounding markdown fence is stripped without widening the accepted shape.
func TestAuthor_DraftParsesFiles(t *testing.T) {
	t.Parallel()

	raw := `[{"path":"AGENT.md","content":"你是 demo。\n"},{"path":"skills\\note.md","content":"# 记录规范\n"}]`
	files, err := core.ParseAuthorDraft(raw)
	require.NoError(t, err)
	require.Equal(t, []core.AuthorFile{
		{Path: "AGENT.md", Content: "你是 demo。\n"},
		{Path: "skills/note.md", Content: "# 记录规范\n"},
	}, files)

	fenced := "```json\n" + raw + "\n```"
	fencedFiles, err := core.ParseAuthorDraft(fenced)
	require.NoError(t, err)
	require.Equal(t, files, fencedFiles)
}

// TestAuthor_DraftRejectsIllegalPaths：绝对路径（含盘符）、越界段、空段、控制字符与
// `.staging` / `.backup` 后缀一律拒绝，且错误点名路径。
// TestAuthor_DraftRejectsIllegalPaths refuses absolute paths, traversal, empty segments,
// control characters and staging/backup suffixes, naming the offending path.
func TestAuthor_DraftRejectsIllegalPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{name: "absolute-unix", path: "/etc/passwd"},
		{name: "absolute-drive", path: `C:\windows\system32`},
		{name: "drive-relative", path: "C:notes.md"},
		{name: "traversal", path: "../escape.md"},
		{name: "inner-traversal", path: "skills/../../escape.md"},
		{name: "empty-segment", path: "skills//note.md"},
		{name: "dot-segment", path: "./note.md"},
		{name: "control-char", path: "bad\x01name.md"},
		{name: "staging-suffix", path: "demo.staging"},
		{name: "backup-suffix", path: "skills/BACKUP.backup"},
		{name: "empty-path", path: ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal([]map[string]string{
				{"path": "AGENT.md", "content": "ok"},
				{"path": tc.path, "content": "x"},
			})
			require.NoError(t, err)

			_, err = core.ParseAuthorDraft(string(raw))
			require.Error(t, err)
			require.Contains(t, err.Error(), "路径")
		})
	}
}

// TestAuthor_DraftRequiresAgentMD：缺 AGENT.md、空 AGENT.md 与纯空白 AGENT.md 都拒绝。
// TestAuthor_DraftRequiresAgentMD refuses a missing, empty or whitespace-only AGENT.md.
func TestAuthor_DraftRequiresAgentMD(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{name: "missing", raw: `[{"path":"README.md","content":"x"}]`},
		{name: "empty", raw: `[{"path":"AGENT.md","content":""}]`},
		{name: "blank", raw: `[{"path":"AGENT.md","content":"  \n\t"}]`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.ParseAuthorDraft(tc.raw)
			require.Error(t, err)
		})
	}
}

// TestAuthor_DraftRejectsMalformedInput：非数组、非对象项、缺字段、大小写重复、
// 非 UTF-8、超量与超限都拒绝。
// TestAuthor_DraftRejectsMalformedInput refuses non-arrays, non-object items, missing
// fields, case-insensitive duplicates, invalid UTF-8 and oversized drafts.
func TestAuthor_DraftRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", 256<<10+1)
	var manyFiles strings.Builder
	manyFiles.WriteString("[")
	for i := 0; i < 65; i++ {
		if i > 0 {
			manyFiles.WriteString(",")
		}
		fmt.Fprintf(&manyFiles, `{"path":"f%d.md","content":"x"}`, i)
	}
	manyFiles.WriteString("]")

	cases := []struct {
		name string
		raw  string
	}{
		{name: "not-json", raw: "不是 JSON"},
		{name: "top-level-object", raw: `{"path":"AGENT.md","content":"x"}`},
		{name: "top-level-null", raw: `null`},
		{name: "item-not-object", raw: `["AGENT.md"]`},
		{name: "missing-content", raw: `[{"path":"AGENT.md"}]`},
		{name: "non-string-content", raw: `[{"path":"AGENT.md","content":1}]`},
		{name: "case-duplicate", raw: `[{"path":"AGENT.md","content":"x"},{"path":"agent.md","content":"y"}]`},
		{name: "invalid-utf8", raw: "[{\"path\":\"AGENT.md\",\"content\":\"\xff\"}]"},
		{name: "too-many-files", raw: manyFiles.String()},
		{name: "file-too-large", raw: fmt.Sprintf(`[{"path":"AGENT.md","content":"ok"},{"path":"big.txt","content":%q}]`, huge)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.ParseAuthorDraft(tc.raw)
			require.Error(t, err)
		})
	}
}

// TestAuthor_PromptWithAnswers：没有答案时与基础提示词逐字节相同；有答案时原样追加
// 「已澄清的问题」段，逐条按传入顺序渲染，要求模型直接输出文件数组。
// TestAuthor_PromptWithAnswers is byte-identical without answers and appends the
// clarified-questions section in slice order otherwise.
func TestAuthor_PromptWithAnswers(t *testing.T) {
	t.Parallel()

	refs := core.AuthorReferences{Providers: []string{"openai-main"}}
	base := core.BuildAuthorPrompt("demo", "整理每日运维日报", refs)
	require.Equal(t, base, core.BuildAuthorPromptWithAnswers("demo", "整理每日运维日报", refs, nil))

	answers := []core.AuthorAnswer{
		{ID: "frequency", Question: "多久运行一次？", Answer: "每天 08:30"},
		{ID: "channel", Question: "结果发到哪里？", Answer: "运维群"},
	}
	withAnswers := core.BuildAuthorPromptWithAnswers("demo", "整理每日运维日报", refs, answers)

	require.True(t, strings.HasPrefix(withAnswers, base), "基础提示词必须原样保留")
	require.Contains(t, withAnswers, "已澄清的问题（信息已足够，请直接输出文件数组）：")
	require.Contains(t, withAnswers, "- [frequency] 多久运行一次？：每天 08:30")
	require.Contains(t, withAnswers, "- [channel] 结果发到哪里？：运维群")
	require.Less(t, strings.Index(withAnswers, "[frequency]"), strings.Index(withAnswers, "[channel]"),
		"答案按传入顺序渲染")
	require.Equal(t, withAnswers,
		core.BuildAuthorPromptWithAnswers("demo", "整理每日运维日报", refs, answers),
		"同一入参必须逐字节稳定")
}

// TestAuthor_NormalizeAnswers：合法答案去首尾空白后返回；空列表合法；超过 3 条、
// 任一字段为空、id 重复、id / question 超过 256 字符、answer 超过 2000 字符都整请求
// 拒绝——不静默丢弃单条。上限按字符（rune）计，不按字节。
// TestAuthor_NormalizeAnswers trims valid answers, accepts none, and refuses the whole
// request on any bad entry; the caps count runes, not bytes.
func TestAuthor_NormalizeAnswers(t *testing.T) {
	t.Parallel()

	got, err := core.NormalizeAuthorAnswers([]core.AuthorAnswer{
		{ID: " frequency ", Question: " 多久运行一次？ ", Answer: " 每天 08:30 "},
	})
	require.NoError(t, err)
	require.Equal(t, []core.AuthorAnswer{
		{ID: "frequency", Question: "多久运行一次？", Answer: "每天 08:30"},
	}, got)

	empty, err := core.NormalizeAuthorAnswers(nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	// 边界：恰好 256 / 256 / 2000 个字符（全用多字节汉字，证明按 rune 计）。
	boundary, err := core.NormalizeAuthorAnswers([]core.AuthorAnswer{{
		ID:       strings.Repeat("测", 256),
		Question: strings.Repeat("问", 256),
		Answer:   strings.Repeat("答", 2000),
	}})
	require.NoError(t, err, "恰好达到上限必须接受")
	require.Len(t, boundary, 1)

	tooMany := []core.AuthorAnswer{
		{ID: "a", Question: "问？", Answer: "答"},
		{ID: "b", Question: "问？", Answer: "答"},
		{ID: "c", Question: "问？", Answer: "答"},
		{ID: "d", Question: "问？", Answer: "答"},
	}

	cases := []struct {
		name    string
		answers []core.AuthorAnswer
		want    string
	}{
		{name: "too-many", answers: tooMany, want: "最多 3 条"},
		{name: "blank-id", answers: []core.AuthorAnswer{{ID: "  ", Question: "问？", Answer: "答"}}, want: "id 不得为空"},
		{name: "blank-question", answers: []core.AuthorAnswer{{ID: "q", Question: " ", Answer: "答"}}, want: "question 不得为空"},
		{name: "blank-answer", answers: []core.AuthorAnswer{{ID: "q", Question: "问？", Answer: "\t"}}, want: "answer 不得为空"},
		{name: "duplicate-id", answers: []core.AuthorAnswer{
			{ID: "q", Question: "问？", Answer: "答"},
			{ID: " q ", Question: "又问？", Answer: "再答"},
		}, want: "id 重复"},
		{name: "id-too-long", answers: []core.AuthorAnswer{{ID: strings.Repeat("测", 257), Question: "问？", Answer: "答"}}, want: "256"},
		{name: "question-too-long", answers: []core.AuthorAnswer{{ID: "q", Question: strings.Repeat("问", 257), Answer: "答"}}, want: "256"},
		{name: "answer-too-long", answers: []core.AuthorAnswer{{ID: "q", Question: "问？", Answer: strings.Repeat("答", 2001)}}, want: "2000"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.NormalizeAuthorAnswers(tc.answers)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestAuthor_DraftParsesQuestions：对象形态 `{"questions":[...]}` 解析为问题列表，
// 项目里的附加字段忽略、id / question 去空白；外层单层围栏照剥。只认文件的
// ParseAuthorDraft 必须拒绝问题，不能把问题当草稿。
// TestAuthor_DraftParsesQuestions parses the questions branch, ignores extra item keys
// and trims values; ParseAuthorDraft refuses questions outright.
func TestAuthor_DraftParsesQuestions(t *testing.T) {
	t.Parallel()

	raw := `{"questions":[` +
		`{"id":" frequency ","question":"每天几点运行？"},` +
		`{"id":"channel","question":" 结果发到哪里？ ","note":"附加字段按契约忽略"}]}`

	result, err := core.ParseAuthorResult(raw)
	require.NoError(t, err)
	require.Empty(t, result.Files)
	require.Equal(t, []core.AuthorQuestion{
		{ID: "frequency", Question: "每天几点运行？"},
		{ID: "channel", Question: "结果发到哪里？"},
	}, result.Questions)

	fenced, err := core.ParseAuthorResult("```json\n" + raw + "\n```")
	require.NoError(t, err)
	require.Equal(t, result.Questions, fenced.Questions)

	files, err := core.ParseAuthorDraft(raw)
	require.Error(t, err)
	require.Contains(t, err.Error(), "澄清问题")
	require.Nil(t, files)
}

// TestAuthor_DraftRejectsMalformedQuestions：对象的顶层只允许 `questions`；空数组、
// 超过 3 个、非对象项、缺字段、空值、id 重复与超过 256 字符都拒绝。
// TestAuthor_DraftRejectsMalformedQuestions refuses unknown top-level keys, empty or
// oversized lists, bad items and duplicate or oversized ids.
func TestAuthor_DraftRejectsMalformedQuestions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{name: "unknown-top-level", raw: `{"questions":[{"id":"q","question":"问？"}],"files":[]}`},
		{name: "missing-questions", raw: `{"answers":[]}`},
		{name: "questions-not-array", raw: `{"questions":{"id":"q"}}`},
		{name: "empty-list", raw: `{"questions":[]}`},
		{name: "too-many", raw: `{"questions":[{"id":"a","question":"问？"},{"id":"b","question":"问？"},{"id":"c","question":"问？"},{"id":"d","question":"问？"}]}`},
		{name: "item-not-object", raw: `{"questions":["frequency"]}`},
		{name: "missing-id", raw: `{"questions":[{"question":"问？"}]}`},
		{name: "missing-question", raw: `{"questions":[{"id":"q"}]}`},
		{name: "non-string-question", raw: `{"questions":[{"id":"q","question":1}]}`},
		{name: "blank-id", raw: `{"questions":[{"id":"  ","question":"问？"}]}`},
		{name: "blank-question", raw: `{"questions":[{"id":"q","question":" "}]}`},
		{name: "duplicate-id", raw: `{"questions":[{"id":"q","question":"问？"},{"id":"q","question":"又问？"}]}`},
		{name: "id-too-long", raw: fmt.Sprintf(`{"questions":[{"id":%q,"question":"问？"}]}`, strings.Repeat("测", 257))},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.ParseAuthorResult(tc.raw)
			require.Error(t, err)
		})
	}
}

// TestAuthor_SchedulePromptStable：定时提示词逐字节稳定，包含 Agent 名、用户指令、
// 公开 id 规则与 6 段 cron / IANA 时区要求；不要求任何凭据。
// TestAuthor_SchedulePromptStable pins the schedule prompt and its fixed rules.
func TestAuthor_SchedulePromptStable(t *testing.T) {
	t.Parallel()

	first := core.BuildScheduleDraftPrompt("ops-agent", "每天 8:30 生成日报")
	second := core.BuildScheduleDraftPrompt("ops-agent", "每天 8:30 生成日报")

	require.Equal(t, first, second)
	require.Contains(t, first, "Agent 名：ops-agent")
	require.Contains(t, first, "用户指令：每天 8:30 生成日报")
	require.Contains(t, first, core.ScheduleDraftIDPattern)
	require.Contains(t, first, "6 段")
	require.Contains(t, first, "Asia/Shanghai")
	require.Contains(t, first, "不得索要 API Key")
}

// TestAuthor_ScheduleCandidateParsing：合法对象四字段去空白返回；外层围栏照剥；
// 非对象、未知字段、缺字段、非字符串与空串输入都拒绝——候选只承载四个字段。
// TestAuthor_ScheduleCandidateParsing accepts exactly the four trimmed fields and
// refuses unknown keys and malformed shapes.
func TestAuthor_ScheduleCandidateParsing(t *testing.T) {
	t.Parallel()

	raw := `{"id":" daily-report ","cron":" 0 30 8 * * * ","zone":" Asia/Shanghai ","message":" 生成每日运维日报 "}`
	candidate, err := core.ParseScheduleCandidate(raw)
	require.NoError(t, err)
	require.Equal(t, core.ScheduleCandidate{
		ID:      "daily-report",
		Cron:    "0 30 8 * * *",
		Zone:    "Asia/Shanghai",
		Message: "生成每日运维日报",
	}, candidate)

	fenced, err := core.ParseScheduleCandidate("```json\n" + raw + "\n```")
	require.NoError(t, err)
	require.Equal(t, candidate, fenced)

	bad := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: "  "},
		{name: "array", raw: `[{"id":"daily"}]`},
		{name: "null", raw: `null`},
		{name: "unknown-key", raw: `{"id":"d","cron":"0 30 8 * * *","zone":"UTC","message":"m","extra":1}`},
		{name: "missing-zone", raw: `{"id":"d","cron":"0 30 8 * * *","message":"m"}`},
		{name: "non-string-cron", raw: `{"id":"d","cron":6,"zone":"UTC","message":"m"}`},
		{name: "invalid-utf8", raw: "{\"id\":\"\xff\"}"},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.ParseScheduleCandidate(tc.raw)
			require.Error(t, err)
		})
	}
}

// TestAuthor_ScheduleInstructionAndID：指令去空白后非空且 ≤ 1000 字符（按 rune 计，
// 不截断）；id 只接受 `^[a-z0-9][a-z0-9-]{0,63}$`。提示词与校验共用同一份正则。
// TestAuthor_ScheduleInstructionAndID pins the instruction cap and the shared id rule.
func TestAuthor_ScheduleInstructionAndID(t *testing.T) {
	t.Parallel()

	trimmed, err := core.NormalizeScheduleInstruction("  每天 8:30 生成日报  ")
	require.NoError(t, err)
	require.Equal(t, "每天 8:30 生成日报", trimmed)

	atCap, err := core.NormalizeScheduleInstruction(strings.Repeat("指", core.ScheduleInstructionMaxLen))
	require.NoError(t, err, "恰好达到上限必须接受")
	require.Len(t, []rune(atCap), core.ScheduleInstructionMaxLen)

	_, err = core.NormalizeScheduleInstruction("   ")
	require.Error(t, err)
	_, err = core.NormalizeScheduleInstruction(strings.Repeat("指", core.ScheduleInstructionMaxLen+1))
	require.Error(t, err)
	require.Contains(t, err.Error(), "1000")

	for _, id := range []string{"daily-report", "a", "0report", "a-" + strings.Repeat("b", 62)} {
		require.NoError(t, core.ValidateScheduleDraftID(id), "id=%q", id)
	}
	for _, id := range []string{"", "Daily", "-daily", "daily_report", "每日", "a" + strings.Repeat("b", 64)} {
		require.Error(t, core.ValidateScheduleDraftID(id), "id=%q", id)
	}
}
