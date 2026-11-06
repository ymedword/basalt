//go:build integration

package app

// 本文件守 `cli.LearningPort` 的**生产接线**（US-22 §3.1.10 / T023 / T027）：
// 独立复核 F-01 指出的缺口是「端口只在测试里构造、真实二进制里 `Deps.Learning`
// 恒为 nil」——命令面因此全部报「学习端口未注入」。这里从 `Run` 入口与端口本身
// 两个层面验证生产装配：查询命令能真的开库读，handoff 能真的取锁、开写库并走到
// 仓储查询（草案不存在是最小可判的领域错误，不需要造草案文件）。
//
// The production wiring of cli.LearningPort: the Run entry point must reach a real
// read-only session, and the handoff must actually take the instance lock, open the
// writable database and reach the draft lookup.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestUS22_LearningPort_ReadCommandsReachProductionPort：走真实 `Run` 入口（轻路径），
// 三条只读命令都必须退出 0——装配缺口时它们全部退出 1 并报「学习端口未注入」。
// 逆改点：删掉 run.go 的 `Learning:` 字段 → 三条命令全部红。
// The three read-only commands must exit 0 through the real Run entry point; removing
// the Learning field from the assembly turns every one of them red.
func TestUS22_LearningPort_ReadCommandsReachProductionPort(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	resetOutboundGuard(t)

	dsn := pgtest.NewTestSchema(t)
	t.Setenv("BASALT_LEARNING_PORT_DSN", dsn)
	config := "database:\n" +
		"  dsn: ${BASALT_LEARNING_PORT_DSN}\n" +
		"  connect_timeout_seconds: 5\n" +
		"http:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n"
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// init 建工作区骨架（`agents/` 是 status 计算未注册名单的扫描根）。
	// init provisions the workspace skeleton, including the agents directory.
	require.Equal(t, 0, Run(ctx, []string{"init"}))
	require.Equal(t, 0, Run(ctx, []string{"db", "migrate"}))

	require.Equal(t, 0, Run(ctx, []string{"learning", "status"}),
		"learning status 必须经生产适配器读库（F-01：端口未接线时退出 1）")
	require.Equal(t, 0, Run(ctx, []string{"learning", "extractions", "list"}),
		"learning extractions list 必须经生产适配器读库")
	require.Equal(t, 0, Run(ctx, []string{"learning", "drafts", "list"}),
		"learning drafts list 必须经生产适配器读库")
}

// TestUS22_LearningPort_HandoffReachesRepository：handoff 的生产适配器必须真的
// 取实例锁、打开可写库并查询草案；草案不存在返回领域错误（不是「未装配」）。
// 逆改点：把 `AcquireLock` 换成 nil 或跳过写会话 → 错误变成「未装配」，本用例红。
// The handoff must take the real lock, open the writable database and reach the draft
// lookup; a missing draft is the domain error, never a wiring error.
func TestUS22_LearningPort_HandoffReachesRepository(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	resetOutboundGuard(t)

	dsn := pgtest.NewTestSchema(t)
	t.Setenv("BASALT_LEARNING_PORT_DSN", dsn)
	config := "database:\n" +
		"  dsn: ${BASALT_LEARNING_PORT_DSN}\n" +
		"  connect_timeout_seconds: 5\n" +
		"http:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n"
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.Equal(t, 0, Run(ctx, []string{"init"}), "实例锁与工作区骨架先就位")

	port := learningPort{
		WorkDir:                workDir,
		SupportedSchemaVersion: postgres.SupportedSchemaVersion,
		Database:               cli.DatabaseConfig{DSN: dsn},
	}
	result, err := port.Handoff(ctx, core.LearningHandoffRequest{
		Actor:      core.Actor{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		DraftID:    "dft_absent01",
		SkillName:  "weekly-report",
		Files:      []core.AuthorFile{{Path: "SKILL.md", Content: "---\nname: weekly-report\n---\n"}},
		SourceJSON: us22HandoffSourceJSON("user_confirmed"),
	})
	require.ErrorIs(t, err, core.ErrLearningDraftNotFound,
		"草案不存在是领域错误——出现「未装配」说明生产接线有缺口")
	require.Empty(t, result)
}
