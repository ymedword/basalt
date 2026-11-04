package tool

import (
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// interpreterBase 同时识别两种路径分隔符，不通过可执行路径前缀推断信任。
// interpreterBase handles both path separators without inferring trust from path prefixes.
func interpreterBase(exe string) string {
	return filepath.Base(strings.ReplaceAll(exe, `\`, "/"))
}

func interpreterSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[interpreterBase(name)] = true
	}
	return set
}

// validateControlledCommand 共用解释器形态与脚本归属判据，保留本机绝对受控脚本兼容性。
// validateControlledCommand shares interpreter and script checks while preserving trusted local absolute paths.
func validateControlledCommand(cmd core.Command, interpreters map[string]bool, roots []string) error {
	if !interpreters[interpreterBase(cmd.Executable)] {
		return nil
	}
	if len(cmd.Args) == 0 || cmd.Args[0] == "" || strings.HasPrefix(cmd.Args[0], "-") {
		return deny(ruleInterpreterShape, "脚本参数", guidanceInterpreterShape)
	}
	script := cmd.Args[0]
	real, err := resolveRealPath(cmd.Dir, script)
	if err != nil {
		return deny(ruleScriptOutside, script, guidanceScriptOutside)
	}
	for _, root := range roots {
		rootReal, err := resolveRealPath("", root)
		if err == nil && withinDir(real, rootReal) {
			return nil
		}
	}
	return deny(ruleScriptOutside, script, guidanceScriptOutside)
}

// translateScriptArg 仅投影已落在任务只读挂载内的脚本。
// translateScriptArg maps only scripts inside the read-only task mount.
func translateScriptArg(hostDir, script, taskDir string) (string, error) {
	real, err := resolveRealPath(hostDir, script)
	root, rootErr := resolveRealPath("", taskDir)
	if err != nil || rootErr != nil || !withinDir(real, root) || real == root {
		return "", deny(ruleScriptOutside, script, guidanceScriptOutside)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return "", err
	}
	return "/task/" + filepath.ToSlash(rel), nil
}
