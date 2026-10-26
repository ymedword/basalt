package builtin

// 本文件是「示例脚本用哪个解释器」的唯一定义：随包 Python 的发行布局、宿主机探测，
// 以及 AGENT.md 里的解释器占位符替换。首次启动的开箱安装与 `basalt demo install`
// 都从这里取同一个答案——两处各算一份，写进 AGENT.md 的解释器迟早与
// `shell.allowed_commands` 对不上，示例脚本就会在 `sandbox_denied` 上反复失败。
//
// 为什么必须是绝对路径：受控执行器把 `executable` 与命令白名单**整串逐字**比对，
// 既不查 PATH、也不补 `.exe` / `.cmd` 后缀。写 `python` 只能在该字面量恰好被登记时
// 命中；随包运行时登记的是绝对路径，因此占位符也只替换成绝对路径。
// This file is the single definition of the interpreter used by the example scripts:
// the bundled runtime layout, the host probe and the AGENT.md placeholder substitution.
// The controlled executor compares the executable against the allowlist byte for byte,
// so the substitution must be an absolute path, never a bare interpreter name.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// PythonPlaceholder 是示例 AGENT.md 里的解释器占位符：安装时替换成本机解释器的绝对路径。
// PythonPlaceholder is the interpreter placeholder the installer replaces with an absolute path.
const PythonPlaceholder = "{{BASALT_PYTHON}}"

