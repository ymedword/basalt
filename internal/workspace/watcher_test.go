package workspace_test

// 本文件是配置热加载 watcher 的 13 例关键回归（颗粒度文档 §3.2 / §4.3.7、约束 28–30）：
// 初始白名单、控制目录结构性排除、非白名单路径零回调、原子替换归属、新建目录补监听、
// 删除上报、坏编辑保旧（§4.3.7 原样落地）、防抖合并、持续事件上限、去重排序、
// 并发注入（§4.4 的 011-race）、取消退出与故障重建。真实 fsnotify 用例一律带 5 秒
// 等待上限；注入式用例走 NotifyForTest，不依赖平台事件时序。
// The watcher regression suite: whitelist, control-directory exclusion, attribution,
// watch-on-demand, removal, the frozen bad-edit case, debounce/max-window, ordering,
// cancellation and rebuild. Real fsnotify cases wait with a five-second ceiling.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// watcherDebounceForTest 与生产防抖窗口同值（300ms）；等待余量按它的整数倍给。
// watcherDebounceForTest mirrors the production debounce window (300ms).
const watcherDebounceForTest = 300 * time.Millisecond

// testRegistryEntries 记住 loadEntry 装载出的旧条目：workspace 层的 watcher 只上报
// 变更集，"校验失败保留旧快照"是装配层的规则；这里以最小注册表模拟装配层，冻结
// "坏编辑不触发摘除"的契约（与 logSinks 同一手法，不引入公开测试助手）。
// testRegistryEntries replays the assembly layer's keep-old rule for the frozen case.
var testRegistryEntries sync.Map

// registryKey 以工作区根与名字拼出注册表键（根不同即不同夹具）。
// registryKey scopes cached entries to one workspace root and name.
func registryKey(root, name string) string {
	return root + "\x00" + name
}

// writeAgent 落一份最小可装载的 AGENT.md（模型名由用例给出）。
// writeAgent writes a minimal, loadable AGENT.md.
func writeAgent(t *testing.T, root, name, model string) {
	t.Helper()
	writeFileInRoot(t, root, filepath.Join("agents", name, "AGENT.md"),
		"---\nname: "+name+"\nprovider:\n  name: \"mock\"\n  model: \""+model+"\"\n---\n\n你是 "+name+"。\n")
}

// writeFile 覆盖写入一个文件（父目录必须已存在）。
// writeFile overwrites one file; its parent must already exist.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// findEntry 装载工作区并查找已注册的指定 Agent；单只损坏不影响其余（装载器契约）。
// findEntry loads the workspace and looks for the registered agent.
func findEntry(t *testing.T, root, name string) (core.AgentEntry, bool) {
	t.Helper()
	entries, err := newTestLoader(t, nil, nil, "mock").LoadAll(root)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name == name && entry.Registered {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// loadEntry 装载并记住旧条目（坏编辑后 oldRegistryEntry 用它证明旧快照未丢）。
// loadEntry loads the entry and caches it for the keep-old assertion.
func loadEntry(t *testing.T, root, name string) core.AgentEntry {
	t.Helper()
	entry, ok := findEntry(t, root, name)
	require.True(t, ok, "夹具 Agent 必须装载成功：%s", name)
	testRegistryEntries.Store(registryKey(root, name), entry)
	return entry
}

// oldRegistryEntry 复读注册表；装载失败时回落到 loadEntry 记录的旧条目——这正是
// 装配层"校验失败保留旧快照"的规则（约束 28），watcher 只负责上报变更。
// oldRegistryEntry re-reads the registry and falls back to the cached old entry,
// mirroring the assembly layer's keep-old rule.
func oldRegistryEntry(t *testing.T, root, name string) (core.AgentEntry, bool) {
	t.Helper()
	if entry, ok := findEntry(t, root, name); ok {
		return entry, true
	}
	cached, ok := testRegistryEntries.Load(registryKey(root, name))
	if !ok {
		return core.AgentEntry{}, false
	}
	return cached.(core.AgentEntry), true
}

// startWatcher 建 watcher 并把每次回调投进带缓冲通道；关闭由用例结束统一执行。
// startWatcher builds a watcher whose callbacks land on a buffered channel.
func startWatcher(t *testing.T, root string) (*workspace.Watcher, <-chan workspace.ChangeSet) {
	t.Helper()
	changes := make(chan workspace.ChangeSet, 8)
	w, err := workspace.NewWatcher(root, func(cs workspace.ChangeSet) {
		changes <- cs
	}, bufferedLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	return w, changes
}

// runWatcher 启动 Run 并注册退出核对；Cleanup 后进先出——先注册核对、再注册取消，
// 结束顺序才是"取消 → 等 Run 退出 → Close"。
// runWatcher starts Run; cleanups run cancel first, then the exit check, then Close.
func runWatcher(t *testing.T, w *workspace.Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("Run 必须在取消后退出")
		}
	})
	t.Cleanup(cancel)
}

