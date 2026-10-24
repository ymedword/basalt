package core_test

// 本文件是运行资源领域类型的验收测试（颗粒度文档 §3.2.1：`internal/core/resources_test.go`，
// 十六例：闸门读写互斥、清单构造与规范化排序、哈希稳定性、缺失绑定拒绝、规范字节序）。
// 命名族按颗粒度文档 §4.4 固定：构造与规范化归 `TestBuildRunResourceManifest_*`，
// 哈希与序列化归 `TestRunResourceManifest_*`（族名会展开为 stage 的精确测试名）。
// 持久形态的 SQL 往返归 storage 侧用例，这里只判领域构造。
// This file covers the resources domain types: the process-wide gate, manifest
// construction/normalization, hash stability, missing-binding refusal and canonical bytes.
// The test-name families follow module card §4.4 exactly.

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// sampleResolved 造一份两 MCP、两技能（含乱序文件）的解析结果。
// sampleResolved builds a resolution with two MCP servers and two skills.
// sampleAgentUID 是构造清单用的合法 Agent UID（v2 起必填）。
// sampleAgentUID is a valid agent uid for manifest construction (required from v2 on).
const sampleAgentUID core.AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"

// sampleTargetBinding 是目标绑定哈希的合法形态（64 位小写十六进制）。
// sampleTargetBinding is a well-formed target-binding hash.
const sampleTargetBinding = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func sampleResolved() core.ResolvedResources {
	return core.ResolvedResources{
		Provider:      core.ResourceVersionRef{Name: "openai-main", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", VersionID: 7, VersionNo: 3, ContentHash: "aaa"},
		ProviderModel: "gpt-test",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: sampleTargetBinding,
		},
		Notify: core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
		MCPServers: []core.ResourceVersionRef{
			{Name: "zeta", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", VersionID: 21, VersionNo: 2, ContentHash: "zzz"},
			{Name: "alpha", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C", VersionID: 20, VersionNo: 1, ContentHash: "yyy"},
		},
		Skills: []core.SkillVersionRef{
			{
				ResourceVersionRef: core.ResourceVersionRef{Name: "writer", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D", VersionID: 31, VersionNo: 1, ContentHash: "www"},
				Files: []core.FileHash{
					{Path: "SKILL.md", SHA256: "b", Size: 2},
					{Path: "skill.json", SHA256: "a", Size: 1},
				},
			},
			{
				ResourceVersionRef: core.ResourceVersionRef{Name: "reporter", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E", VersionID: 30, VersionNo: 1, ContentHash: "rrr"},
				Files:              []core.FileHash{{Path: "SKILL.md", SHA256: "c", Size: 3}},
			},
		},
	}
}

// TestResourceGate_ReadLocksShare：多个准入读锁可以同时持有（发布只与写互斥）。
func TestResourceGate_ReadLocksShare(t *testing.T) {
	t.Parallel()
	var gate core.ResourceGate
	entered := make(chan struct{}, 4)
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.RLock()
			defer gate.RUnlock()
			entered <- struct{}{}
			<-release
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("读锁未能在写者缺席时并行持有")
		}
	}
	close(release)
	wg.Wait()
}

// TestResourceGate_ReadLockBlocksWriter：读锁持有期间发布写锁必须等待。
func TestResourceGate_ReadLockBlocksWriter(t *testing.T) {
	t.Parallel()
	var gate core.ResourceGate

	gate.RLock()
	writerAcquired := make(chan struct{})
	go func() {
		gate.Lock()
		close(writerAcquired)
		gate.Unlock()
	}()

	select {
	case <-writerAcquired:
		t.Fatal("读锁持有期间写锁不得取得")
	case <-time.After(50 * time.Millisecond):
	}
	gate.RUnlock()
	select {
	case <-writerAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("读锁释放后写锁仍未取得")
	}
}

// TestResourceGate_WriterBlocksReaders：发布写锁持有期间准入读锁必须等待。
func TestResourceGate_WriterBlocksReaders(t *testing.T) {
	t.Parallel()
	var gate core.ResourceGate

	gate.Lock()
	readerAcquired := make(chan struct{})
	go func() {
		gate.RLock()
		close(readerAcquired)
		gate.RUnlock()
	}()

	select {
	case <-readerAcquired:
		t.Fatal("写锁持有期间读锁不得取得")
	case <-time.After(50 * time.Millisecond):
	}
	gate.Unlock()
	select {
	case <-readerAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("写锁释放后读锁仍未取得")
	}
}

// TestResourceGate_ConcurrentPublishAndAdmission：并发读写之下"发布中"与"准入中"从不同时
// 成立（-race 下的行为判据，闸门语义就是这一条）。
func TestResourceGate_ConcurrentPublishAndAdmission(t *testing.T) {
	t.Parallel()
	var gate core.ResourceGate
	var readers atomic.Int64
	var publishing atomic.Bool

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				gate.RLock()
				if publishing.Load() {
					t.Error("准入期间观察到发布进行中")
				}
				readers.Add(1)
				readers.Add(-1)
				gate.RUnlock()
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				gate.Lock()
				publishing.Store(true)
				if readers.Load() != 0 {
					t.Error("发布期间观察到准入读者")
				}
				publishing.Store(false)
				gate.Unlock()
			}
		}()
	}
	wg.Wait()
}

