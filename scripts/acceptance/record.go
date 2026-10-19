package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

type authorization struct {
	Ref   string `json:"ref"`
	Scope string `json:"scope"`
}

type authorizationFlags struct {
	Ref   string
	Scope string
}

type recordOptions struct {
	Executor      string
	RunKind       string
	Authorization *authorization
}

type environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`
	CI        bool   `json:"ci"`
}

type recordSubject struct {
	Kind      string   `json:"kind"`
	Level     string   `json:"level"`
	StageID   string   `json:"stage_id,omitempty"`
	StagePath string   `json:"stage_path,omitempty"`
	Profile   string   `json:"profile,omitempty"`
	Items     []string `json:"items,omitempty"`
	CheckID   string   `json:"check_id,omitempty"`
	Test      string   `json:"test,omitempty"`
}

type recordInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type recordCriteria struct {
	AcceptanceSHA256 string        `json:"acceptance_sha256"`
	StageSHA256      string        `json:"stage_sha256,omitempty"`
	Inputs           []recordInput `json:"inputs"`
	CheckIDs         []string      `json:"check_ids,omitempty"`
}

type recordCode struct {
	BaseCommit         string  `json:"base_commit"`
	SourceFingerprint  string  `json:"source_fingerprint"`
	Dirty              bool    `json:"dirty"`
	TestedBinarySHA256 *string `json:"tested_binary_sha256"`
}

type recordCheck struct {
	ID           string   `json:"id"`
	Level        string   `json:"level"`
	Status       string   `json:"status"`
	ExitCode     int      `json:"exit_code"`
	DurationMS   int64    `json:"duration_ms"`
	PassedTests  []string `json:"passed_tests"`
	SkippedTests []string `json:"skipped_tests"`
}

type recordArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type recordEvidence struct {
	ReportPath   string           `json:"report_path"`
	ReportSHA256 string           `json:"report_sha256"`
	Logs         []recordArtifact `json:"logs"`
	Artifacts    []recordArtifact `json:"artifacts,omitempty"`
	Excerpt      string           `json:"excerpt,omitempty"`
}

type redState struct {
	Path      string           `json:"path"`
	SHA256    string           `json:"sha256"`
	AppliesTo []recordArtifact `json:"applies_to"`
}

type record struct {
	Version       int            `json:"version"`
	RecordID      string         `json:"record_id"`
	RunID         string         `json:"run_id"`
	RunKind       string         `json:"run_kind"`
	RunStatus     string         `json:"run_status,omitempty"`
	Executor      string         `json:"executor"`
	StartedAt     string         `json:"started_at"`
	FinishedAt    string         `json:"finished_at"`
	Subject       recordSubject  `json:"subject"`
	Criteria      recordCriteria `json:"criteria"`
	Code          recordCode     `json:"code"`
	Environment   environment    `json:"environment"`
	Authorization *authorization `json:"authorization,omitempty"`
	Verdict       string         `json:"verdict"`
	NABasis       string         `json:"na_basis,omitempty"`
	Checks        []recordCheck  `json:"checks,omitempty"`
	Evidence      recordEvidence `json:"evidence"`
	RedState      *redState      `json:"red_state,omitempty"`
	Notes         string         `json:"notes,omitempty"`
}

type catalogItem struct {
	Item  string
	Check string
	Test  string
	Level string
}

type statusKey struct {
	Item        string
	Fingerprint string
	OS          string
}

type statusEntry struct {
	RecordIDs []string
	Verdict   string
	Conflict  bool
}

func currentEnvironment() environment {
	return environment{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		GoVersion: runtime.Version(),
		CI:        os.Getenv("CI") != "",
	}
}

// gitState 记录基线提交与脏标记；空仓库（无 HEAD）保留空基线与脏标记。
// gitState records the base commit and dirty flag; an unborn HEAD keeps an empty base.
func gitState(root string) (string, bool, error) {
	commit := ""
	rev := exec.Command("git", "rev-parse", "--verify", "HEAD")
	rev.Dir = root
	if raw, err := rev.Output(); err == nil {
		commit = strings.TrimSpace(string(raw))
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = root
	raw, err := status.Output()
	if err != nil {
		return "", false, errors.New("git status requires a worktree")
	}
	return commit, strings.TrimSpace(string(raw)) != "", nil
}

// readUnderRoot 读取仓库根内的仓库相对路径：os.Root 从机制上拒绝 `..` 与绝对
// 路径逃逸，不依赖调用方先做字符串校验（gosec G304 的标准修复面）。
// readUnderRoot reads a repository-relative file through os.Root, which rejects
// `..` and absolute paths at the mechanism level.
func readUnderRoot(root, rel string) ([]byte, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := dir.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// fileSHA256 计算仓库相对路径的 SHA-256（经 os.Root 限定在仓库根内）。
// fileSHA256 hashes a repository-relative file scoped to the repository root.
func fileSHA256(root, rel string) (string, error) {
	raw, err := readUnderRoot(root, rel)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func maxLevel(results []result) string {
	level := "source"
	for _, r := range results {
		switch r.Level {
		case "external":
			return "external"
		case "process":
			level = "process"
		}
	}
	return level
}

func profileFromScope(scope string) string {
	profile, _, _ := strings.Cut(scope, "/")
	return profile
}

func (o *recordOptions) applyDefaults(env environment) error {
	if o.Executor == "" {
		if env.CI {
			o.Executor = "ci"
		} else {
			o.Executor = "ai"
		}
	}
	if o.RunKind == "" {
		o.RunKind = "original"
	}
	if !slices.Contains([]string{"ai", "user", "ci"}, o.Executor) {
		return fmt.Errorf("unknown executor: %s", o.Executor)
	}
	if !slices.Contains([]string{"original", "rerun", "backfill"}, o.RunKind) {
		return fmt.Errorf("unknown run kind: %s", o.RunKind)
	}
	return nil
}

// buildRecord 把同次报告绑定为运行记录；source_changed 也生成记录并如实记失败。
// buildRecord binds a report into a run record; source_changed still produces a failed record.
func buildRecord(root, runDir, scope string, rep acceptanceReport, cfg configuration, s *stage, opts recordOptions) (record, error) {
	_ = cfg
	runDir = filepath.Clean(runDir)
	if !filepath.IsAbs(runDir) {
		runDir = filepath.Join(root, runDir)
	}
	recordID := filepath.Base(runDir)
	if rep.RunID != recordID {
		return record{}, fmt.Errorf("report run_id %q does not match run directory %q", rep.RunID, recordID)
	}
	if !slices.Contains([]string{"passed", "failed", "source_changed"}, rep.Status) {
		return record{}, fmt.Errorf("report status %q cannot produce a record", rep.Status)
	}
	if err := opts.applyDefaults(rep.Environment); err != nil {
		return record{}, err
	}
	acceptancePath := "harness/acceptance.json"
	acceptanceHash, err := fileSHA256(root, acceptancePath)
	if err != nil {
		return record{}, err
	}
	subject := recordSubject{Kind: "run", Level: maxLevel(rep.Checks), Profile: profileFromScope(scope)}
	var stageHash string
	inputs := []recordInput{{Path: acceptancePath, SHA256: acceptanceHash}}
	if s != nil && s.ID != "" {
		subject.StageID = s.ID
		subject.StagePath = filepath.ToSlash(s.path)
		stageHash, err = fileSHA256(root, filepath.ToSlash(s.path))
		if err != nil {
			return record{}, err
		}
		inputs = nil
		for _, p := range s.Inputs {
			sum, hashErr := fileSHA256(root, p)
			if hashErr != nil {
				return record{}, hashErr
			}
			inputs = append(inputs, recordInput{Path: filepath.ToSlash(p), SHA256: sum})
		}
	}
	reportPath, err := filepath.Rel(root, filepath.Join(runDir, "report.json"))
	if err != nil || !relative(reportPath) {
		return record{}, errors.New("report must stay inside the repository")
	}
	reportPath = filepath.ToSlash(reportPath)
	reportHash, err := fileSHA256(root, reportPath)
	if err != nil {
		return record{}, err
	}
	checks := make([]recordCheck, 0, len(rep.Checks))
	logs := make([]recordArtifact, 0, len(rep.Checks))
	var ids []string
	for _, r := range rep.Checks {
		ids = append(ids, r.ID)
		sum, hashErr := fileSHA256(root, r.Log)
		if hashErr != nil {
			return record{}, hashErr
		}
		logs = append(logs, recordArtifact{Path: r.Log, SHA256: sum})
		checks = append(checks, recordCheck{
			ID:           r.ID,
			Level:        r.Level,
			Status:       r.Status,
			ExitCode:     r.ExitCode,
			DurationMS:   r.Millis,
			PassedTests:  slices.Clone(r.PassedTests),
			SkippedTests: slices.Clone(r.SkippedTests),
		})
	}
	verdict := "passed"
	if rep.Status != "passed" {
		verdict = "failed"
	}
	rec := record{
		Version:       1,
		RecordID:      recordID,
		RunID:         rep.RunID,
		RunKind:       opts.RunKind,
		RunStatus:     rep.Status,
		Executor:      opts.Executor,
		StartedAt:     rep.StartedAt,
		FinishedAt:    time.Now().UTC().Format(time.RFC3339),
		Subject:       subject,
		Criteria:      recordCriteria{AcceptanceSHA256: acceptanceHash, StageSHA256: stageHash, Inputs: inputs, CheckIDs: ids},
		Code:          recordCode{BaseCommit: rep.BaseCommit, SourceFingerprint: rep.Fingerprint, Dirty: rep.Dirty, TestedBinarySHA256: rep.TestedBinarySHA256},
		Environment:   rep.Environment,
		Authorization: opts.Authorization,
		Verdict:       verdict,
		Checks:        checks,
		Evidence:      recordEvidence{ReportPath: reportPath, ReportSHA256: reportHash, Logs: logs},
	}
	return rec, nil
}

func validCheckedLength(p string) bool {
	if len(p) != 64 {
		return false
	}
	for _, r := range p {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func badPath(p string) bool {
	return p == "" || !relative(p) || strings.Contains(p, `\`) || strings.HasPrefix(p, "~")
}

var (
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`xox[baprs]-`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	}
	urlPattern = regexp.MustCompile(`https?://[^\s"\\]+`)
)

