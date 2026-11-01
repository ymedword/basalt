package dbmigrate

// 本文件是导出包与目标库的**双向复核**（C-43 / 颗粒度文档 §2.15.3、§3.1.2）：
// `VerifyDump` 在导入前复核导出目录——逐表重算文件字节摘要、行数、行摘要与主键集合摘要
// 并与清单比对，再在 dump 自身的行集上核对关键引用；`VerifyTarget` 在导入后复核
// PostgreSQL——按清单列元数据回读每张表、用同一编码器重新编码后比对行数与摘要，并核对
// 关键引用。两侧**共用**导入端的解码器与摘要规范化（`readJSONL` / `DigestRows` /
// `DigestIDSet`），校验端不另写一套编码——两套编码会随版本各自漂移，"校验通过"就不再
// 说明问题。
//
// 关键引用为什么单独查：外键只保证"父行存在"。把 `run_resource_refs.version_id` 改指
// 另一个同样存在的版本时，外键仍然满足，只有 `content_hash` 与被引用版本的一致性才能
// 发现错位（`TestVerify_ComparesRefsNotJustRowCounts`）。`run_credential_refs` /
// `run_notify_refs` / `notify_deliveries.tool_invocation_id` 三条边在源端契约里**没有**
// 外键，离线只能靠这里查。
//
// 本文件只读：不写目标库、不取锁、不开事务。
// Both ends of the migration share this file. VerifyDump re-derives the dump's digests and
// checks key references offline; VerifyTarget re-reads PostgreSQL through the manifest's own
// column metadata and runs the same reference checks. Nothing here writes.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/storage/postgres"
)

// VerifyDump 离线复核导出目录：逐表重算文件摘要、行数、行摘要与主键集合摘要并与清单
// 逐项比对，随后在 dump 自己的行集上核对关键引用。
//
// 任何一项不符都返回 `ErrVerifyFailed`，报告里保留逐表读数与逐条失败原因——失败时不返回
// 空报告，也不"先导进去再说"。清单自身不合法（格式版本、表不在迁移顺序、列元数据不可用）
// 属于输入错误，与校验不通过分开返回。
// VerifyDump re-checks the export directory against the manifest before any import.
func VerifyDump(ctx context.Context, dir string, manifest Manifest) (report VerifyReport, err error) {
	started := time.Now()
	defer func() { report.Duration = time.Since(started) }()

	tables, err := verifyTables(manifest)
	if err != nil {
		return VerifyReport{}, err
	}
	report = NewVerifyReport(tables)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return report, fmt.Errorf("dbmigrate: 打开 dump 目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	rowsByTable := make(map[string][]Row, len(manifest.Tables))
	var problems []string
	for _, table := range manifest.Tables {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.SourceRows[table.Name] = table.RowCount
		check := verifyDumpTable(root, table)
		problems = append(problems, check.problems...)
		if !check.readable {
			// 读不出来（文件缺失 / 行解析失败）：该表没有现场读数，也不参与引用检查。
			// 判定靠这个显式标志，**不**靠 `rows == nil`——空表的读取结果本来就是 nil 切片，
			// 用 nil 判失败会把一张合法的空表判成读不出来。
			report.RowDigestOK[table.Name] = false
			continue
		}
		rowsByTable[table.Name] = check.rows
		report.TargetRows[table.Name] = int64(len(check.rows))
		if !check.rowDigestOK {
			report.RowDigestOK[table.Name] = false
		}
	}
	report.ReferenceFailures = checkReferences(rowsByTable)
	report = report.Finalize()
	// 文件字节摘要不在 `Finalize` 的逐表判据里（它比的不是行集合），因此这里的失败**必须**
	// 单独并入判定：只改了空白、行集合没变的 dump 就是这样被拦下的。
	if len(problems) > 0 {
		report.OK = false
	}
	if !report.OK {
		return report, fmt.Errorf("%w: %s", ErrVerifyFailed, failureSummary(report, problems))
	}
	return report, nil
}

// VerifyTarget 在导入后复核目标库：按清单列元数据回读每张表，行数 / 主键集合 / 行摘要
// 逐表比对，再核对关键引用。
//
// 含 jsonb 列的表**跳过行摘要**：源侧摘要算在 JSON 原文上，PG 存的是解析后的规范化值
// （键序与空白都可能变），两者本就不可比（§2.11.1）。跳过的事实写进 `Notes`，行数与主键
// 集合照判——缺口如实披露，不静默通过。
// VerifyTarget re-reads PostgreSQL through the manifest and applies the same criteria.
func VerifyTarget(ctx context.Context, db *postgres.DB, manifest Manifest) (report VerifyReport, err error) {
	started := time.Now()
	defer func() { report.Duration = time.Since(started) }()

	tables, err := verifyTables(manifest)
	if err != nil {
		return VerifyReport{}, err
	}
	report = NewVerifyReport(tables)

	rowsByTable := make(map[string][]Row, len(manifest.Tables))
	var problems []string
	for _, table := range manifest.Tables {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.SourceRows[table.Name] = table.RowCount
		rows, err := readTargetRows(ctx, db, table)
		if err != nil {
			report.RowDigestOK[table.Name] = false
			problems = append(problems, fmt.Sprintf("表 %q: %v", table.Name, err))
			continue
		}
		rowsByTable[table.Name] = rows
		report.TargetRows[table.Name] = int64(len(rows))
		rowDigestOK, tableProblems := compareTargetRows(table, rows, &report)
		problems = append(problems, tableProblems...)
		if !rowDigestOK {
			report.RowDigestOK[table.Name] = false
		}
	}
	report.ReferenceFailures = checkReferences(rowsByTable)
	report = report.Finalize()
	// 与离线侧同一口径：任何一条失败读数都必须让整体判定为假，不指望 `Finalize` 兜住
	// 它没有字段承载的那一类（如 jsonb 表以外新出现的读取问题）。
	if len(problems) > 0 {
		report.OK = false
	}
	if !report.OK {
		return report, fmt.Errorf("%w: %s", ErrVerifyFailed, failureSummary(report, problems))
	}
	return report, nil
}

// verifyTables 校验清单自身并把表名按清单顺序取出。
//
// 判据与导入端 `plan` **同一批**：表必须在迁移顺序里、列元数据必须能用于插入。校验端若
// 认识比导入端更宽的口径，就会给一份导不进去的 dump 发"通过"。
// verifyTables applies the same manifest gate as the importer, so a dump that cannot be
// imported can never be reported as verified.
func verifyTables(manifest Manifest) ([]string, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(migrationOrder))
	for _, name := range migrationOrder {
		allowed[name] = struct{}{}
	}
	tables := make([]string, 0, len(manifest.Tables))
	for _, table := range manifest.Tables {
		if _, ok := allowed[table.Name]; !ok {
			return nil, fmt.Errorf("%w: 清单里的表 %q 不在迁移顺序里", ErrInvalidValue, table.Name)
		}
		if err := checkManifestColumns(table); err != nil {
			return nil, err
		}
		tables = append(tables, table.Name)
	}
	return tables, nil
}

