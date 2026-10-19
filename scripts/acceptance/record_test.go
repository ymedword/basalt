package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const recordTestHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func recordFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "harness"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "harness/acceptance.json"), []byte(`{"version":1}`), 0o600))
	return root
}

// writeRecordRun 生成同次报告与记录；日志内容由调用方给出，报告状态可改写。
// writeRecordRun writes a same-run report and record; callers provide the log body.
func writeRecordRun(t *testing.T, root, runID string, logBody string, opts recordOptions, mutate func(*acceptanceReport)) (record, acceptanceReport) {
	t.Helper()
	runDir := filepath.Join(root, ".loop/runs", runID)
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	logPath := filepath.Join(runDir, "contract.log")
	require.NoError(t, os.WriteFile(logPath, []byte(logBody), 0o600))
	rep := acceptanceReport{
		Version:     1,
		RunID:       runID,
		Scope:       "fixture",
		Fingerprint: "fingerprint",
		BaseCommit:  "commit",
		Environment: environment{OS: "linux", Arch: "amd64", GoVersion: "go1.27.2", CI: false},
		StartedAt:   "2026-10-19T10:00:00Z",
		Status:      "passed",
		Checks: []result{{
			ID:           "contract",
			Level:        "process",
			Status:       "passed",
			ExitCode:     0,
			Log:          ".loop/runs/" + runID + "/contract.log",
			PassedTests:  []string{"TestContract"},
			SkippedTests: []string{},
		}},
	}
	if mutate != nil {
		mutate(&rep)
	}
	internal := rep
	internal.Checks = append([]result(nil), rep.Checks...)
	for i := range internal.Checks {
		internal.Checks[i].Log = filepath.Join(root, filepath.FromSlash(internal.Checks[i].Log))
	}
	require.NoError(t, resolveBinaryHash(&internal))
	rep.TestedBinarySHA256 = internal.TestedBinarySHA256
	require.NoError(t, atomicJSON(filepath.Join(runDir, "report.json"), &rep))
	rec, err := buildRecord(root, runDir, "fixture", rep, configuration{}, nil, opts)
	require.NoError(t, err)
	return rec, rep
}

func validRunRecord(t *testing.T) (string, record) {
	t.Helper()
	root := recordFixtureRoot(t)
	rec, _ := writeRecordRun(t, root, "run-1", "binary_sha256="+recordTestHash+"\n", recordOptions{}, nil)
	require.NoError(t, validateRecord(root, rec))
	return root, rec
}

