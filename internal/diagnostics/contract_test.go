package diagnostics

// 本文件是诊断契约的判卷用例（任务 T123）：事件字段与冻结契约 `diagnostic_events` 逐列
// 对齐、枚举与 `openapi.yaml` 一致、标识符合 `ev_` + 26 位 Crockford 的 ULID 形态。
// These tests hold the diagnostics contract: the event fields mirror the frozen table
// column by column, the enumerations match openapi.yaml, and identifiers follow the
// prefixed ULID shape.
//
// 逆改点（每条用例都能指出"实现怎么改它会红"）：
//   - 契约表增删列 / Go 字段改名 → `TestDiagnostics_ContractFields` 列集合不等；
//   - 枚举少一个值或多一个值 → 同上 openapi 对比红；
//   - `NewEventID` 丢掉时间前缀或改随机长度 → `TestNewEventID_UlidLayout` 的毫秒回读红；
//   - 主锚点校验写成"任一标识非空" → `TestDiagnostics_CorrelationRequiresMainAnchor`
//     的"只有 task_id"子用例红。

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

// TestDiagnostics_ContractFields 断言事件结构与两张契约载体一致：列集合来自
// `schema.pg.sql` 的建表语句，枚举来自 `openapi.yaml` 的 schema 定义。
// 逆改点：Go 字段增删 / 改名、枚举取值漂移、`schema_version` 改常量 → 红。
func TestDiagnostics_ContractFields(t *testing.T) {
	require.Equal(t, 0, SchemaVersion, "v0 契约的 schema_version 固定 0")

	ddl := string(readContractFile(t, "schema.pg.sql"))
	require.ElementsMatch(t, diagnosticTableColumns(t, ddl), diagnosticStructColumns(t), "事件字段必须与 diagnostic_events 逐列对应")

	doc := openAPIDoc(t)
	require.ElementsMatch(t, enumStrings(t, dig(t, doc, "components", "schemas", "DiagnosticKind", "enum")), kindStrings())
	require.ElementsMatch(t, enumStrings(t, dig(t, doc, "components", "schemas", "DiagnosticReasonCode", "enum")), reasonCodeStrings())
	require.ElementsMatch(t, enumStrings(t, dig(t, doc, "components", "schemas", "DiagnosticEventView", "properties", "audit_ref", "properties", "table", "enum")), auditRefTableNames())
}

// TestDiagnostics_CorrelationRequiresMainAnchor 断言主锚点规则：四个主锚点任一即可，
// `task_id` 不得单独成立（契约 CHECK 与设计卡 §2.9 同口径）。
// 逆改点：校验退化成"任一字段非空" → 只有 task_id 的子用例红。
func TestDiagnostics_CorrelationRequiresMainAnchor(t *testing.T) {
	accepted := map[string]Correlation{
		"request":   {RequestID: "req-1"},
		"run":       {RunID: "run-1"},
		"operation": {OperationID: "op-1"},
		"ingest":    {IngestJobID: "job-1"},
		"run+task":  {RunID: "run-1", TaskID: "task-1"},
	}
	for name, correlation := range accepted {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, correlation.Validate())
		})
	}

	refused := map[string]Correlation{
		"empty":     {},
		"task only": {TaskID: "task-1"},
	}
	for name, correlation := range refused {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, correlation.Validate(), ErrInvalidEvent)
		})
	}
}

