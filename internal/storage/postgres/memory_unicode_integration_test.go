//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func TestMemoryStore_UnicodeRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "unicode-test"}
	const content = "项目代号：蓝岩-2048；交接时间 09:30；表情 🪨"
	require.NoError(t, store.Append(ctx, scope, core.SectionArchive, content))
	var stored string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT content FROM memory_entries WHERE agent_name = $1`, scope.Agent).Scan(&stored))
	require.Equal(t, content, stored)
	text, err := store.Load(ctx, scope)
	require.NoError(t, err)
	require.Contains(t, text, content)
}
