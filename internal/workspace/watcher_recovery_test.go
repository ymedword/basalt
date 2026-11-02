package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
)

// TestWatcher_RetriesTopologyRebuildWithoutNewEvent 在两次重建故障后无需新拓扑事件也会恢复。
// Two injected rebuild failures must recover without another topology event; real file writes then work.
func TestWatcher_RetriesTopologyRebuildWithoutNewEvent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "reporter")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	file := filepath.Join(dir, "AGENT.md")
	require.NoError(t, os.WriteFile(file, []byte("before"), 0o640))
	type observation struct {
		change  ChangeSet
		watcher *fsnotify.Watcher
	}
	changes := make(chan observation, 8)
	var w *Watcher
	var err error
	w, err = NewWatcher(root, func(cs ChangeSet) { changes <- observation{cs, w.current()} }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	original := w.current()
	var attempts atomic.Int32
	w.newFSWatcher = func() (*fsnotify.Watcher, error) {
		if attempts.Add(1) <= 2 {
			return nil, errors.New("synthetic watcher rebuild failure")
		}
		return fsnotify.NewWatcher()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("watcher must stop after cancellation")
		}
		require.NoError(t, w.Close())
	})
	// 仅注入一次拓扑输入来隔离重试状态机；真实目录交换另由黑盒用例覆盖。
	// A single topology input isolates retry state; the separate black-box case swaps real directories.
	select {
	case w.current().Events <- fsnotify.Event{Name: dir, Op: fsnotify.Create}:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not consume the topology input")
	}
	require.Eventually(t, func() bool { return attempts.Load() >= 3 }, 5*time.Second, 10*time.Millisecond,
		"rebuild must retry after two failures without waiting for another filesystem event")
	deadline := time.After(5 * time.Second)
rescan:
	for {
		select {
		case got := <-changes:
			if got.watcher != original {
				require.Equal(t, []string{"reporter"}, got.change.Agents)
				break rescan
			}
		case <-deadline:
			t.Fatal("successful rebuild must rescan the existing name")
		}
	}
	// 初始脏事件与恢复重扫都是合法回调；等真正静默后再证明新的文件通知。
	// Both the initial dirty event and recovery rescan are valid; drain them before the real write.
	deadline = time.After(5 * time.Second)
quiet:
	for {
		select {
		case <-changes:
		case <-time.After(600 * time.Millisecond):
			break quiet
		case <-deadline:
			t.Fatal("watcher did not settle after rebuilding")
		}
	}
	require.NoError(t, os.WriteFile(file, []byte("after retry"), 0o640))
	select {
	case got := <-changes:
		require.NotSame(t, original, got.watcher)
		require.Equal(t, []string{"reporter"}, got.change.Agents)
	case <-time.After(5 * time.Second):
		t.Fatal("real file edits must be observed after rebuild recovery")
	}
}
