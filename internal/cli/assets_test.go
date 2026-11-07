package cli

// 本文件是资产 bundle 与 Worker 只读列表的**命令面**验收（US-24 模块卡 §3.1.11 /
// §3.1.12，T030）。判据只看命令面自己的行为——端口是记录调用的替身、真实 bundle
// 由 assets.Build 在临时目录构建，不需要数据库：
//
//	C1 `TestCLI_AssetsExportInstallVerifyRoundTrip`——verify → export → install
//	   经命令面往返：三个子命令都委托 assets.CommandRoot、打印人读摘要；install
//	   的输出目录与 Publish 收到的 Dir 逐字相同且都含 `bundles/` 段（FR-24-44），
//	   导出目录可被再次校验。
//	C2 `TestCLI_AssetsErrorsAreNonEmptyAndDoNotEchoContents`——端口缺件 / 源 bundle
//	   损坏两类失败都返回**非空**错误，且错误文本不含文件内容（资产可能是私有
//	   配置；路径与哈希可以出现，正文不行）。
//	C3 `TestCLI_WorkersListReadOnly`——五列只读列表以 instance 为首，`--json` 与
//	   表格同源；空列表与缺端口各有明确文案；命令面只调 ListNodes，不取实例锁、
//	   不做写身份校验（端口上也没有写方法）。
//	C4 `TestCLI_AssetsInstallAgentBuildsThenPublishes`——`install --agent` 把 Agent
//	   名原样交给构建入口，构建结果再走与 `<bundle-dir>` 相同的安装与发布路径；
//	   两种 install 形态互斥，参数被拒时零发布。
//
// The command surface's own acceptance: recording fakes, a real bundle built in a
// temp dir, and no database.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// cliTestAgentUID 是测试用 Agent 身份：`ag_` + 26 位 Crockford Base32（无 I/L/O/U）。
const cliTestAgentUID = core.AgentUID("ag_0123456789ABCDEFGHJKMNPQRS")

// secretMarker 是配置文件正文里的独特标记：任何错误 / 输出出现它即判泄露。
const secretMarker = "TOPSECRET-AGENT-BODY-9f3a"

// fakeAssetsPort 记录 install 的发布调用；未注入 publish 时返回固定版本号。
// fakeAssetsPort records the publish calls; the default returns version 42.
type fakeAssetsPort struct {
	root      string
	published []assets.Installed
	publish   func(context.Context, assets.Installed) (int64, error)
	build     func(ctx context.Context, agent, dest string) (assets.Manifest, error)
}

func (p *fakeAssetsPort) AssetRoot() string { return p.root }

func (p *fakeAssetsPort) Publish(ctx context.Context, installed assets.Installed) (int64, error) {
	p.published = append(p.published, installed)
	if p.publish != nil {
		return p.publish(ctx, installed)
	}
	return 42, nil
}

// Build 默认报"未接线"：C1～C3 都不走 install --agent，误接会立刻变红。
// Build fails by default so an accidental wiring in C1–C3 shows up as a red case.
func (p *fakeAssetsPort) Build(ctx context.Context, agent, dest string) (assets.Manifest, error) {
	if p.build == nil {
		return assets.Manifest{}, errors.New("测试替身未注入 Build")
	}
	return p.build(ctx, agent, dest)
}

// fakeWorkerLister 是只读端口的替身：返回固定行与固定错误。
// fakeWorkerLister returns fixed rows and a fixed error.
type fakeWorkerLister struct {
	nodes []core.WorkerNodeView
	err   error
	calls int
}

func (f *fakeWorkerLister) ListNodes(context.Context) ([]core.WorkerNodeView, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.nodes, nil
}

// executeAssets 跑一次 `basalt assets ...`，Out / Err 共用同一缓冲区（错误文案也
// 要参与"不泄露内容"的检查）。
// executeAssets runs one asset subcommand with a shared output buffer.
func executeAssets(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand(deps)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"assets"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// executeWorkers 跑一次 `basalt workers ...`。
// executeWorkers runs one worker subcommand.
func executeWorkers(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand(deps)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"workers"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// writeCLIFile 在 root 下按相对路径写一个测试文件（父目录自动创建）。
// writeCLIFile writes one fixture file below root.
func writeCLIFile(t *testing.T, root, rel, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte(content), 0o644))
}

