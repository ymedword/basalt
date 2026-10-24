package workspace

// 本文件是 schedules 的唯一写入门（颗粒度文档 §3.1.5 / 约束 27、28、29、30、31）：
// 读 AGENT.md → 在内存中做 upsert / remove → 只替换顶层 `schedules:` 块 → 候选字节
// 重新走装载同一套解析与 validateSchedules → 临时文件 + 原子 rename。
// 块外内容（注释、未知键、CRLF、正文）逐字节保留；任何失败原文件不变、无临时残留。
// The single write gate for schedules: surgical block replacement, same-source
// validation, and temp-file + atomic rename.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// ErrScheduleRejected 表示候选任务集未通过装载同一套校验（工具侧据此归 invalid_input）。
// ErrScheduleRejected marks a candidate set rejected by the same validation the
// loader uses; the tool classifies it as invalid_input.
var ErrScheduleRejected = errors.New("schedules 写入未通过校验")

// ReadSchedules 读取 AGENT.md 当前的任务集（含装载期派生的 id 与缺省 UTC）。
// 不读 DB、不缓存——调用方每次拿到的都是文件当前内容（约束 30）。
// ReadSchedules returns the file's current task set, with derived ids and UTC
// defaults applied; it reads neither the database nor any cache.
func (l *AgentLoader) ReadSchedules(root, agent string) (schedules []core.Schedule, err error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	rel := agentDefinitionPath(agent)
	raw, err := base.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		// fs.ErrNotExist 链保留：工具侧据此归 not_found（未知 Agent）。
		return nil, fmt.Errorf("读取 %s: %w", rel, err)
	}

	profile, _, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", rel, err)
	}
	// 与装载同一套集合规则：派生 id、缺省 zone、6 段与 claim 检查（这里无外部 claim）。
	if _, err := validateSchedules(agent, profile.Schedules, nil); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", rel, err)
	}
	return profile.Schedules, nil
}

// ApplySchedule 在写锁内完成一次读-改-验-写；返回落盘后的任务集（已规范化）。
// ApplySchedule performs one read-modify-validate-write cycle under the write
// lock and returns the persisted task set.
func (l *AgentLoader) ApplySchedule(root string, claims map[string][]core.Schedule, agent string, action core.ScheduleAction, task core.Schedule) (schedules []core.Schedule, err error) {
	// 与 LoadAll 共用同一把锁（约束 29）：读-改-写全程不得与装载交错。
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	rel := agentDefinitionPath(agent)
	raw, err := base.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", rel, err)
	}

	profile, _, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", rel, err)
	}
	// 当前集合先规范化（派生 id / 缺省 UTC），后续按真实 id 做 upsert / remove——
	// 省略 id 的 set 定位的是当前唯一任务的**实际** id，不是再次派生（约束 31 / D-10）。
	current := profile.Schedules
	if _, err := validateSchedules(agent, current, nil); err != nil {
		return nil, fmt.Errorf("%w: 当前定义无效: %v", ErrScheduleRejected, err)
	}

	candidate, err := buildCandidate(agent, current, claims, action, task)
	if err != nil {
		return nil, err
	}

	// 渲染前先规范化候选（zone 缺省 UTC、id 集合规则）：落盘字节里不得留下空 zone，
	// 否则文件回读虽能补默认值，人工查看却看不到生效时区。claim 集在本次一并检查。
	flat := flattenClaims(claims, agent)
	if _, err := validateSchedules(agent, candidate, flat); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScheduleRejected, err)
	}

	candidateBytes, err := replaceSchedulesBlock(raw, candidate)
	if err != nil {
		return nil, err
	}

	// 候选字节必须重新解析并以同一套 validateSchedules 校验（约束 27）——校验对象是
	// 真正要落盘的字节，而不是内存里的候选切片。
	candProfile, _, err := parseFrontmatter(candidateBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: 候选 frontmatter 不可解析: %v", ErrScheduleRejected, err)
	}
	if _, err := validateSchedules(agent, candProfile.Schedules, flat); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScheduleRejected, err)
	}
	// 回读必须与待写入任务集逐字段相同（两侧都已规范化）：渲染是字符串进出的唯一
	// 有损面，任何被 YAML 解释掉的字段（如裸写的 null 变空值、换行被折叠）都会让
	// 落盘身份与工具确认 / 归属预检看到的身份分叉（用户 review ② 中危项）。
	if !slices.Equal(candidate, candProfile.Schedules) {
		return nil, fmt.Errorf("%w: 候选回读与待写入任务集不一致（字段无法无损编码）", ErrScheduleRejected)
	}

	if err := writeSchedulesAtomic(base, rel, candidateBytes, l.rename); err != nil {
		return nil, err
	}
	return candProfile.Schedules, nil
}

