package workspace

// 本文件是配置热加载的 watcher（颗粒度文档 §3.1.14、约束 28–30）：只**非递归**监听
// `agents/` 与 `skills/` 两层目录，新目录出现时补一条 watch；事件只做归属标记与
// 目录补监听，防抖窗口结束后回调一次 `onChange(ChangeSet)`，由装配层触发全量 rescan
// 并原子替换注册表。控制目录（`control/`）与工作区根下的其余路径**从不**加 watch——
// 白名单是结构性排除，不靠事件过滤兜底。
// This file is the hot-reload watcher. Watches are non-recursive and confined to the
// agents/ and skills/ trees; new directories are added on demand; a debounced change
// set is handed to the assembly layer, which rescans and swaps the registry atomically.
// The control directory is excluded structurally: it is never watched at all.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// watcherDebounce 是事件静默窗口：最后一条事件之后再等这么久才回调。
	// watcherDebounce is the quiet window after the last event.
	watcherDebounce = 300 * time.Millisecond
	// watcherMaxWindow 是持续事件下的上限：首条事件后最多等这么久必须回调一次，
	// 防止连续写入把通知无限后推（约束 28 的"持续事件上限 1s"）。
	// watcherMaxWindow bounds a continuous event stream so a long write burst cannot
	// postpone the callback forever.
	watcherMaxWindow = time.Second
	// watcherRetryDelay 是 fsnotify 故障后的重建退避（不静默停摆）。
	// watcherRetryDelay is the rebuild backoff after a watcher failure.
	watcherRetryDelay = time.Second
)

// ChangeSet 是一次防抖窗口内发生变化的名字集合：名字去重、按字节序排序，
// 装配层据此做全量 rescan 与定向日志（它**不是**最终的注册表内容）。
// ChangeSet is the debounced, deduplicated, sorted set of changed names. It tells the
// assembly layer what to rescan; it is never the registry content itself.
type ChangeSet struct {
	// Agents 是变化涉及的 Agent 目录名。
	Agents []string
	// Skills 是变化涉及的技能目录名。
	Skills []string
}

// Watcher 监听工作区内的 Agent 与技能目录，把防抖后的变更集交给回调。
// Watcher watches the agent and skill trees and delivers debounced change sets.
type Watcher struct {
	root     string
	onChange func(ChangeSet)
	logger   *slog.Logger
	debounce time.Duration

	mu      sync.Mutex
	fsw     *fsnotify.Watcher
	watched map[string]struct{}
	closed  bool

	// incoming 接收"一条已归属的变更路径"，防抖循环是唯一消费者；fired 在每次回调
	// 完成后非阻塞投递一次信号（容量 1，重复信号合并）。
	// incoming feeds the debounce loop; fired signals one completed callback.
	incoming chan string
	fired    chan struct{}
	done     chan struct{}
	loopDone chan struct{}
	stopOnce sync.Once
}

// NewWatcher 建立初始监听并启动防抖循环：白名单是 `agents/`、`agents/<name>/`、
// `skills/`、`skills/<name>/`；不存在的目录跳过（工作区可能只有其中一类），
// 其余监听错误如实上抛。logger 为 nil 时使用默认出口。
// NewWatcher installs the initial watches and starts the debounce loop. Missing
// directories are skipped; every other watch error is returned as-is.
func NewWatcher(root string, onChange func(ChangeSet), logger *slog.Logger) (*Watcher, error) {
	if onChange == nil {
		return nil, errors.New("watcher 缺少变更回调")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析工作区根 %s: %w", root, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	w := &Watcher{
		root:     abs,
		onChange: onChange,
		logger:   logger,
		debounce: watcherDebounce,
		incoming: make(chan string, 64),
		fired:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建文件监听器: %w", err)
	}
	w.fsw = fsw
	w.watched = make(map[string]struct{})
	for _, dir := range w.watchDirs() {
		if err := w.addWatch(dir); err != nil {
			_ = fsw.Close()
			return nil, fmt.Errorf("监听目录 %s: %w", dir, err)
		}
	}
	go w.debounceLoop()
	return w, nil
}

// Run 消费 fsnotify 事件直到 ctx 取消或 Close 被调用；监听器报错或通道关闭时记 WARN、
// 退避 1 秒重建（重建包含一次目录静态扫描），**不**静默停摆。返回值只在正常停止时
// 为 nil——非法状态由 NewWatcher 拒绝，运行期故障一律走重建路径。
// Run consumes events until cancellation or Close. Watcher failures are logged and
// rebuilt after a one-second backoff; Run does not silently stop.
func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.done:
			return nil
		default:
		}
		fsw := w.current()
		if fsw == nil {
			return nil
		}
		if !w.watchEvents(ctx, fsw) {
			return nil
		}
		if !w.sleep(ctx, watcherRetryDelay) {
			return nil
		}
		if err := w.rebuild(); err != nil {
			w.logger.Warn("重建文件监听器失败，继续退避", "error", err)
		}
	}
}

