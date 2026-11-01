//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

func TestKnowledgeDescription_RevisionAndRevival(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	created, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "运维资料", "")
	require.NoError(t, err)
	require.EqualValues(t, 0, created.DescriptionRevision)
	revision, err := store.UpdateDescription(ctx, created.CollectionID, "恢复流程与排障手册", 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, revision)
	_, err = store.UpdateDescription(ctx, created.CollectionID, "不应覆盖", 0)
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	rows, err := store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "恢复流程与排障手册", rows[0].Description)
	require.EqualValues(t, 1, rows[0].DescriptionRevision)
	_, err = db.pool.ExecContext(ctx, `UPDATE kb_collections SET deleted_at = 1 WHERE collection_id = $1`, created.CollectionID)
	require.NoError(t, err)
	_, err = store.UpdateDescription(ctx, created.CollectionID, "删除后不可改", 1)
	require.ErrorIs(t, err, knowledge.ErrNotFound)
	revived, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "运维资料", "")
	require.NoError(t, err)
	require.Equal(t, created.CollectionID, revived.CollectionID)
	require.Equal(t, "恢复流程与排障手册", revived.Description)
	require.EqualValues(t, 1, revived.DescriptionRevision)
}

func TestKnowledgeCatalog_ListsProjectedAgentReferences(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	created, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "手册", "")
	require.NoError(t, err)
	bindings := NewBindingStore(db)
	uid := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	require.NoError(t, bindings.Rebuild(ctx, uid, "assistant", "sha256:h1", core.ResourceBindings{}, time.Now(),
		core.KnowledgeCollectionBinding{Name: "手册", CollectionID: created.CollectionID}))
	rows, err := store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, []string{"assistant"}, rows[0].DeclaredAgents)
	require.NoError(t, bindings.Clear(ctx, uid))
	rows, err = store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Empty(t, rows[0].DeclaredAgents)
}

func TestKnowledgeDescription_UpgradeKeepsExistingCollections(t *testing.T) {
	ctx := context.Background()
	db := migrationConn(t)
	require.NoError(t, ensureSchema(ctx, db, 4))
	_, err := db.ExecContext(ctx, `INSERT INTO kb_collections (collection_id, name, project, created_at) VALUES ('old', 'existing', 'team', 1)`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, db, SupportedSchemaVersion))
	var name, project, description string
	var revision int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT name, project, description, description_revision FROM kb_collections WHERE collection_id = 'old'`).Scan(&name, &project, &description, &revision))
	require.Equal(t, "existing", name)
	require.Equal(t, "team", project)
	require.Empty(t, description)
	require.Zero(t, revision)
}
