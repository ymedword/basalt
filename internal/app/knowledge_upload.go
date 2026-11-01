package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// knowledgeUploadAdmin saves a new UTF-8 file beneath uploads/. Ingestion is a
// separate operation: a failed index never deletes the successfully saved file.
type knowledgeUploadAdmin struct {
	root       string
	maxBytes   int64
	operations core.OperationStore
	now        func() time.Time
	logger     *slog.Logger
}

func (s *knowledgeUploadAdmin) Upload(ctx context.Context, actor core.Actor, name, content string) (string, error) {
	if s == nil || s.root == "" || s.maxBytes <= 0 || s.operations == nil || s.now == nil || s.logger == nil {
		return "", errors.New("知识库上传服务未装配")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if name == "" || !filepath.IsLocal(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) ||
		(ext != ".md" && ext != ".txt") || !utf8.ValidString(content) || strings.ContainsRune(content, 0) || int64(len(content)) > s.maxBytes {
		return "", &core.InvalidInputError{Tool: "knowledge.upload", Reason: "只接受大小合规的 UTF-8 .md 或 .txt 文件名，不接受路径"}
	}
	if actor.Subject == "" || actor.Source == "" || (actor.Method != core.AuthMethodBearer && actor.Method != core.AuthMethodLocalTrust) {
		return "", errors.New("上传缺少认证身份")
	}
	sum := sha256.Sum256([]byte(content))
	path := "uploads/" + name
	detail, err := json.Marshal(map[string]any{"source_path": path, "content_sha256": hex.EncodeToString(sum[:]), "size_bytes": len(content)})
	if err != nil {
		return "", err
	}
	now := s.now()
	opID := core.NewOperationID(now)
	if err := s.operations.BeginOperation(ctx, core.OperationRecord{
		OperationID: opID, Action: core.ActionKnowledgeUpload, TargetKind: core.TargetKindKnowledgeUpload, TargetID: path,
		ActorSubject: actor.Subject, AuthMethod: actor.Method, Source: actor.Source,
		Status: core.OperationPending, DetailJSON: detail, CreatedAt: now,
	}); err != nil {
		return "", fmt.Errorf("预写上传审计失败: %w", err)
	}
	err = s.writeFile(ctx, name, []byte(content))
	status, code := core.OperationSuccess, "OK"
	if err != nil {
		status, code = core.OperationFailed, "UPLOAD_FAILED"
	}
	if finishErr := s.operations.FinishOperation(context.WithoutCancel(ctx), opID, status, code, "", detail, s.now()); finishErr != nil {
		s.logger.Error("上传操作终态写入失败", "operation_id", opID, "error", finishErr)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func (s *knowledgeUploadAdmin) writeFile(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return fmt.Errorf("打开工作区: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("uploads", 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat("uploads")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("上传目录不是普通目录")
	}
	uploads, err := root.OpenRoot("uploads")
	if err != nil {
		return err
	}
	defer func() { _ = uploads.Close() }()
	opened, err := uploads.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("上传目录发生变化，请重试")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".upload-" + hex.EncodeToString(nonce[:])
	f, err := uploads.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = uploads.Remove(temp) }()
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link publishes atomically and refuses an existing destination on both
	// platforms; Rename would replace it on Windows.
	if err := uploads.Link(temp, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return core.ErrConflict
		}
		return err
	}
	return nil
}