// buildCandidate 在内存中应用动作：set 是 upsert（显式新 id 追加；省略 id 仅当当前
// 恰一项时定位其实际 id），remove 按 id 删除；跨 Agent 的 id 先经 claims 扁平集拒绝。
// buildCandidate applies the action in memory, rejecting ids claimed by another
// registered agent before any candidate validation runs.
func buildCandidate(agent string, current []core.Schedule, claims map[string][]core.Schedule, action core.ScheduleAction, task core.Schedule) ([]core.Schedule, error) {
	flat := flattenClaims(claims, agent)

	switch action {
	case core.ScheduleSet:
		id := strings.TrimSpace(task.ID)
		if id == "" {
			if len(current) != 1 {
				return nil, fmt.Errorf("%w: set 省略 id 仅限该 Agent 当前恰有一个任务", ErrScheduleRejected)
			}
			id = current[0].ID
		}
		task.ID = id
		for i := range current {
			if current[i].ID == id {
				out := slices.Clone(current)
				out[i] = task
				return out, nil
			}
		}
		if owner, taken := flat[id]; taken {
			return nil, crossAgentError(id, owner)
		}
		// 显式新 id 是 upsert 的"插入"半边——不报 not_found（技术方案 §6.3）。
		return append(slices.Clone(current), task), nil

	case core.ScheduleRemove:
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return nil, fmt.Errorf("%w: remove 必须提供 id", ErrScheduleRejected)
		}
		for i := range current {
			if current[i].ID == id {
				return append(slices.Clone(current[:i]), current[i+1:]...), nil
			}
		}
		if owner, taken := flat[id]; taken {
			return nil, crossAgentError(id, owner)
		}
		return nil, fmt.Errorf("任务 id %s 不存在: %w", id, fs.ErrNotExist)

	default:
		return nil, fmt.Errorf("%w: 未知 action %q", ErrScheduleRejected, action)
	}
}

// flattenClaims 把「Agent → 任务集」压成「任务 id → 占用者」，并排除本 Agent：
// 本 Agent 的既有任务不与自己冲突。映射只读，不修改调用方数据。
// flattenClaims builds the id → owner baseline from registered agents, excluding
// the target agent itself.
func flattenClaims(claims map[string][]core.Schedule, self string) map[string]string {
	flat := make(map[string]string)
	for owner, schedules := range claims {
		if owner == self {
			continue
		}
		for _, s := range schedules {
			flat[s.ID] = owner
		}
	}
	return flat
}

// crossAgentError 是跨 Agent 越界的统一拒绝句（约束 31 / 42）。
func crossAgentError(id, owner string) error {
	return fmt.Errorf("%w: 任务 id %s 属于 Agent %s，不得跨 Agent 修改", ErrScheduleRejected, id, owner)
}

// renderSchedulesBlock 渲染顶层 schedules 块（4 空格缩进的任务字段；空集渲染为
// `schedules: []`）。四个字段一律经 yamlQuote 输出：裸写标量会被 YAML 解释——
// "null" 变空值、含换行的值可闭合当前行并注入顶层键（用户 review ② 的高危与中危项）。
// renderSchedulesBlock renders the top-level schedules block; an empty set is
// rendered as `schedules: []`. Every field is YAML-quoted so no value can be
// reinterpreted or escape its scalar.
func renderSchedulesBlock(schedules []core.Schedule) string {
	if len(schedules) == 0 {
		return "schedules: []\n"
	}
	var b strings.Builder
	b.WriteString("schedules:\n")
	for _, s := range schedules {
		fmt.Fprintf(&b, "  - id: %s\n", yamlQuote(s.ID))
		fmt.Fprintf(&b, "    cron: %s\n", yamlQuote(s.Cron))
		fmt.Fprintf(&b, "    zone: %s\n", yamlQuote(s.Zone))
		fmt.Fprintf(&b, "    message: %s\n", yamlQuote(s.Message))
		// 未配置代投时不渲染该键：既有任务的落盘字节保持不变（迁移零改写）。
		if s.DeliverTo != "" {
			fmt.Fprintf(&b, "    deliver_to: %s\n", yamlQuote(s.DeliverTo))
		}
	}
	return b.String()
}