// TestDiagnostics_EventValidate 断言形状校验逐条生效，且错误可被 `errors.Is` 识别。
// 逆改点：删掉任一枚举 / 版本 / 序号检查 → 对应子用例红。
func TestDiagnostics_EventValidate(t *testing.T) {
	base := func() DiagnosticEvent {
		return DiagnosticEvent{
			EventID:       mustEventID(t, time.UnixMilli(1793016000000)),
			SchemaVersion: SchemaVersion,
			Kind:          KindRetrieval,
			OccurredAt:    1793016000000,
			Correlation:   Correlation{RunID: "run-1"},
			Actor:         ActorCaller,
			Seq:           1,
			ReasonCode:    ReasonPermissionDenied,
			Decision:      DecisionDeny,
		}
	}
	require.NoError(t, base().Validate())

	cases := map[string]struct {
		mutate func(*DiagnosticEvent)
		target error
	}{
		"bad event id":         {func(e *DiagnosticEvent) { e.EventID = "ev_x" }, ErrInvalidEventID},
		"wrong schema version": {func(e *DiagnosticEvent) { e.SchemaVersion = 1 }, ErrInvalidEvent},
		"unknown kind":         {func(e *DiagnosticEvent) { e.Kind = "telepathy" }, ErrInvalidEvent},
		"negative occurred_at": {func(e *DiagnosticEvent) { e.OccurredAt = -1 }, ErrInvalidEvent},
		"missing anchor":       {func(e *DiagnosticEvent) { e.Correlation = Correlation{TaskID: "task-1"} }, ErrInvalidEvent},
		"unknown actor":        {func(e *DiagnosticEvent) { e.Actor = "admin" }, ErrInvalidEvent},
		"negative seq":         {func(e *DiagnosticEvent) { e.Seq = -1 }, ErrInvalidEvent},
		"unknown reason":       {func(e *DiagnosticEvent) { e.ReasonCode = "because" }, ErrInvalidEvent},
		"unknown decision":     {func(e *DiagnosticEvent) { e.Decision = "maybe" }, ErrInvalidEvent},
		"bad audit ref": {func(e *DiagnosticEvent) {
			e.AuditRef = &AuditRef{Table: "runs", ID: "1"}
		}, ErrInvalidEvent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			event := base()
			tc.mutate(&event)
			require.ErrorIs(t, event.Validate(), tc.target)
		})
	}
}

// TestNewEventID_UlidLayout 断言标识形态：`ev_` + 26 位 Crockford，前 48 位回读为输入
// 毫秒，剩余位由随机源填充。
// 逆改点：去掉时间前缀 → 毫秒回读不等；载荷长度改 25 / 27 → 长度与校验红。
func TestNewEventID_UlidLayout(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 34, 56, 789_000_000, time.UTC)
	id, err := NewEventID(at)
	require.NoError(t, err)
	require.Len(t, id, len(EventIDPrefix)+eventIDPayloadLength)
	require.True(t, strings.HasPrefix(id, EventIDPrefix))
	require.NoError(t, ValidateEventID(id))
	require.Equal(t, at.UnixMilli(), decodeEventIDMillis(t, id))
}

// TestNewEventID_UniqueWithinSameMillisecond 断言同一毫秒内不重复：唯一性只能来自 80 位
// 随机后缀，而不是时间。
// 逆改点：随机位固定或长度不足 → 出现重复即红。
func TestNewEventID_UniqueWithinSameMillisecond(t *testing.T) {
	at := time.UnixMilli(1793016000000)
	seen := make(map[string]struct{}, 4096)
	for range 4096 {
		id, err := NewEventID(at)
		require.NoError(t, err)
		_, dup := seen[id]
		require.False(t, dup, "同一毫秒内出现重复标识 %q", id)
		seen[id] = struct{}{}
	}
}

// TestNewEventID_RejectsUnrepresentableTime 断言 48 位毫秒前缀之外的输入一律报错，
// 不截断、不回落到零值。
// 逆改点：去掉范围检查 → 负数或越界时间静默生成。
func TestNewEventID_RejectsUnrepresentableTime(t *testing.T) {
	_, err := NewEventID(time.UnixMilli(-1))
	require.ErrorIs(t, err, ErrInvalidEventID)

	_, err = NewEventID(time.UnixMilli(eventIDMillisLimit + 1))
	require.ErrorIs(t, err, ErrInvalidEventID)

	_, err = NewEventID(time.UnixMilli(eventIDMillisLimit))
	require.NoError(t, err, "上界本身必须可表示")
}

// TestValidateEventID_Rejects 断言格式校验的拒绝面：前缀、长度、字符集与首字符上界。
// 逆改点：只查前缀不查字符集 → 非法字符与超界首字符子用例红。
func TestValidateEventID_Rejects(t *testing.T) {
	valid := mustEventID(t, time.UnixMilli(1793016000000))

	cases := map[string]string{
		"missing prefix":     strings.TrimPrefix(valid, EventIDPrefix),
		"wrong prefix":       "id_" + strings.TrimPrefix(valid, EventIDPrefix),
		"short payload":      EventIDPrefix + strings.TrimPrefix(valid, EventIDPrefix)[:25],
		"long payload":       valid + "0",
		"lowercase payload":  EventIDPrefix + strings.ToLower(strings.TrimPrefix(valid, EventIDPrefix)),
		"excluded letter":    EventIDPrefix + "I" + strings.TrimPrefix(valid, EventIDPrefix)[1:],
		"first char above 7": EventIDPrefix + "8" + strings.TrimPrefix(valid, EventIDPrefix)[1:],
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, ValidateEventID(id), ErrInvalidEventID)
		})
	}
}

