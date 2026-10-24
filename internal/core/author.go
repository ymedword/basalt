package core

// 本文件是作者模型（一句话生成 Agent 文件）的纯领域面：配置、提示词构建与草稿解析。
// 不发起网络调用、不落盘、不注册；模型调用与两阶段审计在 internal/app，请求体上限在
// internal/web。解析只接受模型输出的文件对象数组，绝不把草稿写进工作区。
// This file is the pure domain surface of the author model: config, prompt building and
// draft parsing. It performs no network calls and writes nothing; the app layer owns the
// model call and its audit, and the web layer owns the request-body limit.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 草稿上限（颗粒度文档约束 29）：文件数、单文件字节数与总量。超限一律拒绝，
// 由调用方映射为 400 BAD_REQUEST 并点名问题；这里不截断、不静默丢弃文件。
// Draft limits from module card constraint 29: file count, per-file bytes and total
// bytes. Exceeding any of them is refused outright; nothing is truncated or dropped.
const (
	authorDraftMaxFiles      = 64
	authorDraftMaxFileBytes  = 256 << 10
	authorDraftMaxTotalBytes = 1 << 20

	// 问答补全上限（R6）：问题与答案各 ≤ 3 条，id / 问题 ≤ 256 字符，答案 ≤ 2000 字符。
	// Q&A limits (R6): at most three questions / answers, 256-rune ids and questions,
	// 2000-rune answers. Exceeding any limit is refused, never truncated.
	authorMaxQuestions   = 3
	authorQuestionMaxLen = 256
	authorAnswerMaxLen   = 2000
)

// ScheduleDraftIDPattern 是定时候选 id 的公开规则（R6）：提示词与校验共用同一份
// 字面量，避免模型被要求一种形态、服务端又按另一种判卷。
// ScheduleDraftIDPattern is shared verbatim by the schedule-draft prompt and its check.
const ScheduleDraftIDPattern = `^[a-z0-9][a-z0-9-]{0,63}$`

var scheduleDraftIDPattern = regexp.MustCompile(ScheduleDraftIDPattern)

// AuthorConfig 是 `basalt.yaml` 的 `author:` 段：作者模型调用的 Provider 与模型名。
// 判空与范围校验在 internal/cli（解析）与 internal/app（发布态核对），本类型只承载值。
// AuthorConfig is the author: section of basalt.yaml; validation lives in the cli and
// app layers, this type only carries the values.
type AuthorConfig struct {
	Provider    string
	Model       string
	Temperature float64
}

// AuthorFile 是一份 Agent 文件：路径相对 Agent 目录并规范化为 `/`，内容为 UTF-8 文本。
// 草稿、提交与技能候选共用该类型；路径规整只在解析 / 校验的入口做一次。
// AuthorFile is one agent file: a slash-normalized path relative to the agent directory
// and UTF-8 content. Drafts, submissions and skill candidates share the type.
type AuthorFile struct {
	Path    string // 相对 Agent 目录；规范化为 `/`
	Content string
}

// AuthorReferences 是提示词里可引用的已发布资源集合；Template 是既有 AGENT.md 模板
// 正文，提示词要求"在此结构上改写"，避免模型自由发明 frontmatter 字段。
// AuthorReferences lists the published resources the draft may reference; Template is
// the existing AGENT.md template the model must rewrite within.
type AuthorReferences struct {
	Providers []string
	Skills    []string
	MCP       []string
	Template  string
}

// AuthorQuestion 是作者模型在信息不足时给出的一个澄清问题（R6.3）：`id` 由模型生成，
// 用于把用户的回答对位回去；问题只在页面内存里流转，不落盘、不进审计正文。
// AuthorQuestion is one clarifying question from the author model; its id pairs the
// user's answer back to the question and nothing here is persisted.
type AuthorQuestion struct {
	ID       string
	Question string
}

// AuthorAnswer 是用户对某个问题的回答：只随本次请求进入提示词，不持久化、不写日志。
// AuthorAnswer carries one user answer into the next prompt only.
type AuthorAnswer struct {
	ID       string
	Question string
	Answer   string
}

// AuthorDraftResult 是一次作者调用的结果：文件数组与问题数组互斥（R6.3）——模型要么
// 直接给出文件，要么先提问，不会同时给出两者。
// AuthorDraftResult is either files or questions, never both.
type AuthorDraftResult struct {
	Files     []AuthorFile
	Questions []AuthorQuestion
}