// yamlQuote 把任意字符串渲染为**单行** YAML 双引号标量：转义反斜杠、双引号、C0
// 控制字符，以及 YAML 视为断行的码点（NEL / LS / PS / BOM）。非 ASCII 可打印字符
// 原样输出（UTF-8）；非法 UTF-8 字节会被替换为 U+FFFD，由 ApplySchedule 的回读
// 一致性检查拒绝，不静默落盘。
// yamlQuote renders an arbitrary string as a single-line YAML double-quoted
// scalar; invalid UTF-8 is lossily replaced and caught by the round-trip check.
func yamlQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0x85, 0x2028, 0x2029, 0xfeff:
			// YAML 视为换行或文档边界的码点（NEL / LS / PS / BOM）：必须转义。
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// replaceSchedulesBlock 只替换 frontmatter 内顶层 `schedules:` 块：块外内容逐字节
// 保留（注释 / 未知键 / CRLF / 正文）；无该键时插在收尾围栏之前（约束 28）。
// replaceSchedulesBlock swaps only the top-level schedules block, leaving every
// other byte untouched, and inserts the block before the closing fence when absent.
func replaceSchedulesBlock(raw []byte, schedules []core.Schedule) ([]byte, error) {
	text := string(raw)
	lines := strings.SplitAfter(text, "\n")

	open, closing := -1, -1
	for i, line := range lines {
		content := scheduleLineContent(line)
		if i == 0 {
			content = strings.TrimPrefix(content, "\ufeff")
		}
		if content == "---" {
			if open == -1 {
				open = i
				continue
			}
			closing = i
			break
		}
	}
	if open == -1 {
		return nil, errNoFrontmatterFence
	}
	if closing == -1 {
		return nil, errOpenFrontmatterFence
	}

	blockStart := -1
	for i := open + 1; i < closing; i++ {
		content := scheduleLineContent(lines[i])
		if content == "schedules:" || strings.HasPrefix(content, "schedules: ") {
			blockStart = i
			break
		}
	}

	// 换行风格跟随文件：以块起始行（插入时用收尾围栏行）的终止符为准。
	ref := lines[closing]
	if blockStart >= 0 {
		ref = lines[blockStart]
	}
	newline := "\n"
	if strings.HasSuffix(ref, "\r\n") {
		newline = "\r\n"
	}
	block := strings.ReplaceAll(renderSchedulesBlock(schedules), "\n", newline)

	if blockStart < 0 {
		// 无 schedules 键：插在收尾围栏之前。
		var b strings.Builder
		for _, line := range lines[:closing] {
			b.WriteString(line)
		}
		b.WriteString(block)
		for _, line := range lines[closing:] {
			b.WriteString(line)
		}
		return []byte(b.String()), nil
	}

	// 块结束于下一个顶层键或收尾围栏。两类行仍属块内：合法 YAML 允许的零缩进序列项
	// （`- id: …`），以及顶格注释——YAML 的注释可出现在任意缩进，旧扫描把注释当块结束，
	// 旧任务便残留在文件里，更新时报「id 重复」（用户 review ② 中危项）。
	// 注释归属靠 lookahead：注释串之后若紧跟顶层键（或收尾围栏），它属于下一段，
	// 必须逐字节保留；后面仍是缩进行 / 序列项时才算块内注释。
	// The block ends at the next top-level key or the closing fence. Zero-indent
	// sequence items and column-zero comments are valid YAML inside the block; a
	// comment run stays outside only when the next meaningful line is a top-level
	// key or the closing fence.
	blockEnd := closing
	for i := blockStart + 1; i < closing; {
		content := scheduleLineContent(lines[i])
		switch {
		case content == "" || isScheduleBlockBodyLine(content):
			i++
		case strings.HasPrefix(content, "#"):
			j := i
			for j < closing {
				next := scheduleLineContent(lines[j])
				if next == "" || strings.HasPrefix(next, "#") {
					j++
					continue
				}
				break
			}
			if j < closing && isScheduleBlockBodyLine(scheduleLineContent(lines[j])) {
				// 注释串后面仍是块内内容：注释属块内，越过注释串继续扫描。
				i = j
				continue
			}
			// 注释串属于后续内容（下一顶层键 / 收尾围栏）：保留它与其前的空行。
			blockEnd = trimTrailingScheduleBlanks(lines, blockStart+1, i)
			i = closing
		default:
			// 顶格且非注释 / 非序列项 → 下一顶层键；其前的空行留给下一段。
			blockEnd = trimTrailingScheduleBlanks(lines, blockStart+1, i)
			i = closing
		}
	}

	var b strings.Builder
	for _, line := range lines[:blockStart] {
		b.WriteString(line)
	}
	b.WriteString(block)
	for _, line := range lines[blockEnd:] {
		b.WriteString(line)
	}
	return []byte(b.String()), nil
}

