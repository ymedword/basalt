package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// FlowReadProgressBasis 在原审计读取成功后核验稳定文件身份。
// FlowReadProgressBasis proves a newly consumed stable file identity after the
// original successful audited read. Content changes, path aliases and model text
// cannot mint another source. Unsupported adapters provide no progress proof.
func FlowReadProgressBasis(ctx context.Context, pinned core.Tool, env core.ToolEnv, input json.RawMessage, before string, result core.Result, out core.ActionOutcome) (string, error) {
	if before == "" || out.Outcome != string(core.LLMCallSuccess) || !out.Dispatched || !out.AuditConfirmed {
		return "", nil
	}
	after, err := readFlowProgress(ctx, pinned, env, input, true)
	if err != nil {
		return "", err
	}
	parts := strings.Split(after, ":")
	if len(parts) != 4 || strings.Join(parts[:3], ":") != before || parts[0] != "file" || parts[3] != flowProgressHash(result.Content) {
		return "", nil
	}
	return strings.Join(parts[:3], ":"), nil
}

// PrepareFlowReadProgress 在唯一执行入口前仅取路径／物理身份，不读取文件正文。
// PrepareFlowReadProgress captures metadata only, never pre-audit file contents.
func PrepareFlowReadProgress(ctx context.Context, pinned core.Tool, env core.ToolEnv, input json.RawMessage) (capture string, err error) {
	return readFlowProgress(ctx, pinned, env, input, false)
}

func readFlowProgress(ctx context.Context, pinned core.Tool, env core.ToolEnv, input json.RawMessage, readContent bool) (capture string, err error) {
	reader, ok := pinned.(*ReadFileTool)
	if !ok || reader == nil {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if env.Flow == nil || env.Flow.AgentUID == "" || !env.RunBound {
		return "", core.ErrFlowDispatch
	}
	args, err := decodeFileToolInput(reader.Name(), input)
	if err != nil {
		return "", err
	}
	if err := args.check(reader.Name()); err != nil {
		return "", err
	}
	target := resolveToolPath(reader.root, env.AgentDir, args.Path)
	if err := reader.policy.Check(ctx, core.Action{Type: core.ActionFileRead, Target: actionTarget(reader.root, target), Agent: env.AgentName, SkillDirs: env.SkillDirs, RunBound: env.RunBound}); err != nil {
		return "", err
	}
	real, err := resolveRealPath(reader.root, target)
	if err != nil {
		return "", nil
	}
	rel, err := filepath.Rel(filepath.Clean(reader.root), real)
	if err != nil {
		return "", nil
	}
	root, err := os.OpenRoot(reader.root)
	if err != nil {
		return "", nil
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	file, err := root.Open(rel)
	if err != nil {
		return "", nil
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReadFileBytes {
		return "", nil
	}
	identity, err := flowFileIdentity(file, info)
	if err != nil || identity == "" {
		return "", nil
	}
	encoded, err := json.Marshal([]string{env.Flow.AgentUID, identity})
	if err != nil {
		return "", err
	}
	canonicalPath := filepath.ToSlash(filepath.Clean(rel))
	if runtime.GOOS == "windows" {
		canonicalPath = strings.ToLower(canonicalPath)
	}
	pathValue, err := json.Marshal([]string{env.Flow.AgentUID, canonicalPath})
	if err != nil {
		return "", err
	}
	proof := "file:" + flowProgressHash(string(pathValue)) + ":" + flowProgressHash(string(encoded))
	if !readContent {
		return proof, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReadFileBytes+1))
	if err != nil || len(data) > maxReadFileBytes {
		return "", nil
	}
	return proof + ":" + flowProgressHash(strings.ToValidUTF8(string(data), string(utf8.RuneError))), nil
}

func flowProgressHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
