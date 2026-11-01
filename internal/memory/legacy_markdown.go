package memory

// 本文件是旧 markdown 载体的**只读导入源**（D-18 / D-22）：把 `memory/MEMORY.md` 与
// `memory/agents/<name>/MEMORY.md` 逐条读成 `LegacyEntry`，交给 `ImportLegacy` 对账。
// 三条纪律：
//   - ① **只读**：全程不开写句柄，旧载体一个字节都不动（D-22 不搬迁 / 不删除）。
//   - ② **口径与运行期同源**：分层路径取 `workspace.MemoryFilePath`、分区切分复用
//     `parseSections`——导入口径若另写一份，"导入进去了"与"运行期读得到"就会是两件事。
//   - ③ **不可读显式上报**：文件存在但结构损坏 / 读取失败，产出 `Unreadable` 条目并把
//     文件路径写进原因，**不得**整份跳过——D-18 要求不可读条目显式报告、不静默丢弃。
//
// 根目录既可以是实例工作区根（`<WorkDir>/.basalt`），也可以是迁移包内的 `package/`
// 目录：两者按同一套相对路径布局，故同一个源实现两处通用。
// The read-only legacy source for the markdown carrier. It reuses the runtime path helper
// and section parser so "imported" and "loadable at runtime" mean the same thing; it never
// writes the old carrier and reports a damaged file as an explicit unreadable entry.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// MarkdownLegacySource 按工作区布局读旧 markdown 载体的全部层。
// MarkdownLegacySource reads every layer of the legacy markdown carrier under root.
type MarkdownLegacySource struct {
	root string
}

// NewMarkdownLegacySource 以 root 为根构造只读源（工作区根或迁移包 `package/` 目录）。
// NewMarkdownLegacySource is the constructor; root is a workspace root or a package dir.
func NewMarkdownLegacySource(root string) *MarkdownLegacySource {
	return &MarkdownLegacySource{root: root}
}

// 编译期断言：导入源只服务 `ImportLegacy` 的那一个接口。
var _ LegacySource = (*MarkdownLegacySource)(nil)

// Read 返回旧载体里的全部条目：全局层恒查（文件缺失 = 该层没有记忆，合法空层），
// Agent 层按 `memory/agents/` 下实际存在的目录逐个查。
//
// 读不出来**不**中断整次读取：单层故障产出一条 `Unreadable` 条目继续——把其余层也
// 一并放弃会让"一处损坏"变成"全都导不进去"，而报告里本来就能逐条说清哪层坏了。
// Read never aborts on one damaged layer: it reports that layer as unreadable and keeps
// going, so a single broken file cannot block the rest of the reconciliation.
func (s *MarkdownLegacySource) Read(ctx context.Context) ([]LegacyEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, fmt.Errorf("memory: 打开旧记忆根目录 %s: %w", s.root, err)
	}
	defer func() { _ = root.Close() }()

	entries := readLegacyLayer(root, core.GlobalScope)
	agents, err := legacyAgentLayers(root)
	if err != nil {
		return nil, err
	}
	for _, name := range agents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries = append(entries, readLegacyLayer(root, core.Scope{Agent: name})...)
	}
	return entries, nil
}

// legacyAgentLayers 列出 `memory/agents/` 下实际存在的 Agent 层目录名（排序后返回，
// 导入顺序与目录遍历顺序无关）。目录不存在 = 没有任何 Agent 层记忆，合法空集。
// legacyAgentLayers lists the agent-layer directories that actually exist.
func legacyAgentLayers(root *os.Root) ([]string, error) {
	// `memory/agents/<name>/MEMORY.md` 往上两级就是 `memory/agents`——路径形状只由
	// `workspace.MemoryFilePath` 决定，这里不复制第二个字面量。
	dir := path.Dir(path.Dir(workspace.MemoryFilePath("__layer__")))
	handle, err := root.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: 打开旧记忆 Agent 目录 %s: %w", dir, err)
	}
	entries, readErr := handle.ReadDir(-1)
	closeErr := handle.Close()
	if readErr != nil {
		return nil, fmt.Errorf("memory: 读取旧记忆 Agent 目录 %s: %w", dir, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("memory: 关闭旧记忆 Agent 目录 %s: %w", dir, closeErr)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// readLegacyLayer 读一层并切成条目：核心区在前、归档区在后，均带分层与分区。
//
// 三种"读不出来"都产出 `Unreadable`（原因里带相对路径）：读取失败、结构损坏。空文件与
// 缺失文件**不**产出条目——它们本来就代表"这一层没有记忆"（与运行期 `loadLayer` 的
// 合法空层同一档），报成不可读会让每次迁移都出现假问题。
// readLegacyLayer splits one layer into entries; a missing or empty file is a legal empty
// layer rather than an unreadable one.
func readLegacyLayer(root *os.Root, scope core.Scope) []LegacyEntry {
	rel := workspace.MemoryFilePath(scope.Agent)
	raw, err := root.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []LegacyEntry{{
			Scope:      scope,
			Unreadable: fmt.Sprintf("%s：读取失败（%v）", rel, err),
		}}
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	coreEntries, archiveEntries, err := parseSections(text)
	if err != nil {
		return []LegacyEntry{{
			Scope:      scope,
			Unreadable: fmt.Sprintf("%s：%v", rel, err),
		}}
	}

	entries := make([]LegacyEntry, 0, len(coreEntries)+len(archiveEntries))
	for _, content := range coreEntries {
		entries = append(entries, LegacyEntry{Scope: scope, Section: core.SectionCore, Content: content})
	}
	for _, content := range archiveEntries {
		entries = append(entries, LegacyEntry{Scope: scope, Section: core.SectionArchive, Content: content})
	}
	return entries
}
