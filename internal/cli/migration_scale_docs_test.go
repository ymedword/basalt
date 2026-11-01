package cli

// 本文件是 review ② D-2 的判据：迁移规模读数只能作为「本机、本夹具、单次运行」的
// 观察记录，不得被写成普遍适用的容量保证。三件事被钉住——
//
//  1. R-05 的派生比值必须能由原始表重算（改了表不更新文字 = 红）；
//  2. 运维手册不得把「每消息 KiB × 可用内存」这类换算写成上限（加回公式 = 红）；
//  3. 演练脚本必须真的量在文档声称的那几个探针上（换探针 = 红）。
//
// 逆改点：把 `docs/Operations.md` 的「资源不足怎么办」改回「超限怎么办」并加回
// 93/186/373 万条 → TestOperations_MigrationCapacityGuidanceIsObservational 红；
// 把 R-05 表里任一 RSS 单元格改掉而不改派生序列 → 重算用例红；
// 把 RSS 探针从 `/usr/bin/time -v` 换成 `ps` 采样 → 脚本口径用例红。
//
// This file pins review ② D-2: the migration-scale figures must stay recomputable
// observations, never a capacity formula.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// migrationScaleRow 是 R-05 读数表的一行（列序与文档表头一致）。
// migrationScaleRow is one row of the R-05 measurement table.
type migrationScaleRow struct {
	messages int64
	dump     int64
	importMS int64
	rssKiB   int64
	wal      int64
	target   int64
	index    int64
}

// migrationScaleTableRow 只匹配数据行：七个千分位整数列、两侧竖线。
// migrationScaleTableRow matches data rows only: seven comma-grouped integer columns.
var migrationScaleTableRow = regexp.MustCompile(
	`^\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*([\d,]+)\s*\|\s*$`)

// migrationScaleNumber 去掉千分位后转 int64。
func migrationScaleNumber(t *testing.T, raw string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(strings.ReplaceAll(raw, ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("读数 %q 不是整数：%v", raw, err)
	}
	return value
}

// migrationScaleSection 截出 R-05 一节（到下一个二级标题为止），避免把别处的
// 表格或数字误判成本节内容。
// migrationScaleSection cuts out the R-05 section so stray tables elsewhere cannot pass.
func migrationScaleSection(t *testing.T, text string) string {
	t.Helper()
	at := strings.Index(text, "## R-05")
	if at < 0 {
		t.Fatal("research.md 里找不到 R-05 一节")
	}
	rest := text[at:]
	if end := strings.Index(rest[3:], "\n## "); end >= 0 {
		return rest[:end+3]
	}
	return rest
}

// TestMigrationScaleDrill_EvidenceMatchesRecomputation 判"表与文字必须同源"：
// 表里的四档原始读数重算出的派生序列，必须逐字出现在 R-05 正文里；百万级的
// 总量换算也必须按 1024³ 得出（把 4.17 KiB/条 误写成 4.17 GiB 就红）。
// 验收点：D-2；C-54。
func TestMigrationScaleDrill_EvidenceMatchesRecomputation(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "specs", "016-knowledge-memory-history", "research.md"))
	if err != nil {
		t.Fatalf("读 research.md 失败：%v", err)
	}
	section := migrationScaleSection(t, string(source))

	var rows []migrationScaleRow
	for _, line := range strings.Split(section, "\n") {
		// 表在列表项下缩进两格，先剥掉首尾空白再套行正则。
		// The table is indented under a list item; trim before matching.
		match := migrationScaleTableRow.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		rows = append(rows, migrationScaleRow{
			messages: migrationScaleNumber(t, match[1]),
			dump:     migrationScaleNumber(t, match[2]),
			importMS: migrationScaleNumber(t, match[3]),
			rssKiB:   migrationScaleNumber(t, match[4]),
			wal:      migrationScaleNumber(t, match[5]),
			target:   migrationScaleNumber(t, match[6]),
			index:    migrationScaleNumber(t, match[7]),
		})
	}
	if len(rows) != 4 {
		t.Fatalf("R-05 读数表应恰好四档，实际 %d 行", len(rows))
	}
	wantMessages := []int64{3000, 30000, 300000, 1000000}
	for i, want := range wantMessages {
		if rows[i].messages != want {
			t.Fatalf("第 %d 档消息数 = %d，期望 %d（表被重排过？）", i+1, rows[i].messages, want)
		}
	}

	// 派生序列：重算一遍，再要求正文逐字包含同一条序列。
	var dumpPer, rssPer, walRatio []string
	for _, row := range rows {
		dumpPer = append(dumpPer, fmt.Sprintf("%d", int64(math.Round(float64(row.dump)/float64(row.messages)))))
		rssPer = append(rssPer, fmt.Sprintf("%.2f", float64(row.rssKiB)/float64(row.messages)))
		walRatio = append(walRatio, fmt.Sprintf("%.2f", float64(row.wal)/float64(row.dump)))
	}
	series := map[string][]string{
		"dump 每消息字节":     dumpPer,
		"导入 RSS 每消息 KiB": rssPer,
		"WAL / dump 比":   walRatio,
	}
	for name, values := range series {
		want := strings.Join(values, " → ")
		if !strings.Contains(section, want) {
			t.Errorf("R-05 正文没有与实际表重算一致的%s序列 %q——表改了但文字没跟着改", name, want)
		}
	}

	// 单位换算：百万级的两列总量确实按 1024³ 折成 GiB。
	last := rows[len(rows)-1]
	rssGiB := float64(last.rssKiB) * 1024 / (1024 * 1024 * 1024)
	walGiB := float64(last.wal) / (1024 * 1024 * 1024)
	for _, want := range []string{fmt.Sprintf("%.2f GiB", rssGiB), fmt.Sprintf("%.2f GiB", walGiB)} {
		if !strings.Contains(section, want) {
			t.Errorf("R-05 没有按 KiB/B 实测值换算出的 %q（表内 RSS 是 KiB、WAL 是字节）", want)
		}
	}
	// 4.17 是"每消息 KiB"，4.73 是"十进制 GB"——两者都**不得**被写成 GiB 总量。
	for _, forbidden := range []string{"4.17 GiB", "4.73 GiB"} {
		if strings.Contains(section, forbidden) {
			t.Errorf("R-05 出现 %q：把每消息 KiB 或十进制 GB 当成了 GiB 总量（D-2 的单位口径）", forbidden)
		}
	}
}