// TestBuildRunResourceManifest_FormatVersionIsTwo：v2 清单格式号固定为 2（v1 只读保留，
// 视图按 format_version 区分代际）。
func TestBuildRunResourceManifest_FormatVersionIsTwo(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	require.Equal(t, 2, manifest.FormatVersion)
	require.Equal(t, sampleAgentUID, manifest.AgentUID)
}

// TestBuildRunResourceManifest_SortsMCPServersByName：MCP 顺序只由 name 决定，与输入序无关。
func TestBuildRunResourceManifest_SortsMCPServersByName(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	require.Equal(t, []core.ResourceVersionRef{
		{Name: "alpha", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C", VersionID: 20, VersionNo: 1, ContentHash: "yyy"},
		{Name: "zeta", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", VersionID: 21, VersionNo: 2, ContentHash: "zzz"},
	}, manifest.MCPServers)
}

// TestBuildRunResourceManifest_SortsSkillsByName：技能同样按 name 升序。
func TestBuildRunResourceManifest_SortsSkillsByName(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	require.Len(t, manifest.Skills, 2)
	require.Equal(t, "reporter", manifest.Skills[0].Name)
	require.Equal(t, "writer", manifest.Skills[1].Name)
}

// TestBuildRunResourceManifest_SortsSkillFilesByPath：每个技能的 Files 按 path 升序
// （清单哈希的稳定性来自这条规范化）。
func TestBuildRunResourceManifest_SortsSkillFilesByPath(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	require.Equal(t, []string{"SKILL.md", "skill.json"}, []string{
		manifest.Skills[1].Files[0].Path,
		manifest.Skills[1].Files[1].Path,
	})
}

// TestRunResourceManifest_HashStableAcrossInputOrder：同一集合以不同输入序构造 → 哈希与
// 序列化字节逐字相同。
func TestRunResourceManifest_HashStableAcrossInputOrder(t *testing.T) {
	t.Parallel()
	first := sampleResolved()
	second := sampleResolved()
	second.MCPServers[0], second.MCPServers[1] = second.MCPServers[1], second.MCPServers[0]
	second.Skills[0], second.Skills[1] = second.Skills[1], second.Skills[0]
	second.Skills[1].Files[0], second.Skills[1].Files[1] = second.Skills[1].Files[1], second.Skills[1].Files[0]

	manifestA, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, first)
	require.NoError(t, err)
	manifestB, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, second)
	require.NoError(t, err)

	require.Equal(t, manifestA.Hash, manifestB.Hash)
	rawA, err := core.MarshalManifest(manifestA)
	require.NoError(t, err)
	rawB, err := core.MarshalManifest(manifestB)
	require.NoError(t, err)
	require.Equal(t, rawA, rawB)
}