// BuildAuthorPrompt 组装作者模型的单轮提示词：Agent 名、描述、已发布 Provider / 技能 /
// 已应用 MCP 名与 AGENT.md 模板结构，外加固定的输出契约。输出只由入参决定——列表按
// 传入顺序渲染，不遍历任何 map，因此同一入参逐字节稳定（可判卷前提）。
// BuildAuthorPrompt assembles the single-turn author prompt from the agent name,
// description, published resource names and the AGENT.md template. The output depends
// only on its arguments and preserves slice order, so identical inputs render byte-for-byte
// identical prompts.
func BuildAuthorPrompt(name, description string, refs AuthorReferences) string {
	var b strings.Builder
	b.WriteString("你是 Basalt 的 Agent 定义起草助手。根据下列信息，为一个 Agent 生成完整的文件集合。\n\n")
	b.WriteString("Agent 名：")
	b.WriteString(name)
	b.WriteString("\n描述：")
	b.WriteString(description)
	b.WriteString("\n\n已发布的 Provider（按配置顺序）：")
	writeAuthorNameList(&b, refs.Providers)
	b.WriteString("\n已发布的技能：")
	writeAuthorNameList(&b, refs.Skills)
	b.WriteString("\n已应用的 MCP server：")
	writeAuthorNameList(&b, refs.MCP)
	b.WriteString("\n\n输出要求：\n")
	b.WriteString("1. 只输出一个 JSON 数组，元素形如 {\"path\": \"AGENT.md\", \"content\": \"...\"}；不要输出数组之外的任何文字。\n")
	b.WriteString("2. 必须包含 AGENT.md，且内容非空。\n")
	b.WriteString("3. path 必须是相对路径（用 / 分隔），不得以 / 开头，不得包含 ..、反斜杠或控制字符。\n")
	b.WriteString("4. content 必须是 UTF-8 文本；文件数不超过 64 个。\n")
	b.WriteString("5. 只能使用上面列出的资源名；不得编造 Provider、技能或 MCP 名。\n")
	b.WriteString("6. 在下面的 AGENT.md 模板结构上改写，不得自行发明 frontmatter 字段。\n")
	b.WriteString("7. 只有信息确实不足时才先提问：输出 {\"questions\":[{\"id\":\"...\",\"question\":\"...\"}]}，最多 3 个，不得索要 API Key、Webhook 地址或任何凭据；信息足够时直接输出文件数组，不要为了提问而提问。\n")
	if refs.Template != "" {
		b.WriteString("\nAGENT.md 模板：\n")
		b.WriteString(refs.Template)
		if !strings.HasSuffix(refs.Template, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// BuildAuthorPromptWithAnswers 在基础提示词之后追加「已澄清的问题」段（R6.3.4）：有答案
// 时明确写出信息已足够、请直接输出文件数组；没有答案时与 BuildAuthorPrompt 逐字节相同
// （旧调用方的提示词不受影响）。
// BuildAuthorPromptWithAnswers appends the clarified-questions section; with no answers
// the result is byte-identical to BuildAuthorPrompt.
func BuildAuthorPromptWithAnswers(name, description string, refs AuthorReferences, answers []AuthorAnswer) string {
	prompt := BuildAuthorPrompt(name, description, refs)
	if len(answers) == 0 {
		return prompt
	}

	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n已澄清的问题（信息已足够，请直接输出文件数组）：\n")
	for _, answer := range answers {
		b.WriteString("- [")
		b.WriteString(answer.ID)
		b.WriteString("] ")
		b.WriteString(answer.Question)
		b.WriteString("：")
		b.WriteString(answer.Answer)
		b.WriteString("\n")
	}
	return b.String()
}

// NormalizeAuthorAnswers 校验并规整请求里的回答（R6.3.3）：最多 3 条；`id` / `question`
// 去空白后非空且各 ≤ 256 字符；`answer` 去空白后非空且 ≤ 2000 字符；`id` 不重复。
// 任何一条不合法都整请求拒绝（400），不静默丢弃单条——页面必须补齐后才能继续。
// NormalizeAuthorAnswers validates and trims the answers; one bad entry refuses the
// whole request instead of silently dropping it.
func NormalizeAuthorAnswers(answers []AuthorAnswer) ([]AuthorAnswer, error) {
	if len(answers) > authorMaxQuestions {
		return nil, fmt.Errorf("answers 最多 %d 条（当前 %d 条）", authorMaxQuestions, len(answers))
	}

	normalized := make([]AuthorAnswer, 0, len(answers))
	seen := make(map[string]struct{}, len(answers))
	for i, answer := range answers {
		id := strings.TrimSpace(answer.ID)
		question := strings.TrimSpace(answer.Question)
		text := strings.TrimSpace(answer.Answer)
		if id == "" {
			return nil, fmt.Errorf("answers[%d] 的 id 不得为空", i)
		}
		if question == "" {
			return nil, fmt.Errorf("answers[%d] 的 question 不得为空", i)
		}
		if text == "" {
			return nil, fmt.Errorf("answers[%d] 的 answer 不得为空", i)
		}
		if utf8.RuneCountInString(id) > authorQuestionMaxLen ||
			utf8.RuneCountInString(question) > authorQuestionMaxLen {
			return nil, fmt.Errorf("answers[%d] 的 id / question 超过 %d 字符上限", i, authorQuestionMaxLen)
		}
		if utf8.RuneCountInString(text) > authorAnswerMaxLen {
			return nil, fmt.Errorf("answers[%d] 的 answer 超过 %d 字符上限", i, authorAnswerMaxLen)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("answers 的 id 重复：%s", id)
		}
		seen[id] = struct{}{}
		normalized = append(normalized, AuthorAnswer{ID: id, Question: question, Answer: text})
	}
	return normalized, nil
}

// writeAuthorNameList 按传入顺序渲染名称列表；空列表写"（无）"，避免提示词出现
// 悬空的冒号让模型误以为有隐藏候选。
// writeAuthorNameList renders names in slice order, using a visible placeholder for an
// empty list so the prompt never ends on a dangling colon.
func writeAuthorNameList(b *strings.Builder, names []string) {
	if len(names) == 0 {
		b.WriteString("（无）")
		return
	}
	b.WriteString(strings.Join(names, "、"))
}

// ParseAuthorDraft 把作者模型的原始输出解析为文件列表：只接受文件对象数组；模型返回
// 澄清问题时拒绝——只认文件的调用方不得把问题当成草稿。形状与路径规则见
// ParseAuthorResult（约束 41）。
// ParseAuthorDraft parses files only; a questions response is refused because such a
// caller has no channel to show questions.
func ParseAuthorDraft(raw string) ([]AuthorFile, error) {
	result, err := ParseAuthorResult(raw)
	if err != nil {
		return nil, err
	}
	if len(result.Questions) > 0 {
		return nil, errors.New("作者模型返回的是澄清问题，不是文件草稿")
	}
	return result.Files, nil
}

// ParseAuthorResult 把作者模型的原始输出解析为「文件数组」或「问题对象」二选一
// （R6.3）：数组元素是文件对象（每个必须有字符串 `path` / `content`），路径规则同
// 约束 41（相对、逐段合法、无控制字符、不以 `.staging` / `.backup` 结尾、大小写不敏感
// 不重复），正文按 UTF-8 校验且必须含非空 `AGENT.md`；对象形如
// `{"questions":[{"id":"...","question":"..."}]}`，最多 3 个、id 不重复。
// 模型常在外层包一层代码围栏，这里只剥一层围栏，接受的形态不变；文件顺序原样保留。
// ParseAuthorResult accepts exactly one of the two shapes and strips at most one fence.
func ParseAuthorResult(raw string) (AuthorDraftResult, error) {
	if !utf8.ValidString(raw) {
		return AuthorDraftResult{}, errors.New("作者输出不是合法的 UTF-8 文本")
	}
	body := stripJSONFence(strings.TrimSpace(raw))
	if body == "" {
		return AuthorDraftResult{}, errors.New("作者输出为空")
	}

	switch body[0] {
	case '[':
		files, err := parseAuthorFiles(body)
		if err != nil {
			return AuthorDraftResult{}, err
		}
		return AuthorDraftResult{Files: files}, nil
	case '{':
		questions, err := parseAuthorQuestions(body)
		if err != nil {
			return AuthorDraftResult{}, err
		}
		return AuthorDraftResult{Questions: questions}, nil
	default:
		return AuthorDraftResult{}, errors.New("作者输出必须是文件数组或含 questions 的对象")
	}
}

// parseAuthorFiles 解析文件对象数组（ParseAuthorResult 的数组分支）。
// parseAuthorFiles parses the array branch of ParseAuthorResult.
func parseAuthorFiles(body string) ([]AuthorFile, error) {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		return nil, fmt.Errorf("作者草稿不是 JSON 数组: %w", err)
	}
	if items == nil {
		return nil, errors.New("作者草稿必须是文件对象数组")
	}
	if len(items) > authorDraftMaxFiles {
		return nil, fmt.Errorf("作者草稿包含 %d 个文件，超过上限 %d", len(items), authorDraftMaxFiles)
	}

	files := make([]AuthorFile, 0, len(items))
	seen := make(map[string]string, len(items))
	foundAgentMD := false
	total := 0

	for i, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("作者草稿第 %d 项不是文件对象", i+1)
		}
		rawPath, err := draftStringField(obj, "path")
		if err != nil {
			return nil, fmt.Errorf("作者草稿第 %d 项%w", i+1, err)
		}
		content, err := draftStringField(obj, "content")
		if err != nil {
			return nil, fmt.Errorf("作者草稿第 %d 项%w", i+1, err)
		}

		normalized, err := normalizeAuthorPath(rawPath)
		if err != nil {
			return nil, fmt.Errorf("作者草稿第 %d 项路径非法: %w", i+1, err)
		}
		key := strings.ToLower(normalized)
		if previous, dup := seen[key]; dup {
			return nil, fmt.Errorf("作者草稿路径重复（大小写不敏感）：%s 与 %s", previous, normalized)
		}
		seen[key] = normalized

		if len(content) > authorDraftMaxFileBytes {
			return nil, fmt.Errorf("作者草稿文件 %s 超过单文件上限 %d 字节", normalized, authorDraftMaxFileBytes)
		}
		total += len(content)
		if total > authorDraftMaxTotalBytes {
			return nil, fmt.Errorf("作者草稿总量超过上限 %d 字节", authorDraftMaxTotalBytes)
		}

		if normalized == "AGENT.md" {
			foundAgentMD = true
			if strings.TrimSpace(content) == "" {
				return nil, errors.New("作者草稿的 AGENT.md 不得为空")
			}
		}
		files = append(files, AuthorFile{Path: normalized, Content: content})
	}

	if !foundAgentMD {
		return nil, errors.New("作者草稿缺少 AGENT.md")
	}
	return files, nil
}