// buildCLIBundle 在临时目录铺来源树并构建一份真实 bundle，返回清单与暂存目录。
// buildCLIBundle builds a real bundle from a temp-dir source tree.
func buildCLIBundle(t *testing.T, work, agent string) (assets.Manifest, string) {
	t.Helper()
	configRoot := filepath.Join(work, "src-"+agent)
	writeCLIFile(t, configRoot, "AGENT.md",
		"---\nname: "+agent+"\n---\n\n# 角色\n\n评审招标要求。\n\n<!-- "+secretMarker+" -->\n")
	writeCLIFile(t, configRoot, "bootstrap/policy.md", "# 策略\n\n只读检索。\n")
	stage := filepath.Join(work, "stage-"+agent)
	manifest, err := assets.Build(t.Context(), assets.BuildSpec{
		Agent:       agent,
		AgentUID:    cliTestAgentUID,
		ConfigHash:  "cfg-" + agent,
		ConfigRoot:  configRoot,
		ConfigFiles: []string{"AGENT.md", "bootstrap/policy.md"},
		Dest:        stage,
	})
	require.NoError(t, err)
	return manifest, stage
}

// C1：verify → export → install 经命令面往返。
func TestCLI_AssetsExportInstallVerifyRoundTrip(t *testing.T) {
	work := t.TempDir()
	manifest, stage := buildCLIBundle(t, work, "writer")
	port := &fakeAssetsPort{root: filepath.Join(work, "assets-root")}
	deps := Deps{WorkDir: work, Logger: quietLogger(), Assets: port}

	// ① verify：只复算并打印清单摘要（agent / bundle / 文件总数）。
	out, err := executeAssets(t, deps, "verify", stage)
	require.NoError(t, err)
	require.Contains(t, out, "校验通过")
	require.Contains(t, out, "agent=writer")
	require.Contains(t, out, "bundle="+manifest.BundleHash)
	require.Contains(t, out, "文件总数=2")

	// ② export：复制到目标目录，目标目录必须能被独立复算成同一 bundle。
	exported := filepath.Join(work, "exported")
	out, err = executeAssets(t, deps, "export", stage, exported)
	require.NoError(t, err)
	require.Contains(t, out, "已导出")
	require.Contains(t, out, "目标目录："+exported)
	require.FileExists(t, filepath.Join(exported, "agent", "AGENT.md"))
	require.FileExists(t, filepath.Join(exported, "bundle.json"))
	copyManifest, err := assets.Verify(t.Context(), exported)
	require.NoError(t, err)
	require.Equal(t, manifest.BundleHash, copyManifest.BundleHash, "导出副本必须与源同一 bundle")

	// ③ install：本地不可变目录 + 共享发布记录（Publish 由装配层注入）。
	out, err = executeAssets(t, deps, "install", stage)
	require.NoError(t, err)
	require.Contains(t, out, "已安装并发布")
	require.Contains(t, out, "published_version_id=42")
	require.Len(t, port.published, 1, "install 必须恰好调用一次 Publish")
	installed := port.published[0]
	wantDir := filepath.Join(port.root, "bundles", "writer", manifest.BundleHash)
	require.Equal(t, wantDir, installed.Dir, "安装目录必须是 <root>/bundles/<agent>/<hash>（FR-24-44）")
	require.Contains(t, filepath.ToSlash(installed.Dir), "/bundles/")
	require.Contains(t, out, "安装目录："+wantDir, "打印的目录与 Publish 收到的必须是同一路径")
	require.FileExists(t, filepath.Join(wantDir, "agent", "AGENT.md"))
	require.FileExists(t, filepath.Join(wantDir, "bundle.json"))

	// ④ 重复安装幂等：同一内容再装一次仍成功，目录不重写、Publish 再确认一次。
	out, err = executeAssets(t, deps, "install", stage)
	require.NoError(t, err)
	require.Contains(t, out, "published_version_id=42")
	require.Len(t, port.published, 2)
	require.Equal(t, wantDir, port.published[1].Dir)
}