// dumpTableCheck 是一张表的离线复核读数：行、可读性、行摘要判定与逐条失败。
//
// `rowDigestOK` 只覆盖**行集合**两项（行摘要与主键集合摘要），文件字节摘要算在
// `problems` 里——它比对的是字节而不是行，混进 `RowDigestOK` 会让这个字段名说谎。
type dumpTableCheck struct {
	rows        []Row
	readable    bool
	rowDigestOK bool
	problems    []string
}

// verifyDumpTable 复核一张表的 JSONL：文件字节摘要 → 行数 → 行摘要 → 主键集合摘要。
//
// 读失败返回 `readable=false`（该表没有现场读数）；摘要不符仍返回已读到的行与
// `readable=true`，让引用检查继续跑——一份被改过的 dump 要把它错在哪儿报全，
// 而不是只报第一处。
func verifyDumpTable(root *os.Root, table TableManifest) dumpTableCheck {
	name := table.Name + ".jsonl"
	check := dumpTableCheck{rowDigestOK: true}

	fileSHA, err := fileSHA256(root, name)
	if err != nil {
		check.problems = append(check.problems, fmt.Sprintf("表 %q: %v", table.Name, err))
		return check
	}
	if fileSHA != table.JSONLSHA256 {
		check.problems = append(check.problems, fmt.Sprintf("表 %q 的文件摘要 %s，清单声明 %s",
			table.Name, fileSHA, table.JSONLSHA256))
	}

	rows, err := readJSONL(root, name, table.ColumnNames())
	if err != nil {
		check.problems = append(check.problems, fmt.Sprintf("表 %q: %v", table.Name, err))
		return check
	}
	check.rows = rows
	check.readable = true
	if int64(len(rows)) != table.RowCount {
		check.problems = append(check.problems, fmt.Sprintf("表 %q 实际 %d 行，清单声明 %d 行",
			table.Name, len(rows), table.RowCount))
	}
	digest := DigestRows(rows)
	switch {
	case digest.Err != nil:
		check.rowDigestOK = false
		check.problems = append(check.problems, fmt.Sprintf("表 %q 的行摘要: %v", table.Name, digest.Err))
	case digest.RowSHA256 != table.RowSHA256:
		check.rowDigestOK = false
		check.problems = append(check.problems, fmt.Sprintf("表 %q 的行摘要 %s，清单声明 %s",
			table.Name, digest.RowSHA256, table.RowSHA256))
	}
	idSet, err := DigestIDSet(sourcePrimaryKeys[table.Name], rows)
	switch {
	case err != nil:
		check.rowDigestOK = false
		check.problems = append(check.problems, fmt.Sprintf("表 %q 的 ID 集合摘要: %v", table.Name, err))
	case idSet != table.IDSetSHA256:
		check.rowDigestOK = false
		check.problems = append(check.problems, fmt.Sprintf("表 %q 的 ID 集合摘要 %s，清单声明 %s",
			table.Name, idSet, table.IDSetSHA256))
	}
	return check
}