// parseAuthorQuestions 解析问题对象（ParseAuthorResult 的对象分支）：顶层只允许
// `questions` 一个键，数组非空且不超过 3 项，每项必须带非空的字符串 `id` / `question`，
// id 不重复、id 与问题各不超过 256 字符。附加字段按契约忽略（模型常带解释性文字）。
// parseAuthorQuestions parses the questions branch: one top-level key, 1–3 items with
// non-empty id/question, unique ids and the 256-character cap; extra item keys are
// ignored as the contract allows.
func parseAuthorQuestions(body string) ([]AuthorQuestion, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil || obj == nil {
		return nil, errors.New("作者输出的问题不是合法的 JSON 对象")
	}
	for key := range obj {
		if key != "questions" {
			return nil, fmt.Errorf("作者输出的对象含未知字段 %q（只允许 questions）", key)
		}
	}
	raw, ok := obj["questions"]
	if !ok {
		return nil, errors.New("作者输出的对象缺少 questions 字段")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("作者输出的 questions 必须是数组")
	}
	if len(items) == 0 {
		return nil, errors.New("作者输出的 questions 不得为空")
	}
	if len(items) > authorMaxQuestions {
		return nil, fmt.Errorf("作者输出包含 %d 个问题，超过上限 %d", len(items), authorMaxQuestions)
	}

	questions := make([]AuthorQuestion, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		var itemObj map[string]json.RawMessage
		if err := json.Unmarshal(item, &itemObj); err != nil || itemObj == nil {
			return nil, fmt.Errorf("作者输出第 %d 个问题不是对象", i+1)
		}
		id, err := draftStringField(itemObj, "id")
		if err != nil {
			return nil, fmt.Errorf("作者输出第 %d 个问题%w", i+1, err)
		}
		question, err := draftStringField(itemObj, "question")
		if err != nil {
			return nil, fmt.Errorf("作者输出第 %d 个问题%w", i+1, err)
		}
		id = strings.TrimSpace(id)
		question = strings.TrimSpace(question)
		if id == "" || question == "" {
			return nil, fmt.Errorf("作者输出第 %d 个问题的 id / question 不得为空", i+1)
		}
		if utf8.RuneCountInString(id) > authorQuestionMaxLen ||
			utf8.RuneCountInString(question) > authorQuestionMaxLen {
			return nil, fmt.Errorf("作者输出第 %d 个问题的 id / question 超过 %d 字符上限", i+1, authorQuestionMaxLen)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("作者输出的问题 id 重复：%s", id)
		}
		seen[id] = struct{}{}
		questions = append(questions, AuthorQuestion{ID: id, Question: question})
	}
	return questions, nil
}

