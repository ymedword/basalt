package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newProviderCommand 组装 provider 命令组：本 US 只交付 list 一条子命令。
// newProviderCommand builds the provider command group; this US ships list only.
func newProviderCommand(deps Deps) *cobra.Command {
	provider := &cobra.Command{
		Use:   "provider",
		Short: "管理 LLM 供应商配置（本阶段：list）",
		Long: "管理 LLM 供应商配置。本阶段只读：list 逐条列出已配置的 Provider。\n" +
			"Manage the LLM vendor configuration; this phase is read-only and " +
			"provides the list subcommand.",
		// 与根命令同一形态：可运行的父命令 + 显式未知命令分支（不可运行的父命令会
		// 让 cobra 把未知子命令按"请求帮助"处理并以 0 退出）。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}
	provider.AddCommand(newProviderListCommand(deps))
	return provider
}

// newProviderListCommand 组装 provider list。
// newProviderListCommand builds provider list.
//
// 轻路径（技术方案 §6.1.2）：只读配置文件——**不开库、不取实例锁、不做恢复、不构造
// 客户端**，故 Deps 里的两个端口在这一路上一次都不碰（判卷侧干脆注入 nil：任何
// 意外的开库动作都会当场 panic 而不是悄悄通过）。
// A light path: it reads the configuration file only and never touches the store
// or workspace ports.
func newProviderListCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已配置的 Provider（只读配置）",
		Long: "逐条列出配置里的 Provider：名称、脱敏地址与缺省模型，不含凭证值。\n" +
			"List the configured providers with redacted base URLs; credentials " +
			"are never printed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProviderList(cmd.OutOrStdout(), deps)
		},
	}
}

// runProviderList 装载配置后逐条渲染；三种失败形态各自点名（缺文件点名路径、段空
// 点名段名、配置无效原错误上抛），与 status 的容忍语义**刻意不同**——「列出已配置
// 的 Provider」在没有 Provider 时属命令目的未达成（颗粒度文档 §2.4 裁决注 2）。
// runProviderList loads the configuration and renders it; all three failure shapes
// name what is missing. Empty means failure here, deliberately unlike status.
func runProviderList(out io.Writer, deps Deps) error {
	path := filepath.Join(deps.WorkDir, configFileName)

	cfg, err := LoadConfig(path, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e := fmt.Errorf("未发现配置（%s）——先创建 basalt.yaml 并配置 providers 段", path)
		deps.Logger.Error("provider list 未通过：未发现配置", "path", path, "error", e)
		return e
	case err != nil:
		// 原错误上抛：装载层已点名键路径与变量名，且不含凭证值（US-1 的口径照用）。
		deps.Logger.Error("provider list 未通过：配置无效", "path", path, "error", err)
		return err
	case len(cfg.Providers) == 0:
		e := fmt.Errorf("providers 段为空（%s）——至少配置一家 Provider", path)
		deps.Logger.Error("provider list 未通过：providers 段为空", "path", path, "error", e)
		return e
	}

	renderProviderList(out, cfg.Providers)
	return nil
}

// renderProviderList 逐条渲染 + 汇总行。与装载分开成一小段，是为了让展示层能被
// **直接注入**条目判卷（配置面已拒绝的地址成分，展示层仍要按收窄规则渲染——
// 纵深防御不依赖上一层已拦干净）。
// renderProviderList is split out so the presentation layer can be judged with
// injected entries: it narrows every address on its own, never trusting the
// loader to have rejected the unsupported parts first.
func renderProviderList(out io.Writer, entries []ProviderConfig) {
	for _, entry := range entries {
		fmt.Fprintf(out, "  %s：%s（缺省模型 %s，凭证 %s）\n",
			entry.Name,
			redactedBaseURL(entry.BaseURL),
			orDefault(entry.DefaultModel, "未配置"),
			credentialState(entry.APIKey))
	}
	fmt.Fprintf(out, "汇总：%d 家 Provider\n", len(entries))
}

// credentialState 只报"已配置 / 未上送"两种状态——**值不回显**（约束 8：凭证值
// 不得出现在输出、日志与错误文案里）。
// credentialState reports configured / not supplied and never the value.
func credentialState(apiKey string) string {
	if apiKey == "" {
		return "未上送"
	}
	return "已配置"
}

// redactedBaseURL 是展示层的收窄渲染：只留 scheme://host/path 三部分，userinfo /
// query / fragment 一律不出现在输出里（颗粒度文档 §2.4 裁决注 9）。
// redactedBaseURL keeps scheme://host/path only; userinfo, query and fragment
// never reach the output.
//
// 收窄规则**自实现**，不从 provider 包复用：命令面不得 import 实现包（依赖方向
// 红线，ports_test.go 判卷），这是同一份口径的第二处实现，不是同一份代码。
// The rule is implemented here on purpose: the command surface must not import the
// implementation package, so this is a second implementation of the same rule.
//
// 解析不出 scheme + host 的取值**不原样回显**——回显等于把未脱敏的原文当输出，
// 而这一层恰恰是防"上游没拦干净"的场合。
// Anything without a scheme and host is hidden rather than echoed: echoing would
// print the un-narrowed original, which is exactly what this layer guards against.
func redactedBaseURL(raw string) string {
	if raw == "" {
		return "未配置"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "（不可识别——已隐去）"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
}
