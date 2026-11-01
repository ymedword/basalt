package app

// 本文件只放**不依赖数据库**的测试助手：它们被单元用例与集成用例共用，所以不能挂在
// 任何带 `//go:build integration` 的文件里——否则单元用例会被迫跟着打标签，
// "不依赖 PostgreSQL 的测试继续留作单元测试"的口径就守不住。
// Database-free test helpers shared by unit and integration cases; keeping them here
// lets the unit cases stay untagged.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// baselineClock 是基线用例的固定时钟：操作标识、版本时间与状态文件都落在同一读数上，
// 读数可复算。The fixed clock keeps every recorded timestamp reproducible.
func baselineClock() time.Time {
	return time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
}

// newCapturedLogger 返回写进缓冲区的 logger：WARN 是"待发布 / 已撤回 / 跳过"的唯一
// 可判痕迹，必须能在用例里逐字断言。
// newCapturedLogger captures log output so WARN-only outcomes stay assertable.
func newCapturedLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// sha256Hex 在用例侧独立计算摘要——期望值不调用被测实现。
// sha256Hex computes the digest independently of the implementation under test.
func sha256Hex(t *testing.T, raw []byte) string {
	t.Helper()
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// detailMap 把从库里读回的 `detail_json` 解成通用映射：该列在 PostgreSQL 下是 jsonb，
// 读回时空白被规范化（`{"kind": "host"}`），SQLite 时代逐字节比对的紧凑写法不再成立。
// 判据因此落在**字段值**上，与序列化格式无关。
// detailMap decodes a stored detail_json document: jsonb normalises whitespace on the
// way back, so assertions must target field values rather than byte formatting.
func detailMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), "detail_json 必须是 JSON 对象")
	return out
}

// writeWorkspaceFile 在临时工作区里落一份文件（自动创建父目录）。
func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}
