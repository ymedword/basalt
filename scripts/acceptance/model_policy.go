package main

import (
	"errors"
	"strings"
)

// offlineModelPolicy 在启动子进程前拒绝真实模型验证；历史授权不能绕过用户的新裁决。
// offlineModelPolicy rejects real-model validation before starting any child process.
func offlineModelPolicy(checks []check, goflags string) error {
	if strings.Contains(goflags, "-tags=") && realBuildTags("-tags="+goflags) {
		return errors.New("automatic validation forbids real-model GOFLAGS")
	}
	for _, value := range strings.Fields(goflags) {
		if strings.HasPrefix(strings.TrimLeft(value, "-"), "overlay") || realBuildTags(value) {
			return errors.New("automatic validation forbids real-model or overlay GOFLAGS")
		}
	}
	for _, c := range checks {
		args := c.Args
		goExecution := executableGoCommand(args)
		for i, value := range args {
			flag := strings.TrimLeft(value, "-")
			if strings.HasPrefix(flag, "provider=") && flag != "provider=" || flag == "provider" && i+1 < len(args) && args[i+1] != "" {
				return errors.New("automatic validation forbids real model providers")
			}
			if goExecution && (strings.HasPrefix(flag, "overlay") || realBuildTags(value) || flag == "tags" && i+1 < len(args) && realBuildTags("-tags="+args[i+1])) {
				return errors.New("automatic validation forbids executable real-model tests; historical authorization is not reusable")
			}
		}
	}
	return nil
}

// executableGoCommand 识别带 -C/--C 的执行命令，保留 vet 等只读装载。
// executableGoCommand recognizes directory flags before test/run while allowing vet.
func executableGoCommand(args []string) bool {
	if len(args) < 2 || args[0] != "go" {
		return false
	}
	rest := args[1:]
	for len(rest) > 0 {
		flag := strings.TrimLeft(rest[0], "-")
		if flag == "C" && len(rest) > 1 {
			rest = rest[2:]
		} else if strings.HasPrefix(flag, "C=") {
			rest = rest[1:]
		} else {
			return rest[0] == "test" || rest[0] == "run"
		}
	}
	return false
}

// realBuildTags 匹配完整 real 标签，避免误判仅加载源码的 vet 命令。
// realBuildTags recognizes an exact real build tag, including comma-separated lists.
func realBuildTags(value string) bool {
	value = strings.TrimLeft(value, "-")
	if !strings.HasPrefix(value, "tags=") {
		return false
	}
	for _, tag := range strings.FieldsFunc(strings.TrimPrefix(value, "tags="), func(r rune) bool { return r == ',' || r == ' ' || r == '\'' || r == '"' }) {
		if tag == "real" {
			return true
		}
	}
	return false
}

// offlineModelEnvironment 移除已知真实模型入口，保留数据库和合成夹具环境。
// offlineModelEnvironment strips known paid-model secrets without removing fixture keys.
func offlineModelEnvironment(environ []string) []string {
	filtered := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "BASALT_REAL_") || strings.HasPrefix(key, "BASALT_DEV_EMBEDDING_") ||
			key == "DASHSCOPE_API_KEY" || key == "DEEPSEEK_API_KEY" || key == "DEEPSEEK_BASE_URL" ||
			key == "OPENAI_API_KEY" || key == "ANTHROPIC_API_KEY" || key == "BASALT_US19_GATEWAY_TOKEN" || key == "BASALT_US19_DEMO_GATEWAY" || key == "BASALT_AUTOMATED_VALIDATION" {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "BASALT_AUTOMATED_VALIDATION=1")
}
