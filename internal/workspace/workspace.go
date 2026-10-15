package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// dirPerm 是工作区目录的权限：0o750（属主全权、同组可读可进入、其余不可见）。
// dirPerm is the workspace directory permission: owner rwx, group rx, others none.
//
// 取 0o750 而非 0o755：工作区里是 Agent 定义、长期记忆、日志与库文件（企业私有
// 数据），默认不给"其他用户"留读取面；同一台机器上的运维组仍可读（G301 的阈值
// 口径）。跨用户共享的需要（NFS / SMB 工作区）在需求里明确不做，日后要放宽
// 也是显式决策，不是默认值。
// 0750 rather than 0755: the workspace holds private enterprise data, and sharing
// it across users (NFS/SMB) is explicitly out of scope.
const dirPerm os.FileMode = 0o750

// filePerm 是引导文件的权限：0o644——内容不是秘密（模板正文），且父目录 0o750
// 已经挡住了目录穿越，"谁能进目录"才是这里真正的访问控制。
// filePerm is 0644: the templates are not secret, and the 0750 parent directory
// is what actually gates access.
const filePerm os.FileMode = 0o644

// InitReport 是 Init 的结果清单，两类条目都是相对工作区根的路径（目录不带尾斜杠）。
// InitReport lists what a single Init run created and what it left untouched;
// every entry is a path relative to the workspace root.
//
// 二跑幂等的判据就是 Created 为空；Kept 记录"打开时已存在、逐字节未动"的条目。
// An idempotent second run reports an empty Created slice.
type InitReport struct {
	Created []string
	Kept    []string
}

// Init 幂等创建 .basalt/ 工作区骨架：六个目录 + 五个引导文件（清单见 templates.go）。
// Init creates the .basalt/ skeleton idempotently: six directories and five
// bootstrap files.
//
// 三处纪律：
//  1. **已存在的一律不动**——目录记 Kept，文件连内容都不读、不写、不比较（逐字节
//     保持原样，运维手工改过的引导文件不会被 init 覆盖）；
//  2. **不预建 memory/agents/**——Agent 层记忆在 save_memory 时按需落盘（流程 A）；
//  3. 全部落盘操作经 os.Root 钉在工作区目录内——路径是调用方给的，越界（软连接
//     跳出去）在根上就被挡住，越是只写不读的路径越不该留这个口子。
//
// Existing entries are never touched, memory/agents/ is left to save_memory, and
// every filesystem call is confined to the workspace root.
func Init(root string) (report InitReport, err error) {
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return report, fmt.Errorf("创建工作区目录 %s: %w", root, err)
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return report, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	for _, rel := range directories {
		created, err := ensureDir(base, rel)
		if err != nil {
			return report, err
		}
		report.record(rel, created)
	}

	for _, tpl := range fileTemplates {
		created, err := ensureFile(base, tpl.path, tpl.content)
		if err != nil {
			return report, err
		}
		report.record(tpl.path, created)
	}

	return report, nil
}

// record 把一条结果计入清单：created=false 一律归 Kept。
// record files one outcome into the report; anything not created is kept.
func (r *InitReport) record(rel string, created bool) {
	if created {
		r.Created = append(r.Created, rel)
		return
	}
	r.Kept = append(r.Kept, rel)
}

// ensureDir 缺则建（含父目录），返回是否本次创建；已存在但**不是目录**时报错——
// 骨架里该是目录的位置被普通文件占了，静默跳过只会把问题推到后面更难懂的地方。
// ensureDir creates a missing directory, reports whether it created it, and
// rejects a same-named regular file rather than glossing over it.
func ensureDir(base *os.Root, rel string) (bool, error) {
	name := filepath.FromSlash(rel)

	info, err := base.Stat(name)
	switch {
	case err == nil:
		if !info.IsDir() {
			return false, fmt.Errorf("工作区路径 %s 已存在且不是目录", rel)
		}
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("检查工作区目录 %s: %w", rel, err)
	}

	if err := base.MkdirAll(name, dirPerm); err != nil {
		return false, fmt.Errorf("创建工作区目录 %s: %w", rel, err)
	}
	return true, nil
}

// ensureFile 缺则写模板，返回是否本次创建；已存在 → false（逐字节不动）。
// ensureFile writes the template only when the file is missing.
//
// 用 O_EXCL 而不是"先 Stat 再写"：这不只是少一次系统调用——已存在的判定由内核
// 在创建的那一刻做出，"存在即跳过"没有 TOCTOU 窗口（并发 init 时也只有一方写成）。
// O_EXCL rather than stat-then-write: existence is judged by the kernel at
// creation time, so "existing file wins" has no time-of-check window.
func ensureFile(base *os.Root, rel, content string) (bool, error) {
	file, err := base.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("创建工作区文件 %s: %w", rel, err)
	}

	if _, err := file.WriteString(content); err != nil {
		// 写失败也要收口：连接失败与关闭失败一并并入错误链，不留半截句柄。
		return false, errors.Join(fmt.Errorf("写入工作区文件 %s: %w", rel, err), file.Close())
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("关闭工作区文件 %s: %w", rel, err)
	}
	return true, nil
}