// pythonCandidates 按随包发行布局列出 Python 解释器的候选路径（按优先顺序）：
// Windows 发行包放 `runtime/python/python.exe`；官方 tarball 展开或
// python-build-standalone 是 `bin/python3`；`python`（无扩展名）留给手工摊平的安装。
// pythonCandidates lists the bundled interpreter paths in release-layout order.
func pythonCandidates(runtimeRoot string) []string {
	if strings.TrimSpace(runtimeRoot) == "" {
		return nil
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	pythonRoot := filepath.Join(runtimeRoot, "python")
	return []string{
		filepath.Join(pythonRoot, "python"+suffix),
		filepath.Join(pythonRoot, "bin", "python3"),
		filepath.Join(pythonRoot, "bin", "python"),
		filepath.Join(pythonRoot, "python"),
	}
}

// RuntimePython 返回随包 Python 解释器路径：取第一个存在的候选；都不存在时返回首选，
// 让写进配置与 AGENT.md 的路径在缺件时也与齐全时同形（缺什么由存在性检查点名）。
// RuntimePython is the bundled interpreter path; a missing bundle still answers the
// preferred candidate so the written configuration does not change shape.
func RuntimePython(runtimeRoot string) string {
	return FirstExisting(pythonCandidates(runtimeRoot)...)
}

// ResolvePython 依次尝试随包运行时与宿主 PATH：随包候选存在但跑不起来时**报错**，
// 不静默回落到宿主解释器——那会让 AGENT.md 里写下的路径与配置登记的解释器分家。
// 随包候选不存在（源码检出、未打包）时才探测 PATH。
// ResolvePython prefers the bundled runtime and only falls back to PATH when no bundled
// candidate exists; a present but broken bundle is an error, not a silent fallback.
func ResolvePython(ctx context.Context, runtimeRoot string) (string, error) {
	for _, candidate := range pythonCandidates(runtimeRoot) {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if probeErr := probePython(ctx, candidate); probeErr != nil {
			return "", fmt.Errorf("随包 Python %s %w", candidate, probeErr)
		}
		return candidate, nil
	}
	return FindPython(ctx)
}

// probePython 用固定探针判断一个候选能不能当 Python 3 用。Windows 上 `python` 常是
// Microsoft Store 的占位程序：LookPath 成功而执行即退出 9009——只信 LookPath 会把
// 「没有解释器」误报成「脚本失败」。
// probePython runs the fixed probe so a Store stub is never mistaken for an interpreter.
func probePython(ctx context.Context, path string) error {
	// #nosec G204 -- 解释器路径来自本次 LookPath / 发行布局，实参全是常量（-c + pythonProbe）。
	// #nosec G204 -- the path comes from LookPath or the release layout; argv are constants.
	output, err := exec.CommandContext(ctx, path, "-c", pythonProbe).CombinedOutput()
	if err != nil {
		return fmt.Errorf("不可执行（%w）", err)
	}
	if strings.TrimSpace(string(output)) != "3" {
		return errors.New("不是 Python 3")
	}
	return nil
}

// FirstExisting 返回第一个存在的候选路径；都不存在时返回第一个（空输入返回空串）。
// 发行布局的每个位置都给「优先候选 + 兼容候选」，取第一个存在的；都不存在时仍返回首选，
// 让写出的配置与齐全时逐字节相同。
// FirstExisting returns the first candidate on disk, else the first candidate itself.
func FirstExisting(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// SubstitutePython 把 AGENT.md 里的 {{BASALT_PYTHON}} 全部替换为解释器的绝对路径；
// 文件里没有占位符时零写入（重复安装幂等）。空解释器与非绝对路径一律拒绝——写进去的
// 那串必须能整串命中 `shell.allowed_commands`，"以后再说"的占位或裸名只会推迟失败。
// SubstitutePython replaces the placeholder with an absolute interpreter path and is a
// no-op when the file carries none; an empty or relative interpreter is refused.
func SubstitutePython(agentFile, interpreter string) (err error) {
	interpreter = strings.TrimSpace(interpreter)
	if interpreter == "" {
		return errors.New("解释器为空：拒绝写入空串")
	}
	if !filepath.IsAbs(interpreter) {
		return fmt.Errorf("解释器必须是绝对路径（收到 %q）：命令白名单按整串逐字比对，裸名不会命中", interpreter)
	}
	// 读写都经 os.Root 钉在 AGENT.md 所在目录内，与 BlankModel 同一口径：目标是安装器
	// 拼出来的固定文件名，用根句柄把"只能是这个目录里的这个名字"写进代码。
	// Both the read and the write go through an os.Root on the agent directory.
	dir, err := os.OpenRoot(filepath.Dir(agentFile))
	if err != nil {
		return fmt.Errorf("打开 %s 所在目录：%w", agentFile, err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("关闭 %s 所在目录：%w", agentFile, closeErr)
		}
	}()

	name := filepath.Base(agentFile)
	raw, err := dir.ReadFile(name)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", agentFile, err)
	}
	if !strings.Contains(string(raw), PythonPlaceholder) {
		return nil
	}
	text := strings.ReplaceAll(string(raw), PythonPlaceholder, interpreter)
	target, err := dir.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("写出 %s：%w", agentFile, err)
	}
	if _, err := target.WriteString(text); err != nil {
		closeErr := target.Close()
		if closeErr != nil {
			return fmt.Errorf("写出 %s：%w（关闭也失败：%v）", agentFile, err, closeErr)
		}
		return fmt.Errorf("写出 %s：%w", agentFile, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("写出 %s：%w", agentFile, err)
	}
	return nil
}

// ResolveAgentPython 对**已存在**的 AGENT.md 做占位符替换，文件不存在时静默跳过：
// 保留态目录（StateKept）也可能还留着未替换的占位符——旧版本装出来的 Agent 就是这样，
// 升级后第一次安装 / 启动要能把它们对齐，否则示例脚本会一直 `sandbox_denied`。
// 只替换占位符、不动别的内容，因此对用户改过模型绑定的既有 Agent 也是安全的；
// 而 BlankModel 只在**新装**时调用，绝不拿它碰保留态。
// ResolveAgentPython substitutes the placeholder in an existing AGENT.md and silently
// skips a missing file, so upgraded installs get aligned without touching anything else.
func ResolveAgentPython(agentFile, interpreter string) error {
	if strings.TrimSpace(interpreter) == "" {
		return nil
	}
	if _, err := os.Stat(agentFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("检查 %s：%w", agentFile, err)
	}
	return SubstitutePython(agentFile, interpreter)
}
