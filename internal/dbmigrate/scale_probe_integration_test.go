//go:build integration

package dbmigrate

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestMigration_SmallScaleProbe uses isolated synthetic databases. WAL is the
// cluster-wide inserted-byte delta, not an operation-specific measurement or
// peak; concurrent writers may increase it. No safe upper bound is inferred.
func TestMigration_SmallScaleProbe(t *testing.T) {
	for _, count := range []int{100, 1000, 3000} {
		t.Run(fmt.Sprintf("rows_%d", count), func(t *testing.T) {
			source := migrationFixture(t)
			_, err := source.Exec(`INSERT INTO sessions (session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
				VALUES ('scale-session', 'demo', 'cli', 'scale', 'active', ?, 1000, 1000)`, count)
			require.NoError(t, err)
			tx, err := source.Begin()
			require.NoError(t, err)
			stmt, err := tx.Prepare(`INSERT INTO messages (id, session_id, seq, role, content, created_at) VALUES (?, 'scale-session', ?, 'user', ?, 1000)`)
			require.NoError(t, err)
			for i := 1; i <= count; i++ {
				_, err = stmt.Exec(i, i, fmt.Sprintf("synthetic migration row %d", i))
				require.NoError(t, err)
			}
			require.NoError(t, stmt.Close())
			require.NoError(t, tx.Commit())
			exportStart := time.Now()
			dir, manifest := exportFixture(t, source)
			exportDuration := time.Since(exportStart)
			var dumpBytes int64
			require.NoError(t, filepath.WalkDir(dir, func(_ string, item fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if item.IsDir() {
					return nil
				}
				info, err := item.Info()
				if err != nil {
					return err
				}
				dumpBytes += info.Size()
				return nil
			}))
			target := pgtest.NewTestDB(t)
			const schemaBytesSQL = `SELECT COALESCE(SUM(pg_total_relation_size(c.oid)), 0)
				FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'm')`
			var beforeBytes, afterBytes int64
			require.NoError(t, target.QueryRowContext(importCtx, schemaBytesSQL).Scan(&beforeBytes))
			var startLSN string
			walErr := target.QueryRowContext(importCtx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&startLSN)
			start := time.Now()
			report, err := NewImporter(target, dir, manifest).Import(importCtx)
			elapsed := time.Since(start)
			require.NoError(t, err)
			require.False(t, report.Failed())
			require.Equal(t, int64(count), report.Rows["messages"])
			var actual int
			require.NoError(t, target.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&actual))
			require.Equal(t, count, actual)
			require.NoError(t, target.QueryRowContext(importCtx, schemaBytesSQL).Scan(&afterBytes))
			var walDelta float64
			if walErr == nil {
				walErr = target.QueryRowContext(importCtx, `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)`, startLSN).Scan(&walDelta)
			}
			walText := fmt.Sprintf("%.0f", walDelta)
			if walErr != nil {
				walText = "unavailable"
			}
			t.Logf("synthetic_messages=%d dump_bytes=%d export=%s import=%s pg_table_total_bytes_before=%d after=%d cluster_wal_inserted_delta_bytes=%s (not peak; includes concurrent activity)", count, dumpBytes, exportDuration, elapsed, beforeBytes, afterBytes, walText)
		})
	}
}