// requireLogEventually 轮询测试日志缓冲，直到出现给定子串（故障 WARN 由 Run 协程
// 异步写出，判定必须等它落地）；3 秒仍未出现即失败并贴出实际日志。
// requireLogEventually polls the captured log until the substring shows up.
func requireLogEventually(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logText(t), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("日志始终缺少 %q；实际日志:\n%s", substr, logText(t))
}

// waitChange 等待一次变更集；超时即判失败（真实 fsnotify 用例的等待上限 5 秒）。
// waitChange waits for one change set with a five-second ceiling.
func waitChange(t *testing.T, changes <-chan workspace.ChangeSet) workspace.ChangeSet {
	t.Helper()
	select {
	case cs := <-changes:
		return cs
	case <-time.After(5 * time.Second):
		t.Fatal("等待变更集超时")
		return workspace.ChangeSet{}
	}
}

// requireNoChange 在给定窗口内断言没有回调（防抖余量由调用方给足）。
// requireNoChange asserts silence for the given window.
func requireNoChange(t *testing.T, changes <-chan workspace.ChangeSet, window time.Duration) {
	t.Helper()
	select {
	case cs := <-changes:
		t.Fatalf("不该有变更集，实得 %+v", cs)
	case <-time.After(window):
	}
}

// TestWatcher_NewStartsWithWhitelistedDirs：初始 watch 恰好是 `agents/`、`skills/`
// 两个根与各自的一层子目录；控制目录不在其中（约束 30 的结构性排除）。
// 逆改点：递归监听 / 监听工作区根 → 集合多出 control 或 roots 断言红。
func TestWatcher_NewStartsWithWhitelistedDirs(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgent(t, root, "reporter", "ok")
	writeFileInRoot(t, root, filepath.Join("skills", "digest", "SKILL.md"), "---\nname: digest\n---\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "control", "agents"), 0o750))

	w, _ := startWatcher(t, root)

	want := []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "agents", "reporter"),
		filepath.Join(root, "skills"),
		filepath.Join(root, "skills", "digest"),
	}
	sort.Strings(want)
	require.Equal(t, want, w.WatchPathsForTest())
}

// TestWatcher_ControlDirNeverWatched：控制目录与工作区根文件都不在监听集合里，
// 注入这些路径也不产生回调（约束 30：状态文件变化不得触发注册表刷新）。
// 逆改点：把 control/ 加进白名单 → 监听集合或零回调断言红。
func TestWatcher_ControlDirNeverWatched(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	stateFile := filepath.Join(root, "control", "agents", "reporter.json")
	writeFileInRoot(t, root, filepath.Join("control", "agents", "reporter.json"), "{}")

	w, changes := startWatcher(t, root)
	for _, dir := range w.WatchPathsForTest() {
		require.NotContains(t, dir, "control", "控制目录不得出现在监听集合")
	}

	w.NotifyForTest(stateFile)
	requireNoChange(t, changes, 2*watcherDebounceForTest)
}

