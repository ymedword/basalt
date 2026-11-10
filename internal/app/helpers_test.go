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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// defaultPayloadMaxBytes 是发布夹具的载荷上限：直接取 `assets.max_bundle_bytes` 的
// 配置缺省值，不硬编码数字——单测不装载配置，判据仍与生产同一个上限来源。
// The fixtures use the configured default so the limit has one source.
var defaultPayloadMaxBytes = cli.DefaultAssetsConfig().MaxBundleBytes

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

// fixtureResourceUID 造一个**形态合法**的资源 UID：判据要经过真实 `core.ParseResourceUID`，
// 不能拿随手字符串冒充资源标识。
// fixtureResourceUID derives a shape-valid resource UID for fixtures.
func fixtureResourceUID(t *testing.T, kind core.ResourceKind, id string) core.ResourceUID {
	t.Helper()
	uid := fixtureResourceUIDFor(kind, id)
	_, err := core.ParseResourceUID(string(uid))
	require.NoError(t, err)
	return uid
}

// fixtureResourceUIDFor 是 fixtureResourceUID 的无 *testing.T 版本：绑定声明要在构造
// Agent 入口时同步给出同名 UID，那里拿不到 t。
// fixtureResourceUIDFor is the t-free form so entry builders can declare bindings.
func fixtureResourceUIDFor(kind core.ResourceKind, id string) core.ResourceUID {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // 与 core 的字符集一致
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + id))
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = crockford[int(sum[i%len(sum)])%len(crockford)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix) + core.ResourceUID(payload)
}

// manifestHashes 把配置快照的清单折成「逻辑路径 → 哈希」：清单里记的是逻辑路径，
// 断言只关心"这个路径对应的字节是哪一份"。
// manifestHashes folds a snapshot manifest into logical path → sha256.
func manifestHashes(snapshot core.ConfigSnapshot) map[string]string {
	m := make(map[string]string, len(snapshot.Manifest))
	for _, entry := range snapshot.Manifest {
		m[entry.Path] = entry.SHA256
	}
	return m
}

// unlockTreeOnCleanup 在清理阶段解除只读位：已发布技能版本目录按要求是 0o444 / 0o555，
// 而 t.TempDir 的 RemoveAll 两平台都会因此失败——Windows 删不掉只读属性为只读的文件，
// Linux 的 0o555 目录不允许 unlink 其子项（CI 实测：TempDir RemoveAll permission denied）。
// unlockTreeOnCleanup clears read-only bits so temp-dir cleanup can remove them on both
// platforms (Windows read-only attributes; Linux 0o555 directories blocking unlink).
func unlockTreeOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})
}
