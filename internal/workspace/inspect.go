package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/core"
)

// Report 是工作区的只读清点结果：根路径 + 缺失条目清单（相对路径，拼法与
// InitReport 一致）。Missing 为空即骨架齐备。
// Report is the read-only inventory of a workspace: the root plus the entries
// that are missing. An empty Missing means the skeleton is complete.
//
// 只报"缺什么"不报"有什么"：清点方（status 节 1）要判的就是"能不能用"，而齐备时
// 列出全部条目只是噪音。
// Only absences are listed; a complete skeleton needs no enumeration.
type Report struct {
	// Root 是清点的工作区根路径（调用方给的，原样回显，便于报错时明示路径）。
	Root string

	// Missing 是应存在而缺失的条目（相对 Root 的路径）；类型不符（该是目录却是
	// 普通文件）同样计入——两者都让工作区不可用。
	Missing []string
}

// Inspect 只读清点工作区骨架：逐项核对六个目录与五个引导文件，返回缺失清单。
// Inspect checks the six directories and five bootstrap files and reports what is
// missing.
//
// 未初始化（.basalt/ 本身不存在）返回 core.ErrWorkspaceNotInitialized——这是
// "没跑过 init"这一类的判读，与"跑过但骨架被改坏了"（返回 Missing 非空的
// Report）是两种不同的诊断，调用方据此给出不同文案。
// A missing root reports ErrWorkspaceNotInitialized; an existing but incomplete
// skeleton comes back as a Report, which is a different diagnosis.
//
// 只读：不建、不修、不碰任何字节——status 是纯诊断。
// Read-only: nothing is created, repaired or written.
func Inspect(root string) (report Report, err error) {
	report = Report{Root: root}

	base, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return report, fmt.Errorf("%w: %s", core.ErrWorkspaceNotInitialized, root)
		}
		// 目录在但打不开（权限、不是目录）——原样上抛，不冒充"未初始化"。
		return report, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	for _, rel := range directories {
		missing, err := missingIn(base, rel, true)
		if err != nil {
			return report, err
		}
		if missing {
			report.Missing = append(report.Missing, rel)
		}
	}
	for _, tpl := range fileTemplates {
		missing, err := missingIn(base, tpl.path, false)
		if err != nil {
			return report, err
		}
		if missing {
			report.Missing = append(report.Missing, tpl.path)
		}
	}

	// 清点顺序固定（目录后文件），Missing 为 nil 而非空切片——两种"空"在
	// 断言与打印里表现不同，统一成前者。
	if len(report.Missing) == 0 {
		report.Missing = nil
	}
	return report, nil
}

// missingIn 判定 root 内某条目是否不可用：不存在，或类型与期望不符（该是目录却是
// 普通文件）。返回错误即"查不动"——区别于"查到了但没有"。
// missingIn reports whether an entry is unusable: absent, or of the wrong kind.
func missingIn(base *os.Root, rel string, wantDir bool) (bool, error) {
	info, err := base.Stat(filepath.FromSlash(rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("检查工作区条目 %s: %w", rel, err)
	}
	return info.IsDir() != wantDir, nil
}