// readTargetRows 按清单列序回读整表。
//
// **不**查 `information_schema`：列集合与编码口径的唯一来源是清单，目标库只被当作
// "给定列的行集合"来读。表名来自 `migrationOrder` 白名单（`verifyTables` 已判），列名来自
// 清单并经 `quoteIdent` 转义——与 `insertQuery` / `selectAllQuery` 同一安全依据。
func readTargetRows(ctx context.Context, db *postgres.DB, table TableManifest) ([]Row, error) {
	columns := table.Columns
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdent(column.Name)
	}
	query := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdent(table.Name)
	result, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("读表失败: %w", err)
	}
	defer func() { _ = result.Close() }()

	out := make([]Row, 0, table.RowCount)
	for index := 0; result.Next(); index++ {
		holders := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range holders {
			pointers[i] = &holders[i]
		}
		if err := result.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("读第 %d 行失败: %w", index+1, err)
		}
		row, err := encodeTargetRow(table.Name, index+1, columns, holders)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("读表失败: %w", err)
	}
	return out, nil
}

// encodeTargetRow 把 `database/sql` 取回的一行按清单 `encoding` 重新编码成 `Row`。
// 产物必须与导出端 `encodeStoredValue` **同形**，否则摘要无从比较；类型不合就点名
// 表 / 行 / 列，不做宽泛转换。
func encodeTargetRow(table string, rowNumber int, columns []ColumnMeta, values []any) (Row, error) {
	row := make(Row, len(columns))
	for i, column := range columns {
		value, err := encodeTargetValue(column, values[i])
		if err != nil {
			return nil, fmt.Errorf("表 %q 第 %d 行的列 %q: %w", table, rowNumber, column.Name, err)
		}
		row[column.Name] = value
	}
	return row, nil
}

// encodeTargetValue 按清单 `encoding` 把目标值编码成与源侧同形的 `EncodedValue`：
// 整数是十进制、实数是 IEEE754 位模式、文本是原字节、二进制是 base64。布尔列在源侧
// 按 0 / 1 编码，目标侧读回 bool，这里翻回 0 / 1；其余类型不做隐式转换。
func encodeTargetValue(column ColumnMeta, value any) (EncodedValue, error) {
	if value == nil {
		return Null(), nil
	}
	switch column.Encoding {
	case TagInteger:
		switch v := value.(type) {
		case int64:
			return Int(v), nil
		case bool:
			if v {
				return Int(1), nil
			}
			return Int(0), nil
		}
	case TagReal:
		if v, ok := value.(float64); ok {
			return Real(v), nil
		}
	case TagText:
		switch v := value.(type) {
		case string:
			return Text(v), nil
		case []byte:
			// jsonb 与部分驱动把文本列交成字节切片；两者都是目标端的原文。
			return Text(string(v)), nil
		}
	case TagBytes:
		if v, ok := value.([]byte); ok {
			return Bytes(v), nil
		}
	}
	return EncodedValue{}, fmt.Errorf("%w: 目标值类型 %T 与清单 encoding %q 不符",
		ErrInvalidValue, value, column.Encoding)
}