// TestWatcher_IgnoresNonWhitelistedPaths：工作区根下的 mcp_servers.yaml / AGENTS.md /
// 记忆目录都不触发注册表刷新（约束 29：这些变更需重启或走别的通道）。
// 逆改点：归属只看"是否在根下" → 回调断言红。
func TestWatcher_IgnoresNonWhitelistedPaths(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)

	for _, rel := range []string{"mcp_servers.yaml", "AGENTS.md", filepath.Join("memory", "MEMORY.md")} {
		w.NotifyForTest(filepath.Join(root, rel))
	}
	requireNoChange(t, changes, 2*watcherDebounceForTest)
}

// TestWatcher_AtomicReplaceMarksAgent：编辑器式原子替换（写临时文件再 rename）只
// 触发一条归属到该 Agent 的变更集（约束 28：rename 也要被看到）。
// 逆改点：只认 WRITE 事件 / 丢弃临时文件所在目录的路径 → 变更集为空。
func TestWatcher_AtomicReplaceMarksAgent(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgent(t, root, "reporter", "old")
	w, changes := startWatcher(t, root)

	target := filepath.Join(root, "agents", "reporter", "AGENT.md")
	tmp := filepath.Join(root, "agents", "reporter", ".tmp-AGENT.md")
	writeFile(t, tmp, "---\nname: reporter\nprovider:\n  name: \"mock\"\n  model: \"new\"\n---\n\n新正文。\n")
	require.NoError(t, os.Rename(tmp, target))

	w.NotifyForTest(target)
	cs := waitChange(t, changes)
	require.Equal(t, []string{"reporter"}, cs.Agents)
	require.Empty(t, cs.Skills)
}

// TestWatcher_NewDirectoryAddsWatch：新建 Agent 目录补一条 watch——目录创建上报一次，
// 随后写进该目录的文件必须**再次**触发变更（没有补监听就只有第一条）。
// 逆改点：不补 watch → 第二次等待超时。
func TestWatcher_NewDirectoryAddsWatch(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)
	runWatcher(t, w)

	dir := filepath.Join(root, "agents", "newbie")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	cs := waitChange(t, changes)
	require.Equal(t, []string{"newbie"}, cs.Agents)

	writeFile(t, filepath.Join(dir, "AGENT.md"),
		"---\nname: newbie\nprovider:\n  name: \"mock\"\n  model: \"ok\"\n---\n\n你好。\n")
	cs = waitChange(t, changes)
	require.Equal(t, []string{"newbie"}, cs.Agents, "补监听后目录内写入也必须上报")
}

// TestWatcher_RemovedAgentIsReported：删除 Agent 目录必须上报该名字（装配层据此
// 退休调度并移出注册表）。
// 逆改点：只上报 Create/Write → 删除后无变更集，等待超时。
func TestWatcher_RemovedAgentIsReported(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgent(t, root, "reporter", "ok")
	w, changes := startWatcher(t, root)
	runWatcher(t, w)

	require.NoError(t, os.RemoveAll(filepath.Join(root, "agents", "reporter")))
	cs := waitChange(t, changes)
	require.Equal(t, []string{"reporter"}, cs.Agents)
}