// scheduleLineContent 去掉行终止符（LF 或 CRLF），保留行内其余字节。
func scheduleLineContent(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

// isScheduleBlockBodyLine 判定一行是否仍属 schedules 块的内容（缩进行或序列项）；
// 顶格注释由调用方单独分类——它的归属取决于后续行。
// isScheduleBlockBodyLine reports whether a line is block content (indented or a
// sequence item); column-zero comments are classified separately.
func isScheduleBlockBodyLine(content string) bool {
	return strings.HasPrefix(content, " ") || strings.HasPrefix(content, "\t") ||
		content == "-" || strings.HasPrefix(content, "- ")
}

// trimTrailingScheduleBlanks 把块结束位置前移过紧随其前的空行：这些空行属于下一段，
// 保留原字节（块替换不得吞掉段间分隔）。
// trimTrailingScheduleBlanks moves the block end back over trailing blank lines so
// the separator before the next section survives the rewrite.
func trimTrailingScheduleBlanks(lines []string, lower, end int) int {
	for end > lower && scheduleLineContent(lines[end-1]) == "" {
		end--
	}
	return end
}

// writeSchedulesAtomic 写同目录唯一名临时文件 → Sync → Close → 原子 rename；任何一步
// 失败都清理临时文件并返回错误，原文件保持不变（约束 27）。
// writeSchedulesAtomic writes a sibling temp file and atomically renames it over
// the target; every failure path removes the temp file and leaves the original.
func writeSchedulesAtomic(root *os.Root, rel string, data []byte, rename func(*os.Root, string, string) error) error {
	dir, base := path.Split(rel)
	tmp := path.Join(dir, base+".tmp-"+scheduleTempSuffix())

	f, err := root.OpenFile(filepath.FromSlash(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("建临时文件 %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		return scheduleTempFailure(root, tmp, fmt.Errorf("写入 %s: %w", tmp, err), f)
	}
	if err := f.Sync(); err != nil {
		return scheduleTempFailure(root, tmp, fmt.Errorf("刷盘 %s: %w", tmp, err), f)
	}
	if err := f.Close(); err != nil {
		return scheduleTempFailure(root, tmp, fmt.Errorf("关闭临时文件 %s: %w", tmp, err), nil)
	}

	if rename == nil {
		rename = func(root *os.Root, oldname, newname string) error {
			return root.Rename(oldname, newname)
		}
	}
	if err := rename(root, filepath.FromSlash(tmp), filepath.FromSlash(rel)); err != nil {
		return scheduleTempFailure(root, tmp, fmt.Errorf("替换 %s: %w", rel, err), nil)
	}
	return nil
}

// scheduleTempFailure 收拢"失败即清理"：无论关闭是否成功都尝试删除临时文件，两个错误
// 都进返回文本（不静默丢弃）。
// scheduleTempFailure removes the temp file on any failure and joins cleanup errors.
func scheduleTempFailure(root *os.Root, tmp string, cause error, f *os.File) error {
	err := cause
	if f != nil {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭临时文件 %s: %w", tmp, closeErr))
		}
	}
	if removeErr := root.Remove(filepath.FromSlash(tmp)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("清理临时文件 %s: %w", tmp, removeErr))
	}
	return err
}

// scheduleTempSuffix 生成临时文件唯一后缀（crypto/rand——随机源红线禁止 math/rand）。
func scheduleTempSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败是系统级故障；保留空后缀会让 O_EXCL 撞名，但错误来自
		// rand.Read 的极端场景，返回空串并由 O_EXCL 兜底为明确错误（不 panic）。
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}
