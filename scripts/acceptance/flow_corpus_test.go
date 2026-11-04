package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func flowFrozenBytes(name string, raw []byte, digest string) error {
	canonical := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("flow corpus changed: %s", name)
	}
	return nil
}

// 原十九组与追加六组判据均须指向本 US 的实际执行项，不能用名称假装覆盖。
// Every task and frozen behavior group must resolve to an executable exact-stage check.
func TestFlow_StageMappingCoversFrozenTasksAndBehaviorGroups(t *testing.T) {
	root := stageTestRoot(t)
	base := readStageForTest(t, root, "harness/stages/us19-base.json")
	process := readStageForTest(t, root, "harness/stages/us19-process.json")
	experience := readStageForTest(t, root, "harness/stages/us19-experience.json")
	declared := map[string]bool{}
	for _, s := range []stage{base, process, experience} {
		checks := stageChecksByID(s)
		for _, c := range s.Checks {
			declared[c.ID] = true
			for _, name := range c.RequiredTests {
				declared[name] = true
			}
		}
		for _, task := range s.Tasks {
			require.NotEmpty(t, task.Checks)
			for _, id := range task.Checks {
				require.Contains(t, checks, id, task.ID)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "specs/019-flow-collaboration/tasks.md"))
	require.NoError(t, err)
	ids := regexp.MustCompile(`(?m)^- \[[ x]\] (T[0-9]{3}) `).FindAllStringSubmatch(string(raw), -1)
	require.Len(t, ids, 37)
	for _, id := range ids {
		found := false
		for _, task := range append(base.Tasks, experience.Tasks...) {
			found = found || task.ID == id[1]
		}
		require.True(t, found, "unmapped task %s", id[1])
	}
	var mapping struct {
		Version int                 `json:"version"`
		Note    string              `json:"note"`
		Groups  map[string][]string `json:"groups"`
	}
	raw, err = os.ReadFile(filepath.Join(root, "specs/019-flow-collaboration/acceptance-map.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &mapping))
	require.Equal(t, 1, mapping.Version)
	require.Len(t, mapping.Groups, 25)
	for i := 1; i <= 25; i++ {
		group := fmt.Sprintf("F%02d", i)
		require.NotEmpty(t, mapping.Groups[group])
		for _, item := range mapping.Groups[group] {
			require.True(t, declared[item], "%s points to undeclared execution %s", group, item)
		}
	}
}

// 固定材料不替代真实12任务执行；修改评分器／资料必须显式重新评审。
// Frozen material is not evidence that the twelve production workloads ran.
func TestFlow_EvaluationCorpusIsFrozen(t *testing.T) {
	root := stageTestRoot(t)
	for name, digest := range map[string]string{
		"testdata/flow/definitions.json":               "a102a973df3a6ec3626fb29bbb78e11a1070b40813a02bbf6df479abda3b519e",
		"testdata/flow/workloads.json":                 "c36a057f625d69b8190696f47e9bb1479942e6a63d0ed1777de996e10ce7cba0",
		"testdata/flow/corpus/policy-v1.json":          "a49de244d26968af3f2c869e830c09796529609b3105aadb21801e34e78c7af8",
		"testdata/flow/corpus/policy-conflict-v1.json": "efb50329194a6676d9764555f0048167e8113c42b1f8d69e0c224ba7ecbfb2d7",
		"evals/flow/baseline.json":                     "001e342de1c55f90ad578e98c37077af0c00806b01be02af10dbfd5ab227a480",
		"evals/flow/model-scripts.json":                "dc59f83bdda99a5d09b2157e8ae18a8e8178d7aca9ff8d7ea0624be96bd29922",
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		require.NoError(t, err)
		require.NoError(t, flowFrozenBytes(name, raw, digest))
		require.ErrorContains(t, flowFrozenBytes(name, append(bytes.Clone(raw), ' '), digest), "flow corpus changed")
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "flow", "workloads.json"))
	require.NoError(t, err)
	var catalog struct {
		Version string                                    `json:"corpus_version"`
		Cases   []struct{ ID, Scenario, Expected string } `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &catalog))
	require.Equal(t, "us19-local-control-v1", catalog.Version)
	require.Len(t, catalog.Cases, 12)
	seen := map[string]bool{}
	for i, item := range catalog.Cases {
		require.Equal(t, fmt.Sprintf("F19-%02d", i+1), item.ID)
		require.False(t, seen[item.Scenario])
		seen[item.Scenario] = true
		require.Contains(t, []string{"succeeded", "blocked", "stopped", "failed", "cancelled"}, item.Expected)
	}
}
