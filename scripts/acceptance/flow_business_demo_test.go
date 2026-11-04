package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 检查冻结资料与隔离角色，不将静态检查冒充真实模型效果。
// Validate frozen demo materials without claiming live-model quality.
func TestFlow_BusinessDemoSourceAndRoleBoundaries(t *testing.T) {
	root := filepath.Join(stageTestRoot(t), "examples", "workflows", "bid-readiness")
	var manifest struct {
		Synthetic bool `json:"not_a_real_tender"`
		Files     []struct {
			Path, SHA256 string
			Bytes        int
		} `json:"files"`
		Context []struct {
			Path, SHA256 string
			Bytes        int
		} `json:"context_files"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "source-manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.True(t, manifest.Synthetic)
	require.Len(t, manifest.Files, 11)
	seen := map[string]bool{}
	for _, item := range append(manifest.Files, manifest.Context...) {
		require.False(t, seen[item.Path], "duplicate source %s", item.Path)
		seen[item.Path] = true
		require.False(t, filepath.IsAbs(item.Path))
		require.NotContains(t, item.Path, "..")
		content, err := os.ReadFile(filepath.Join(root, item.Path))
		require.NoError(t, err)
		require.Len(t, content, item.Bytes, item.Path)
		digest := sha256.Sum256(content)
		require.Equal(t, item.SHA256, hex.EncodeToString(digest[:]), item.Path)
	}
	for _, path := range []string{"scenario.py", "demo.py", "matrix_definition.py", "matrix_report.py", "collect.py", "evaluation-protocol.json", "roles.json", "budget_gateway.py", "start-review.ps1", "serve_fixture_test.go.txt"} {
		require.True(t, seen[path], "controller context must be frozen: %s", path)
	}
	for chapter := 1; chapter <= 8; chapter++ {
		raw, err := os.ReadFile(filepath.Join(root, "sources", fmt.Sprintf("request-%02d.md", chapter)))
		require.NoError(t, err)
		require.Contains(t, string(raw), "合成演示")
		for index := (chapter-1)*6 + 1; index <= chapter*6; index++ {
			require.Equal(t, 1, strings.Count(string(raw), fmt.Sprintf("## R%02d\n", index)))
		}
	}
	var roles map[string]struct {
		Collections []string `json:"collections"`
		Memory      string   `json:"memory"`
	}
	raw, err = os.ReadFile(filepath.Join(root, "roles.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &roles))
	require.Len(t, roles, 5)
	for _, role := range []string{"bid-requirements", "bid-technical", "bid-delivery", "bid-reviewer", "bid-general"} {
		require.Contains(t, roles, role)
		require.NotEmpty(t, roles[role].Collections)
		require.Contains(t, roles[role].Memory, "合成演示经验")
		raw, err = os.ReadFile(filepath.Join(root, "agents", role, "AGENT.md"))
		require.NoError(t, err)
		require.Contains(t, string(raw), "tools:\n  - read_file\nsettings:")
		require.Contains(t, string(raw), "include_global_memory: false")
		require.Contains(t, string(raw), "max_iterations: 4")
		require.Contains(t, string(raw), "不得替用户发出投标")
		require.True(t, seen["agents/"+role+"/AGENT.md"])
	}
}

// 对照协议与总授权不能按新任务重置；三组结果必须另由真实证据判定。
// The protocol bounds shared authorization; real outcomes need separate evidence.
func TestFlow_BusinessDemoThreeArmProtocol(t *testing.T) {
	root := filepath.Join(stageTestRoot(t), "examples", "workflows", "bid-readiness")
	var protocol struct {
		Cases []struct {
			ID                     string `json:"id"`
			Requirements, Critical []string
		} `json:"paired_cases"`
		Arms    map[string]string `json:"arms"`
		Budget  map[string]int    `json:"global_authorization"`
		Scoring struct {
			Points map[string]int `json:"per_requirement"`
			Judge  string         `json:"judge"`
			Limit  string         `json:"sample_limit"`
		} `json:"scoring"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "evaluation-protocol.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &protocol))
	require.Len(t, protocol.Cases, 3)
	require.Len(t, protocol.Arms, 3)
	for _, arm := range []string{"A", "B", "C"} {
		require.NotEmpty(t, protocol.Arms[arm])
	}
	for _, group := range protocol.Cases {
		require.Len(t, group.Requirements, 8)
		require.Len(t, group.Critical, 8)
		seen := map[string]bool{}
		for _, id := range group.Requirements {
			require.False(t, seen[id])
			seen[id] = true
		}
	}
	require.Equal(t, map[string]int{"chat_calls": 90, "input_tokens": 1000000, "output_tokens": 80000, "cny_including_embeddings": 30}, protocol.Budget)
	require.Equal(t, map[string]int{"fact": 1, "source": 1, "boundary": 1}, protocol.Scoring.Points)
	require.Contains(t, protocol.Scoring.Judge, "no model self-grading")
	require.Contains(t, protocol.Scoring.Limit, "not statistical significance")
}
