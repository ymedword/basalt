package builtin

// 本文件是「模板 vs 已安装」的**只读**三态判别（review ② D-6 / C-55）。
//
// 为什么需要它：`Install` 对已存在目录只有两条路——原样保留，或 `--force` 整目录删掉
// 重建。前者让模板更新永远到不了实例，后者会把用户改过的提示词、脚本与 `runs/` 一起
// 清掉。运维需要第三条信息："这个目录现在和模板差在哪"，才能自己决定备份、合并还是
// 放着不动。本包只**报告**差异，**不**改写任何文件——覆盖永远由人显式发起。
//
// This file is the read-only three-way comparison between a bundled template and an
// installed directory. It only reports drift: overwriting always stays an explicit human
// action, because `--force` deletes the whole directory.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/examples"
)

// 三态字面量：与 `basalt demo install --dry-run` 的输出逐字对应（C-55）。
// The three literals the dry-run prints.
const (
	// TreeStateMissing 表示目标目录不存在——安装会新建。
	TreeStateMissing = "missing"

	// TreeStateIdentical 表示已存在且正文与模板一致——安装会保留，无需处理。
	TreeStateIdentical = "identical"

	// TreeStateModified 表示已存在但正文与模板不同——安装**仍会保留**，差异由用户决定。
	TreeStateModified = "modified"
)

// TreeComparison 是一棵模板树与已安装目录的比较结果。两个路径列表都是相对路径
// （斜杠分隔、按遍历顺序），供运维在输出里逐项点名。
// TreeComparison lists the drift by relative path.
type TreeComparison struct {
	// State 取 TreeStateMissing / TreeStateIdentical / TreeStateModified 之一。
	State string
	// Changed 是归一化后正文与模板不同的相对路径。
	Changed []string
	// Missing 是模板里有、目标目录里没有的相对路径（目录被改残缺时出现）。
	Missing []string
}

// CompareTree 比较内嵌模板 `srcRoot`（如 `daily/github-radar`）与已安装目录 `dstRoot`。
//
// 归一化只抹掉**安装器自己造成**的两处差异，其余一律算用户的修改：
//   - `{{BASALT_PYTHON}}` 占位符 → 安装时解析出的解释器路径（`interpreter` 为空时两侧
//     都不替换，保留占位符也算一致）；
//   - `AGENT.md` 只比 frontmatter 之后的**正文**：`provider.model`、`schedule`、`settings`
//     是安装后由运维填写的实例数据，不属于模板正文，填了不该被判成"改过模板"。
//
// 行尾统一成 LF 再比：素材在 Windows 检出上是 CRLF，检出方式不该制造假差异。
// 目标不存在 → `TreeStateMissing`；逐文件一致 → `TreeStateIdentical`；否则
// `TreeStateModified` 并列出差异路径。本函数**不写任何文件**。
//
// CompareTree normalises only the differences the installer itself creates (the python
// placeholder and everything before an AGENT.md frontmatter), then reports drift by path.
// It never writes.
func CompareTree(srcRoot, dstRoot string, skip map[string]bool, interpreter string) (TreeComparison, error) {
	if _, err := os.Stat(dstRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TreeComparison{State: TreeStateMissing}, nil
		}
		return TreeComparison{}, fmt.Errorf("检查目标 %s：%w", dstRoot, err)
	}

	comparison := TreeComparison{State: TreeStateIdentical}
	walkErr := fs.WalkDir(examples.Assets, srcRoot, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(filepath.FromSlash(srcRoot), filepath.FromSlash(p))
		if relErr != nil {
			return relErr
		}
		// 与 `CopyTree` 同一份跳过规则：`skills/` 由安装器单独装到 <workspace>/skills，
		// Agent 目录里本来就不该有它，拿它判漂移只会制造假阳性。
		// The same skip rule as CopyTree, so a correctly installed tree never looks dirty.
		if first := firstPathElement(rel); first != "" && skip[first] {
			return nil
		}
		want, readErr := fs.ReadFile(examples.Assets, p)
		if readErr != nil {
			return readErr
		}
		relSlash := filepath.ToSlash(rel)
		target := filepath.Join(dstRoot, rel)
		// #nosec G304 —— 路径由内嵌模板的相对路径拼出，根目录由调用方给定；这里只读。
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				comparison.Missing = append(comparison.Missing, relSlash)
				comparison.State = TreeStateModified
				return nil
			}
			return fmt.Errorf("读取 %s：%w", target, readErr)
		}
		if normalizeForCompare(rel, string(want), interpreter) != normalizeForCompare(rel, string(got), "") {
			comparison.Changed = append(comparison.Changed, relSlash)
			comparison.State = TreeStateModified
		}
		return nil
	})
	if walkErr != nil {
		return TreeComparison{}, walkErr
	}
	return comparison, nil
}

// normalizeForCompare 把一份正文归一到"安装器会写成什么样"：行尾统一 LF、替换解释器
// 占位符、`AGENT.md` 只留正文。两侧各调一次（模板侧带 interpreter，安装侧不带——安装
// 结果里已经没有占位符了）。
//
// 占位符替换**只对 `AGENT.md` 生效**：安装器只改这一个文件（`ResolveAgentPython`），
// SETUP.md 里讲"把 `{{BASALT_PYTHON}}` 换成解释器"的那几句是文档正文，装完仍是占位符
// ——对所有文件统一替换会把它们判成"已修改"，是纯假阳性。
// The placeholder substitution applies to AGENT.md only, which is the only file the
// installer rewrites; SETUP.md mentions the placeholder as prose and keeps it verbatim.
func normalizeForCompare(rel, content, interpreter string) string {
	text := strings.ReplaceAll(content, "\r\n", "\n")
	if filepath.Base(rel) != agentDefinitionFile {
		return text
	}
	if interpreter != "" {
		text = strings.ReplaceAll(text, PythonPlaceholder, interpreter)
	}
	return agentBody(text)
}

// agentBody 取 `AGENT.md` 第二个 `---` 分隔行之后的正文；没有 frontmatter 时返回原文。
// agentBody returns everything after the closing frontmatter fence.
func agentBody(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return text
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return text
}
