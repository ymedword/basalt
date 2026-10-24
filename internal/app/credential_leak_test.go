package app

// 本文件是泄露哨兵的 golden 扫描（模块卡 R4.8 / T137；宪法架构约束「凭据纪律」）：
//   - `TestCredentialLeak_NoPlaintextInSurfaces`：把哨兵 Key 与哨兵完整地址经**生产写路径**
//     写进去（env 占位连接走 `ResourceAdmin` 落 `basalt.yaml`，托管 Key 与托管地址走
//     `CredentialOps` 落库），再逐面扫描响应体、`detail_json`、审计、manifest、
//     `basalt.yaml`、日志、错误信息与前端产物；
//   - `TestCredentialLeak_GoldenFixtures`：对契约、测试夹具与前端产物做**真实凭据形态**扫描，
//     带阳性与阴性对照——示例值可以像凭据，但不得长成能直接拿去调用的形态。
//
// 判据不是"某个函数没返回 Key"，而是"明文在整个可观察面里找不到"：库面遍历**全部表全部
// 列**（表名与列名运行时现取，新增迁移自动纳入扫描），前端面遍历整个产物目录。每条扫描
// 都配一个阳性对照（哨兵允许出现的形态真的在语料里），否则"零命中"只说明扫描器没工作。
// The judgement is "the plaintext is absent from the whole observable surface", and every
// sweep carries a positive control proving it actually saw the corpus.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 哨兵值在**运行时拼接**：源码里不出现完整形态，扫描器因此不会命中自己。
// `last4` 是 `cafe`——它**允许**出现在库里（契约只留尾号），正是"扫描器真的读到了这一行"
// 的阳性对照；主机 `https://hooks.example.com` 同样允许出现。
// The sentinels are joined at run time so the scanner never matches its own source.
const (
	leakKeySentinel = "sk-leak-" + "0123cafe"
	leakURLToken    = "T0LEAK" + "SENTINEL" + "99"
	leakFullURL     = "https://hooks.example.com/services/" + leakURLToken + "/B0/X0"

	leakConnName    = "leak-alpha"
	leakChannelName = "leak-ops"
	leakEnvConnName = "leak-env"
	leakLogConnName = "leak-log"
	leakModelName   = "leak-model"
	leakRunID       = "run-leak-0001"

	// leakRepoRoot 是 `internal/app` 相对仓库根的路径（`go test` 的工作目录是包目录）。
	leakRepoRoot = "../.."
)

// leakForbidden 是任何可观察面都**不得**出现的明文形态：Key 原文、完整投递地址、地址里
// 的唯一令牌。主机名与尾号不在其中——契约明确允许这两段出现在视图与库里。
// leakForbidden lists the plaintext shapes no observable surface may carry.
func leakForbidden() []string {
	return []string{leakKeySentinel, leakFullURL, leakURLToken}
}

// assertNoLeak 断言一份语料里找不到任何明文哨兵；`what` 直接进失败信息，指明是哪一面。
// assertNoLeak reports the offending surface and the line it was found on.
func assertNoLeak(t *testing.T, what string, data []byte, forbidden []string) {
	t.Helper()
	for _, needle := range forbidden {
		if at := bytes.Index(data, []byte(needle)); at >= 0 {
			line := 1 + bytes.Count(data[:at], []byte("\n"))
			t.Fatalf("%s 第 %d 行出现明文 %q：凭据纪律被破坏", what, line, needle)
		}
	}
}

