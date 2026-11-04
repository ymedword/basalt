package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/ymedword/basalt/internal/core"
)

type artifact struct {
	Name   string
	Size   int64
	SHA256 string
}

type (
	artifactEntry struct {
		Name string
		Mode fs.FileMode
		Size int64
	}
	artifactLimits struct {
		MaxFiles                                                         int
		MaxFileBytes, MaxTotalBytes, MaxInstanceBytes, InstanceUsedBytes int64
	}
)

func classifyArtifactEntries(entries []artifactEntry, limits artifactLimits) ([]string, string) {
	if limits.MaxFiles <= 0 || limits.MaxFileBytes <= 0 || limits.MaxTotalBytes <= 0 || len(entries) > limits.MaxFiles {
		return nil, "artifact_rejected"
	}
	names := make([]string, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if !safeExecutionSegment(entry.Name) || !entry.Mode.IsRegular() || entry.Size < 0 || entry.Size > limits.MaxFileBytes || entry.Size > limits.MaxTotalBytes-total {
			return nil, "artifact_rejected"
		}
		total += entry.Size
		names = append(names, entry.Name)
	}
	if limits.MaxInstanceBytes > 0 && (limits.InstanceUsedBytes > limits.MaxInstanceBytes || total > limits.MaxInstanceBytes-limits.InstanceUsedBytes) {
		return nil, "limit_exceeded"
	}
	return names, ""
}

