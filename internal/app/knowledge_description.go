package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

type knowledgeDescriptionAdmin struct {
	store      *postgres.KnowledgeCatalogStore
	operations core.OperationStore
	now        func() time.Time
	logger     *slog.Logger
}

// UpdateDescription records the operation before changing the collection. The
// audit contains only a digest, never the user's description itself.
func (s *knowledgeDescriptionAdmin) UpdateDescription(ctx context.Context, actor core.Actor, id, description string, expected int64) (int64, error) {
	id = strings.TrimSpace(id)
	if id == "" || expected < 0 || !utf8.ValidString(description) || len([]rune(description)) > 2000 {
		return 0, &core.InvalidInputError{Tool: "knowledge.description", Reason: "集合标识、简介或版本无效（简介最多 2000 字符）"}
	}
	// 审计身份必须在**预写之前**就位：`ActorSubject` 为空的行写进 `management_operations`
	// 就是一条无法追溯的变更记录，事后既找不到人、也判不出该不该恢复（宪法第 5 条
	// fail-closed）。判定口径与上传 / 重建两处一致，不另立一套。
	if actor.Subject == "" || actor.Source == "" ||
		(actor.Method != core.AuthMethodBearer && actor.Method != core.AuthMethodLocalTrust) {
		return 0, fmt.Errorf("集合简介更新缺少经过认证的审计身份")
	}
	if s == nil || s.store == nil || s.operations == nil || s.now == nil || s.logger == nil {
		return 0, fmt.Errorf("集合简介管理未装配")
	}
	sum := sha256.Sum256([]byte(description))
	detail, err := json.Marshal(struct {
		DescriptionSHA256 string `json:"description_sha256"`
		ExpectedRevision  int64  `json:"expected_revision"`
	}{hex.EncodeToString(sum[:]), expected})
	if err != nil {
		return 0, err
	}
	now := s.now()
	opID := core.NewOperationID(now)
	if err := s.operations.BeginOperation(ctx, core.OperationRecord{
		OperationID: opID, Action: core.ActionKnowledgeDescription, TargetKind: core.TargetKindKnowledgeCollection,
		TargetID: id, ActorSubject: actor.Subject, AuthMethod: actor.Method, Source: actor.Source,
		Status: core.OperationPending, DetailJSON: detail, CreatedAt: now,
	}); err != nil {
		return 0, fmt.Errorf("预写集合简介操作失败: %w", err)
	}
	revision, updateErr := s.store.UpdateDescription(ctx, id, description, expected)
	status, code := core.OperationSuccess, "OK"
	if updateErr != nil {
		status, code = core.OperationFailed, "UPDATE_FAILED"
	}
	if finishErr := s.operations.FinishOperation(context.WithoutCancel(ctx), opID, status, code, "", detail, s.now()); finishErr != nil {
		s.logger.Error("集合简介操作终态写入失败", "operation_id", opID, "error", finishErr)
	}
	return revision, updateErr
}
