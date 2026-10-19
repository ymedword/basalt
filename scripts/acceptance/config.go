// Command acceptance 提供确定性的开发验收，不启动编码 Agent。
// Command acceptance runs deterministic development checks without coding agents.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type command struct {
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type check struct {
	ID string `json:"id"`
	command
	EvalReport    string   `json:"eval_report,omitempty"`
	Required      []string `json:"required_cases,omitempty"`
	AllowedSkips  []string `json:"allowed_skips,omitempty"`
	RequiredTests []string `json:"required_tests,omitempty"`
	Level         string   `json:"level,omitempty"`
}

// checkLevel 把未声明的旧 check 归一为 source；显式层级只在 stage 中声明。
// checkLevel normalizes legacy checks to source; explicit levels live in stages.
func checkLevel(c check) string {
	if c.Level == "" {
		return "source"
	}
	return c.Level
}

type configuration struct {
	Version  int                 `json:"version"`
	Profiles map[string][]string `json:"profiles"`
	Checks   []check             `json:"checks"`
}

type task struct {
	ID     string   `json:"id"`
	Checks []string `json:"checks"`
}

type stage struct {
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	Approval         string            `json:"approval"`
	Inputs           []string          `json:"inputs"`
	Protect          []string          `json:"protect"`
	Checks           []check           `json:"checks"`
	Tasks            []task            `json:"tasks"`
	SupersededChecks map[string]string `json:"superseded_checks,omitempty"`
	// path 记录 stage 文件自身的仓库相对路径，供记录写入 stage_sha256。
	// path keeps the stage file's repo-relative location for record criteria.
	path string
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func readJSON(path string, dst any) error {
	raw, err := readLocal(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s: trailing JSON data", path)
	}
	return nil
}

func readLocal(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(filepath.Base(path))
}

func relative(path string) bool {
	p := filepath.Clean(filepath.FromSlash(path))
	return path != "" && filepath.IsLocal(p) && p != "." && !strings.Contains(path, ":") && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, `\`)
}

func validateCommand(c command) error {
	if len(c.Args) == 0 || c.Args[0] == "" || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 7200 {
		return errors.New("command requires argv and timeout_seconds in 1..7200")
	}
	return nil
}

// isGoTest 识别 `go test` 与 `go -C <dir> test`，避免换写法绕过具名测试要求。
// isGoTest recognizes `go test` and `go -C <dir> test`, so rewording cannot bypass
// the named-test requirement.
func isGoTest(args []string) bool {
	if len(args) == 0 || args[0] != "go" {
		return false
	}
	rest := args[1:]
	for len(rest) >= 2 && rest[0] == "-C" {
		rest = rest[2:]
	}
	return len(rest) > 0 && rest[0] == "test"
}

func validateChecks(checks []check) error {
	seen := map[string]bool{}
	for _, c := range checks {
		if !identifier.MatchString(c.ID) || seen[c.ID] {
			return fmt.Errorf("invalid/duplicate check ID: %s", c.ID)
		}
		seen[c.ID] = true
		if err := validateCommand(c.command); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
		if c.EvalReport != "" && (len(c.Required) == 0 || !relative(c.EvalReport)) {
			return fmt.Errorf("%s: eval needs required_cases and relative eval_report", c.ID)
		}
		for _, id := range c.Required {
			if slices.Contains(c.AllowedSkips, id) {
				return fmt.Errorf("%s: required case cannot be optional: %s", c.ID, id)
			}
		}
		if len(c.RequiredTests) > 0 && (!slices.Contains(c.Args, "-json") || !slices.Contains(c.Args, "-count=1")) {
			return fmt.Errorf("%s: required_tests needs go test -json -count=1", c.ID)
		}
		switch checkLevel(c) {
		case "source", "process", "external":
		default:
			return fmt.Errorf("%s: unknown level %q", c.ID, c.Level)
		}
	}
	return nil
}

func loadConfiguration(root, path string) (configuration, error) {
	var cfg configuration
	if err := readJSON(filepath.Join(root, path), &cfg); err != nil {
		return cfg, err
	}
	if cfg.Version != 1 || len(cfg.Profiles["full"]) == 0 || len(cfg.Profiles["task"]) == 0 {
		return cfg, errors.New("configuration requires version=1 and full/task profiles")
	}
	if err := validateChecks(cfg.Checks); err != nil {
		return cfg, err
	}
	for name, ids := range cfg.Profiles {
		if _, err := selectChecks(cfg.Checks, ids); err != nil || len(ids) == 0 {
			return cfg, fmt.Errorf("invalid profile %s: %v", name, err)
		}
	}
	return cfg, nil
}

func loadStage(root, path string, cfg configuration) (stage, error) {
	var s stage
	if !relative(path) {
		return s, errors.New("stage must be inside the repository")
	}
	if err := readJSON(filepath.Join(root, path), &s); err != nil {
		return s, err
	}
	s.path = path
	if s.Version != 1 || !identifier.MatchString(s.ID) || strings.TrimSpace(s.Approval) == "" || len(s.Inputs) == 0 || len(s.Tasks) == 0 {
		return s, errors.New("stage needs version=1, ID, approval reference, inputs and tasks")
	}
	if strings.HasPrefix(strings.TrimSpace(s.Approval), "REPLACE") {
		return s, errors.New("approval still holds the template placeholder; cite the approved decision")
	}
	for _, p := range append(slices.Clone(s.Inputs), s.Protect...) {
		clean := filepath.ToSlash(filepath.Clean(p))
		if !relative(p) || clean == ".loop" || strings.HasPrefix(clean, ".loop/") {
			return s, fmt.Errorf("invalid input/protected path: %s", p)
		}
		if _, err := os.Lstat(filepath.Join(root, p)); err != nil {
			return s, fmt.Errorf("input/protected path %s: %w", p, err)
		}
	}
	all := append(slices.Clone(cfg.Checks), s.Checks...)
	for _, c := range s.Checks {
		if isGoTest(c.Args) && len(c.RequiredTests) == 0 {
			return s, fmt.Errorf("%s: stage Go test check requires named required_tests", c.ID)
		}
	}
	// superseded_checks 只做显式替代映射，目标必须真实存在且不得自指。
	// superseded_checks is an explicit replacement map; targets must exist and differ.
	ids := map[string]bool{}
	for _, c := range all {
		ids[c.ID] = true
	}
	for old, replacement := range s.SupersededChecks {
		if !identifier.MatchString(old) || !identifier.MatchString(replacement) || old == replacement || !ids[replacement] {
			return s, fmt.Errorf("invalid superseded_checks entry: %s -> %s", old, replacement)
		}
	}
	if err := validateChecks(all); err != nil {
		return s, err
	}
	seen := map[string]bool{}
	for _, t := range s.Tasks {
		if !identifier.MatchString(t.ID) || seen[t.ID] || len(t.Checks) == 0 {
			return s, errors.New("each task needs a unique ID and task-specific checks")
		}
		seen[t.ID] = true
		if _, err := selectChecks(all, t.Checks); err != nil {
			return s, err
		}
		for _, id := range t.Checks {
			if slices.Contains(cfg.Profiles["full"], id) {
				return s, fmt.Errorf("task %s: %s is a project gate, add a specific assertion", t.ID, id)
			}
		}
	}
	return s, nil
}

func selectChecks(all []check, ids []string) ([]check, error) {
	var selected []check
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		idx := slices.IndexFunc(all, func(c check) bool { return c.ID == id })
		if idx < 0 {
			return nil, fmt.Errorf("unknown check: %s", id)
		}
		seen[id] = true
		selected = append(selected, all[idx])
	}
	return selected, nil
}
