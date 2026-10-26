package builtin

// 本文件是样例只读文档库的安装面：`docs-diff` Demo 的输入是**一个专库存档**，不是整个
// 工作区。内置只读 server 的 `-root` 指向这里，库里的文档数因此稳定、可复现；不指
// `-root` 时 server 会落在进程当前目录——真实实例上那就是整个工作区，`list_documents`
// 会把 `runs/`、`sessions/`、`versions/` 这些生成物也算成文档，逐份取元信息随即烧完
// 工具预算（2026-10-27 实测：36 份文档 × 1 次 metadata = 50 次上限用尽）。
// This file installs the sample read-only document library the docs-diff demo diffs.
// Pinning the server root here keeps the document set small and reproducible: without
// `-root` the server falls back to the workspace, where generated files count as documents.

import (
	"fmt"
	"os"
	"path/filepath"
)

// SampleDocsPath 是样例只读文档库的固定落点（安装、MCP 条目与报告共用一份来源）。
// SampleDocsPath is the single source of the sample document library location.
func SampleDocsPath(wsRoot string) string {
	return filepath.Join(wsRoot, "samples", "docs")
}

// DocsLibrary 记录样例文档库的安装结果：State 取 installed / kept。
// DocsLibrary records the library outcome.
type DocsLibrary struct {
	Path  string
	State string
}

// EnsureDocsLibrary 把内嵌的共享夹具文档复制成样例只读库；已存在且不覆盖时保持逐字节
// 不变（用户往库里放自己的文档后，重装不会把它们抹掉）。
// EnsureDocsLibrary copies the shared fixture docs into the sample library; an existing
// library is kept byte for byte unless force is set.
func EnsureDocsLibrary(wsRoot string, force bool) (DocsLibrary, error) {
	target := SampleDocsPath(wsRoot)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return DocsLibrary{}, fmt.Errorf("创建样例目录 %s：%w", filepath.Dir(target), err)
	}
	state, err := CopyTree("shared/fixtures/docs", target, force, nil)
	if err != nil {
		return DocsLibrary{}, fmt.Errorf("安装样例文档库：%w", err)
	}
	return DocsLibrary{Path: target, State: state}, nil
}