// draftStringField 取一个必填的字符串字段：缺失与非字符串都拒绝（空字符串合法，
// 是否允许为空由调用点按字段语义判断）。
// draftStringField requires a string field; both a missing key and a non-string value
// are refused, while an empty string is left to the caller's semantics.
func draftStringField(obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("缺少 %s 字段", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s 字段必须是字符串", key)
	}
	return value, nil
}

// normalizeAuthorPath 按约束 41 校验并规整路径：`\` 归一为 `/`，拒绝绝对路径
// （前导 `/`、Windows 盘符）、空段、`.` / `..` 段、控制字符与 `.staging` / `.backup`
// 后缀。只做校验与分隔符归一，不做 `path.Clean`——清洗会把 `a/../b` 洗成合法路径，
// 正好放过要拒绝的越界形态。
// normalizeAuthorPath validates and normalizes a path under constraint 41. It never
// applies path.Clean: cleaning would fold a/../b into a legal path and defeat the rule.
func normalizeAuthorPath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("路径不得为空")
	}
	if !utf8.ValidString(raw) {
		return "", errors.New("路径不是合法的 UTF-8 文本")
	}

	normalized := strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("路径不得是绝对路径：%q", raw)
	}
	if len(normalized) >= 2 && normalized[1] == ':' && isASCIIAlpha(normalized[0]) {
		return "", fmt.Errorf("路径不得是绝对路径：%q", raw)
	}
	for _, r := range normalized {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("路径不得包含控制字符：%q", raw)
		}
	}
	lower := strings.ToLower(normalized)
	if strings.HasSuffix(lower, ".staging") || strings.HasSuffix(lower, ".backup") {
		return "", fmt.Errorf("路径不得以 .staging / .backup 结尾：%q", raw)
	}
	for _, segment := range strings.Split(normalized, "/") {
		switch segment {
		case "":
			return "", fmt.Errorf("路径存在空段：%q", raw)
		case ".", "..":
			return "", fmt.Errorf("路径不得包含 . 或 .. 段：%q", raw)
		}
	}
	return normalized, nil
}

