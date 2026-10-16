package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// snapshotRow 是 config_snapshots 一行的判卷形态（列全部显式）。
// snapshotRow is the assertion shape of one config_snapshots row.
type snapshotRow struct {
	ID           int64
	AgentName    string
	Hash         string
	Content      string
	ManifestJSON string
	CreatedAt    int64
}

// readSnapshotRows 按 (agent_name, hash) 读回快照行（in-package 判卷走 db.read——
// 生产写入路径仍唯一经仓储）。
// readSnapshotRows reads the snapshot rows back through the read pool.
func readSnapshotRows(t *testing.T, db *DB, agentName, hash string) []snapshotRow {
	t.Helper()
	rows, err := db.read.QueryContext(context.Background(),
		`SELECT id, agent_name, hash, content, manifest_json, created_at
		   FROM config_snapshots WHERE agent_name = ? AND hash = ? ORDER BY id`, agentName, hash)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var out []snapshotRow
	for rows.Next() {
		var row snapshotRow
		require.NoError(t, rows.Scan(&row.ID, &row.AgentName, &row.Hash, &row.Content,
			&row.ManifestJSON, &row.CreatedAt))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// sampleSnapshot 造一份内容寻址的快照夹具：同一份 `manifest` 序列化 → 同一 `hash`。
// sampleSnapshot builds a content-addressed snapshot fixture.
func sampleSnapshot(content string) *core.ConfigSnapshot {
	manifest := []core.ManifestEntry{
		{Path: "AGENT.md", SHA256: strings.Repeat("a", 64)},
		{Path: "workspace/SOUL.md", SHA256: strings.Repeat("b", 64)},
	}
	return &core.ConfigSnapshot{
		AgentName:  "demo",
		Hash:       "3f9a1c07b2d84e55",
		Content:    content,
		Manifest:   manifest,
		SectionOne: "段一：你是 demo。",
	}
}

// TestSnapshotStore_SaveIsInsertIfAbsent —— L1：同一份快照连存两次只有一行（幂等）。
// TestSnapshotStore_SaveIsInsertIfAbsent: saving the same snapshot twice leaves one row.
func TestSnapshotStore_SaveIsInsertIfAbsent(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSnapshotStore(db)

	snap := sampleSnapshot("---\nname: demo\n---\n正文\n")
	before := time.Now().Add(-time.Minute).UnixMilli()
	require.NoError(t, store.SaveSnapshot(ctx, snap))
	after := time.Now().Add(time.Minute).UnixMilli()
	require.NoError(t, store.SaveSnapshot(ctx, snap), "重复落库必须是 no-op，不是错误")

	rows := readSnapshotRows(t, db, snap.AgentName, snap.Hash)
	require.Len(t, rows, 1, "内容寻址：同一 (agent_name, hash) 只有一行")
	require.Equal(t, snap.Content, rows[0].Content)
	require.Greater(t, rows[0].CreatedAt, before)
	require.Less(t, rows[0].CreatedAt, after, "created_at 存 Unix 毫秒（迁移纪律 4）——不是秒、不是纳秒")
}

// TestSnapshotStore_ConflictDoesNotOverwriteExisting —— L2：唯一键冲突**不覆盖**既有行
// （同一哈希配不同正文是异常形态；落库侧仍不得改写已写下的历史版本）。
// TestSnapshotStore_ConflictDoesNotOverwriteExisting pins that a unique-key conflict never
// rewrites the row already stored.
func TestSnapshotStore_ConflictDoesNotOverwriteExisting(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSnapshotStore(db)

	original := sampleSnapshot("第一次写下的原文\n")
	require.NoError(t, store.SaveSnapshot(ctx, original))

	conflicting := sampleSnapshot("后来者（同哈希、不同正文）\n")
	conflicting.Manifest = nil // 连清单都不一样：冲突分支不得把任何一列改成新值
	require.NoError(t, store.SaveSnapshot(ctx, conflicting))

	rows := readSnapshotRows(t, db, original.AgentName, original.Hash)
	require.Len(t, rows, 1)
	require.Equal(t, original.Content, rows[0].Content, "既有行的 content 不被覆盖")

	var manifest []core.ManifestEntry
	require.NoError(t, json.Unmarshal([]byte(rows[0].ManifestJSON), &manifest))
	require.Len(t, manifest, 2, "既有行的 manifest_json 也不被覆盖")
}

// TestSnapshotStore_ContentStoresFullAgentMD —— L3：`content` 存 AGENT.md **全文**
// （含 frontmatter 的原文，不是正文片段、不是哈希）。
// TestSnapshotStore_ContentStoresFullAgentMD: the content column keeps the raw file verbatim.
func TestSnapshotStore_ContentStoresFullAgentMD(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSnapshotStore(db)

	raw := "---\nname: demo\ntools:\n  - read_file\n---\n你是演示助手。\n\n第二段（多字节：中文 + emoji 🧪）\n"
	snap := sampleSnapshot(raw)
	require.NoError(t, store.SaveSnapshot(ctx, snap))

	rows := readSnapshotRows(t, db, snap.AgentName, snap.Hash)
	require.Len(t, rows, 1)
	require.Equal(t, raw, rows[0].Content, "content 列 = 原文逐字")
	require.Contains(t, rows[0].Content, "---\nname: demo", "frontmatter 也算全文的一部分")
}

// TestSnapshotStore_ManifestJSONRoundTrip —— L4：`manifest_json` 往返一致，且**键名是契约
// 声明的 `path` / `sha256`**（列内容就是哈希的派生来源，落库侧重序列化会让两列脱钩）。
// TestSnapshotStore_ManifestJSONRoundTrip: the manifest round-trips, and its key names match
// the contract shape `[{path, sha256}]`.
func TestSnapshotStore_ManifestJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSnapshotStore(db)

	snap := sampleSnapshot("---\nname: demo\n---\n正文\n")
	require.NoError(t, store.SaveSnapshot(ctx, snap))

	rows := readSnapshotRows(t, db, snap.AgentName, snap.Hash)
	require.Len(t, rows, 1)

	var back []core.ManifestEntry
	require.NoError(t, json.Unmarshal([]byte(rows[0].ManifestJSON), &back))
	require.Equal(t, snap.Manifest, back, "清单往返逐条一致（顺序也保真）")

	require.Equal(t, `[{"path":"AGENT.md","sha256":"`+strings.Repeat("a", 64)+`"},`+
		`{"path":"workspace/SOUL.md","sha256":"`+strings.Repeat("b", 64)+`"}]`,
		rows[0].ManifestJSON, "列内容 = `[{path, sha256}]` 的规整序列化（契约列注释）")
}

// TestSnapshotStore_WriteFailureIsReturned —— L5：写失败**如实上抛**（不得吞掉、不得
// 造一个"已保存"的成功返回——调用方要靠这个错误拒绝启动运行）。
// TestSnapshotStore_WriteFailureIsReturned: a write failure surfaces as an error.
func TestSnapshotStore_WriteFailureIsReturned(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSnapshotStore(db)

	snap := sampleSnapshot("---\nname: demo\n---\n正文\n")
	require.NoError(t, store.SaveSnapshot(ctx, snap))

	require.NoError(t, db.Close()) // 写池关掉：后续写入必失败
	err := store.SaveSnapshot(ctx, sampleSnapshot("另一份\n"))
	require.Error(t, err, "写失败必须返回错误")
	require.Contains(t, err.Error(), "保存配置快照", "错误必须点名失败的操作")
}