// watchEvents 消费单个监听实例；返回 false = 停止（ctx / Close），true = 需要重建。
// watchEvents drains one watcher instance; false means stop, true means rebuild.
func (w *Watcher) watchEvents(ctx context.Context, fsw *fsnotify.Watcher) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-w.done:
			return false
		case ev, ok := <-fsw.Events:
			if !ok {
				w.warnRebuild()
				return true
			}
			w.handleEvent(ev)
		case err, ok := <-fsw.Errors:
			if !ok {
				w.warnRebuild()
				return true
			}
			if err == nil {
				continue
			}
			w.logger.Warn("文件监听器报错，1 秒后重建", "error", err)
			return true
		}
	}
}

// warnRebuild 在监听通道被外部关闭时留 WARN；正常 Close 先关 done，不误报。
// warnRebuild logs a rebuild warning for an externally closed channel; a normal
// Close has already set closed and stays silent.
func (w *Watcher) warnRebuild() {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if !closed {
		w.logger.Warn("文件监听通道已关闭，1 秒后重建")
	}
}

// handleEvent 只做三件事：白名单归属、新目录补监听、把路径投进防抖通道。
// handleEvent attributes the path, adds a watch for a new first-level directory, and
// queues the path; anything outside the whitelist is dropped here.
func (w *Watcher) handleEvent(ev fsnotify.Event) {
	rel, ok := w.relPath(ev.Name)
	if !ok {
		return
	}
	if _, _, ok := watchTarget(rel); !ok {
		return
	}
	if ev.Op&fsnotify.Create != 0 && isFirstLevelDir(rel) {
		info, err := os.Stat(ev.Name)
		if err == nil && info.IsDir() {
			if addErr := w.addWatch(ev.Name); addErr != nil {
				w.logger.Warn("watcher 补监听目录失败", "path", rel, "error", addErr)
			}
		}
	}
	w.queue(ev.Name)
}

// queue 把一条已归属路径交给防抖循环；watcher 已关闭时丢弃。
// queue hands a path to the debounce loop; a closed watcher drops it.
func (w *Watcher) queue(path string) {
	select {
	case w.incoming <- path:
	case <-w.done:
	}
}

// debounceLoop 是唯一持有待发集合与计时器的协程：静默 debounce 后回调，持续事件
// 不超过 maxWindow。回调在锁外同步执行，回调内部可以安全地读 watcher 状态。
// debounceLoop owns the pending set and the timer; the callback runs synchronously
// outside any lock so it may inspect the watcher safely.
func (w *Watcher) debounceLoop() {
	defer close(w.loopDone)
	pending := make(map[string]map[string]struct{})
	var since time.Time
	var timer *time.Timer
	var timerC <-chan time.Time

	fire := func() {
		agents := sortedNames(pending["agents"])
		skills := sortedNames(pending["skills"])
		pending = make(map[string]map[string]struct{})
		since = time.Time{}
		timer, timerC = nil, nil
		if len(agents) == 0 && len(skills) == 0 {
			return
		}
		w.onChange(ChangeSet{Agents: agents, Skills: skills})
		select {
		case w.fired <- struct{}{}:
		default:
		}
	}

	for {
		select {
		case <-w.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case path := <-w.incoming:
			rel, ok := w.relPath(path)
			if !ok {
				continue
			}
			kind, name, ok := watchTarget(rel)
			if !ok {
				continue
			}
			if pending[kind] == nil {
				pending[kind] = make(map[string]struct{})
			}
			pending[kind][name] = struct{}{}

			now := time.Now()
			if since.IsZero() {
				since = now
			}
			wait := w.debounce
			if remain := time.Until(since.Add(watcherMaxWindow)); remain < wait {
				wait = remain
			}
			if wait <= 0 {
				fire()
				continue
			}
			timer, timerC = resetTimer(timer, wait)
		case <-timerC:
			timer, timerC = nil, nil
			fire()
		}
	}
}

// Close 停止防抖循环并关闭底层监听器；可重复调用。
// Close stops the debounce loop and the underlying watcher; it is idempotent.
func (w *Watcher) Close() error {
	var err error
	w.stopOnce.Do(func() {
		close(w.done)
		w.mu.Lock()
		w.closed = true
		fsw := w.fsw
		w.fsw = nil
		w.watched = nil
		w.mu.Unlock()
		if fsw != nil {
			err = fsw.Close()
		}
		<-w.loopDone
	})
	return err
}

// AddForTest 给测试补一条 watch；生产路径只经白名单在 Run 里补监听。
// AddForTest adds one watch for tests; production paths only add via the whitelist.
func (w *Watcher) AddForTest(dir string) error {
	return w.addWatch(dir)
}

// NotifyForTest 注入一条变更路径，走与真实事件相同的归属与防抖通道。
// NotifyForTest injects a change path through the same attribution and debounce path.
func (w *Watcher) NotifyForTest(path string) {
	w.queue(path)
}