// mustEventID 生成一个合法标识，失败直接终止用例。
func mustEventID(t *testing.T, at time.Time) string {
	t.Helper()
	id, err := NewEventID(at)
	require.NoError(t, err)
	return id
}

// decodeEventIDMillis 从载荷前 10 个字符（50 位 = 2 位零填充 + 48 位时间）回读毫秒。
func decodeEventIDMillis(t *testing.T, id string) int64 {
	t.Helper()
	payload := strings.TrimPrefix(id, EventIDPrefix)
	require.Len(t, payload, eventIDPayloadLength)
	var value uint64
	for _, c := range payload[:10] {
		idx := strings.IndexRune(crockfordAlphabet, c)
		require.GreaterOrEqual(t, idx, 0, "载荷含非法字符 %q", c)
		value = value<<5 | uint64(idx)
	}
	return int64(value)
}

// diagnosticTableColumns 从契约的建表语句里取出列名（跳过 CHECK 行与注释）。
func diagnosticTableColumns(t *testing.T, ddl string) []string {
	t.Helper()
	const header = "CREATE TABLE IF NOT EXISTS diagnostic_events ("
	start := strings.Index(ddl, header)
	require.GreaterOrEqual(t, start, 0, "契约缺少 diagnostic_events 建表语句")
	rest := ddl[start+len(header):]
	end := strings.Index(rest, "\n);")
	require.GreaterOrEqual(t, end, 0, "diagnostic_events 的建表语句没有闭合")

	var columns []string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "CHECK" {
			continue
		}
		columns = append(columns, fields[0])
	}
	require.NotEmpty(t, columns, "diagnostic_events 的列解析为空——契约形态变了")
	return columns
}

// diagnosticStructColumns 把事件结构映射成列名：`Correlation` 展开成五个关联列，
// 其余字段按驼峰转下划线。
func diagnosticStructColumns(t *testing.T) []string {
	t.Helper()
	eventType := reflect.TypeOf(DiagnosticEvent{})
	var columns []string
	for i := range eventType.NumField() {
		field := eventType.Field(i)
		if field.Name == "Correlation" {
			for j := range field.Type.NumField() {
				columns = append(columns, snakeCase(field.Type.Field(j).Name))
			}
			continue
		}
		columns = append(columns, snakeCase(field.Name))
	}
	return columns
}

// camelBoundary 在"小写或数字紧跟大写"处断开，`EventID` → `Event_ID`。
var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// snakeCase 把 Go 字段名转成契约列名。
func snakeCase(name string) string {
	return strings.ToLower(camelBoundary.ReplaceAllString(name, "${1}_${2}"))
}

// readContractFile 读取 `docs/contracts/` 下的契约文件；缺文件即失败，不静默跳过。
func readContractFile(t *testing.T, name string) []byte {
	t.Helper()
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile(name)
	require.NoError(t, err)
	return raw
}

// openAPIDoc 解析 `openapi.yaml` 供枚举比对。
func openAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(readContractFile(t, "openapi.yaml"), &doc))
	return doc
}

// dig 按路径取子节点；任一步缺失即失败——契约结构漂移必须让测试红。
func dig(t *testing.T, node any, path ...string) any {
	t.Helper()
	current := node
	for i, key := range path {
		mapping, ok := current.(map[string]any)
		require.Truef(t, ok, "路径 %v 的第 %d 步不是映射", path, i)
		current, ok = mapping[key]
		require.Truef(t, ok, "openapi.yaml 缺少路径 %v", path[:i+1])
	}
	return current
}

// enumStrings 把 openapi 的 enum 节点转成字符串切片。
func enumStrings(t *testing.T, node any) []string {
	t.Helper()
	items, ok := node.([]any)
	require.Truef(t, ok, "enum 节点不是数组: %T", node)
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		require.Truef(t, ok, "enum 成员不是字符串: %T", item)
		out = append(out, text)
	}
	return out
}

// kindStrings 返回 Go 侧的 kind 枚举字面量。
func kindStrings() []string { return literalStrings(allKinds) }

// reasonCodeStrings 返回 Go 侧的原因码枚举字面量。
func reasonCodeStrings() []string { return literalStrings(allReasonCodes) }

// auditRefTableNames 返回审计引用允许的表名。
func auditRefTableNames() []string {
	out := make([]string, 0, len(auditRefTables))
	for name := range auditRefTables {
		out = append(out, name)
	}
	return out
}

// literalStrings 把枚举切片转成字符串切片。
func literalStrings[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}
