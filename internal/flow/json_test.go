package flow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_CanonicalInputPreservesExactSemantics(t *testing.T) {
	first, err := CanonicalJSON(context.Background(), json.RawMessage(`{"b":1.00,"a":9007199254740993,"c":1000,"z":-0.000}`))
	require.NoError(t, err)
	second, err := CanonicalJSON(context.Background(), json.RawMessage(`{"z":0,"a":9007199254740993,"b":1,"c":1e3}`))
	require.NoError(t, err)
	require.Equal(t, string(first), string(second), "same semantic input must retain the same identity")
	require.Contains(t, string(first), "9007199254740993")
	var typed struct{ C int }
	require.NoError(t, json.Unmarshal(first, &typed), "canonical integer fields must decode into their typed representation")
	require.Equal(t, 1000, typed.C)
}

func TestFlow_DefinitionHashFreezesContract(t *testing.T) {
	def := validDefinition()
	before, err := DefinitionHash(def)
	require.NoError(t, err)
	def.Contract.Goal = "different target"
	after, err := DefinitionHash(def)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "a contract change must change immutable definition identity")
}