// TestWatcher_BadEditKeepsOldEntry 按模块卡 §4.3.7 **原样落地**（保留 t.Parallel 与
// 全部断言；`old` 在末断言中参与比较——冻结片段未使用该变量，原样会编译失败，
// 这是最小必要适配）：写坏 AGENT.md 后变更集必须点名该 Agent，且旧条目不被摘除。
func TestWatcher_BadEditKeepsOldEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeAgent(t, root, "reporter", "ok")
	old := loadEntry(t, root, "reporter")

	var (
		mu  sync.Mutex
		got workspace.ChangeSet
	)
	w, err := workspace.NewWatcher(root, func(cs workspace.ChangeSet) {
		mu.Lock()
		got = cs
		mu.Unlock()
	}, bufferedLogger(t))
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()
	require.NoError(t, w.AddForTest(filepath.Join(root, "agents", "reporter")))

	// 写坏 AGENT.md
	writeFile(t, filepath.Join(root, "agents", "reporter", "AGENT.md"), "not: [valid")
	w.NotifyForTest(filepath.Join(root, "agents", "reporter", "AGENT.md"))

	select {
	case <-time.After(2 * time.Second):
		t.Fatal("no change set")
	case <-w.EventsForTest():
	}
	mu.Lock()
	changeSet := got
	mu.Unlock()
	if len(changeSet.Agents) == 0 {
		t.Fatalf("change set = %+v", changeSet)
	}
	entry, ok := oldRegistryEntry(t, root, "reporter")
	if !ok || entry.Name != old.Name {
		t.Fatal("old entry dropped after invalid edit")
	}
}

// TestWatcher_DebounceCoalescesBurst：静默窗口内的多条事件合并成一次回调；窗口之后
// 的新事件再产生第二次回调（循环不退出）。
// 逆改点：每条事件一次回调 → 第一次回调之后 600ms 内出现第二次，红。
func TestWatcher_DebounceCoalescesBurst(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)

	var calls atomic.Int32
	stop := make(chan struct{})
	counting := make(chan workspace.ChangeSet, 8)
	go func() {
		defer close(counting)
		for {
			select {
			case cs := <-changes:
				calls.Add(1)
				counting <- cs
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop) })

	for _, name := range []string{"a", "b", "c"} {
		w.NotifyForTest(filepath.Join(root, "agents", name, "AGENT.md"))
	}
	cs := waitChange(t, counting)
	require.Equal(t, []string{"a", "b", "c"}, cs.Agents, "窗口内的事件必须合并成一次回调")
	requireNoChange(t, counting, 2*watcherDebounceForTest)
	require.Equal(t, int32(1), calls.Load())

	w.NotifyForTest(filepath.Join(root, "agents", "d", "AGENT.md"))
	require.Equal(t, []string{"d"}, waitChange(t, counting).Agents, "循环必须继续工作")
}

// TestWatcher_ConcurrentEvents：多协程并发注入变更路径时，防抖循环（待发集合的唯一
// 持有者）不得丢名字，单个变更集保持排序去重，爆发之后循环仍存活；-race 下同时校验
// 注入通道与回调通道无竞态（模块卡 §4.4 的 011-race required_tests）。
// 逆改点：让回调协程共享待发 map 直接写 → 竞态检测红；并发路径被丢弃 → 并集缺名。
// Concurrent injection must neither drop names nor break ordering, and the debounce
// loop must survive the burst; the test is meaningful under -race.
func TestWatcher_ConcurrentEvents(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)

	const writers = 8
	const perWriter = 8
	want := make([]string, 0, writers)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < writers; i++ {
		name := fmt.Sprintf("concurrent-%02d", i)
		want = append(want, name)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-gate
			for j := 0; j < perWriter; j++ {
				w.NotifyForTest(filepath.Join(root, "agents", name, "AGENT.md"))
			}
		}(name)
	}
	close(gate)
	wg.Wait()
	sort.Strings(want)

	seen := make(map[string]struct{}, len(want))
	deadline := time.After(5 * time.Second)
	for len(seen) < len(want) {
		select {
		case cs := <-changes:
			require.True(t, slices.IsSorted(cs.Agents), "变更集必须按字节序排序：%v", cs.Agents)
			require.Len(t, slices.Compact(slices.Clone(cs.Agents)), len(cs.Agents),
				"变更集不得含重复名字：%v", cs.Agents)
			for _, name := range cs.Agents {
				seen[name] = struct{}{}
			}
		case <-deadline:
			t.Fatalf("并发注入后仍缺名字：已见 %d/%d %v", len(seen), len(want), seen)
		}
	}

	// 让残余防抖回调落地并清空缓冲，再验证循环在爆发后仍然存活。
	time.Sleep(2 * watcherDebounceForTest)
	for {
		select {
		case <-changes:
			continue
		default:
		}
		break
	}
	w.NotifyForTest(filepath.Join(root, "agents", "after-burst", "AGENT.md"))
	require.Equal(t, []string{"after-burst"}, waitChange(t, changes).Agents,
		"爆发之后防抖循环必须仍然存活")
}

