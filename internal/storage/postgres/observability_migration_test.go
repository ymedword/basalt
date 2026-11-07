package postgres

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservability_MigrationForwardOnly(t *testing.T) {
	historical := map[string]string{
		"0001_postgres_v1.sql":                      "e6f8ff0b2f2b10a67151c09a394e24610fb6ec03ebb3c44643d66da47beb14f7",
		"0002_agent_binding_knowledge_kind.sql":     "b40dfdd1761e05af619258ec747c3a41f9c95408cc3f6e6dbbc1a0336bbfc008",
		"0003_knowledge_source_text_repair.sql":     "c8699114dfe934bc3feed4cd70e0dfdd5443921c164b44f298f40e5f43b6c373",
		"0004_knowledge_version_revision.sql":       "51dec80f9d163e3226e01806430f402dd76057f49c8aa926407147f5ec1241ca",
		"0005_knowledge_collection_description.sql": "54b3a2c420933f9fa33e4179e6f1aadb7c857f0b5791ab78279cdf37a7ed761d",
	}
	for name, want := range historical {
		data, err := os.ReadFile("migrations/" + name)
		require.NoError(t, err)
		require.Equal(t, want, fmt.Sprintf("%x", sha256.Sum256(data)), "published migration %s changed", name)
	}
	contract, err := os.ReadFile("../../../docs/contracts/schema.pg.sql")
	require.NoError(t, err)
	first, err := os.ReadFile("migrations/0001_postgres_v1.sql")
	require.NoError(t, err)
	require.Equal(t, first, contract, "v1 contract must stay frozen")
	require.GreaterOrEqual(t, SupportedSchemaVersion, 11,
		"US-22 adds v11 (later modules may advance it); published v1 and observability migrations stay immutable")
	flowContract, err := os.ReadFile("../../../docs/contracts/flow.pg.sql")
	require.NoError(t, err)
	flowMigration, err := os.ReadFile("migrations/0008_flow.sql")
	require.NoError(t, err)
	require.Equal(t, flowMigration, flowContract, "Flow supplemental contract must match new migration exactly")
}