// TestRunResourceManifest_HashExcludesHashField：Hash 是派生值，重算时置空——把已有 Hash
// 灌回去不影响结果。
func TestRunResourceManifest_HashExcludesHashField(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)

	recomputed, err := core.HashManifest(manifest)
	require.NoError(t, err)
	require.Equal(t, manifest.Hash, recomputed)

	manifest.Hash = "sha256:not-the-hash"
	again, err := core.HashManifest(manifest)
	require.NoError(t, err)
	require.Equal(t, recomputed, again)
}

// TestRunResourceManifest_HashChangesWithLifecycleState：生命周期状态进哈希——同一资源集合
// 在 active / disabled 下是不同的清单。
func TestRunResourceManifest_HashChangesWithLifecycleState(t *testing.T) {
	t.Parallel()
	active, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	disabled, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentDisabled, sampleResolved())
	require.NoError(t, err)

	require.NotEqual(t, active.Hash, disabled.Hash)
	require.Equal(t, "active", active.AgentLifecycle)
	require.Equal(t, "disabled", disabled.AgentLifecycle)
}

// TestBuildRunResourceManifest_EmptyListsAreNonNil：无 MCP / 无技能时列表序列化为 `[]`
// 而不是 `null`——"空清单"与"未记录"必须在字节层面可区分。
func TestBuildRunResourceManifest_EmptyListsAreNonNil(t *testing.T) {
	t.Parallel()
	resolved := core.ResolvedResources{
		Provider: core.ResourceVersionRef{Name: "openai-main", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", VersionID: 1, VersionNo: 1, ContentHash: "x"},
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: sampleTargetBinding,
		},
		Notify: core.NotifyManifest{Mode: core.NotifyModeNone, Channels: []core.NotifyChannelRef{}},
	}
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	require.NotNil(t, manifest.MCPServers)
	require.NotNil(t, manifest.Skills)
	require.NotNil(t, manifest.Notify.Channels, "通知渠道集合序列化为 [] 而非 null")
	require.Empty(t, manifest.MCPServers)
	require.Empty(t, manifest.Skills)

	raw, err := core.MarshalManifest(manifest)
	require.NoError(t, err)
	require.True(t, bytes.Contains(raw, []byte(`"mcp_servers":[]`)), "空 MCP 列表必须是 []：%s", raw)
	require.True(t, bytes.Contains(raw, []byte(`"skills":[]`)), "空技能列表必须是 []：%s", raw)
}

// TestBuildRunResourceManifest_RejectsMissingProvider：缺 Agent UID 或 Provider 发布版本
// → 拒绝构造（不得落下"这一跑没用 Provider"或身份不明的假清单）。
func TestBuildRunResourceManifest_RejectsMissingProvider(t *testing.T) {
	t.Parallel()
	_, err := core.BuildRunResourceManifest("", core.AgentActive, sampleResolved())
	require.Error(t, err)
	require.Contains(t, err.Error(), "Agent UID")

	_, err = core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, core.ResolvedResources{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Provider")
}

// TestBuildRunResourceManifest_RejectsEmptyMCPOrSkillName：MCP / 技能发布版本缺名字同样拒绝。
func TestBuildRunResourceManifest_RejectsEmptyMCPOrSkillName(t *testing.T) {
	t.Parallel()

	resolved := sampleResolved()
	resolved.MCPServers = append(resolved.MCPServers, core.ResourceVersionRef{VersionID: 99})
	_, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.Error(t, err)
	require.Contains(t, err.Error(), "MCP")

	resolved = sampleResolved()
	resolved.Skills = append(resolved.Skills, core.SkillVersionRef{})
	_, err = core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.Error(t, err)
	require.Contains(t, err.Error(), "技能")
}

// TestBuildRunResourceManifest_DoesNotMutateInput：构造只读输入——调用方持有的解析结果
// 不被排序副作用改写。
func TestBuildRunResourceManifest_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	resolved := sampleResolved()
	before := resolved.MCPServers[0].Name
	skillFiles := resolved.Skills[0].Files[0].Path

	_, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	require.Equal(t, before, resolved.MCPServers[0].Name)
	require.Equal(t, skillFiles, resolved.Skills[0].Files[0].Path)
}