// C4：`install --agent` 先构建、再走与 `<bundle-dir>` 完全相同的安装与发布路径。
// 判据是"命令面把 --agent 原样交给构建入口、构建结果进了安装与发布"，而不是重测
// 构建器本身（构建器的生产链路由 internal/app 的集成用例判）。
func TestCLI_AssetsInstallAgentBuildsThenPublishes(t *testing.T) {
	work := t.TempDir()
	port := &fakeAssetsPort{root: filepath.Join(work, "assets-root")}
	var builtAgent, builtDest string
	port.build = func(ctx context.Context, agent, dest string) (assets.Manifest, error) {
		builtAgent, builtDest = agent, dest
		require.DirExists(t, dest, "命令面用一个新建的暂存目录接构建结果")
		entries, readErr := os.ReadDir(dest)
		require.NoError(t, readErr)
		require.Empty(t, entries, "交给构建入口的暂存目录必须是空的")
		// 用真实 Build 铺一份**工作区相对形态**的 bundle：配置路径是
		// `agents/<agent>/AGENT.md`，与执行期读取面同一基准。
		source := filepath.Join(work, "ws")
		writeCLIFile(t, source, "agents/"+agent+"/AGENT.md",
			"---\nname: "+agent+"\n---\n\n# 角色\n\n评审招标要求。\n")
		return assets.Build(ctx, assets.BuildSpec{
			Agent:       agent,
			AgentUID:    cliTestAgentUID,
			ConfigHash:  "cfg-" + agent,
			ConfigRoot:  source,
			ConfigFiles: []string{"agents/" + agent + "/AGENT.md"},
			Dest:        dest,
		})
	}
	deps := Deps{WorkDir: work, Logger: quietLogger(), Assets: port}

	out, err := executeAssets(t, deps, "install", "--agent", "writer")
	require.NoError(t, err)
	require.Contains(t, out, "已安装并发布")
	require.Equal(t, "writer", builtAgent, "构建入口收到的必须是 --agent 的值")
	require.NotEmpty(t, builtDest, "构建入口必须收到一个暂存目录")

	require.Len(t, port.published, 1, "install --agent 必须恰好发布一次")
	installed := port.published[0]
	require.Equal(t, "writer", installed.Manifest.Agent)
	require.Len(t, installed.Manifest.ConfigFiles, 1)
	require.Equal(t, "agents/writer/AGENT.md", installed.Manifest.ConfigFiles[0].Path,
		"配置路径保持工作区相对形态（与执行期读取面同一基准）")
	require.Contains(t, filepath.ToSlash(installed.Dir), "/bundles/writer/")
	require.Contains(t, out, "安装目录："+installed.Dir)
	require.NoDirExists(t, builtDest, "暂存目录用毕即删（构建结果已复制进 hash 目录）")

	// 两种形态互斥：同时给出时必须失败，且不得调用构建或发布。
	before := len(port.published)
	_, err = executeAssets(t, deps, "install", "some-dir", "--agent", "writer")
	require.Error(t, err)
	require.Contains(t, err.Error(), "只能二选一")
	require.Len(t, port.published, before, "参数被拒时不得发生任何发布")
}

// C2：失败一律非空错误、不回显文件内容。
func TestCLI_AssetsErrorsAreNonEmptyAndDoNotEchoContents(t *testing.T) {
	work := t.TempDir()

	// 缺端口：命令面不得自己解析资产根或打开数据库补出第二条安装路径。
	out, err := executeAssets(t, Deps{WorkDir: work, Logger: quietLogger()}, "verify", work)
	require.Error(t, err)
	require.NotEmpty(t, strings.TrimSpace(err.Error()), "失败必须带非空原因")
	require.Contains(t, err.Error(), "资产端口未注入")
	require.Contains(t, out, "资产端口未注入", "错误文本必须留痕到输出")

	// 源 bundle 损坏：哈希 / 大小不符即拒绝，且错误只含路径与哈希，不含正文。
	_, stage := buildCLIBundle(t, work, "broken")
	writeCLIFile(t, stage, "agent/AGENT.md", "TRUNCATED "+secretMarker+"\n")
	deps := Deps{
		WorkDir: work,
		Logger:  quietLogger(),
		Assets:  &fakeAssetsPort{root: filepath.Join(work, "assets-root")},
	}
	out, err = executeAssets(t, deps, "verify", stage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "文件 agent/AGENT.md 与清单不符")
	require.NotContains(t, err.Error(), secretMarker, "错误文本不得回显文件内容")
	require.NotContains(t, out, secretMarker, "命令输出与错误都不得回显文件内容")

	// install 同样在源校验处失败（损坏来源不写任何目标目录）。
	out, err = executeAssets(t, deps, "install", stage)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secretMarker)
	require.NotContains(t, out, secretMarker)
	require.NoDirExists(t, filepath.Join(work, "assets-root", "bundles"))
}

