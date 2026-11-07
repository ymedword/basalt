package assets

// 本文件是 `basalt assets export|install|verify` 的入口实现（模块卡 §3.1.11 /
// §3.1.12）。命令面**只**有这三个子命令，不做"列出最新版本"之类的解析型操作——
// 安装目录只由 hash 寻址，没有 current 指针可供枚举。`install` 的两种形态对应
// bundle 的两个来源：现成目录（跨机器分发）与工作区当前定义（首次部署 / 升级）。
// This file implements the three assets subcommands; install accepts either a
// prebuilt directory or --agent, and nothing in the layout resolves by recency.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// Deps 是 CLI 入口的装配注入（模块卡 §3.1.11）。CommandRoot 自己不读数据库、
// 不读配置：资产根与"写共享发布记录"两个能力都由装配层注入，这样本包只依赖 core
// 的纯类型，不把存储与业务耦合进资产层。
//
// `Publish` 对 `install` 是**必需**的：模块卡 §2.3.9 的首次初始化由显式安装写入
// 首个指针（INSERT，非 CAS），已存在指针走 CAS 更新协议。把"只装本地、不发布"做成
// 可选分支等于给 §2.3.9 留后门——缺接线时必须失败，**不得**静默降级。
// Deps injects the asset root and the publish step. Publish is required for install:
// a local-only install would silently skip the shared pointer and reintroduce the
// divergence §2.3.9 exists to remove.
type Deps struct {
	// AssetRoot 是 `assets.bundle_root`（必须非空；空值即配置缺陷，不是"缺省"）。
	AssetRoot string
	// Publish 把一个已安装的 bundle 写进共享发布记录并推进指针，返回本次发布的
	// `published_version_id`。实现由 app 装配层提供（EnsureVersion 的哈希幂等 +
	// 指针 CAS），本包不感知存储细节。
	Publish func(ctx context.Context, installed Installed) (publishedVersionID int64, err error)
	// Build 从工作区把**一个 Agent 的当前定义**构建成 bundle 到 `dest`（首次部署与
	// 内容升级的起点）。实现同样由 app 装配层提供：它读工作区、读技能发布记录，
	// 本包不感知工作区布局与存储细节。`install --agent` 缺它即失败关闭——没有
	// 构建入口时"先 install 再 export"是环形的，首次部署无从开始。
	// Build assembles one agent's current definition into dest; install --agent fails
	// closed without it, because otherwise the first bundle can never come to exist.
	Build func(ctx context.Context, agent, dest string) (Manifest, error)
}

// assetsUsage 是命令用法；错误信息一律带它，避免"报错了但不知道该怎么敲"。
// 两种 install 形态都列出来：`<bundle-dir>` 装现成 bundle（跨机器分发），
// `--agent <agent>` 从工作区构建后安装（首次部署 / 内容升级）。
// Both install forms are listed: a prebuilt bundle for cross-host distribution, and
// --agent for building the agent's current definition in place.
const assetsUsage = "basalt assets <export <bundle-dir> <dest-dir> | " +
	"install <bundle-dir> | install --agent <agent> | verify <bundle-dir>>"