// TestRunResourceManifest_CanonicalJSONIsCompactAndOrdered：规范字节是紧凑 JSON 且字段按
// 声明序输出（后面跟视图字段即偏离契约形态）。
func TestRunResourceManifest_CanonicalJSONIsCompactAndOrdered(t *testing.T) {
	t.Parallel()
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, sampleResolved())
	require.NoError(t, err)
	raw, err := core.MarshalManifest(manifest)
	require.NoError(t, err)

	prefix := `{"format_version":2,"hash":"` + manifest.Hash +
		`","agent_uid":"` + sampleAgentUID.String() + `","agent_lifecycle_state":"active","provider":`
	require.True(t, strings.HasPrefix(string(raw), prefix), "字段顺序偏离契约：%s", raw)
	require.NotContains(t, string(raw), "\n")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded, 8)
	require.Contains(t, decoded, "notify")
}

// TestRunSnapshotBuilder_ProducesCanonicalHash：配置快照的构建入口把清单按 Path 升序后
// 紧凑序列化，哈希取这串字节的 sha256（固定向量钉住算法；读取顺序不得影响哈希）。
// TestRunSnapshotBuilder_ProducesCanonicalHash pins the snapshot hash algorithm with a
// fixed vector and requires the manifest order to be irrelevant.
func TestRunSnapshotBuilder_ProducesCanonicalHash(t *testing.T) {
	t.Parallel()
	profile := &core.Profile{Name: "writer"} // 段一 / Profile 来自同一次读取，由调用方保证
	snap, err := core.BuildConfigSnapshot("writer",
		[]byte("---\nname: writer\n---\n正文"), "段一", profile,
		[]core.ManifestEntry{
			{Path: "skills/writer/SKILL.md", SHA256: "bbb"},
			{Path: "AGENT.md", SHA256: "aaa"},
		})
	require.NoError(t, err)
	require.Equal(t, "ac07ac1163444e395cd3664afa6fa952c4b799f2c25cb733506b9a33a12ca458", snap.Hash,
		"哈希对象是「按 Path 升序后的紧凑 JSON」，算法不得改动")
	require.Equal(t, []core.ManifestEntry{
		{Path: "AGENT.md", SHA256: "aaa"},
		{Path: "skills/writer/SKILL.md", SHA256: "bbb"},
	}, snap.Manifest, "快照里的清单必须是排序后的形态")
	require.Equal(t, "writer", snap.AgentName)
	require.Equal(t, "---\nname: writer\n---\n正文", snap.Content)
	require.Equal(t, "段一", snap.SectionOne)
	require.Same(t, profile, snap.Profile)

	// 声明顺序换一下——同内容必须同哈希（顺序不是持久事实）。
	reordered, err := core.BuildConfigSnapshot("writer",
		[]byte("---\nname: writer\n---\n正文"), "段一", profile,
		[]core.ManifestEntry{
			{Path: "AGENT.md", SHA256: "aaa"},
			{Path: "skills/writer/SKILL.md", SHA256: "bbb"},
		})
	require.NoError(t, err)
	require.Equal(t, snap.Hash, reordered.Hash)
}

