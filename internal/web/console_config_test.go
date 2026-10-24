package web

// 本文件是管理台写面的 Go 侧源码校验（模块卡 R4.12 的 `012-console-config`，B7）：
// 源码不得出现危险 DOM 注入点、12 个资源 / 凭据错误码的页面文案必须齐全、绑定回填
// 的证据缺口文案必须存在。前端单元测试与 `typecheck` / `build` 由 CI console job 与
// 统一验收单独执行——Go 门禁不调用 Node（约束 46）；这里只做文本级判据。
// Go-side source checks for the write surface: no dangerous DOM, all twelve
// resource/credential error copies present, and the binding backfill evidence-gap copy.
// Front-end tests and builds stay in the CI console job (constraint 46).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// consoleSourceRoot 解析 `assets/console/src`：`go test` 的工作目录是包目录，因此
// 从 `internal/web` 上溯两级到仓库根。目录缺失即判失败——源码不在，判据无从谈起。
// consoleSourceRoot resolves the console sources from the package directory.
func consoleSourceRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "assets", "console", "src"))
	require.NoError(t, err)
	info, err := os.Stat(root)
	require.NoError(t, err, "前端源码目录缺失：%s", root)
	require.True(t, info.IsDir(), "前端源码路径不是目录：%s", root)
	return root
}

// consoleSourceFiles 读出 `src` 下全部 `.ts` 文件（路径相对 src，正斜杠）。
// consoleSourceFiles reads every TypeScript source under src.
func consoleSourceFiles(t *testing.T) map[string]string {
	t.Helper()
	root := consoleSourceRoot(t)

	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ts") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, files, "前端源码为空：至少应包含 api.ts 与视图实现")
	return files
}

// TestConsoleConfig_NoDangerousDOM —— FR-044 / 约束 40：管理台只用既有静态壳与自实现
// 渲染，任何把服务端字符串当 HTML 解析的注入点都必须不存在。
// 逆改点：任一源文件出现 `innerHTML` / `outerHTML` / `insertAdjacentHTML` /
// `document.write` / `v-html` → 立即红。
func TestConsoleConfig_NoDangerousDOM(t *testing.T) {
	t.Parallel()
	files := consoleSourceFiles(t)

	tokens := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "v-html"}
	for name, source := range files {
		for _, token := range tokens {
			require.NotContains(t, source, token,
				"%s 出现危险 DOM 注入点 %q：管理台不得把字符串当 HTML 解析", name, token)
		}
	}
}

// resourceErrorCodes 是契约 run-and-errors.md 的 12 个资源 / 凭据错误码；页面文案与
// 服务端固定短语互补，缺一条用户就看不到"下一步怎么做"。
// resourceErrorCodes lists the twelve contracted resource/credential codes.
var resourceErrorCodes = []string{
	"AGENT_RESOURCE_BINDING_MISSING",
	"AGENT_RESOURCE_BINDING_INVALID",
	"AGENT_RESOURCE_UNAVAILABLE",
	"RESOURCE_NOT_ELIGIBLE",
	"CREDENTIAL_REVOKED",
	"CREDENTIAL_UNAVAILABLE",
	"CREDENTIAL_MODE_REQUIRED",
	"CREDENTIAL_REF_READ_ONLY",
	"CREDENTIAL_OWNER_MISMATCH",
	"RESOURCE_SCOPE_IN_USE",
	"RESOURCE_UID_MISMATCH",
	"AGENT_COPY_BLOCKED",
}

// TestConsoleConfig_ErrorCopyPresent —— FR-044：12 个错误码在 `RESOURCE_ERROR_COPY`
// 里必须各有一条非空页面文案，且码表恰为 12 项（多出的条目会让文案与契约分叉）。
// 逆改点：删一条码 / 清空一条文案 / 码表混入契约外代码 → 各自断言红。
func TestConsoleConfig_ErrorCopyPresent(t *testing.T) {
	t.Parallel()
	files := consoleSourceFiles(t)
	api, ok := files["api.ts"]
	require.True(t, ok, "api.ts 必须存在：错误文案码表是写面的唯一文案来源")

	start := strings.Index(api, "export const RESOURCE_ERROR_COPY")
	require.GreaterOrEqual(t, start, 0, "api.ts 缺少 RESOURCE_ERROR_COPY 码表")
	block := api[start:]
	if end := strings.Index(block, "\n}"); end >= 0 {
		block = block[:end]
	}

	keys := regexp.MustCompile(`(?m)^\s{2}([A-Z][A-Z0-9_]+):`).FindAllStringSubmatch(block, -1)
	require.Len(t, keys, len(resourceErrorCodes), "码表条目数必须与契约一致：%s", block)

	found := map[string]bool{}
	for _, key := range keys {
		found[key[1]] = true
	}
	for _, code := range resourceErrorCodes {
		require.True(t, found[code], "码表缺少 %s", code)
		require.Contains(t, block, code+":", "码表缺少 %s 的条目", code)
	}

	// 每条文案都必须是"下一步怎么做"的完整句子：只留下码名不算文案。
	copies := []string{
		"请显式选择资源后重新保存。",
		"请重新选择当前资源。",
		"请先发布资源或重新关联。",
		"资源不属于该 Agent",
		"请在该资源上重新录入凭据（旧版本不会复活）。",
		"请检查 secrets.master_key_file 与文件权限。",
		"请显式选择「环境变量」或「加密托管」。",
		"credential_ref 是服务端只读字段",
		"凭据属于其他资源",
		"先解除，再切换为专属。",
		"请重新读取后再操作。",
		"请为目标 Agent 选择映射资源。",
	}
	require.Len(t, copies, len(resourceErrorCodes))
	for _, copy := range copies {
		require.Contains(t, block, copy, "错误码文案缺失：%s", copy)
	}
}

// TestConsoleConfig_BackfillEvidenceCopy —— R4 裁决：无法证明连续性的绑定只标
// 「引用未记录」，不补造身份；页面必须把这个证据缺口说清楚，否则用户会把"未记录"
// 当成"没有引用"。
// 逆改点：文案改成推断性说法（如按名称显示属主）→ 本断言红。
func TestConsoleConfig_BackfillEvidenceCopy(t *testing.T) {
	t.Parallel()
	files := consoleSourceFiles(t)
	detail, ok := files["views/resource-detail.ts"]
	require.True(t, ok, "views/resource-detail.ts 必须存在：引用视图在资源详情页")

	require.Contains(t, detail, "引用未记录（迁移前无法证明身份）",
		"绑定回填缺证据时必须逐字显示「引用未记录（迁移前无法证明身份）」")
}
