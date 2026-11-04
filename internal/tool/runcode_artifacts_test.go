package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

type us20CancelWriter struct{ cancel context.CancelFunc }

func (w us20CancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestUS20_ArtifactCancelFinalWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := copyArtifact(ctx, us20CancelWriter{cancel: cancel}, strings.NewReader("last chunk"))
	if n != 10 || !errors.Is(err, context.Canceled) {
		t.Fatalf("last write cancellation accepted: %d %v", n, err)
	}
}

func us20ArtifactLimits() artifactLimits {
	return artifactLimits{MaxFiles: 2, MaxFileBytes: 32, MaxTotalBytes: 32, MaxInstanceBytes: 64}
}

func TestUS20_ArtifactScanner_RejectsSymlink(t *testing.T) {
	for _, mode := range []fs.FileMode{fs.ModeSymlink, fs.ModeNamedPipe, fs.ModeDevice, fs.ModeSocket} {
		if _, reason := classifyArtifactEntries([]artifactEntry{{Name: "out", Mode: mode, Size: 1}}, us20ArtifactLimits()); reason != "artifact_rejected" {
			t.Fatalf("unsafe type passed %s", mode)
		}
	}
}

func TestUS20_ArtifactScan_Directory(t *testing.T) {
	if _, reason := classifyArtifactEntries([]artifactEntry{{Name: "nested", Mode: fs.ModeDir}}, us20ArtifactLimits()); reason != "artifact_rejected" {
		t.Fatal("directory accepted")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, reason := scanArtifacts(dir, us20ArtifactLimits()); reason != "artifact_rejected" {
		t.Fatal("scanner skipped directory")
	}
}

func TestUS20_ArtifactLimits(t *testing.T) {
	for _, entries := range [][]artifactEntry{{{Name: "../out", Size: 1}}, {{Name: "big", Size: 33}}, {{Name: "a", Size: 17}, {Name: "b", Size: 16}}, {{Name: "a"}, {Name: "b"}, {Name: "c"}}, {{Name: "a", Size: -1}}} {
		if _, note := classifyArtifactEntries(entries, us20ArtifactLimits()); note == "" {
			t.Fatalf("invalid artifacts accepted %+v", entries)
		}
	}
	limits := us20ArtifactLimits()
	limits.InstanceUsedBytes = 60
	if _, note := classifyArtifactEntries([]artifactEntry{{Name: "out", Size: 5}}, limits); note != "limit_exceeded" {
		t.Fatalf("instance limit: %s", note)
	}
}

func us20PublishFixture(t *testing.T) (publishOptions, []artifact) {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "agents", "demo")
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	data := []byte("hello")
	if err := os.WriteFile(filepath.Join(source, "out.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return publishOptions{Policy: NewWhitelistPolicy(PolicyOptions{Root: root}), Mu: &sync.Mutex{}, Root: root, SourceDir: source, AgentDir: agent, AgentName: "demo", RunID: "run", ToolInvocationID: 1, MaxInstanceBytes: 5}, []artifact{{Name: "out.txt", Size: 5, SHA256: hex.EncodeToString(sum[:])}}
}

func TestUS20_ArtifactRoundTripSource(t *testing.T) {
	opts, want := us20PublishFixture(t)
	items, note := scanArtifacts(opts.SourceDir, us20ArtifactLimits())
	if note != "" || len(items) != 1 || items[0] != want[0] {
		t.Fatalf("scan: %+v %s", items, note)
	}
	got, err := publishArtifacts(context.Background(), opts, items)
	if err != nil || len(got) != 1 {
		t.Fatalf("publish: %+v %v", got, err)
	}
	body, err := os.ReadFile(filepath.Join(opts.AgentDir, filepath.FromSlash(got[0].Name)))
	if err != nil || string(body) != "hello" {
		t.Fatalf("roundtrip: %q %v", body, err)
	}
}

func TestUS20_InstanceTotal(t *testing.T) {
	opts, items := us20PublishFixture(t)
	other := filepath.Join(opts.Root, "agents", "other", "artifacts")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "old"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := publishArtifacts(context.Background(), opts, items); isolationReason(err) != "limit_exceeded" {
		t.Fatalf("other agent ignored: %v", err)
	}
}

func TestUS20_ArtifactPublishSerialized(t *testing.T) {
	opts, items := us20PublishFixture(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	start := make(chan struct{})
	for i := int64(1); i <= 12; i++ {
		o := opts
		o.ToolInvocationID = i
		wg.Go(func() {
			<-start
			if _, err := publishArtifacts(context.Background(), o, items); err == nil {
				successes.Add(1)
			} else if isolationReason(err) != "limit_exceeded" {
				t.Errorf("unexpected publish error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("instance check/write race: published=%d want=1", successes.Load())
	}
	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	used, err := instanceArtifactBytes(root, 5)
	if err != nil || used != 5 {
		t.Fatalf("published bytes=%d %v", used, err)
	}
}

func TestUS20_ArtifactHashFailureRollsBack(t *testing.T) {
	opts, items := us20PublishFixture(t)
	items[0].SHA256 = "bad"
	if _, err := publishArtifacts(context.Background(), opts, items); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(opts.AgentDir, "artifacts", "run", "1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial artifact left visible")
	}
}

type us20CleanupRunner struct {
	err   error
	calls int
}

func (r *us20CleanupRunner) Run(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}

func (r *us20CleanupRunner) RemoveOutputDir(_ context.Context, path string) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	return os.RemoveAll(path)
}

func TestUS20_ReleaseStage(t *testing.T) {
	for _, test := range []struct {
		name       string
		exec       *core.CommandExecution
		cleanupErr error
		want       core.CleanupState
	}{
		{"unknown", nil, nil, core.CleanupFailed}, {"container_failed", &core.CommandExecution{CleanupState: core.CleanupFailed}, nil, core.CleanupFailed}, {"output_failed", &core.CommandExecution{CleanupState: core.CleanupCleaned}, os.ErrPermission, core.CleanupFailed}, {"cleaned", &core.CommandExecution{CleanupState: core.CleanupCleaned}, nil, core.CleanupCleaned},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			stage := runCodeStage{StageDir: filepath.Join(base, "stage"), OutputDir: filepath.Join(base, "output")}
			if err := os.Mkdir(stage.StageDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(stage.OutputDir, 0o755); err != nil {
				t.Fatal(err)
			}
			r := &us20CleanupRunner{err: test.cleanupErr}
			if got := releaseStage(core.ToolEnv{}, r, stage, test.exec); got != test.want {
				t.Fatalf("cleanup=%s want=%s", got, test.want)
			}
			if test.exec == nil || test.exec.CleanupState != core.CleanupCleaned {
				if r.calls != 0 {
					t.Fatal("removed output while container unknown")
				}
				if _, err := os.Stat(stage.StageDir); err != nil {
					t.Fatal("unknown stage removed")
				}
			}
		})
	}
}