// TestOperations_MigrationCapacityGuidanceIsObservational 判运维手册的口径：只能
// 写"本机演练读数 + 需要时重跑"，不得再出现按可用内存换算条数的上限公式。
// 验收点：D-2；C-54。
func TestOperations_MigrationCapacityGuidanceIsObservational(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "Operations.md"))
	if err != nil {
		t.Fatalf("读 docs/Operations.md 失败：%v", err)
	}
	text := string(data)

	// 只禁"正向写出来的上限"，不禁**禁令句里点名的反例**——手册要用
	// "不得按 `可用内存 ÷ 4.5 KiB` 推算"这种写法把话说死。
	forbidden := []string{
		"93 万条", "186 万条", "373 万条",
		"4.5 KiB × 消息数",
		"RSS 4.17 GiB", "WAL 4.73 GiB",
	}
	for _, bad := range forbidden {
		if strings.Contains(text, bad) {
			t.Errorf("docs/Operations.md 仍把规模写成可换算的容量保证：出现 %q", bad)
		}
	}

	required := []string{
		"不是容量保证",
		"migration_scale_drill.sh",
		"资源不足怎么办",
		"4.2–4.7 KiB/条",
		"specs/016-knowledge-memory-history/research.md",
		"4,165,116 KiB",
	}
	for _, want := range required {
		if !strings.Contains(text, want) {
			t.Errorf("docs/Operations.md 缺少 %q——观察口径与处置指引必须写在运维手册里", want)
		}
	}
}

// TestScaleDrillScript_CalibersAreRecorded 判演练脚本真的在文档声称的三个探针上量：
// 进程峰值 RSS、导入前后 LSN 差、50 ms 采样峰值。这三处一旦换成别的探针，文档里的
// 计量口径就与脚本脱钩。
// 验收点：D-2；C-54。
func TestScaleDrillScript_CalibersAreRecorded(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "exp", "migration_scale_drill.sh"))
	if err != nil {
		t.Fatalf("读演练脚本失败：%v", err)
	}
	script := string(data)

	required := []string{
		`Maximum resident set size (kbytes)`, // RSS 探针
		`pg_current_wal_lsn`,                 // LSN 起点 / 终点
		`pg_wal_lsn_diff`,                    // WAL 字节 = 两点之差
		`wal_peak_bytes_per_s`,               // 采样峰值列
		`50ms 采样峰值，非瞬时真峰值`,                   // 峰值口径必须自证
		`pg_total_relation_size`,             // 目标表字节含索引与 TOAST
		`pg_relation_size`,                   // 索引字节
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Errorf("演练脚本缺少口径探针 %q——文档声称的计量方式与脚本脱钩", want)
		}
	}
}
