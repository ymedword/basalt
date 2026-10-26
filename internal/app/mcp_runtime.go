package app

// 本文件实现 MCP 诊断的「依赖」轴（设计 §2.3.4 / 契约 D-02）：回答"这条条目依赖的
// 运行时组件是否已就位、版本是否符合清单"。判据只有两件只读事实——运行时目录里的
// 组件路径与版本清单里的期望版本。**不**启动进程取版本（`node --version` 也是进程）、
// **不**安装、**不**访问网络。没有登记的组件（自定义命令、宿主 node / python）一律不
// 回答：视图输出 `null`，而不是把"没登记"伪装成"就绪"。
// The dependency axis reads the runtime directory and the version manifest only. It
// starts no process, installs nothing, and answers nil for unregistered components.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// mcpRuntimeComponents 是本仓声明的运行时组件与期望版本（设计 §2.1 的目录布局与
// §2.3.5 的首轮组件范围）。宿主解释器（node / python）**不**在此表：它们由用户环境
// 安装、设计未钉版本，本轴不替它们作结论（"命令能否定位"由命令轴回答）。新增组件必须
// 同时改这张表与文档——**不**接受"运行时目录里出现任何 `mcp/<name>` 都算登记"，那样
// 版本就没有可核对的基准，version_mismatch 永远不会被判断出来。
// The declared runtime components and their expected versions. Host interpreters stay
// out: unregistered components answer null rather than a guessed verdict.
var mcpRuntimeComponents = map[string]string{
	"dbhub":             "1.5.0",
	"brave-search":      "2.1.4",
	"fetch":             "2026.10.24",
	"github-mcp-server": "2.0.3",
	"docs-readonly":     "1.0.0",
}

// mcpDependencyProbe 按运行时根目录（`<程序目录>/runtime`）只读比对组件。
// runtimeRoot 为空表示本次装配拿不到程序目录（例如 `go run` 的临时二进制）：
// 该轴一律不回答，也不猜一个默认位置。
// mcpDependencyProbe compares components under the runtime root; an empty root answers
// nothing rather than guessing a default location.
type mcpDependencyProbe struct {
	runtimeRoot string
}

// programRuntimeRoot 是**程序目录**下的运行时根（设计 §2.1：`<程序目录>/runtime`）。
// 取不到自身可执行文件路径时返回空串——调用方据此"不回答 / 不写配置"，不退回"当前工作
// 目录"之类的猜测（猜错会把别的目录里的组件报成就绪，或把临时目录写进发布配置）。
// 依赖探测与开箱安装器共用这一处，两边的"运行时在哪"因此只有一个答案。
// programRuntimeRoot anchors on the program directory; an unresolvable executable answers
// empty, and both the dependency probe and the open-box installer share this one answer.
func programRuntimeRoot() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(executable), "runtime")
}

// newProgramMCPDependencyProbe 用**程序目录**下的 `runtime/` 装配依赖探测面；取不到时
// 返回空探测面，该轴输出 `null`。
// The probe anchors on the program directory; an unresolvable executable answers null.
func newProgramMCPDependencyProbe() mcpDependencyProbe {
	return mcpDependencyProbe{runtimeRoot: programRuntimeRoot()}
}

// Diagnose 回答一条条目的依赖轴：按 command → args 的顺序取第一个落在
// `<runtimeRoot>/mcp/<component>/<version>/…` 且 component 已登记的路径。一条 server
// 只回答一个组件（契约只有一个对象）；多个组件并存时以最先出现的为准。
// Diagnose answers at most one component per server: the first registered runtime path
// in command-then-args order.
func (p mcpDependencyProbe) Diagnose(cfg core.MCPServerConfig) *core.MCPDependencyDiagnosis {
	if p.runtimeRoot == "" {
		return nil
	}
	component, found, entryPath, ok := p.componentOf(cfg)
	if !ok {
		return nil
	}
	expected := mcpRuntimeComponents[component]
	diagnosis := &core.MCPDependencyDiagnosis{
		Component:       component,
		ExpectedVersion: expected,
	}
	// 存在性只信 `os.Stat`（与命令轴的绝对路径分支同一证据口径）：跑不起来的最常见
	// 原因就是组件没装全，这时只有 `missing` 是诚实的答案——版本不参与判定，免得把
	// "文件不在"报成"版本不符"。
	if _, err := os.Stat(entryPath); err != nil {
		diagnosis.State = core.MCPDependencyMissing
		return diagnosis
	}
	diagnosis.FoundVersion = found
	if found == expected {
		diagnosis.State = core.MCPDependencyReady
	} else {
		diagnosis.State = core.MCPDependencyVersionMismatch
	}
	return diagnosis
}

// componentOf 在命令与参数里认出已登记的运行时组件。
// componentOf finds the first registered runtime component among command and args.
func (p mcpDependencyProbe) componentOf(cfg core.MCPServerConfig) (string, string, string, bool) {
	candidates := make([]string, 0, len(cfg.Args)+1)
	candidates = append(candidates, cfg.Command)
	candidates = append(candidates, cfg.Args...)
	for _, candidate := range candidates {
		if component, version, entryPath, ok := p.parseRuntimePath(candidate); ok {
			return component, version, entryPath, true
		}
	}
	return "", "", "", false
}

// parseRuntimePath 判定一个候选是否落在 `<runtimeRoot>/mcp/<component>/<version>/…`。
// 只认绝对路径：相对入口由启动时的 `cmd.Dir` 解析，不启动就无法证明它落在运行时目录
// 里（与命令轴的 relative_path 同口径）。`mcp` 段之后必须同时有组件名与版本段——没有
// 版本段的路径无法与清单比对，按"未登记"处理。
// parseRuntimePath accepts absolute paths only, and requires both the component and
// version segments; a path without a version cannot be compared against the manifest.
func (p mcpDependencyProbe) parseRuntimePath(candidate string) (string, string, string, bool) {
	cleaned := filepath.Clean(strings.TrimSpace(candidate))
	if !filepath.IsAbs(cleaned) {
		return "", "", "", false
	}
	relative, err := filepath.Rel(filepath.Clean(p.runtimeRoot), cleaned)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", "", false
	}
	segments := strings.Split(relative, string(filepath.Separator))
	if len(segments) < 3 || segments[0] != "mcp" {
		return "", "", "", false
	}
	component, version := segments[1], segments[2]
	if _, registered := mcpRuntimeComponents[component]; !registered || version == "" {
		return "", "", "", false
	}
	return component, version, cleaned, true
}
