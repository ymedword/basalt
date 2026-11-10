package app

// 本文件是 `basalt assets status` 的**生产适配器**（US-25C 模块卡 §3.1.5 / C15）：
// 逐 Agent 汇总「指针哈希 / 已安装哈希 / 载荷存在 / 技能物化 / 阻塞原因」。
//
// 三条纪律：
//  1. **只读**：只开 `postgres.OpenReadOnly`（服务端固定只读事务），不取实例锁、不
//     触发同步、不写任何行——服务运行中也能执行。
//  2. **判定顺序与同步一致**：载荷 → 安装 → 物化，逐段复用同步的同一批判定函数
//     （`resolveSkillMaterialization` / `verifyMaterializedSkill`），阻塞原因用同一组
//     字面量；本文件**不**另写一套"为什么没就绪"的推导。
//  3. **不降级**：任一读失败整条命令失败并带上原因——把"读不到"渲染成"没有"会让
//     运维按错误的前提处置；反过来，缺失也必须显式标出来（C15）。
//
// The production adapter behind `basalt assets status`: strictly read-only, reusing the
// sync's own judgement helpers and reason literals, and failing the whole command rather
// than degrading a read error into an empty reading.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// assetStatusPageSize 是资源指针全量翻页的页大小（与同步的全量兜底同量级：诊断命令
// 面向几十到几百个 Agent，不需要另立配置键）。
// assetStatusPageSize is the pointer paging size; the diagnostic needs no config key.
const assetStatusPageSize = 200

// 诊断的**读数**词表（成功侧与未知侧）。阻塞原因不在这里——它复用 `asset_sync.go`
// 的同一组常量，两种输出因此不会各说一套。
// The reading vocabulary; blocked reasons reuse the sync's own constants.
const (
	assetStatusPayloadPresent     = "present"
	assetStatusSkillsMaterialized = "materialized"
	assetStatusSkillsUnknown      = "unknown"
)

// 编译期断言：适配器满足命令面声明的只读诊断端口。
// Compile-time assertion that the adapter satisfies the read-only diagnostic port.
var _ cli.AssetStatusReader = assetStatusPort{}

// assetStatusPort 是 `cli.AssetStatusReader` 的生产实现。`Root` 已由装配层按
// `assets.bundle_root` 解析成绝对路径；`openReadOnly` 是测试注入点。
// assetStatusPort carries the resolved bundle root and the testable open hook.
type assetStatusPort struct {
	WorkDir  string
	Database cli.DatabaseConfig
	Logger   *slog.Logger

	// Root 是已经解析（绝对化）的内容寻址 bundle 根目录。
	// Root is the already-resolved content-addressed bundle root.
	Root string

	// openReadOnly 默认为 postgres.OpenReadOnly：诊断**从不**打开可写会话。
	// openReadOnly defaults to the read-only opener; the diagnostic never writes.
	openReadOnly func(context.Context, postgres.Config) (*postgres.DB, error)
}

// Status 只读汇总全部已发布 Agent 的分发读数。
// Status collects every published agent's distribution readings.
func (p assetStatusPort) Status(ctx context.Context) (cli.AssetStatusReport, error) {
	open := p.openReadOnly
	if open == nil {
		open = postgres.OpenReadOnly
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return cli.AssetStatusReport{}, err
	}
	agents, statusErr := collectAssetStatus(ctx, p.Root, p.root(), postgres.NewAssetDistributionStore(db))
	closeErr := db.Close()
	if statusErr != nil {
		return cli.AssetStatusReport{}, errors.Join(statusErr, closeErr)
	}
	if closeErr != nil {
		return cli.AssetStatusReport{}, fmt.Errorf("assets status 关闭数据库失败: %w", closeErr)
	}
	return cli.AssetStatusReport{AssetRoot: p.Root, Agents: agents}, nil
}

// root 是工作区根目录（`<WorkDir>/.basalt`）：技能物化目录相对它解析，与同步同一基准。
// root is <WorkDir>/.basalt, the same base the sync materializes against.
func (p assetStatusPort) root() string {
	return filepath.Join(p.WorkDir, workspaceDirName)
}