// publishedReportPath 是运行报告在发布面的固定落点（与记录文件名一一对应）。
// publishedReportPath is the fixed publish-surface location of a run report.
func publishedReportPath(recordID string) string {
	return "harness/acceptance/records/reports/" + recordID + ".json"
}

// resolveReportPath 先取发布副本，缺失时回退记录登记的原始路径：记录发布后
// 新鲜 clone 不含 .loop，只有发布副本可解析。
// resolveReportPath prefers the published copy and falls back to the recorded
// path; a fresh clone has no .loop directory, only the published copy.
func resolveReportPath(root string, rec record) (string, error) {
	if rec.Evidence.ReportPath == "" {
		return "", errors.New("record requires evidence.report_path")
	}
	published := publishedReportPath(rec.RecordID)
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(published))); err == nil {
		return published, nil
	}
	return rec.Evidence.ReportPath, nil
}

func reportForRecord(root string, rec record) (acceptanceReport, error) {
	var rep acceptanceReport
	path, err := resolveReportPath(root, rec)
	if err != nil {
		return rep, err
	}
	raw, err := readUnderRoot(root, path)
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func sameHash(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func validateRecord(root string, rec record) error {
	if rec.Version != 1 {
		return errors.New("record version must be 1")
	}
	if rec.RecordID == "" || rec.RunID == "" {
		return errors.New("record_id and run_id are required")
	}
	if !slices.Contains([]string{"original", "rerun", "backfill"}, rec.RunKind) {
		return fmt.Errorf("unknown run_kind: %s", rec.RunKind)
	}
	if !slices.Contains([]string{"ai", "user", "ci"}, rec.Executor) {
		return fmt.Errorf("unknown executor: %s", rec.Executor)
	}
	if !slices.Contains([]string{"run", "manual_item"}, rec.Subject.Kind) {
		return fmt.Errorf("unknown subject.kind: %s", rec.Subject.Kind)
	}
	if !slices.Contains([]string{"source", "process", "external"}, rec.Subject.Level) {
		return fmt.Errorf("unknown subject.level: %s", rec.Subject.Level)
	}
	if !slices.Contains([]string{"passed", "failed", "skipped", "not_verified", "not_applicable"}, rec.Verdict) {
		return fmt.Errorf("unknown verdict: %s", rec.Verdict)
	}
	for _, c := range rec.Checks {
		if !slices.Contains([]string{"not_run", "passed", "failed", "error"}, c.Status) {
			return fmt.Errorf("unknown check status: %s", c.Status)
		}
	}
	if _, err := time.Parse(time.RFC3339, rec.StartedAt); err != nil || !strings.HasSuffix(rec.StartedAt, "Z") {
		return errors.New("started_at must be RFC3339 UTC")
	}
	if _, err := time.Parse(time.RFC3339, rec.FinishedAt); err != nil || !strings.HasSuffix(rec.FinishedAt, "Z") {
		return errors.New("finished_at must be RFC3339 UTC")
	}
	if rec.Verdict == "not_applicable" && strings.TrimSpace(rec.NABasis) == "" {
		return errors.New("not_applicable requires na_basis")
	}
	if rec.Verdict == "not_verified" {
		if rec.Subject.Kind != "manual_item" || strings.TrimSpace(rec.Notes) == "" || len(rec.Subject.Items) == 0 {
			return errors.New("not_verified requires a manual item, notes and items")
		}
	}
	if rec.Verdict == "skipped" && (rec.Subject.Kind != "manual_item" || strings.TrimSpace(rec.Notes) == "") {
		return errors.New("skipped requires a manual item and notes")
	}
	if rec.Subject.Kind == "run" {
		if !slices.Contains([]string{"passed", "failed", "source_changed"}, rec.RunStatus) {
			return errors.New("run records require run_status")
		}
		if !slices.Contains([]string{"passed", "failed"}, rec.Verdict) {
			return errors.New("run records only allow passed/failed verdicts")
		}
		if len(rec.Checks) == 0 || rec.Evidence.ReportPath == "" || len(rec.Evidence.Logs) == 0 {
			return errors.New("run records require checks, report_path and logs")
		}
		if rec.RecordID != rec.RunID {
			return errors.New("run record_id must equal run_id")
		}
	} else if rec.RunStatus != "" {
		return errors.New("manual item records must omit run_status")
	}
	verified := rec.Verdict == "not_verified"
	if (rec.Subject.Level == "process" || rec.Subject.Level == "external") && !verified {
		if rec.Code.TestedBinarySHA256 == nil || !validCheckedLength(*rec.Code.TestedBinarySHA256) {
			return errors.New("process/external records require a tested_binary_sha256")
		}
	}
	if rec.Subject.Level == "external" && !verified {
		if rec.Authorization == nil || strings.TrimSpace(rec.Authorization.Ref) == "" || strings.TrimSpace(rec.Authorization.Scope) == "" {
			return errors.New("external records require authorization ref and scope")
		}
	} else if rec.Subject.Level != "external" && rec.Authorization != nil {
		return errors.New("authorization is only allowed for external records")
	}
	paths := []string{rec.Subject.StagePath, rec.Evidence.ReportPath, rec.RedStatePath()}
	for _, in := range rec.Criteria.Inputs {
		paths = append(paths, in.Path)
	}
	for _, log := range rec.Evidence.Logs {
		paths = append(paths, log.Path)
	}
	for _, artifact := range rec.Evidence.Artifacts {
		paths = append(paths, artifact.Path)
	}
	if rec.RedState != nil {
		for _, artifact := range rec.RedState.AppliesTo {
			paths = append(paths, artifact.Path)
		}
	}
	for _, p := range paths {
		if p != "" && badPath(p) {
			return fmt.Errorf("record path must be repo-relative with slash separators: %s", p)
		}
	}
	if rec.RedState != nil {
		if len(rec.RedState.AppliesTo) == 0 || !validCheckedLength(rec.RedState.SHA256) {
			return errors.New("red_state requires path, sha256 and applies_to")
		}
		for _, artifact := range rec.RedState.AppliesTo {
			if !validCheckedLength(artifact.SHA256) {
				return errors.New("red_state applies_to hashes must be 64 lowercase hex")
			}
		}
	}
	if rec.Evidence.ReportSHA256 != "" && !validCheckedLength(rec.Evidence.ReportSHA256) {
		return errors.New("report_sha256 must be 64 lowercase hex")
	}
	// 通过 / 失败记录必须能解析同次报告：优先发布副本，其次记录登记的原始
	// 路径；两者都缺失才判红。运行记录与人工项同一口径。
	// Passed/failed records must resolve their same-run report, published copy
	// first and recorded path second; run records follow the manual-item rule.
	needsReport := rec.Verdict == "passed" || rec.Verdict == "failed"
	if rec.Evidence.ReportPath != "" {
		reportRel, err := resolveReportPath(root, rec)
		if err != nil {
			return err
		}
		reportFile := filepath.Join(root, filepath.FromSlash(reportRel))
		if _, statErr := os.Stat(reportFile); statErr == nil {
			rep, err := reportForRecord(root, rec)
			if err != nil {
				return err
			}
			reportHash, err := fileSHA256(root, reportRel)
			if err != nil {
				return err
			}
			if rec.Evidence.ReportSHA256 != reportHash {
				return errors.New("report_sha256 does not match the report file")
			}
			if rep.RunID != rec.RunID {
				return errors.New("record run_id does not match its report")
			}
			if !sameHash(rep.TestedBinarySHA256, rec.Code.TestedBinarySHA256) {
				return errors.New("record tested_binary_sha256 does not match its report")
			}
			if rec.Subject.Level == "external" && !verified {
				if rep.Authorization == nil || *rep.Authorization != *rec.Authorization {
					return errors.New("record authorization differs from its report")
				}
			}
		} else if needsReport {
			return statErr
		}
	} else if needsReport {
		return errors.New("passed/failed record requires evidence.report_path")
	}
	if rec.RunStatus == "source_changed" && rec.Verdict != "failed" {
		return errors.New("source_changed requires a failed verdict")
	}
	if rec.Verdict == "passed" {
		for _, c := range rec.Checks {
			if c.Status != "passed" {
				return fmt.Errorf("passed record contains a non-passed check: %s", c.ID)
			}
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	text := string(raw)
	for _, pattern := range secretPatterns {
		if pattern.MatchString(text) {
			return errors.New("record contains a credential pattern")
		}
	}
	// URL 形态扫描跳过 checks 的测试名清单：它们是仓库测试的结构化标识（表驱动子测试名
	// 可能含示例地址，如 `TestHttpGet_PortFrozenCombinations/放行_http://api.example.com/a`），
	// 与源码同源、不承载运行时凭证；发布面同扫的
	// TestRecords_NoSecretsOrAbsolutePaths 也只对凭证与绝对路径判红。其余字段（notes /
	// authorization / excerpt / subject 等）照旧拒绝 webhook 路径、查询串与 userinfo。
	// The URL-shape scan skips repository test identifiers in checks; credential patterns
	// still scan the full record.
	scan, err := json.Marshal(urlScanRecord(rec))
	if err != nil {
		return err
	}
	for _, match := range urlPattern.FindAllString(string(scan), -1) {
		rest := strings.TrimPrefix(strings.TrimPrefix(match, "https://"), "http://")
		if strings.ContainsAny(rest, "/?#@") {
			return errors.New("record contains a webhook path, query string or userinfo")
		}
	}
	if rec.Subject.Kind == "manual_item" && (rec.Verdict == "passed" || rec.Verdict == "failed") {
		if err := validateBackfillBinding(root, rec); err != nil {
			return err
		}
	}
	return nil
}

// urlScanRecord 返回去掉测试名清单的记录副本，供 URL 形态扫描使用；凭证模式仍扫全量。
// urlScanRecord clears repository test identifiers for the URL-shape scan only.
func urlScanRecord(rec record) record {
	out := rec
	out.Checks = slices.Clone(rec.Checks)
	for i := range out.Checks {
		out.Checks[i].PassedTests = nil
		out.Checks[i].SkippedTests = nil
	}
	return out
}

// RedStatePath 是路径校验用的访问器；没有红态时返回空串。
// RedStatePath exposes the red-state path for validation; it is empty when absent.
func (r record) RedStatePath() string {
	if r.RedState == nil {
		return ""
	}
	return r.RedState.Path
}

// validateBackfillBinding 要求人工项结论能在同次报告的同 check 具名测试中找到。
// 目录映射（item -> check/test）由目录覆盖测试另行核对。
// validateBackfillBinding requires a manual item verdict to be backed by the same run's
// report, the same check and, for passed, a named passing test. Catalog mapping is checked
// separately by the coverage tests.
func validateBackfillBinding(root string, rec record) error {
	if rec.Subject.Kind != "manual_item" {
		return errors.New("binding validation only applies to manual items")
	}
	if rec.Verdict != "passed" && rec.Verdict != "failed" {
		return errors.New("manual item binding requires passed/failed")
	}
	if len(rec.Subject.Items) == 0 || rec.Subject.CheckID == "" || rec.Subject.Test == "" {
		return errors.New("manual item binding requires items, check_id and test")
	}
	rep, err := reportForRecord(root, rec)
	if err != nil {
		return err
	}
	if rep.Fingerprint != rec.Code.SourceFingerprint || rep.BaseCommit != rec.Code.BaseCommit || rep.Dirty != rec.Code.Dirty {
		return errors.New("record code state differs from its report")
	}
	if rep.Environment != rec.Environment {
		return errors.New("record environment differs from its report")
	}
	if !sameHash(rep.TestedBinarySHA256, rec.Code.TestedBinarySHA256) {
		return errors.New("record binary hash differs from its report")
	}
	idx := slices.IndexFunc(rep.Checks, func(c result) bool { return c.ID == rec.Subject.CheckID })
	if idx < 0 {
		return fmt.Errorf("report has no check %s", rec.Subject.CheckID)
	}
	check := rep.Checks[idx]
	if check.Level != rec.Subject.Level {
		return errors.New("record level differs from the report check level")
	}
	if rec.Verdict == "passed" {
		if check.Status != "passed" || !slices.Contains(check.PassedTests, rec.Subject.Test) {
			return errors.New("report does not show the named test passing in this check")
		}
	}
	return nil
}

func resolveAuthorization(flags authorizationFlags, checks []check) (*authorization, error) {
	ref, scope := strings.TrimSpace(flags.Ref), strings.TrimSpace(flags.Scope)
	hasExternal := slices.ContainsFunc(checks, func(c check) bool { return checkLevel(c) == "external" })
	if hasExternal {
		if ref == "" || scope == "" {
			return nil, errors.New("external checks require --authorization-ref and --authorization-scope")
		}
		return &authorization{Ref: ref, Scope: scope}, nil
	}
	if ref != "" || scope != "" {
		return nil, errors.New("authorization flags require an external check")
	}
	return nil, nil
}

// computeRecordStatus 在读取时计算三视图之二：同键取最新，冲突显式保留，不跨平台淘汰。
// computeRecordStatus derives per-key status at read time; conflicts stay visible and
// cross-platform results are never masked.
func computeRecordStatus(records []record, catalog []catalogItem) map[statusKey]statusEntry {
	known := map[string]bool{}
	for _, item := range catalog {
		known[item.Item] = true
	}
	ordered := slices.Clone(records)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].FinishedAt != ordered[j].FinishedAt {
			return ordered[i].FinishedAt < ordered[j].FinishedAt
		}
		return ordered[i].RecordID < ordered[j].RecordID
	})
	status := map[statusKey]statusEntry{}
	for _, rec := range ordered {
		if rec.Subject.Kind != "manual_item" {
			continue
		}
		for _, item := range rec.Subject.Items {
			if !known[item] {
				continue
			}
			key := statusKey{Item: item, Fingerprint: rec.Code.SourceFingerprint, OS: rec.Environment.OS}
			entry := status[key]
			if entry.Verdict != "" && entry.Verdict != rec.Verdict {
				entry.Conflict = true
				entry.RecordIDs = []string{rec.RecordID, entry.RecordIDs[0]}
			} else {
				entry.RecordIDs = []string{rec.RecordID}
			}
			entry.Verdict = rec.Verdict
			status[key] = entry
		}
	}
	byItem := map[string][]statusKey{}
	for key := range status {
		byItem[key.Item] = append(byItem[key.Item], key)
	}
	for _, keys := range byItem {
		passed, notPassed := false, false
		for _, key := range keys {
			if status[key].Verdict == "passed" {
				passed = true
			} else {
				notPassed = true
			}
		}
		if passed && notPassed {
			for _, key := range keys {
				entry := status[key]
				entry.Conflict = true
				status[key] = entry
			}
		}
	}
	return status
}

// writeRecord 只追加：同名记录已存在即停止，绝不覆盖。
// writeRecord is append-only: an existing record stops the write.
func writeRecord(runDir string, rec record) error {
	path := filepath.Join(runDir, "record.json")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("record already exists (append-only): %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicJSON(path, &rec)
}