// C3：Worker 只读列表。
func TestCLI_WorkersListReadOnly(t *testing.T) {
	startedA := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	heartbeatA := startedA.Add(90 * time.Minute)
	stoppedAt := startedA.Add(2 * time.Hour)
	lister := &fakeWorkerLister{nodes: []core.WorkerNodeView{
		{
			InstanceID:      "abcdef0123456789abcdef0123456789",
			Name:            "worker-a",
			Hostname:        "host-a",
			BuildVersion:    "0.24.0",
			SchemaVersion:   12,
			StartedAt:       startedA,
			LastHeartbeatAt: heartbeatA,
			ActiveLeases:    2,
		},
		{
			InstanceID:      "0123456789abcdef0123456789abcdef",
			Name:            "worker-a", // Name 允许重复：身份是 InstanceID，不是 Name。
			StartedAt:       startedA,
			LastHeartbeatAt: heartbeatA,
			StoppedAt:       &stoppedAt,
			ActiveLeases:    0,
		},
	}}
	deps := Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Workers: lister}

	// 表格：五列只读信息，instance 在前、name 只作诊断。
	out, err := executeWorkers(t, deps, "list")
	require.NoError(t, err)
	require.Contains(t, out, "INSTANCE")
	require.Contains(t, out, "ACTIVE LEASES")
	require.Contains(t, out, "abcdef0123456789abcdef0123456789")
	require.Contains(t, out, "0123456789abcdef0123456789abcdef")
	require.Contains(t, out, "2026-11-06T10:00:00Z")
	require.Contains(t, out, "2026-11-06T11:30:00Z")
	require.Contains(t, out, "2026-11-06T12:00:00Z", "已停机实例显示停机时间")
	require.Contains(t, out, "—", "未停机实例的停机列是占位符")
	require.Less(t, strings.Index(out, "INSTANCE"), strings.Index(out, "NAME"),
		"instance 是首列：身份以 InstanceID 为准，Name 只作诊断")
	require.Less(t, strings.Index(out, "NAME"), strings.Index(out, "ACTIVE LEASES"))

	// JSON：与表格同源同形；空表是 [] 而不是 null。
	out, err = executeWorkers(t, deps, "list", "--json")
	require.NoError(t, err)
	var payload []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	require.Len(t, payload, 2)
	require.Equal(t, "abcdef0123456789abcdef0123456789", payload[0]["instance"])
	require.Equal(t, "worker-a", payload[0]["name"])
	require.Equal(t, "2026-11-06T10:00:00Z", payload[0]["started_at"])
	require.Equal(t, "2026-11-06T11:30:00Z", payload[0]["heartbeat_at"])
	require.Equal(t, float64(2), payload[0]["active_leases"])
	require.NotContains(t, payload[0], "stopped_at", "未停机实例不输出 stopped_at")
	require.Equal(t, "2026-11-06T12:00:00Z", payload[1]["stopped_at"])
	require.Equal(t, 2, lister.calls, "每条子命令只调一次只读端口")

	// 空列表：表格给出明确文案，JSON 是空数组。
	empty := &fakeWorkerLister{}
	out, err = executeWorkers(t, Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Workers: empty}, "list")
	require.NoError(t, err)
	require.Contains(t, out, "（没有已登记的 Worker 实例）")
	out, err = executeWorkers(t, Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Workers: empty}, "list", "--json")
	require.NoError(t, err)
	require.JSONEq(t, "[]", out)

	// 缺端口：命令面不得自己打开数据库补出第二条读面。
	_, err = executeWorkers(t, Deps{WorkDir: t.TempDir(), Logger: quietLogger()}, "list")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Worker 端口")

	// 端口失败：原样上抛、非空，不重试。
	broken := &fakeWorkerLister{err: errors.New("数据库不可用：连接被拒绝")}
	_, err = executeWorkers(t, Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Workers: broken}, "list")
	require.Error(t, err)
	require.Contains(t, err.Error(), "连接被拒绝")
	require.Equal(t, 1, broken.calls, "失败不得重试")
}