// TestWatcher_MaxWindowBoundsContinuousEvents：持续事件最多推迟到首条事件后 1 秒——
// 用例以 80ms 间隔连发 2 秒，纯防抖复位要到 ~2.3 秒才回调，必须在此之前看到第一次。
// 逆改点：只做 debounce 复位 → 首次回调晚于断言上限，红。
func TestWatcher_MaxWindowBoundsContinuousEvents(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)

	var firstAt time.Duration
	start := time.Now()
	for i := 0; i < 25; i++ {
		select {
		case <-changes:
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
		default:
		}
		w.NotifyForTest(filepath.Join(root, "agents", "reporter", "AGENT.md"))
		time.Sleep(80 * time.Millisecond)
	}
	if firstAt == 0 {
		select {
		case <-changes:
			firstAt = time.Since(start)
		default:
		}
	}
	require.NotZero(t, firstAt, "持续事件也必须至少回调一次")
	require.Less(t, firstAt, 1600*time.Millisecond, "首次回调不得晚于 maxWindow + 余量")
}

// TestWatcher_ChangeSetSortedAndDeduped：同一窗口内重复路径只算一次，名字按字节序
// 稳定排序（事件到达顺序不影响结果）。
// 逆改点：直接回传到达顺序 / 不去重 → 切片断言红。
func TestWatcher_ChangeSetSortedAndDeduped(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, changes := startWatcher(t, root)

	for _, path := range []string{
		filepath.Join(root, "agents", "writer", "AGENT.md"),
		filepath.Join(root, "agents", "reporter", "AGENT.md"),
		filepath.Join(root, "agents", "writer", "AGENT.md"),
		filepath.Join(root, "skills", "digest", "SKILL.md"),
		filepath.Join(root, "skills", "archiver", "SKILL.md"),
	} {
		w.NotifyForTest(path)
	}
	cs := waitChange(t, changes)
	require.Equal(t, []string{"reporter", "writer"}, cs.Agents)
	require.Equal(t, []string{"archiver", "digest"}, cs.Skills)
}

// TestWatcher_RunStopsOnContextCancel：ctx 取消后 Run 退出（nil），且 Close 幂等。
// 逆改点：Run 忽略 ctx → 退出等待超时。
func TestWatcher_RunStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	w, _ := startWatcher(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run 必须在 ctx 取消后退出")
	}
	require.NoError(t, w.Close())
	require.NoError(t, w.Close(), "Close 必须可重复调用")
}

// TestWatcher_RebuildsAfterFailure：监听实例故障（模拟为被关闭）后记 WARN、退避
// 重建，重建之后的文件变更仍能送达（不静默停摆）。
// 逆改点：故障后直接退出 → 重建后的变更集等待超时；日志断言同步红。
func TestWatcher_RebuildsAfterFailure(t *testing.T) {
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgent(t, root, "reporter", "ok")
	w, changes := startWatcher(t, root)
	runWatcher(t, w)

	require.NoError(t, w.BreakForTest())
	requireLogEventually(t, "重建")

	// 退避 1 秒 + 防抖余量之后，重建完成再制造变更。
	time.Sleep(time.Second + 2*watcherDebounceForTest)
	writeFile(t, filepath.Join(root, "agents", "reporter", "AGENT.md"),
		"---\nname: reporter\nprovider:\n  name: \"mock\"\n  model: \"after-rebuild\"\n---\n\n重建后。\n")
	cs := waitChange(t, changes)
	require.Equal(t, []string{"reporter"}, cs.Agents)
}