// collectAssetStatus 是全量游标的唯一循环：按资源标识（AgentUID）翻页，与同步的全量
// 兜底同一把键；每行从头独立判定，不跨行复用任何读数。
// collectAssetStatus pages by resource id and judges each row from scratch.
func collectAssetStatus(ctx context.Context, assetRoot, workspaceRoot string,
	distribution core.AssetDistributionPort,
) ([]cli.AssetStatusRow, error) {
	rows := make([]cli.AssetStatusRow, 0, 16)
	after := ""
	for {
		bundles, err := distribution.ListPublishedBundlesPage(ctx, after, assetStatusPageSize)
		if err != nil {
			return nil, fmt.Errorf("读取已发布 bundle 指针: %w", err)
		}
		for _, bundle := range bundles {
			row, err := describeAssetStatus(ctx, assetRoot, workspaceRoot, distribution, bundle)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if len(bundles) < assetStatusPageSize {
			return rows, nil
		}
		after = string(bundles[len(bundles)-1].AgentUID)
	}
}

// describeAssetStatus 判定一个 Agent 的五个读数。阻塞原因按同步的判定顺序取**第一个**
// 不成立项——与"这一轮同步会怎么判"同源，避免诊断和报告给出两种说法。
// describeAssetStatus judges one agent's five readings in the sync's own order.
func describeAssetStatus(ctx context.Context, assetRoot, workspaceRoot string,
	distribution core.AssetDistributionPort, bundle core.PublishedBundle,
) (cli.AssetStatusRow, error) {
	row := cli.AssetStatusRow{
		Agent:       bundle.Agent,
		PointerHash: bundle.BundleHash,
		Skills:      assetStatusSkillsUnknown,
	}

	// ① 载荷：只在"未安装"时才为安装所必需，但诊断对每一行都读——C15 的不变量是
	// 「指针存在 ⇒ 载荷存在」，它按共享记录判定，与本机装没装无关。
	payload, payloadOK, payloadErr := distribution.ReadBundlePayload(ctx, bundle.BundleHash)
	switch {
	case payloadErr != nil:
		row.Payload = syncBlockedPayloadRead
	case !payloadOK:
		row.Payload = syncBlockedPayloadMissing
	default:
		row.Payload = assetStatusPayloadPresent
		if payload.BundleHash != bundle.BundleHash {
			return row, fmt.Errorf("bundle %s 的载荷行哈希为 %q，与指针不符",
				bundle.BundleHash, payload.BundleHash)
		}
	}

	// ② 安装：与同步的安装分支同一判定（Resolve → Verify → 哈希逐字相等）。
	corrupt := false
	dir, resolveErr := assets.Resolve(ctx, assetRoot, bundle.Agent, bundle.BundleHash)
	switch {
	case resolveErr == nil:
		manifest, verifyErr := assets.Verify(ctx, dir)
		if verifyErr != nil || manifest.BundleHash != bundle.BundleHash {
			corrupt = true
			break
		}
		row.InstalledHash = manifest.BundleHash
		if skillsErr := verifyMaterializedSkills(ctx, workspaceRoot, distribution, manifest.Skills); skillsErr != nil {
			row.Skills = syncBlockedSkills
		} else {
			row.Skills = assetStatusSkillsMaterialized
		}
	case errors.Is(resolveErr, core.ErrAssetUnavailable):
		// 本机还没有这个哈希的目录：这不是错误，是"待装"。
	default:
		corrupt = true
	}

	// ③ 阻塞原因：载荷 → 安装 → 自洽 → 物化，取第一个不成立项。
	switch {
	case payloadErr != nil:
		row.BlockedReason = syncBlockedPayloadRead
	case !payloadOK:
		row.BlockedReason = syncBlockedPayloadMissing
	case corrupt:
		row.BlockedReason = syncBlockedCorrupt
	case row.InstalledHash == "":
		row.BlockedReason = syncBlockedNotInstalled
	case row.Skills != assetStatusSkillsMaterialized:
		row.BlockedReason = syncBlockedSkills
	}
	return row, nil
}

// verifyMaterializedSkills 只读核对一个 bundle 声明的全部技能：版本行可读、与清单
// 逐字相符、发布目录存在且逐文件哈希相符。判定函数与同步物化共用，本函数**不**复制
// 那套规则，只做"全部技能都通过才算物化"的汇总。
// verifyMaterializedSkills proves every declared skill is materialized, reusing the
// sync's own resolution rules rather than restating them.
func verifyMaterializedSkills(ctx context.Context, workspaceRoot string,
	distribution core.AssetDistributionPort, refs []core.SkillVersionRef,
) error {
	for _, ref := range refs {
		_, dest, err := resolveSkillMaterialization(ctx, distribution, workspaceRoot, ref)
		if err != nil {
			return err
		}
		if err := verifyMaterializedSkill(dest, ref.Files); err != nil {
			return fmt.Errorf("技能 %s: %w", ref.Name, err)
		}
	}
	return nil
}