// isASCIIAlpha 判定 ASCII 字母（盘符范围）。
// isASCIIAlpha reports an ASCII letter (drive-letter range).
func isASCIIAlpha(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// ScheduleInstructionMaxLen 是定时任务一句话指令的字符上限（R6.4.1）：超限按 400
// 拒绝，不截断——截断后的指令会让候选与用户本意分叉。
// ScheduleInstructionMaxLen caps one-line schedule instructions in characters; an
// oversized instruction is refused, never truncated.
const ScheduleInstructionMaxLen = 1000

// NormalizeScheduleInstruction 校验并规整一句话指令（R6.4.1）：去空白后不得为空，
// 且不超过 ScheduleInstructionMaxLen 字符（按字符计，不按字节）。超限拒绝而不截断。
// NormalizeScheduleInstruction trims the instruction and enforces the character cap;
// an oversized instruction is refused rather than truncated.
func NormalizeScheduleInstruction(instruction string) (string, error) {
	trimmed := strings.TrimSpace(instruction)
	if trimmed == "" {
		return "", errors.New("instruction 不得为空")
	}
	if utf8.RuneCountInString(trimmed) > ScheduleInstructionMaxLen {
		return "", fmt.Errorf("instruction 超过 %d 字符上限", ScheduleInstructionMaxLen)
	}
	return trimmed, nil
}

// ScheduleCandidate 是模型给出的定时候选（R6.4）：四个字段与 AGENT.md 的
// `schedules[]` 一项一一对应；候选不自动保存，确认后走既有的条件写。
// ScheduleCandidate mirrors one schedules[] entry; it is never saved automatically.
type ScheduleCandidate struct {
	ID      string // id；匹配 ScheduleDraftIDPattern
	Cron    string // cron；6 段，不带 CRON_TZ= 前缀
	Zone    string // zone；IANA 时区名（不得写 Local）
	Message string // message；必填
}

// BuildScheduleDraftPrompt 组装定时候选的单轮提示词（R6.4.2）：Agent 名、用户指令与
// 固定的输出契约。输出只由入参决定，同一入参逐字节稳定。
// BuildScheduleDraftPrompt assembles the single-turn schedule prompt; identical inputs
// always render byte-for-byte identical prompts.
func BuildScheduleDraftPrompt(agent, instruction string) string {
	var b strings.Builder
	b.WriteString("你是 Basalt 的定时任务候选助手。根据下列信息，为一个已有的 Agent 生成一条定时任务候选。\n\n")
	b.WriteString("Agent 名：")
	b.WriteString(agent)
	b.WriteString("\n用户指令：")
	b.WriteString(instruction)
	b.WriteString("\n\n输出要求（定时任务候选）：\n")
	b.WriteString("1. 只输出一个 JSON 对象，形如 {\"id\": \"...\", \"cron\": \"...\", \"zone\": \"...\", \"message\": \"...\"}；不要输出对象之外的任何文字。\n")
	b.WriteString("2. id 必须匹配 ")
	b.WriteString(ScheduleDraftIDPattern)
	b.WriteString("（小写字母或数字开头，其后只允许小写字母、数字与连字符）。\n")
	b.WriteString("3. cron 必须是 6 段（秒 分 时 日 月 周），不得带 CRON_TZ= 前缀——时区只写在 zone 字段。\n")
	b.WriteString("4. zone 必须是 IANA 时区名（如 Asia/Shanghai），不得写 Local。\n")
	b.WriteString("5. message 必填，写清这次定时运行要做什么。\n")
	b.WriteString("6. 指令信息不足时按最保守的频率给出候选；不得索要 API Key、Webhook 地址或任何凭据。\n")
	return b.String()
}

// ParseScheduleCandidate 把模型输出解析为定时候选：只接受带字符串 `id` / `cron` /
// `zone` / `message` 的 JSON 对象，未知字段拒绝（候选只承载这四个字段）；外层单层
// 代码围栏会被剥掉。候选的语义校验（cron / zone / message）归 workspace，id 形状归
// ValidateScheduleDraftID——本函数只看形状。
// ParseScheduleCandidate parses the candidate shape only; semantic checks belong to
// workspace and the id pattern to ValidateScheduleDraftID.
func ParseScheduleCandidate(raw string) (ScheduleCandidate, error) {
	if !utf8.ValidString(raw) {
		return ScheduleCandidate{}, errors.New("定时候选不是合法的 UTF-8 文本")
	}
	body := stripJSONFence(strings.TrimSpace(raw))
	if body == "" {
		return ScheduleCandidate{}, errors.New("定时候选为空")
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil || obj == nil {
		return ScheduleCandidate{}, errors.New("定时候选必须是 JSON 对象")
	}
	allowed := map[string]struct{}{"id": {}, "cron": {}, "zone": {}, "message": {}}
	for key := range obj {
		if _, ok := allowed[key]; !ok {
			return ScheduleCandidate{}, fmt.Errorf("定时候选含未知字段 %q", key)
		}
	}

	var candidate ScheduleCandidate
	fields := []struct {
		key string
		dst *string
	}{
		{key: "id", dst: &candidate.ID},
		{key: "cron", dst: &candidate.Cron},
		{key: "zone", dst: &candidate.Zone},
		{key: "message", dst: &candidate.Message},
	}
	for _, field := range fields {
		value, err := draftStringField(obj, field.key)
		if err != nil {
			return ScheduleCandidate{}, fmt.Errorf("定时候选%w", err)
		}
		*field.dst = strings.TrimSpace(value)
	}
	return candidate, nil
}

// ValidateScheduleDraftID 校验候选 id 的公开规则（R6.4.3）：提示词里写的正则与这里的
// 判定共用同一个字面量，模型不会被要求一种形态、服务端又按另一种判卷。
// ValidateScheduleDraftID checks the id rule shared verbatim with the prompt.
func ValidateScheduleDraftID(id string) error {
	if !scheduleDraftIDPattern.MatchString(id) {
		return fmt.Errorf("id 必须匹配 %s（小写字母或数字开头，其后只允许小写字母、数字与连字符，最长 64 字符）",
			ScheduleDraftIDPattern)
	}
	return nil
}

// stripJSONFence 剥掉模型输出外层的单层 Markdown 代码围栏（```json ... ```）；
// 没有围栏时原样返回。围栏只是传输包装——剥完仍必须是文件对象数组。
// stripJSONFence removes one surrounding markdown fence; without one the input is
// returned unchanged. The fence is transport wrapping only.
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	newline := strings.IndexByte(s, '\n')
	if newline < 0 {
		return ""
	}
	s = strings.TrimSpace(s[newline+1:])
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