func cloneRecord(t *testing.T, rec record) record {
	t.Helper()
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	var out record
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func manualRecord(t *testing.T, level, verdict string) record {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	return record{
		Version:   1,
		RecordID:  "backfill-test",
		RunID:     "run-1",
		RunKind:   "backfill",
		Executor:  "ai",
		StartedAt: now,
		Subject: recordSubject{
			Kind:    "manual_item",
			Level:   level,
			Items:   []string{"us1-6-1a"},
			CheckID: "015-backfill",
			Test:    "TestBackfill_Bootstrap/bootstrap",
		},
		Criteria: recordCriteria{
			AcceptanceSHA256: recordTestHash,
			Inputs:           []recordInput{{Path: "harness/acceptance.json", SHA256: recordTestHash}},
		},
		Code:        recordCode{SourceFingerprint: "fingerprint"},
		Environment: environment{OS: "linux", Arch: "amd64", GoVersion: "go1.27.2"},
		Verdict:     verdict,
		Notes:       "missing BASALT_REAL_LLM_API_KEY",
		FinishedAt:  now,
	}
}

// TestRecord_BuildFromReport 逐字段绑定同次报告，并验证三层 binary_sha256 提取规则。
// TestRecord_BuildFromReport binds every field to its report and checks marker extraction.
func TestRecord_BuildFromReport(t *testing.T) {
	root := recordFixtureRoot(t)
	rec, rep := writeRecordRun(t, root, "run-1", "binary_sha256="+recordTestHash+"\n", recordOptions{}, nil)
	require.Equal(t, 1, rec.Version)
	require.Equal(t, "run-1", rec.RecordID)
	require.Equal(t, "run-1", rec.RunID)
	require.Equal(t, "original", rec.RunKind)
	require.Equal(t, "passed", rec.RunStatus)
	require.Equal(t, "ai", rec.Executor)
	require.Equal(t, "run", rec.Subject.Kind)
	require.Equal(t, "process", rec.Subject.Level)
	require.Equal(t, "commit", rec.Code.BaseCommit)
	require.Equal(t, "fingerprint", rec.Code.SourceFingerprint)
	require.False(t, rec.Code.Dirty)
	require.NotNil(t, rec.Code.TestedBinarySHA256)
	require.Equal(t, recordTestHash, *rec.Code.TestedBinarySHA256)
	acceptanceHash, err := fileSHA256(root, "harness/acceptance.json")
	require.NoError(t, err)
	require.Equal(t, acceptanceHash, rec.Criteria.AcceptanceSHA256)
	require.Len(t, rec.Criteria.Inputs, 1)
	reportHash, err := fileSHA256(root, ".loop/runs/run-1/report.json")
	require.NoError(t, err)
	require.Equal(t, reportHash, rec.Evidence.ReportSHA256)
	require.Equal(t, ".loop/runs/run-1/report.json", rec.Evidence.ReportPath)
	require.Len(t, rec.Evidence.Logs, 1)
	require.Equal(t, []string{"TestContract"}, rec.Checks[0].PassedTests)
	require.NoError(t, validateRecord(root, rec))

	// 删掉全部标记：必须报错且层级保持 process，不得降级放行。
	missing := rep
	missing.Checks = append([]result(nil), rep.Checks...)
	missing.Checks[0].Log = filepath.Join(root, ".loop/runs/run-1/plain.log")
	require.NoError(t, os.WriteFile(missing.Checks[0].Log, []byte("no marker here\n"), 0o600))
	err = resolveBinaryHash(&missing)
	require.ErrorContains(t, err, "exactly one binary_sha256 marker")
	require.Equal(t, "process", missing.Checks[0].Level)

	duplicated := rep
	duplicated.Checks = append([]result(nil), rep.Checks...)
	duplicated.Checks[0].Log = filepath.Join(root, ".loop/runs/run-1/twice.log")
	require.NoError(t, os.WriteFile(duplicated.Checks[0].Log, []byte("binary_sha256="+recordTestHash+"\nbinary_sha256="+recordTestHash+"\n"), 0o600))
	require.ErrorContains(t, resolveBinaryHash(&duplicated), "found 2")

	conflicting := rep
	conflicting.Checks = append([]result(nil), rep.Checks...)
	conflicting.Checks = append(conflicting.Checks, result{ID: "second", Level: "process", Status: "passed"})
	conflicting.Checks[0].Log = filepath.Join(root, ".loop/runs/run-1/a.log")
	conflicting.Checks[1].Log = filepath.Join(root, ".loop/runs/run-1/b.log")
	other := strings.Repeat("f", 64)
	require.NoError(t, os.WriteFile(conflicting.Checks[0].Log, []byte("binary_sha256="+recordTestHash+"\n"), 0o600))
	require.NoError(t, os.WriteFile(conflicting.Checks[1].Log, []byte("binary_sha256="+other+"\n"), 0o600))
	require.ErrorContains(t, resolveBinaryHash(&conflicting), "distinct binary_sha256")
}

func TestRecord_BuildFromSourceChanged(t *testing.T) {
	root := recordFixtureRoot(t)
	rec, _ := writeRecordRun(t, root, "changed", "binary_sha256="+recordTestHash+"\n", recordOptions{}, func(rep *acceptanceReport) {
		rep.Status = "source_changed"
	})
	require.Equal(t, "source_changed", rec.RunStatus)
	require.Equal(t, "failed", rec.Verdict)
	require.NoError(t, validateRecord(root, rec))

	failed, _ := writeRecordRun(t, root, "failed", "binary_sha256="+recordTestHash+"\n", recordOptions{}, func(rep *acceptanceReport) {
		rep.Status = "failed"
	})
	require.Equal(t, "failed", failed.RunStatus)
	require.Equal(t, "failed", failed.Verdict)

	rep := acceptanceReport{RunID: "error", Status: "error"}
	_, err := buildRecord(root, ".loop/runs/error", "fixture", rep, configuration{}, nil, recordOptions{})
	require.ErrorContains(t, err, "cannot produce a record")
}

func TestRecord_ValidateRequiredFields(t *testing.T) {
	root, base := validRunRecord(t)
	for _, tc := range []struct {
		name   string
		mutate func(*record)
		want   string
	}{
		{"version", func(r *record) { r.Version = 2 }, "version must be 1"},
		{"record-id", func(r *record) { r.RecordID = "" }, "record_id and run_id"},
		{"run-id", func(r *record) { r.RunID = "" }, "record_id and run_id"},
		{"run-kind", func(r *record) { r.RunKind = "weekly" }, "unknown run_kind"},
		{"executor", func(r *record) { r.Executor = "robot" }, "unknown executor"},
		{"subject-kind", func(r *record) { r.Subject.Kind = "job" }, "unknown subject.kind"},
		{"subject-level", func(r *record) { r.Subject.Level = "sandbox" }, "unknown subject.level"},
		{"verdict", func(r *record) { r.Verdict = "maybe" }, "unknown verdict"},
		{"check-status", func(r *record) { r.Checks[0].Status = "skipped" }, "unknown check status"},
		{"started-at", func(r *record) { r.StartedAt = "2026-10-19T10:00:00+08:00" }, "started_at must be RFC3339 UTC"},
		{"finished-at", func(r *record) { r.FinishedAt = "2026-10-19 10:00:00" }, "finished_at must be RFC3339 UTC"},
		{"no-checks", func(r *record) { r.Checks = nil }, "run records require checks"},
		{"no-report", func(r *record) { r.Evidence.ReportPath = ""; r.Evidence.ReportSHA256 = "" }, "run records require checks, report_path and logs"},
		{"run-status", func(r *record) { r.RunStatus = "" }, "run records require run_status"},
		{"run-verdict", func(r *record) { r.Verdict = "skipped" }, "skipped requires a manual item and notes"},
		{"run-id-mismatch", func(r *record) { r.RunID = "other" }, "run record_id must equal run_id"},
		{"passed-with-failed-check", func(r *record) { r.Checks[0].Status = "failed" }, "passed record contains a non-passed check"},
		{"manual-with-run-status", func(r *record) {
			r.Subject.Kind = "manual_item"
			r.RunStatus = "passed"
		}, "manual item records must omit run_status"},
		{"process-without-hash", func(r *record) { r.Code.TestedBinarySHA256 = nil }, "process/external records require a tested_binary_sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cloneRecord(t, base)
			tc.mutate(&got)
			require.ErrorContains(t, validateRecord(root, got), tc.want)
		})
	}
}