// EventsForTest 返回"一次回调已完成"的信号通道（容量 1，重复信号合并）。
// EventsForTest signals completed callbacks; repeated signals coalesce.
func (w *Watcher) EventsForTest() <-chan struct{} {
	return w.fired
}

// WatchPathsForTest 返回当前 watch 的目录集合（排序）。
// WatchPathsForTest returns the currently watched directories in sorted order.
func (w *Watcher) WatchPathsForTest() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	dirs := make([]string, 0, len(w.watched))
	for dir := range w.watched {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// BreakForTest 关闭当前监听实例以模拟故障；Run 必须按退避重建。
// BreakForTest closes the current watcher instance to simulate a failure.
func (w *Watcher) BreakForTest() error {
	w.mu.Lock()
	fsw := w.fsw
	w.mu.Unlock()
	if fsw == nil {
		return nil
	}
	return fsw.Close()
}

// sleep 是可取消的退避等待；返回 false 表示 ctx 取消或 Close 先行。
// sleep is a cancellable backoff wait; false means stop before the delay elapsed.
func (w *Watcher) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case <-timer.C:
		return true
	}
}

// current 返回当前监听实例（可能为 nil = 已关闭）。
// current returns the live watcher instance, or nil once closed.
func (w *Watcher) current() *fsnotify.Watcher {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fsw
}

// addWatch 幂等添加一条目录 watch；目录已在事件与补监听之间被删（ENOENT）按成功处理。
// addWatch is idempotent; a directory removed between event and watch is not an error.
func (w *Watcher) addWatch(dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("watcher 已关闭")
	}
	if _, ok := w.watched[dir]; ok {
		return nil
	}
	if err := w.fsw.Add(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	w.watched[dir] = struct{}{}
	return nil
}

// rebuild 用目录静态扫描重建监听实例：白名单内的目录全部重新加 watch，
// 换出与关闭旧实例在锁外完成，关闭竞态下新实例直接丢弃。
// rebuild replaces the watcher with a freshly scanned one; a concurrent Close wins.
func (w *Watcher) rebuild() error {
	nw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("重建文件监听器: %w", err)
	}
	added := make(map[string]struct{})
	for _, dir := range w.watchDirs() {
		if err := nw.Add(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			_ = nw.Close()
			return fmt.Errorf("重建监听目录 %s: %w", dir, err)
		}
		added[dir] = struct{}{}
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		_ = nw.Close()
		return nil
	}
	old := w.fsw
	w.fsw = nw
	w.watched = added
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// watchDirs 静态扫描白名单：两个根目录 + 各自的一层子目录（不递归）。
// watchDirs scans the two roots and their first-level children, never deeper.
func (w *Watcher) watchDirs() []string {
	var dirs []string
	for _, root := range []string{"agents", "skills"} {
		base := filepath.Join(w.root, root)
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, base)
		entries, err := os.ReadDir(base)
		if err != nil {
			w.logger.Warn("watcher 扫描目录失败", "path", root, "error", err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(base, entry.Name()))
			}
		}
	}
	sort.Strings(dirs)
	return dirs
}

// relPath 把事件绝对路径换算成相对工作区根的路径；越界路径一律拒绝。
// relPath maps an event path under the workspace root; escapes are rejected.
func (w *Watcher) relPath(path string) (string, bool) {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// watchTarget 判定一条相对路径是否属于白名单内的 Agent 或技能目录：
// 只有 `agents/<name>` 与 `skills/<name>` 两层，更深层文件归属到所在目录名。
// watchTarget maps agents/<name> and skills/<name> (files included) to a change name.
func watchTarget(rel string) (kind, name string, ok bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	switch parts[0] {
	case "agents":
		if ValidateAgentName(parts[1]) != nil {
			return "", "", false
		}
		return "agents", parts[1], true
	case "skills":
		if ValidateAgentName(parts[1]) != nil {
			return "", "", false
		}
		return "skills", parts[1], true
	default:
		return "", "", false
	}
}

// isFirstLevelDir 判定相对路径是否正好是 `agents/<name>` / `skills/<name>`：
// 只有这一层需要补监听，更深的子目录按非递归约定不追。
// isFirstLevelDir reports whether the path is exactly one level under a root.
func isFirstLevelDir(rel string) bool {
	return len(strings.Split(filepath.ToSlash(rel), "/")) == 2
}

// resetTimer 复位防抖计时器（必要时先停止并排空已触发的信号）。
// resetTimer re-arms the debounce timer, draining a fired signal first.
func resetTimer(timer *time.Timer, wait time.Duration) (*time.Timer, <-chan time.Time) {
	if timer == nil {
		timer = time.NewTimer(wait)
		return timer, timer.C
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(wait)
	return timer, timer.C
}

// sortedNames 把集合排成稳定切片（回调顺序与 map 遍历顺序无关）。
// sortedNames turns a set into a stably ordered slice.
func sortedNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