func scanArtifacts(dir string, limits artifactLimits) ([]artifact, string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "artifact_rejected"
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, "artifact_rejected"
	}
	entries, readErr := directory.ReadDir(limits.MaxFiles + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return nil, "artifact_rejected"
	}
	values := make([]artifactEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return nil, "artifact_rejected"
		}
		values = append(values, artifactEntry{Name: entry.Name(), Mode: info.Mode(), Size: info.Size()})
	}
	names, note := classifyArtifactEntries(values, limits)
	if note != "" {
		return nil, note
	}
	items := make([]artifact, 0, len(names))
	for i, name := range names {
		file, err := root.Open(name)
		if err != nil {
			return nil, "artifact_rejected"
		}
		hash := sha256.New()
		n, copyErr := io.Copy(hash, io.LimitReader(file, limits.MaxFileBytes+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || n != values[i].Size || n > limits.MaxFileBytes {
			return nil, "artifact_rejected"
		}
		items = append(items, artifact{Name: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	return items, ""
}

type publishOptions struct {
	Policy                                      core.Policy
	Mu                                          *sync.Mutex
	Root, SourceDir, AgentDir, AgentName, RunID string
	ToolInvocationID                            int64
	MaxInstanceBytes                            int64
}

func instanceArtifactBytes(root *os.Root, limit int64) (int64, error) {
	agents, err := fs.ReadDir(root.FS(), "agents")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		base := "agents/" + agent.Name() + "/artifacts"
		err := fs.WalkDir(root.FS(), base, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) && path == base {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return errors.New("既有产物目录包含链接")
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit-total {
				return isolationError("limit_exceeded", errors.New("实例产物总量已满或包含非法项"))
			}
			total += info.Size()
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func publishArtifacts(ctx context.Context, opts publishOptions, items []artifact) (published []artifact, err error) {
	if opts.Mu == nil || opts.Policy == nil || opts.MaxInstanceBytes <= 0 {
		return nil, errors.New("产物发布未装配")
	}
	opts.Mu.Lock()
	defer opts.Mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	used, err := instanceArtifactBytes(root, opts.MaxInstanceBytes)
	if err != nil {
		return nil, err
	}
	var bytes int64
	for _, item := range items {
		if !safeExecutionSegment(item.Name) || item.Size < 0 || item.Size > opts.MaxInstanceBytes-used-bytes {
			return nil, isolationError("limit_exceeded", errors.New("实例产物总量超限"))
		}
		bytes += item.Size
	}
	if len(items) == 0 {
		return []artifact{}, nil
	}
	rel := filepath.Join("artifacts", opts.RunID, strconv.FormatInt(opts.ToolInvocationID, 10))
	if !safeExecutionSegment(opts.RunID) || opts.ToolInvocationID <= 0 || !safeExecutionSegment(opts.AgentName) {
		return nil, errors.New("产物发布身份无效")
	}
	if foldCase(filepath.Clean(opts.AgentDir)) != foldCase(filepath.Join(opts.Root, "agents", opts.AgentName)) {
		return nil, errors.New("产物 Agent 目录不一致")
	}
	// 先授权已有 Agent 目录下的产物根，再建受控目录；逐文件判定仍在写入前。
	// Authorize the artifact root before creating controlled parents, then recheck each file before writing.
	if err := opts.Policy.Check(ctx, core.Action{Type: core.ActionFileWrite, Target: actionTarget(opts.Root, filepath.Join(opts.AgentDir, "artifacts")), Agent: opts.AgentName, RunBound: true}); err != nil {
		return nil, err
	}
	agent, err := root.OpenRoot(filepath.Join("agents", opts.AgentName))
	if err != nil {
		return nil, err
	}
	defer agent.Close()
	if err := agent.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return nil, err
	}
	if err := agent.Mkdir(rel, 0o755); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, agent.RemoveAll(rel))
			published = nil
		}
	}()
	for _, item := range items {
		target := filepath.Join(opts.AgentDir, rel, item.Name)
		if err := opts.Policy.Check(ctx, core.Action{Type: core.ActionFileWrite, Target: actionTarget(opts.Root, target), Agent: opts.AgentName, RunBound: true}); err != nil {
			return nil, err
		}
	}
	source, err := os.OpenRoot(opts.SourceDir)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := source.Lstat(item.Name)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("产物复制前类型改变")
		}
		from, err := source.Open(item.Name)
		if err != nil {
			return nil, err
		}
		to, err := agent.OpenFile(filepath.Join(rel, item.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			_ = from.Close()
			return nil, err
		}
		hash := sha256.New()
		n, copyErr := copyArtifact(ctx, io.MultiWriter(to, hash), io.LimitReader(from, item.Size+1))
		closeErr := errors.Join(to.Close(), from.Close())
		if copyErr != nil || closeErr != nil {
			return nil, errors.Join(copyErr, closeErr)
		}
		if n != item.Size || hex.EncodeToString(hash.Sum(nil)) != item.SHA256 {
			return nil, errors.New("产物复制字节或哈希校验失败")
		}
		item.Name = filepath.ToSlash(filepath.Join(rel, item.Name))
		published = append(published, item)
	}
	return published, ctx.Err()
}

// 每块读写及 EOF 后都检查取消；最后一块期间取消也不得提交产物。
// Check cancellation at chunk boundaries and EOF, including the final write.
func copyArtifact(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if n > 0 {
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (t *RunCodeTool) collectAndPublishRunCode(ctx context.Context, env core.ToolEnv, stage runCodeStage, _ core.CommandResult) ([]artifact, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, reasonForContext(err, "artifact_publish_failed"), err
	}
	items, note := scanArtifacts(stage.OutputDir, artifactLimits{MaxFiles: t.opts.MaxArtifactFiles, MaxFileBytes: t.opts.MaxArtifactBytes, MaxTotalBytes: t.opts.MaxArtifactBytes, MaxInstanceBytes: t.opts.MaxTotalArtifactBytes})
	if note != "" {
		return nil, note, errors.New("执行产物未发布")
	}
	published, err := publishArtifacts(ctx, publishOptions{Policy: t.opts.Policy, Mu: t.opts.PublishMu, Root: t.opts.Root, SourceDir: stage.OutputDir, AgentDir: env.AgentDir, AgentName: env.AgentName, RunID: env.RunID, ToolInvocationID: env.ToolInvocationID, MaxInstanceBytes: t.opts.MaxTotalArtifactBytes}, items)
	if err != nil {
		if isolationReason(err) == "limit_exceeded" {
			return nil, "limit_exceeded", err
		}
		return nil, reasonForContext(err, "artifact_publish_failed"), err
	}
	return published, "", nil
}