func TestRecord_ValidateNotVerified(t *testing.T) {
	root := recordFixtureRoot(t)
	rec := manualRecord(t, "external", "not_verified")
	// 缺环境项允许空二进制哈希与空授权，但必须逐项列出并写明缺什么。
	require.NoError(t, validateRecord(root, rec))

	missingNotes := rec
	missingNotes.Notes = " "
	require.ErrorContains(t, validateRecord(root, missingNotes), "not_verified requires a manual item, notes and items")

	missingItems := rec
	missingItems.Subject.Items = nil
	require.ErrorContains(t, validateRecord(root, missingItems), "not_verified requires a manual item, notes and items")

	run := rec
	run.Subject.Kind = "run"
	require.ErrorContains(t, validateRecord(root, run), "not_verified requires a manual item")

	skipped := manualRecord(t, "source", "skipped")
	skipped.Notes = "not a mandatory item"
	require.NoError(t, validateRecord(root, skipped))
	skipped.Notes = ""
	require.ErrorContains(t, validateRecord(root, skipped), "skipped requires a manual item and notes")
}

func TestRecord_OptionsDefaults(t *testing.T) {
	local := environment{OS: "linux", Arch: "amd64", GoVersion: "go1.27.2"}
	opts := recordOptions{}
	require.NoError(t, opts.applyDefaults(local))
	require.Equal(t, "ai", opts.Executor)
	require.Equal(t, "original", opts.RunKind)

	ci := local
	ci.CI = true
	opts = recordOptions{}
	require.NoError(t, opts.applyDefaults(ci))
	require.Equal(t, "ci", opts.Executor)

	user := recordOptions{Executor: "user", RunKind: "rerun"}
	require.NoError(t, user.applyDefaults(ci))
	require.Equal(t, "user", user.Executor)
	require.Equal(t, "rerun", user.RunKind)

	require.ErrorContains(t, (&recordOptions{Executor: "robot"}).applyDefaults(local), "unknown executor")
	require.ErrorContains(t, (&recordOptions{RunKind: "weekly"}).applyDefaults(local), "unknown run kind")
}