// scanDatabaseForLeaks 遍历库内**全部表全部列**并逐值扫描，返回扫描过的字符串 / 字节值
// 个数（阳性对照用：0 说明扫描器没读到东西）。表名从 `sqlite_master` 现取、列由
// `SELECT *` 现取——抄一份列清单的话，下一次迁移加出来的列会悄悄跑到扫描面之外。
// The sweep enumerates tables and columns at run time so a future migration cannot
// silently escape the scan.
func scanDatabaseForLeaks(t *testing.T, path string, forbidden []string) int {
	t.Helper()
	ctx := context.Background()
	db := openRawWrite(t, path)

	names, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	var tables []string
	for names.Next() {
		var name string
		require.NoError(t, names.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, names.Err())
	require.NoError(t, names.Close())
	require.NotEmpty(t, tables, "库内必须能找到表（否则扫描器在空转）")

	scanned := 0
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			for i, value := range values {
				switch typed := value.(type) {
				case string:
					assertNoLeak(t, table+"."+columns[i], []byte(typed), forbidden)
					scanned++
				case []byte:
					assertNoLeak(t, table+"."+columns[i], typed, forbidden)
					scanned++
				}
			}
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return scanned
}

// walkTextFiles 遍历文件或目录下的全部普通文件（不跟随软链接），把路径与原文交给 `fn`，
// 返回处理过的文件数。
// walkTextFiles visits every regular file under root and hands the bytes to fn.
func walkTextFiles(t *testing.T, root string, fn func(path string, raw []byte)) int {
	t.Helper()
	scanned := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		fn(filepath.ToSlash(path), raw)
		return nil
	})
	require.NoError(t, err)
	return scanned
}

// leakAdminFixture 是泄露扫描用的资源写面：定义文件是工作区**同级**的 `basalt.yaml`
// （与生产形状一致），库 / 闸门 / 操作台账 / 凭据仓储全部复用同一个夹具。
// leakAdminFixture wires the write surface onto the fixture store with a sibling config file.
type leakAdminFixture struct {
	admin      *ResourceAdmin
	configPath string
	logs       *bytes.Buffer
}

