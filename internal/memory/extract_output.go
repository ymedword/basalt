package memory

// 本文件实现 US-22 抽取模型输出的**严格解析**（颗粒度文档 §2.3.4 / §3.1.12，FR-22-05）：
// 模型输出是不可信数据。解析层只判「整次输出是否可用」——非 JSON 对象、超过 64 KiB、
// 非法 UTF-8、未知键、尾随内容、items 超过 5 条，任一命中即整次失败（invalid_output），
// 不做"尽力修复"；逐项的丢弃 / 降级是 extract_validate.go 的职责。
//
// 错误一律是**固定文案哨兵**：`encoding/json` 的原始错误文本可能含模型生成的长键名，
// 本包不把它拼进错误串——抽取账本的失败消息会落库，不得携带任何输出内容片段（C-19）。
//
// Strict parsing of the extractor's JSON. It decides only whether the whole output is
// usable; per-item drops live in extract_validate.go. Errors are fixed sentinels so no
// fragment of the model's output can reach the persisted failure message.

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// extractRules 是解析与逐项校验的固定上限（值来自模块卡 §3.1.12 的 LearningLimits 同名
// 字段；T011 的 LearningService 从 LearningLimits 取值构造）。规则是代码常量，不是配置键。
// extractRules carries the fixed limits of module card §3.1.12; they are code constants,
// not configuration keys.
type extractRules struct {
	MaxItems         int
	MaxContentBytes  int
	MaxEvidence      int
	MaxSourceEntries int
	MaxOutputBytes   int
}

// defaultExtractRules 返回与模块卡 §3.1.12 同值的默认上限。
// defaultExtractRules returns the defaults from module card §3.1.12.
func defaultExtractRules() extractRules {
	return extractRules{
		MaxItems:         5,
		MaxContentBytes:  4096,
		MaxEvidence:      3,
		MaxSourceEntries: 5,
		MaxOutputBytes:   65536,
	}
}

// 解析层哨兵：整次失败的所有可能原因各有一个固定文案，互不携带输出内容。
// Fixed sentinels for whole-output failures; none carries any of the model's output.
var (
	errExtractNotJSONObject = errors.New("learning: extract output is not a JSON object")
	errExtractOversize      = errors.New("learning: extract output exceeds the size limit")
	errExtractMalformedUTF8 = errors.New("learning: extract output is not valid UTF-8")
	errExtractStrictJSON    = errors.New("learning: extract output violates the strict JSON schema")
	errExtractTooManyItems  = errors.New("learning: extract output exceeds the item limit")
)

// extractOutput 是模型顶层的唯一形状：`{"items":[...]}`。空 items 合法（本次零候选，
// 提交路径照常推进游标）。
// extractOutput is the only accepted top-level shape; an empty item list is legal.
type extractOutput struct {
	Items []extractItem `json:"items"`
}

// extractItem 是一条模型自报条目；字段名即提示词契约（§3.1.13 的 buildPrompt 必须与此
// 逐字一致）。这里**没有** state / source / entry_id 之类的字段——模型无法指定落库状态或
// 目标主键，DisallowUnknownFields 会把这类越权键判成整次失败。
// extractItem is one model-reported item; field names are the prompt contract. There is
// deliberately no state/source/entry-id field, so the model cannot dictate state.
type extractItem struct {
	Kind                  string            `json:"kind"`
	Content               string            `json:"content"`
	Name                  string            `json:"name"`
	Confidence            float64           `json:"confidence"`
	Evidence              []extractEvidence `json:"evidence"`
	SourceEntryIDs        []int64           `json:"source_entry_ids"`
	SuggestedSupersedesID int64             `json:"suggested_supersedes_id"`
	ValidTo               string            `json:"valid_to"`
}

// extractEvidence 是一条证据引用；程序只信 message_id 落在冻结窗口内且 quote 逐字出现。
// extractEvidence is one evidence claim; only window membership and a verbatim quote count.
type extractEvidence struct {
	MessageID int64  `json:"message_id"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
}

// parseExtractOutput 严格解析模型输出。判定顺序固定为：大小 → UTF-8 → 顶层对象 →
// 无未知键且无尾随内容 → 条数上限；任一不满足返回对应哨兵，调用方记 invalid_output。
// parseExtractOutput enforces size, UTF-8, object shape, unknown-key/trailing checks and
// the item cap, in that order.
func parseExtractOutput(raw string, rules extractRules) (extractOutput, error) {
	if len(raw) > rules.MaxOutputBytes {
		return extractOutput{}, errExtractOversize
	}
	if !utf8.ValidString(raw) {
		return extractOutput{}, errExtractMalformedUTF8
	}
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return extractOutput{}, errExtractNotJSONObject
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var out extractOutput
	if err := decoder.Decode(&out); err != nil {
		return extractOutput{}, errExtractStrictJSON
	}
	// 第一次解码之后必须只剩空白：再解一个值若返回 io.EOF 才算「无尾随内容」。
	// Exactly one value is allowed; a second decode must hit EOF.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return extractOutput{}, errExtractStrictJSON
	}
	if len(out.Items) > rules.MaxItems {
		return extractOutput{}, errExtractTooManyItems
	}
	return out, nil
}