// TestRunSnapshotBuilder_DoesNotMutateManifestInput：排序在克隆上做——调用方的清单切片
// 保持原样（准入清单随后还要交给仓储写入，读方不得被组装动作改序）。
// TestRunSnapshotBuilder_DoesNotMutateManifestInput keeps the caller's slice untouched.
func TestRunSnapshotBuilder_DoesNotMutateManifestInput(t *testing.T) {
	t.Parallel()
	manifest := []core.ManifestEntry{
		{Path: "skills/writer/SKILL.md", SHA256: "bbb"},
		{Path: "AGENT.md", SHA256: "aaa"},
	}
	_, err := core.BuildConfigSnapshot("writer", nil, "", nil, manifest)
	require.NoError(t, err)
	require.Equal(t, "skills/writer/SKILL.md", manifest[0].Path)
	require.Equal(t, "AGENT.md", manifest[1].Path)
}

// TestRunSnapshotBuilder_EmptyManifestHashesCanonicalEmptyList：nil 与空清单必须落到同一
// 串字节（`[]`，不是 `null`）——"没有文件"是同一份事实，不得有两个哈希。
// TestRunSnapshotBuilder_EmptyManifestHashesCanonicalEmptyList canonicalizes nil to [].
func TestRunSnapshotBuilder_EmptyManifestHashesCanonicalEmptyList(t *testing.T) {
	t.Parallel()
	fromNil, err := core.BuildConfigSnapshot("writer", nil, "", nil, nil)
	require.NoError(t, err)
	fromEmpty, err := core.BuildConfigSnapshot("writer", nil, "", nil, []core.ManifestEntry{})
	require.NoError(t, err)
	require.Equal(t, fromNil.Hash, fromEmpty.Hash)
	require.Equal(t, "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945", fromNil.Hash,
		"空清单的规范字节是 `[]`")
	require.NotNil(t, fromNil.Manifest)
}

// TestRunSnapshotBuilder_InFlightSnapshotIgnoresLaterEdits：准入构建快照后，调用方仍持有的
// 内容字节与清单切片怎么改都不影响已冻结的快照——在途运行只读准入那一刻的版本目录与内容，
// 工作副本随后被编辑不会让它中途换配置（约束 31）。
// TestRunSnapshotBuilder_InFlightSnapshotIgnoresLaterEdits proves a built snapshot never
// aliases the caller's mutable slices: later work-copy edits cannot reach an in-flight run.
func TestRunSnapshotBuilder_InFlightSnapshotIgnoresLaterEdits(t *testing.T) {
	t.Parallel()
	profile := &core.Profile{Name: "writer"}
	content := []byte("---\nname: writer\n---\n正文")
	manifest := []core.ManifestEntry{
		{Path: "AGENT.md", SHA256: "agent-v1"},
		{Path: ".basalt/versions/skills/writer/1/SKILL.md", SHA256: "skill-v1"},
	}
	sectionOne := "## 技能\n- writer：写作（.basalt/versions/skills/writer/1/SKILL.md）\n"
	snap, err := core.BuildConfigSnapshot("writer", content, sectionOne, profile, manifest)
	require.NoError(t, err)
	wantHash := snap.Hash

	// 准入后工作副本被改：调用方手里的原始字节与清单条都变。
	content[0] = 'X'
	manifest[0].Path = ".basalt/skills/writer/SKILL.md"
	manifest[1].SHA256 = "work-copy-drift"

	require.Equal(t, "---\nname: writer\n---\n正文", snap.Content, "快照正文是准入时的字节副本")
	require.Equal(t, []core.ManifestEntry{
		{Path: ".basalt/versions/skills/writer/1/SKILL.md", SHA256: "skill-v1"},
		{Path: "AGENT.md", SHA256: "agent-v1"},
	}, snap.Manifest, "快照清单是准入时的副本，不随后续改动")
	require.Contains(t, snap.SectionOne, ".basalt/versions/skills/writer/1/SKILL.md",
		"在途运行读发布版本目录，不读工作副本")
	require.NotContains(t, snap.SectionOne, "（.basalt/skills/writer/SKILL.md）")
	require.Equal(t, wantHash, snap.Hash)
	require.Same(t, profile, snap.Profile, "Profile 与段一同出一次读取，按引用冻结")
}