func TestRecord_ValidateRejectsNAWithoutBasis(t *testing.T) {
	root := recordFixtureRoot(t)
	rec := manualRecord(t, "source", "not_applicable")
	rec.Notes = ""
	require.ErrorContains(t, validateRecord(root, rec), "not_applicable requires na_basis")
	rec.NABasis = "plan.md §4 declares no integration layer"
	require.NoError(t, validateRecord(root, rec))
}

func TestRecord_ValidateRejectsAbsolutePath(t *testing.T) {
	root, base := validRunRecord(t)
	for _, path := range []string{`C:/runs/report.json`, "/runs/report.json", `..\escape\report.json`, `\\server\share\report.json`} {
		got := cloneRecord(t, base)
		got.Evidence.ReportPath = path
		require.ErrorContains(t, validateRecord(root, got), "repo-relative")
	}
	logPath := cloneRecord(t, base)
	logPath.Evidence.Logs[0].Path = "D:/logs/contract.log"
	require.ErrorContains(t, validateRecord(root, logPath), "repo-relative")
	inputPath := cloneRecord(t, base)
	inputPath.Criteria.Inputs[0].Path = "/absolute/acceptance.json"
	require.ErrorContains(t, validateRecord(root, inputPath), "repo-relative")
}

func TestRecord_ValidateRejectsSecret(t *testing.T) {
	root, base := validRunRecord(t)
	secret := cloneRecord(t, base)
	secret.Notes = "token sk-" + strings.Repeat("a", 16)
	require.ErrorContains(t, validateRecord(root, secret), "credential pattern")
	webhook := cloneRecord(t, base)
	webhook.Notes = "https://hooks.example.com/services/T000/B000/secret"
	require.ErrorContains(t, validateRecord(root, webhook), "webhook path")
	hostOnly := cloneRecord(t, base)
	hostOnly.Notes = "receiver https://hooks.example.com"
	require.NoError(t, validateRecord(root, hostOnly))
}

