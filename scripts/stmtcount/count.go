// Package main 实现 stmtcount：按包统计可执行语句数，供覆盖率判卷脚本判定
// 「无可执行语句的包」（该栏跳过）与「有语句但无测试文件的包」（该栏计 0%）。
// Package main implements stmtcount: a per-package executable-statement count
// that the coverage judge uses to tell "no statements" (skip) apart from
// "statements but no tests" (count as zero).
//
// 本工具只依赖标准库（go/parser），不进生产二进制。
// It uses the standard library only and never ships in the product binary.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Count 统计 dir 目录下该包的可执行语句数。
// Count returns the number of executable statements in the package at dir.
//
// 口径（固定）：
//   - 只读非测试 `.go` 文件（`_test.go` 不计），不递归子目录——Go 的一个包
//     就是同目录的一批文件；
//   - 语句 = `ast.Stmt` 节点，但排除 `ast.BlockStmt`（语句容器）与
//     `ast.EmptyStmt`（空语句）；顶层声明（const / var / type / 函数签名）
//     不是可执行语句。
//
// 消费方只判「零 / 非零」，故数值本身允许是近似；零值判定必须可靠。
// Consumers only distinguish zero from non-zero, so the number may be an
// approximation while the zero verdict must be exact.
//
// dir 不存在或任一文件解析失败 → 返回错误（不吞）。
// A missing directory or an unparsable file is reported, never swallowed.
func Count(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files) // 顺序稳定，错误点可复现

	total := 0
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return 0, fmt.Errorf("解析 %s: %w", path, err)
		}
		total += countStatements(file)
	}
	return total, nil
}

// countStatements 数单个文件里的可执行语句。
// countStatements counts the executable statements of one parsed file.
func countStatements(file *ast.File) int {
	total := 0
	ast.Inspect(file, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.BlockStmt, *ast.EmptyStmt:
			// 块与空语句本身不执行——只作为容器存在，不计入。
		case ast.Stmt:
			total++
		}
		return true
	})
	return total
}