// compareTargetRows 比对一张表的三项：行数、主键集合、行摘要；返回"行集合两项是否相符"
// 与逐条失败。行数不符只进失败列表——它已由 `Finalize` 的行数判据承载，不属于行摘要。
// 含 jsonb 列的表跳过**行摘要**并在 `Notes` 里写明原因；另外两项照判。
func compareTargetRows(table TableManifest, rows []Row, report *VerifyReport) (bool, []string) {
	var problems []string
	rowDigestOK := true
	if int64(len(rows)) != table.RowCount {
		problems = append(problems, fmt.Sprintf("表 %q 实际 %d 行，清单声明 %d 行",
			table.Name, len(rows), table.RowCount))
	}
	idSet, err := DigestIDSet(sourcePrimaryKeys[table.Name], rows)
	switch {
	case err != nil:
		rowDigestOK = false
		problems = append(problems, fmt.Sprintf("表 %q 的 ID 集合摘要: %v", table.Name, err))
	case idSet != table.IDSetSHA256:
		rowDigestOK = false
		problems = append(problems, fmt.Sprintf("表 %q 的 ID 集合摘要 %s，清单声明 %s",
			table.Name, idSet, table.IDSetSHA256))
	}
	if jsonColumns := jsonbColumns(table); len(jsonColumns) > 0 {
		report.Notes[table.Name] = fmt.Sprintf(
			"行摘要按设计不在目标侧比对：%s 是 jsonb 列，PG 会重排键与空白（§2.11.1），源侧原文摘要与目标侧规范化值不可直接比较；行数与主键集合仍已判",
			strings.Join(jsonColumns, "、"))
		return rowDigestOK, problems
	}
	digest := DigestRows(rows)
	switch {
	case digest.Err != nil:
		rowDigestOK = false
		problems = append(problems, fmt.Sprintf("表 %q 的行摘要: %v", table.Name, digest.Err))
	case digest.RowSHA256 != table.RowSHA256:
		rowDigestOK = false
		problems = append(problems, fmt.Sprintf("表 %q 的行摘要 %s，清单声明 %s",
			table.Name, digest.RowSHA256, table.RowSHA256))
	}
	return rowDigestOK, problems
}

// jsonbColumns 列出清单里目标类型是 jsonb 的列（当前只有 `management_operations.detail_json`）。
func jsonbColumns(table TableManifest) []string {
	var out []string
	for _, column := range table.Columns {
		if column.PGType == pgTypeJSONB {
			out = append(out, column.Name)
		}
	}
	return out
}

// checkReferences 复核 §2.15.3「关键引用」维度的四条边，通用于离线 dump 与在线目标库
// （两边都传"表名 → 已解码行"）。每条只在**父表已读成功**时执行：读不出来的父表本身
// 已在行摘要判据里失败，这里不再制造一串"悬空"噪声。
//
//   - `run_resource_refs.version_id` 必须存在，且 `content_hash` 必须等于被引用版本的
//     `content_hash`——外键只保证版本存在，改指另一个也存在（同样满足外键）的版本时，
//     只有这一步能发现错位；
//   - `run_credential_refs.(credential_id, credential_version)` 必须命中 `credential_versions`；
//   - `run_notify_refs.channel_uid` 必须命中 `notify_channels.resource_uid`；
//   - `notify_deliveries.tool_invocation_id` 必须命中 `tool_invocations.id`。
//
// 后三条在源端契约里**没有**外键，是纯粹的引用完整性事实；通用外键闭包由目标库在导入时
// 自身拒绝（孤儿行会让整笔导入失败），这里不重复造一套没有列级信息的表级检查。
func checkReferences(rowsByTable map[string][]Row) map[string][]string {
	failures := make(map[string][]string)
	add := func(table, reason string) {
		failures[table] = append(failures[table], reason)
	}

	if versions, ok := rowsByTable["resource_versions"]; ok {
		hashes := make(map[int64]string, len(versions))
		for _, row := range versions {
			id, idOK := valueInt(row, "id")
			hash, hashOK := valueText(row, "content_hash")
			if !idOK || !hashOK {
				continue // 父行本身不完整：那是行摘要判据的事，不在引用检查里重复报
			}
			hashes[id] = hash
		}
		for _, row := range rowsByTable["run_resource_refs"] {
			versionID, idOK := valueInt(row, "version_id")
			hash, hashOK := valueText(row, "content_hash")
			name := refName(row, "run_id", "resource_kind", "resource_id")
			if !idOK || !hashOK {
				add("run_resource_refs", name+" 缺 version_id 或 content_hash")
				continue
			}
			versionHash, found := hashes[versionID]
			switch {
			case !found:
				add("run_resource_refs", fmt.Sprintf("%s 的 version_id=%d 悬空", name, versionID))
			case versionHash != hash:
				add("run_resource_refs", fmt.Sprintf(
					"%s 的 content_hash=%s 与被引用版本 %d 的 content_hash=%s 不一致",
					name, hash, versionID, versionHash))
			}
		}
	}

	if parents, ok := rowsByTable["credential_versions"]; ok {
		known := make(map[credentialVersionKey]struct{}, len(parents))
		for _, row := range parents {
			id, idOK := valueText(row, "credential_id")
			no, noOK := valueInt(row, "version_no")
			if !idOK || !noOK {
				continue
			}
			known[credentialVersionKey{credentialID: id, versionNo: no}] = struct{}{}
		}
		for _, row := range rowsByTable["run_credential_refs"] {
			id, idOK := valueText(row, "credential_id")
			no, noOK := valueInt(row, "credential_version")
			name := refName(row, "run_id", "resource_kind", "resource_uid")
			switch {
			case !idOK || !noOK:
				add("run_credential_refs", name+" 缺 credential_id 或 credential_version")
			default:
				key := credentialVersionKey{credentialID: id, versionNo: no}
				if _, found := known[key]; !found {
					add("run_credential_refs", fmt.Sprintf("%s 的凭据版本 %s@%d 悬空", name, id, no))
				}
			}
		}
	}

	if channels, ok := rowsByTable["notify_channels"]; ok {
		known := make(map[string]struct{}, len(channels))
		for _, row := range channels {
			if uid, uidOK := valueText(row, "resource_uid"); uidOK {
				known[uid] = struct{}{}
			}
		}
		for _, row := range rowsByTable["run_notify_refs"] {
			uid, uidOK := valueText(row, "channel_uid")
			name := refName(row, "run_id", "agent_uid", "channel_uid")
			switch {
			case !uidOK:
				add("run_notify_refs", name+" 缺 channel_uid")
			default:
				if _, found := known[uid]; !found {
					add("run_notify_refs", fmt.Sprintf("%s 指向的渠道 %s 不存在", name, uid))
				}
			}
		}
		for _, row := range rowsByTable["notify_deliveries"] {
			uid, uidOK := valueText(row, "channel_uid")
			name := refName(row, "run_id", "agent_uid", "channel_uid")
			switch {
			case !uidOK:
				add("notify_deliveries", name+" 缺 channel_uid")
			default:
				if _, found := known[uid]; !found {
					add("notify_deliveries", fmt.Sprintf("%s 指向的渠道 %s 不存在", name, uid))
				}
			}
		}
	}

	if invocations, ok := rowsByTable["tool_invocations"]; ok {
		known := make(map[int64]struct{}, len(invocations))
		for _, row := range invocations {
			if id, idOK := valueInt(row, "id"); idOK {
				known[id] = struct{}{}
			}
		}
		for _, row := range rowsByTable["notify_deliveries"] {
			id, idOK := valueInt(row, "tool_invocation_id")
			if !idOK {
				add("notify_deliveries", "行缺 tool_invocation_id（主键不为空）")
				continue
			}
			if _, found := known[id]; !found {
				add("notify_deliveries", fmt.Sprintf("tool_invocation_id=%d 悬空", id))
			}
		}
	}
	return failures
}