// CommandRoot 执行一次 `basalt assets` 子命令，返回给人读的一行/多行摘要。
//
// 三个子命令都先校验再动手：
//   - verify：只复算，不写任何东西；
//   - export：先 Verify 源，再逐文件比对复制，最后自校验目标；
//   - install：先 Verify 源（截断 / 损坏在这里被拒，目标目录一个字节都没动），
//     再安装到 <asset_root>/bundles/<agent>/<bundle_hash>，最后由 Publish 推进
//     共享指针。
//
// 错误信息只含路径、哈希与原因，**不回显文件内容**（资产可能是私有配置）。
// It validates before acting on every path, and never echoes file contents.
func CommandRoot(ctx context.Context, deps Deps, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("缺少子命令：%s", assetsUsage)
	}
	switch args[0] {
	case "verify":
		if len(args) != 2 {
			return "", fmt.Errorf("verify 需要且只需要一个 bundle 目录：%s", assetsUsage)
		}
		manifest, err := Verify(ctx, args[1])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("校验通过：agent=%s bundle=%s 配置文件=%d 技能=%d 文件总数=%d",
			manifest.Agent, manifest.BundleHash, len(manifest.ConfigFiles), len(manifest.Skills), totalFiles(manifest)), nil

	case "export":
		if len(args) != 3 {
			return "", fmt.Errorf("export 需要源 bundle 目录与目标目录：%s", assetsUsage)
		}
		src, dest := args[1], args[2]
		if src == dest {
			return "", errors.New("export 的源目录与目标目录不得相同")
		}
		manifest, err := Verify(ctx, src)
		if err != nil {
			return "", fmt.Errorf("校验导出源: %w", err)
		}
		if err := Export(ctx, manifest, src, dest); err != nil {
			return "", err
		}
		return fmt.Sprintf("已导出：agent=%s bundle=%s 文件总数=%d\n目标目录：%s",
			manifest.Agent, manifest.BundleHash, totalFiles(manifest), dest), nil

	case "install":
		agent, bundleDir, err := parseInstallArgs(args[1:])
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(deps.AssetRoot) == "" {
			return "", fmt.Errorf("%w: assets.bundle_root 不得为空", core.ErrAssetUnavailable)
		}
		if deps.Publish == nil {
			return "", errors.New("资产发布未接线：install 必须同时写入共享发布记录（不得只装本地）")
		}
		source := bundleDir
		if agent != "" {
			// `--agent`：先按工作区当前内容构建到暂存目录，再走与 `<bundle-dir>`
			// 完全相同的安装与发布路径——构建不是第二条安装路径，只是"bundle 从
			// 哪来"的另一半答案。
			// --agent builds into a staging directory first and then runs the very same
			// install and publish path; it is not a second way to install.
			if deps.Build == nil {
				return "", errors.New("资产构建未接线：install --agent 需要装配层注入的构建入口")
			}
			staging, mkErr := os.MkdirTemp("", "basalt-assets-build-")
			if mkErr != nil {
				return "", fmt.Errorf("创建构建暂存目录: %w", mkErr)
			}
			defer func() { _ = os.RemoveAll(staging) }() // 安装会把结果复制到 hash 目录
			if _, buildErr := deps.Build(ctx, agent, staging); buildErr != nil {
				return "", buildErr
			}
			source = staging
		}
		installed, err := Install(ctx, source, deps.AssetRoot)
		if err != nil {
			return "", err
		}
		versionID, err := deps.Publish(ctx, installed)
		if err != nil {
			// 本地目录已就位但共享指针未推进：目录无人引用即无害（§2.3.9），
			// 下一次 install 是幂等的；错误照实上抛，不宣称"已发布"。
			return "", fmt.Errorf("已安装到 %s，但发布指针未推进: %w", installed.Dir, err)
		}
		return fmt.Sprintf("已安装并发布：agent=%s bundle=%s 文件总数=%d\n安装目录：%s\npublished_version_id=%d",
			installed.Manifest.Agent, installed.Manifest.BundleHash,
			totalFiles(installed.Manifest), installed.Dir, versionID), nil

	default:
		return "", fmt.Errorf("未知子命令 %q：%s", args[0], assetsUsage)
	}
}

// parseInstallArgs 解析 `install` 的参数，两种形态互斥且必须恰好给出一种：
//   - `<bundle-dir>`：装一个**现成**的 bundle（跨机器分发）；
//   - `--agent <agent>`（等价 `--agent=<agent>`）：从工作区按当前定义构建后安装。
//
// 两种形态之后的安装与发布路径完全相同——本函数只回答"bundle 从哪来"。
// parseInstallArgs accepts exactly one of the two install forms.
func parseInstallArgs(args []string) (agent, bundleDir string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--agent":
			name, next, takeErr := takeAgentFlag(args, i)
			if takeErr != nil {
				return "", "", takeErr
			}
			if agent != "" {
				return "", "", fmt.Errorf("install 的 --agent 不得重复：%s", assetsUsage)
			}
			agent, i = name, next
		case strings.HasPrefix(arg, "--agent="):
			if agent != "" {
				return "", "", fmt.Errorf("install 的 --agent 不得重复：%s", assetsUsage)
			}
			agent = strings.TrimSpace(strings.TrimPrefix(arg, "--agent="))
			if agent == "" {
				return "", "", fmt.Errorf("install --agent 的 Agent 名不得为空：%s", assetsUsage)
			}
		case strings.HasPrefix(arg, "-"):
			return "", "", fmt.Errorf("install 不认识的选项 %q：%s", arg, assetsUsage)
		default:
			if bundleDir != "" {
				return "", "", fmt.Errorf("install 需要且只需要一个 bundle 目录：%s", assetsUsage)
			}
			bundleDir = arg
		}
	}
	switch {
	case agent != "" && bundleDir != "":
		return "", "", fmt.Errorf("install 的 <bundle-dir> 与 --agent 只能二选一：%s", assetsUsage)
	case agent == "" && bundleDir == "":
		return "", "", fmt.Errorf("install 需要 bundle 目录或 --agent：%s", assetsUsage)
	}
	return agent, bundleDir, nil
}

// takeAgentFlag 读 `--agent` 后面那个参数，返回名字与"已消费到哪一位"。
// takeAgentFlag consumes the value after --agent and reports the new index.
func takeAgentFlag(args []string, i int) (string, int, error) {
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("install --agent 需要一个 Agent 名：%s", assetsUsage)
	}
	name := strings.TrimSpace(args[i+1])
	if name == "" {
		return "", i, fmt.Errorf("install --agent 的 Agent 名不得为空：%s", assetsUsage)
	}
	return name, i + 1, nil
}
