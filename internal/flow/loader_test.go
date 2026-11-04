package flow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

func writeFlowFixture(t *testing.T, root string) {
	t.Helper()
	def := validDefinition()
	contract := def.Contract
	def.Contract = TaskContract{}
	data, err := json.Marshal(def)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(data, &object))
	delete(object, "contract")
	data, err = json.Marshal(object)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "report"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "report", "FLOW.yaml"), data, 0o600))
	data, err = json.Marshal(contract)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "report", "TASK.json"), data, 0o600))
}

func TestFlow_LoaderBoundariesAndHashes(t *testing.T) {
	root := t.TempDir()
	writeFlowFixture(t, root)
	loader := &Loader{Root: root, Validator: definitionValidator(), Decode: func(data []byte) (Definition, error) {
		var def Definition
		err := yaml.UnmarshalWithOptions(data, &def, yaml.Strict(), yaml.UseJSONUnmarshaler())
		return def, err
	}}
	first, err := loader.Load(context.Background(), "report")
	require.NoError(t, err)
	second, err := loader.Load(context.Background(), "report")
	require.NoError(t, err)
	require.Equal(t, first.ContentHash, second.ContentHash)
	require.Len(t, first.ContentHash, 64)
	for _, path := range []string{"../report", filepath.Join(root, "report"), ".", "a/b", `a\b`} {
		_, err := loader.Load(context.Background(), path)
		require.Error(t, err, "loader must reject escape or multi-level directory")
	}
	file := filepath.Join(root, "report", "TASK.json")
	require.NoError(t, os.WriteFile(file, []byte(strings.Repeat(" ", (1<<20)+1)), 0o600))
	_, err = loader.Load(context.Background(), "report")
	require.Error(t, err, "loader must reject oversized file")
}

func TestFlow_LoaderRejectsNonRegularContract(t *testing.T) {
	root := t.TempDir()
	writeFlowFixture(t, root)
	path := filepath.Join(root, "report", "TASK.json")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	loader := &Loader{Root: root, Validator: definitionValidator(), Decode: func(data []byte) (Definition, error) {
		var def Definition
		err := yaml.UnmarshalWithOptions(data, &def, yaml.Strict(), yaml.UseJSONUnmarshaler())
		return def, err
	}}
	_, err := loader.Load(context.Background(), "report")
	require.Error(t, err, "non-regular contract must be refused")
	require.ErrorContains(t, err, "flow file must be ordinary", "the intended file-type guard must fire")
}

func TestFlow_LoaderDoesNotMergeContractFiles(t *testing.T) {
	root := t.TempDir()
	writeFlowFixture(t, root)
	def := validDefinition()
	def.Contract.ContractVersion = 0
	data, err := json.Marshal(def)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "report", "FLOW.yaml"), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "report", "TASK.json"), []byte(`{"contract_version":1}`), 0o600))
	loader := &Loader{Root: root, Validator: definitionValidator(), Decode: func(data []byte) (Definition, error) {
		var def Definition
		err := yaml.UnmarshalWithOptions(data, &def, yaml.Strict(), yaml.UseJSONUnmarshaler())
		return def, err
	}}
	_, err = loader.Load(context.Background(), "report")
	require.Error(t, err, "contract files must not merge controls from FLOW.yaml")
}