// credentialVersionKey 是 `(credential_id, version_no)` 组合键——凭据版本的主键是复合键，
// **不得**退化成"把两列拼成一个字符串"（拼接会让 ("a","1") 与 ("a1","") 相撞）。
type credentialVersionKey struct {
	credentialID string
	versionNo    int64
}

// refName 给一条引用行一个人可读的标识（不输出整行，避免把业务内容带进错误串）。
func refName(row Row, runColumn, kindColumn, idColumn string) string {
	runID, _ := valueText(row, runColumn)
	kind, _ := valueText(row, kindColumn)
	id, _ := valueText(row, idColumn)
	return fmt.Sprintf("run=%q %s/%s", runID, kind, id)
}

// valueInt 取行里的整数列并解析成 int64；缺列 / NULL / 标签不符返回 false。
func valueInt(row Row, column string) (int64, bool) {
	value, ok := row[column]
	if !ok || value.IsNull() || value.Tag != TagInteger {
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(value.Raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// valueText 取行里的文本列；缺列 / NULL / 标签不符返回 false。
func valueText(row Row, column string) (string, bool) {
	value, ok := row[column]
	if !ok || value.IsNull() || value.Tag != TagText {
		return "", false
	}
	return string(value.Raw), true
}

// failureSummary 汇总失败原因：先列逐表明细，再**按表名排序列出**关键引用失败——
// map 遍历顺序随机，报告文案不能跟着随机。
func failureSummary(report VerifyReport, problems []string) string {
	parts := append([]string(nil), problems...)
	tables := make([]string, 0, len(report.ReferenceFailures))
	for table := range report.ReferenceFailures {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		for _, reason := range report.ReferenceFailures[table] {
			parts = append(parts, table+": "+reason)
		}
	}
	if len(parts) == 0 {
		parts = []string{"逐表判据不符（见报告）"}
	}
	return strings.Join(parts, "; ")
}