// newLeakAdminFixture 按生产依赖形状装配资源写面。
// newLeakAdminFixture assembles the write surface with production-shaped dependencies.
func newLeakAdminFixture(t *testing.T, f *credentialOpsFixture) leakAdminFixture {
	t.Helper()
	configPath := filepath.Join(filepath.Dir(f.root), "basalt.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(initialResourceConfig), 0o600))
	logger, logs := newCapturedLogger()
	return leakAdminFixture{
		admin: NewResourceAdmin(ResourceAdminDeps{
			ConfigPath:  configPath,
			Root:        f.root,
			Agents:      func() ([]core.AgentEntry, error) { return nil, nil },
			Store:       f.versions,
			Operations:  f.ops,
			Gate:        f.gate,
			Logger:      logger,
			Now:         baselineClock,
			Credentials: f.credentials,
		}),
		configPath: configPath,
		logs:       logs,
	}
}

// finishFailingOperations 让终态回填必定失败：写面按设计只记 ERROR、不改写入结论，
// 于是这条路径是**唯一**能在成功写入的同时逼出真实日志的办法。它同时复现了崩溃窗口的
// 残局（操作行停在 pending），所以库扫描也顺带覆盖了 pending 行的形态。
// finishFailingOperations forces the terminal write-back to fail without failing the write.
type finishFailingOperations struct{ core.OperationStore }

// FinishOperation 恒定失败，只为把写面的 ERROR 分支走通。
// FinishOperation always fails so the write surface takes its error branch.
func (finishFailingOperations) FinishOperation(context.Context, string, core.OperationStatus,
	string, string, []byte, time.Time,
) error {
	return errors.New("terminal write-back unavailable（泄露哨兵探针）")
}

// TestCredentialLeak_NoPlaintextInSurfaces 把哨兵 Key 与哨兵完整地址经生产写路径写进去，
// 再逐面确认明文**一次都没有**出现在任何可观察面上。
//
// 逆改点：视图回显定义原文 / `detail_json` 记了值 / 清单里塞了地址 / 错误文案回显入参 /
// 日志打了密文材料 → 对应面的断言各自红。
// TestCredentialLeak_NoPlaintextInSurfaces writes real sentinels through the production
// paths and proves the plaintext never reaches any observable surface.
func TestCredentialLeak_NoPlaintextInSurfaces(t *testing.T) {
	ctx := context.Background()
	forbidden := leakForbidden()
	f := newCredentialOpsFixture(t)

	// ① 三条生产写路径都要走到。只走一条会让扫描变成"自造状态"——那证明不了产品路径不泄露。
	//    定义文件面：env 占位连接由 ResourceAdmin 真写进 basalt.yaml。
	leakAdmin := newLeakAdminFixture(t, f)
	envConn, err := leakAdmin.admin.UpsertModelConnection(ctx, resourceActor(), ModelConnectionRequest{
		Name:         leakEnvConnName,
		BaseURL:      "https://api.example.com/v1",
		APIKey:       "${" + resourceTestKeyEnv + "}",
		APIKeySet:    true,
		DefaultModel: leakModelName,
		Create:       true,
	})
	require.NoError(t, err)
	require.Equal(t, leakEnvConnName, envConn.ResourceID)

	//    托管面：模型连接与通知渠道都走 CredentialOps.Write（真实加密 + 预写 + 追加版本）。
	managedVersion := f.seedManagedModelConnection(t, leakConnName, "https://api.example.com/v1")
	written := writeConnection(t, f, leakConnName, leakKeySentinel)
	require.Equal(t, 1, written.VersionNo)
	//    日志面专用的第四条连接：下面第 ⑧ 步会用它走一条**真实的失败路径**。
	f.seedManagedModelConnection(t, leakLogConnName, "https://api.example.com/v1")

	channel := f.seedManagedChannel(t, leakChannelName, "https://hooks.example.com")
	sealed, err := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingNotifyChannel,
		Name:  leakChannelName,
		UID:   channel.ResourceUID,
		Mode:  core.CredentialModeManaged,
		Value: []byte(leakFullURL),
	})
	require.NoError(t, err)
	require.True(t, sealed.Changed)

	// ② 响应体面：只读视图 / 版本详情 / 渠道行——JSON 编码后的字节就是响应体。
	//    先断言"读到的确实是这一行"，否则扫描空结构也会零命中。
	connViews, err := f.views.ModelConnections(ctx)
	require.NoError(t, err)
	connDetail, err := f.views.ModelConnection(ctx, leakConnName)
	require.NoError(t, err)
	mcpViews, err := f.views.MCPServers(ctx)
	require.NoError(t, err)
	skillViews, err := f.views.Skills(ctx)
	require.NoError(t, err)
	versionDetail, err := leakAdmin.admin.VersionDetail(ctx, managedVersion.ID)
	require.NoError(t, err)
	channelRow, err := f.admission.NotifyAdmin.Get(ctx, leakChannelName)
	require.NoError(t, err)

	require.Equal(t, leakConnName, connDetail.Name, "阳性对照：详情必须读到托管连接")
	require.Empty(t, connDetail.APIKeyEnv, "托管连接不得把凭据渲染成环境变量名")
	require.Equal(t, core.CredentialModeManaged, channelRow.CredentialMode)
	require.Equal(t, sealed.CredentialID, channelRow.CredentialID, "阳性对照：渠道行必须挂着刚写的凭据")
	require.Empty(t, channelRow.URL, "托管渠道的 url 列必须为空（地址在密文里）")
	require.Equal(t, "https://hooks.example.com", channelRow.URLHost, "展示面只留 scheme + host")
	require.Equal(t, "cafe", written.Last4, "尾号是允许出现在库里的唯一片段（阳性对照）")

	for _, surface := range []struct {
		what  string
		value any
	}{
		{"响应体（模型连接列表）", connViews},
		{"响应体（模型连接详情）", connDetail},
		{"响应体（MCP 列表）", mcpViews},
		{"响应体（技能列表）", skillViews},
		{"响应体（版本详情）", versionDetail},
		{"响应体（通知渠道行）", channelRow},
	} {
		raw, err := json.Marshal(surface.value)
		require.NoError(t, err)
		assertNoLeak(t, surface.what, raw, forbidden)
	}

	// ③ 清单面：v2 清单固定凭据 ID / 版本 / 目标绑定哈希，明文不得出现在序列化字节里。
	agentUID, err := core.NewAgentUID()
	require.NoError(t, err)
	targetBinding, err := CredentialTargetBinding(core.BindingModelConnection,
		"https://api.example.com/v1", "")
	require.NoError(t, err)
	manifest, err := core.BuildRunResourceManifest(agentUID, core.AgentActive, core.ResolvedResources{
		Provider:      versionRef(leakConnName, managedVersion),
		ProviderModel: "alpha-model",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeManaged,
			ID:            written.CredentialID,
			Version:       written.VersionNo,
			TargetBinding: targetBinding,
		},
		Notify: core.NotifyManifest{
			Mode:     core.NotifyModeAllowlist,
			Channels: []core.NotifyChannelRef{{Name: leakChannelName, ResourceUID: channel.ResourceUID}},
		},
	})
	require.NoError(t, err)
	manifestJSON, err := core.MarshalManifest(manifest)
	require.NoError(t, err)
	assertNoLeak(t, "资源清单序列化", manifestJSON, forbidden)
	require.Contains(t, string(manifestJSON), written.CredentialID,
		"阳性对照：清单必须真的固定凭据引用，否则扫描的是空清单")

	//    落库面：清单与引用行在同一事务里写进 runs / run_resource_manifests /
	//    run_credential_refs / run_notify_refs；下面的库扫描据此覆盖持久形态。
	require.NoError(t, storage.NewRunStore(f.db).StartRun(ctx, core.RunRecord{
		RunID:     leakRunID,
		Agent:     "writer",
		Channel:   "cli",
		UserID:    "leak-tester",
		StartedAt: baselineClock(),
	}, manifest))

	rawDB := openRawWrite(t, f.dbPath)
	var pinned int
	require.NoError(t, rawDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_credential_refs WHERE run_id = ? AND credential_id = ?`,
		leakRunID, written.CredentialID).Scan(&pinned))
	require.Equal(t, 1, pinned, "阳性对照：凭据固定行必须真的落库")

	// ④ `detail_json` 面：预写详情真的写了凭据元数据，随后由库扫描一并覆盖。
	var detailJSON string
	require.NoError(t, rawDB.QueryRowContext(ctx,
		`SELECT detail_json FROM management_operations WHERE operation_id = ?`,
		written.OperationID).Scan(&detailJSON))
	require.Contains(t, detailJSON, written.CredentialID,
		"阳性对照：预写详情必须真的带凭据元数据")
	assertNoLeak(t, "management_operations.detail_json", []byte(detailJSON), forbidden)

	// ⑤ 定义文件面：basalt.yaml 只留占位，解析出来的值不得回写。
	configBytes, err := os.ReadFile(leakAdmin.configPath)
	require.NoError(t, err)
	assertNoLeak(t, "basalt.yaml", configBytes, forbidden)
	require.Contains(t, string(configBytes), "${"+resourceTestKeyEnv+"}",
		"阳性对照：定义文件必须留占位")
	assertNoLeak(t, "basalt.yaml 里的已解析 Key", configBytes, []string{resourceTestKeySentinel})

	// ⑥ 错误文案面：把哨兵塞进**必然被拒**的输入，错误信息不得回显它。
	_, userinfoErr := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingNotifyChannel,
		Name:  leakChannelName,
		UID:   channel.ResourceUID,
		Mode:  core.CredentialModeManaged,
		Value: []byte("https://user:" + leakURLToken + "@hooks.example.com/services/x"),
	})
	require.Error(t, userinfoErr)
	require.Contains(t, userinfoErr.Error(), "userinfo", "阳性对照：必须命中地址 userinfo 分支")
	assertNoLeak(t, "错误信息（userinfo 地址）", []byte(userinfoErr.Error()), forbidden)

	_, missingErr := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  leakEnvConnName,
		UID:   connectionUID(t, leakEnvConnName),
		Mode:  core.CredentialModeManaged,
		Value: []byte(leakKeySentinel),
	})
	require.Error(t, missingErr)
	require.Contains(t, missingErr.Error(), leakEnvConnName, "阳性对照：拒绝必须点名资源")
	assertNoLeak(t, "错误信息（env 定义拒绝托管写入）", []byte(missingErr.Error()), forbidden)

	// ⑦ 撤销面：撤销清空密文并升纪元，是最容易在文案里回显原值的一步。
	revoked, err := f.credOps.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  leakConnName,
		UID:   connectionUID(t, leakConnName),
	})
	require.NoError(t, err)
	require.Equal(t, 1, revoked.RevocationEpoch)
	require.NotEmpty(t, revoked.OperationID, "撤销必须留下可查证的操作标识")

	// ⑧ 收尾扫描：库面（全部表全部列）、日志面、前端产物面。
	scannedValues := scanDatabaseForLeaks(t, f.dbPath, forbidden)
	require.Greater(t, scannedValues, 100, "阳性对照：库扫描必须真的读到值")

	//    日志面：写面**只在失败时**记 ERROR，成功路径的空缓冲是正常结果——所以先走一条真实
	//    的回填失败路径把日志逼出来，否则"日志里没有明文"只是"日志里没有东西"的空断言。
	var finishLogs strings.Builder
	finishOps := newTestCredentialOps(t, f.resolverFixture, finishFailingOperations{f.opStore()}, &finishLogs)
	_, finishErr := finishOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  leakLogConnName,
		UID:   connectionUID(t, leakLogConnName),
		Mode:  core.CredentialModeManaged,
		Value: []byte(leakKeySentinel),
	})
	require.NoError(t, finishErr, "终态回填失败只记 ERROR，不得改写写入结论")
	require.NotEmpty(t, finishLogs.String(), "阳性对照：回填失败必须真的记 ERROR")
	assertNoLeak(t, "日志（凭据写面，回填失败）", []byte(finishLogs.String()), forbidden)

	//    成功路径与资源写面的缓冲按现状**可能为空**（同样只记失败），有内容才扫——它们的
	//    阳性对照在第 ⑧ 步上半段已经给过，这里不再重复要求非空。
	for _, surface := range []struct {
		what string
		data []byte
	}{
		{"日志（凭据写面，成功路径）", []byte(f.logs.String())},
		{"日志（资源写面）", leakAdmin.logs.Bytes()},
	} {
		assertNoLeak(t, surface.what, surface.data, forbidden)
	}

	for _, dir := range []string{
		filepath.Join(leakRepoRoot, "assets", "console", "src"),
		filepath.Join(leakRepoRoot, "assets", "console", "dist"),
	} {
		files := walkTextFiles(t, dir, func(path string, raw []byte) {
			assertNoLeak(t, "前端产物 "+path, raw, forbidden)
		})
		require.Positive(t, files, "阳性对照：%s 必须扫到文件", dir)
	}
}

// leakCredentialPatterns 是**真实凭据形态**：长度与字符集按发行方实际发放的形状取，
// 不是"含 `sk-` 就算"。示例值可以长得像凭据（`${VAR}`、带省略号的片段），但不得落到
// 能直接拿去调用的形态。
// leakCredentialPatterns matches the real shapes issuers hand out, not mere prefixes.
func leakCredentialPatterns() []string {
	return []string{
		`sk-[A-Za-z0-9]{32,}`,                                               // OpenAI 经典 Key
		`sk-ant-[A-Za-z0-9_-]{20,}`,                                         // Anthropic
		`sk-proj-[A-Za-z0-9_-]{20,}`,                                        // OpenAI 项目 Key
		`ghp_[A-Za-z0-9]{30,}`,                                              // GitHub 个人访问令牌
		`xox[baprs]-[0-9A-Za-z-]{20,}`,                                      // Slack 令牌
		`AKIA[0-9A-Z]{16}`,                                                  // AWS Access Key ID
		`hooks\.slack\.com/services/[A-Z0-9]{8,}`,                           // Slack webhook
		`discord\.com/api/webhooks/[0-9]{6,}`,                               // Discord webhook
		`open\.feishu\.cn/open-apis/bot/v2/hook/[0-9a-fA-F-]{8,}`,           // 飞书机器人
		`qyapi\.weixin\.qq\.com/cgi-bin/webhook/send\?key=[0-9a-fA-F-]{8,}`, // 企业微信机器人
	}
}

// leakCorpusRoots 是"示例与夹具"的载体：契约样例、Go 源码与测试夹具、前端源码与产物。
// 这些地方最可能被贴进真值，也正是"示例不得长成可用凭据"的判卷对象。
// leakCorpusRoots covers the places where example credentials actually live.
func leakCorpusRoots() []string {
	return []string{
		filepath.Join(leakRepoRoot, "docs", "contracts"),
		filepath.Join(leakRepoRoot, "internal"),
		filepath.Join(leakRepoRoot, "assets", "console", "src"),
		filepath.Join(leakRepoRoot, "assets", "console", "dist"),
		filepath.Join(leakRepoRoot, "assets", "console", "tests"),
		filepath.Join(leakRepoRoot, "basalt.yaml.example"),
	}
}

// TestCredentialLeak_GoldenFixtures 扫描契约、夹具与前端产物里的**真实凭据形态**。
//
// 阳性对照：运行时拼出的样本必须命中（否则"零命中"只说明正则写错了）；
// 阴性对照：文档里合法的合成示例必须不命中（否则门禁会逼着大家删掉示例，反而鼓励把真值
// 贴进文档）。
// TestCredentialLeak_GoldenFixtures scans the fixture corpus for real credential shapes,
// with both a positive and a negative control.
func TestCredentialLeak_GoldenFixtures(t *testing.T) {
	sources := leakCredentialPatterns()
	patterns := make([]*regexp.Regexp, len(sources))
	for i, source := range sources {
		patterns[i] = regexp.MustCompile(source)
	}

	// 阳性对照：样本在运行时拼出，源码里不出现完整形态。
	// 样本里刻意用 `strings.Repeat` 而不是字面量，避免用例自己变成命中项。
	for _, sample := range []string{
		"sk-" + strings.Repeat("a1B2", 8),
		"sk-ant-" + strings.Repeat("api03", 5),
		"sk-proj-" + strings.Repeat("Q1", 11),
		"ghp_" + strings.Repeat("Z9", 16),
		"xoxb-" + strings.Repeat("1234567890", 3),
		"AKIA" + strings.Repeat("Q7", 8),
		"https://hooks.slack.com/services/" + strings.Repeat("A7", 5),
		"https://discord.com/api/webhooks/" + strings.Repeat("7", 10),
		"https://open.feishu.cn/open-apis/bot/v2/hook/" + strings.Repeat("0a", 5),
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + strings.Repeat("0a", 5),
	} {
		matched := false
		for _, pattern := range patterns {
			if pattern.MatchString(sample) {
				matched = true
				break
			}
		}
		require.True(t, matched, "阳性对照未命中：%q 必须被真实凭据形态识别", sample)
	}

	// 阴性对照：这些是本仓文档与夹具里合法的合成示例，不得被误判。
	for _, sample := range []string{
		"${DEEPSEEK_API_KEY}",
		"${BASALT_RESOURCE_TEST_KEY}",
		"sk-....（省略号）",
		"ghp_SENTINEL_9f2c7d",
		"https://api.example.com/v1",
		"https://open.feishu.cn/open-apis/bot/v2/hook/T1",
	} {
		for _, pattern := range patterns {
			require.False(t, pattern.MatchString(sample),
				"阴性对照误判：%q 不该被 %s 命中", sample, pattern)
		}
	}

	scanned := 0
	for _, root := range leakCorpusRoots() {
		files := walkTextFiles(t, root, func(path string, raw []byte) {
			for _, pattern := range patterns {
				if at := pattern.FindIndex(raw); at != nil {
					line := 1 + bytes.Count(raw[:at[0]], []byte("\n"))
					t.Errorf("%s:%d 命中真实凭据形态 %s：示例不得长成可用凭据",
						path, line, pattern)
				}
			}
		})
		require.Positive(t, files, "阳性对照：%s 必须扫到文件", root)
		scanned += files
	}
	require.Greater(t, scanned, 200, "阳性对照：语料必须真的扫到足够多的文件")
}
