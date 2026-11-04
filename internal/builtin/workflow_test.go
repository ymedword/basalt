package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/examples"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

func TestFlow_DefaultWorkflowInstallPreservesOwnedContent(t *testing.T) {
	t.Run("all-preexisting-without-control", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range WorkflowAgents() {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "agents", name), 0o750))
		}
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "samples", "bid-readiness"), 0o750))
		results, err := InstallWorkflow(dir, nil)
		require.NoError(t, err)
		for _, result := range results {
			require.Equal(t, StateKept, result.State)
		}
		prepared, err := WorkflowPrepared(dir)
		require.NoError(t, err)
		require.True(t, prepared)
	})
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "agents", "bid-requirements"), 0o750))
	custom := filepath.Join(dir, "agents", "bid-requirements", "AGENT.md")
	require.NoError(t, os.WriteFile(custom, []byte("user-owned"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "control", "agents"), 0o750))
	// 损坏状态同样不能被安装器覆盖；数据库里已有身份也不复活。
	// A corrupt state or existing database identity must never be recreated.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "control", "agents", "bid-reviewer.json"), []byte("archived or corrupt"), 0o600))
	results, err := InstallWorkflow(dir, func(name string) (bool, error) { return name == "bid-delivery", nil })
	require.NoError(t, err)
	require.Len(t, results, 4)
	require.Equal(t, StateKept, results[0].State)
	require.Equal(t, StateInstalled, results[1].State)
	require.Equal(t, StateKept, results[2].State)
	require.Equal(t, StateKept, results[3].State)
	data, err := os.ReadFile(custom)
	require.NoError(t, err)
	require.Equal(t, "user-owned", string(data))
	require.NoDirExists(t, filepath.Join(dir, "agents", "bid-reviewer"))
	require.NoDirExists(t, filepath.Join(dir, "agents", "bid-delivery"))
	material := filepath.Join(dir, "samples", "bid-readiness", "workflow.json")
	require.NoError(t, os.WriteFile(material, []byte("custom-template"), 0o600))
	_, err = InstallWorkflow(dir, nil)
	require.NoError(t, err)
	data, err = os.ReadFile(material)
	require.NoError(t, err)
	require.Equal(t, "custom-template", string(data))
	_, err = InstallWorkflow(dir, func(string) (bool, error) { return false, fs.ErrPermission })
	require.ErrorIs(t, err, fs.ErrPermission)
}

type workflowFailFS struct{ fs.FS }

func (f workflowFailFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "second.md") {
		return nil, fs.ErrPermission
	}
	return f.FS.Open(name)
}

func TestFlow_DefaultWorkflowCopyFailureHasNoPartialTarget(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	source := fstest.MapFS{"unit/AGENT.md": {Data: []byte("first")}, "unit/second.md": {Data: []byte("second")}}
	_, err = installWorkflowTree(root, workflowFailFS{source}, "unit", "agents/test")
	require.ErrorIs(t, err, fs.ErrPermission)
	require.NoDirExists(t, filepath.Join(dir, "agents", "test"))
	state, err := installWorkflowTree(root, source, "unit", "agents/test")
	require.NoError(t, err)
	require.Equal(t, StateInstalled, state)
	require.FileExists(t, filepath.Join(dir, "agents", "test", "second.md"))
	staged, err := filepath.Glob(filepath.Join(dir, "control", "workflow-stage-*"))
	require.NoError(t, err)
	require.Empty(t, staged)
}

func TestFlow_DefaultWorkflowAssetsPortableAndBounded(t *testing.T) {
	manifestRaw, err := examples.Assets.ReadFile(workflowAssets + "/materials/source-manifest.json")
	require.NoError(t, err)
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Bytes  int    `json:"bytes"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(manifestRaw, &manifest))
	require.Len(t, manifest.Files, 11)
	for _, file := range manifest.Files {
		data, readErr := examples.Assets.ReadFile(path.Join(workflowAssets, "materials", file.Path))
		require.NoError(t, readErr)
		require.Len(t, data, file.Bytes, file.Path)
		require.Equal(t, file.SHA256, fmt.Sprintf("%x", sha256.Sum256(data)), file.Path)
	}
	raw, err := examples.Assets.ReadFile(workflowAssets + "/materials/workflow.json")
	require.NoError(t, err)
	var def flow.Definition
	require.NoError(t, json.Unmarshal(raw, &def))
	require.Len(t, def.Nodes, 17)
	roles := map[string]bool{}
	for i := range def.Nodes {
		ref := def.Nodes[i].Agent
		require.NotNil(t, ref)
		require.Empty(t, ref.UID)
		require.Empty(t, ref.ConfigHash)
		roles[ref.Name] = true
		ref.UID, ref.Version = "ag_current_"+ref.Name, 1
		ref.ConfigHash, ref.ManifestHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
	}
	require.Len(t, roles, 4)
	validator := flow.ContractValidator{Schemas: tool.NewSchemaCompiler()}
	require.NoError(t, validator.ValidateStructure(context.Background(), def))
	require.Equal(t, int64(1800), def.Limits.MaxDurationSeconds)
	dir := t.TempDir()
	_, err = InstallWorkflow(dir, nil)
	require.NoError(t, err)
	for _, name := range WorkflowAgents() {
		data, err := os.ReadFile(filepath.Join(dir, "agents", name, "AGENT.md"))
		require.NoError(t, err)
		require.Contains(t, string(data), "model: \"\"")
		require.Contains(t, string(data), "knowledge: []")
		require.NotContains(t, string(data), "schedule:")
		require.NotContains(t, string(data), "resource_bindings:")
	}
	err = fs.WalkDir(examples.Assets, workflowAssets, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".py") || strings.Contains(p, "bid-general") {
			return errors.New("experimental assets bundled")
		}
		return nil
	})
	require.NoError(t, err)
}