// TestRunSnapshotBuilder_FrozenSkillVersionsSurviveResolutionMutation：准入构造的运行资源
// 清单把技能发布版本与文件哈希整份固定；解析结果随后被改（技能文件、版本 id、目录映射）
// 不得改写已落账的清单——在途运行按准入清单读发布目录（约束 31）。
// TestRunSnapshotBuilder_FrozenSkillVersionsSurviveResolutionMutation proves the built
// run-resource manifest owns its skill file lists and version fields.
func TestRunSnapshotBuilder_FrozenSkillVersionsSurviveResolutionMutation(t *testing.T) {
	t.Parallel()
	resolved := sampleResolved()
	resolved.SkillDirs = map[string]string{
		"writer":   ".basalt/versions/skills/writer/1",
		"reporter": ".basalt/versions/skills/reporter/1",
	}
	manifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	wantHash := manifest.Hash

	// 解析结果被后续发布 / 工作副本改动（同一份内存仍由调用方持有）。
	resolved.Skills[0].Files[0].SHA256 = "work-copy-drift"
	resolved.Skills[0].VersionID = 99
	resolved.SkillDirs["writer"] = ".basalt/skills/writer"

	require.Equal(t, wantHash, manifest.Hash, "已落账的清单哈希不得被后续解析结果改动")
	require.Len(t, manifest.Skills, 2)
	require.Equal(t, "reporter", manifest.Skills[0].Name)
	writer := manifest.Skills[1]
	require.Equal(t, "writer", writer.Name)
	require.Equal(t, int64(31), writer.VersionID, "版本 id 冻结在准入构造时")
	require.Equal(t, []core.FileHash{
		{Path: "SKILL.md", SHA256: "b", Size: 2},
		{Path: "skill.json", SHA256: "a", Size: 1},
	}, writer.Files, "技能文件清单是发布版本的副本，不与解析结果共享底层数组")

	// 对照：下一次准入读到改动后的发布版本 → 新清单、新哈希（新运行用新配置）。
	next, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	require.NotEqual(t, wantHash, next.Hash, "新运行按改动后的版本重建清单")
}

// TestRunSnapshotBuilder_SkillVersionDirsAreRuntimeOnly：SkillDirs（发布版本目录）只在运行
// 时视图里固定，不进入运行资源清单的字节与哈希——同一份发布版本在不同宿主目录下必须得到
// 同一份清单（约束 32 的"不同机器同哈希"），清单只记技能内相对路径。
// TestRunSnapshotBuilder_SkillVersionDirsAreRuntimeOnly pins the frozen ledger to the
// version refs: host directories live in ResolvedResources, never in the manifest bytes.
func TestRunSnapshotBuilder_SkillVersionDirsAreRuntimeOnly(t *testing.T) {
	t.Parallel()
	left := sampleResolved()
	left.SkillDirs = map[string]string{"writer": "/srv/basalt/.basalt/versions/skills/writer/1"}
	left.Runtime = &fakeRuntime{}
	right := sampleResolved()
	right.SkillDirs = map[string]string{"writer": `C:\basalt\.basalt\versions\skills\writer\1`}
	right.Runtime = &fakeRuntime{}

	leftManifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, left)
	require.NoError(t, err)
	rightManifest, err := core.BuildRunResourceManifest(sampleAgentUID, core.AgentActive, right)
	require.NoError(t, err)
	require.Equal(t, leftManifest.Hash, rightManifest.Hash, "宿主目录与运行时实例不得进入清单哈希")

	leftRaw, err := core.MarshalManifest(leftManifest)
	require.NoError(t, err)
	rightRaw, err := core.MarshalManifest(rightManifest)
	require.NoError(t, err)
	require.Equal(t, leftRaw, rightRaw)
	require.NotContains(t, string(leftRaw), "/srv/basalt", "清单不落宿主绝对路径")
	require.NotContains(t, string(leftRaw), ".basalt/versions", "发布目录是运行时视图，不进入持久清单")
	require.Contains(t, string(leftRaw), `"path":"SKILL.md"`, "技能文件路径相对技能目录")
}