// TestRecord_ValidateSkipsTestNameURLs 守住 URL 形态扫描的边界：checks 的测试名清单是
// 仓库测试的结构化标识（表驱动子测试名可能含示例地址），不阻断记录生成；同一形态出现在
// 自由文本字段（如 evidence.excerpt）仍判红——豁免不外溢到运行时证据。
// The URL-shape scan ignores repository test identifiers but still rejects evidence text.
func TestRecord_ValidateSkipsTestNameURLs(t *testing.T) {
	root, base := validRunRecord(t)
	withTestNames := cloneRecord(t, base)
	withTestNames.Checks[0].PassedTests = []string{
		"TestHttpGet_PortFrozenCombinations/放行_http://api.example.com/a",
		"TestHttpGet_NonDefaultPortRefusedBeforeSend/https://api.example.com:8443/x",
	}
	require.NoError(t, validateRecord(root, withTestNames))

	evidence := cloneRecord(t, base)
	evidence.Evidence.Excerpt = "receiver https://hooks.example.com/services/T000/B000"
	require.ErrorContains(t, validateRecord(root, evidence), "webhook path")
}

func TestRecord_ValidateRedStateRequiresAppliesTo(t *testing.T) {
	root, base := validRunRecord(t)
	missing := cloneRecord(t, base)
	missing.RedState = &redState{Path: "harness/red.txt", SHA256: recordTestHash}
	require.ErrorContains(t, validateRecord(root, missing), "red_state requires path, sha256 and applies_to")

	badHash := cloneRecord(t, base)
	badHash.RedState = &redState{Path: "harness/red.txt", SHA256: recordTestHash, AppliesTo: []recordArtifact{{Path: "harness/red.txt", SHA256: "not-a-hash"}}}
	require.ErrorContains(t, validateRecord(root, badHash), "red_state applies_to hashes")

	badPath := cloneRecord(t, base)
	badPath.RedState = &redState{Path: "harness/red.txt", SHA256: recordTestHash, AppliesTo: []recordArtifact{{Path: "C:/red.txt", SHA256: recordTestHash}}}
	require.ErrorContains(t, validateRecord(root, badPath), "repo-relative")

	valid := cloneRecord(t, base)
	valid.RedState = &redState{Path: "harness/red.txt", SHA256: recordTestHash, AppliesTo: []recordArtifact{{Path: "harness/red.txt", SHA256: recordTestHash}}}
	require.NoError(t, validateRecord(root, valid))
}

func TestRecord_ExternalAuthorization(t *testing.T) {
	root := recordFixtureRoot(t)
	auth := &authorization{Ref: "user-2026-10-19-real-llm-webhook", Scope: "real-llm:openai-compatible; real-webhook:feishu; synthetic-data-only; model-calls<=30; cny<=10; sends<=5"}
	rec, _ := writeRecordRun(t, root, "real", "binary_sha256="+recordTestHash+"\n", recordOptions{Authorization: auth}, func(rep *acceptanceReport) {
		rep.Checks[0].Level = "external"
		rep.Authorization = auth
	})
	require.Equal(t, "external", rec.Subject.Level)
	require.NoError(t, validateRecord(root, rec))

	missing := cloneRecord(t, rec)
	missing.Authorization = nil
	require.ErrorContains(t, validateRecord(root, missing), "external records require authorization")

	mismatch := cloneRecord(t, rec)
	mismatch.Authorization = &authorization{Ref: auth.Ref, Scope: "narrower"}
	require.ErrorContains(t, validateRecord(root, mismatch), "differs from its report")

	extra := cloneRecord(t, rec)
	extra.Subject.Level = "process"
	require.ErrorContains(t, validateRecord(root, extra), "authorization is only allowed for external records")

	externalChecks := []check{{ID: "real", Level: "external"}}
	_, err := resolveAuthorization(authorizationFlags{}, externalChecks)
	require.ErrorContains(t, err, "--authorization-ref")
	_, err = resolveAuthorization(authorizationFlags{Ref: auth.Ref, Scope: auth.Scope}, []check{{ID: "src"}})
	require.ErrorContains(t, err, "authorization flags require an external check")
	got, err := resolveAuthorization(authorizationFlags{Ref: auth.Ref, Scope: auth.Scope}, externalChecks)
	require.NoError(t, err)
	require.Equal(t, auth, got)
}
