package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFingerprint_IgnoresAcceptanceEvidence 断言证据面不计入源码指纹，判据输入仍计入。
// TestFingerprint_IgnoresAcceptanceEvidence excludes the evidence surface but keeps
// criteria inputs inside the source fingerprint.
func TestFingerprint_IgnoresAcceptanceEvidence(t *testing.T) {
	root, _, _ := fixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "harness"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "harness/acceptance.json"), []byte(`{"version":1}`), 0o600))
	before, err := sourceFingerprint(root)
	require.NoError(t, err)

	records := filepath.Join(root, "harness/acceptance/records")
	require.NoError(t, os.MkdirAll(filepath.Join(records, "reports"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(records, "run-1.json"), []byte(`{"record_id":"run-1"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(records, "reports/run-1.json"), []byte(`{"run_id":"run-1"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(records, "README.md"), []byte("publish steps"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "harness/acceptance/ledger.md"), []byte("| record_id |\n| --- |\n"), 0o600))
	after, err := sourceFingerprint(root)
	require.NoError(t, err)
	require.Equal(t, before, after, "published records and the ledger are evidence")

	require.NoError(t, os.WriteFile(filepath.Join(root, "harness/acceptance.json"), []byte(`{"version":2}`), 0o600))
	changed, err := sourceFingerprint(root)
	require.NoError(t, err)
	require.NotEqual(t, before, changed, "criteria inputs still count")
}

// backfillCatalogItems 把回填目录投影为记录状态视图使用的类型。
// backfillCatalogItems projects the backfill catalog into the status-view type.
func backfillCatalogItems() []catalogItem {
	out := make([]catalogItem, 0, len(backfillCatalog))
	for _, entry := range backfillCatalog {
		out = append(out, catalogItem{Item: entry.Item, Check: entry.Check, Test: entry.Test, Level: entry.Level})
	}
	return out
}

// catalogByItem 按编号索引回填目录，供执行绑定测试核对 check / test 映射。
// catalogByItem indexes the catalog so binding tests can check the check/test mapping.
func catalogByItem() map[string]catalogEntry {
	out := make(map[string]catalogEntry, len(backfillCatalog))
	for _, entry := range backfillCatalog {
		out[entry.Item] = entry
	}
	return out
}

// publishedRecords 读取已发布记录；文件名必须等于 record_id，重复即失败。
// publishedRecords loads published records; the file name must equal record_id.
func publishedRecords(t *testing.T, root string) []record {
	t.Helper()
	dir := filepath.Join(root, "harness", "acceptance", "records")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "published records directory")
	var out []record
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		var rec record
		require.NoError(t, json.Unmarshal(raw, &rec), entry.Name())
		require.Equal(t, strings.TrimSuffix(entry.Name(), ".json"), rec.RecordID,
			"published file name must equal record_id")
		require.False(t, seen[rec.RecordID], "duplicate published record %s", rec.RecordID)
		seen[rec.RecordID] = true
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b record) int { return strings.Compare(a.RecordID, b.RecordID) })
	return out
}

// executedCatalogItems 是视图 1「历史曾执行」：任一指纹 / 平台 / 执行者下存在
// 绑定合格的 passed 回填记录。失败、未验证与未适用记录不计数。
// executedCatalogItems is view 1: a passed backfill record exists under any
// fingerprint/platform/executor. Failed and unverified records never count.
func executedCatalogItems(records []record) map[string]bool {
	executed := map[string]bool{}
	for _, rec := range records {
		if rec.Subject.Kind != "manual_item" || rec.Verdict != "passed" {
			continue
		}
		for _, item := range rec.Subject.Items {
			executed[item] = true
		}
	}
	return executed
}

// missingExecutedItems 返回冻结目录中尚无通过记录覆盖的编号。
// missingExecutedItems lists catalog items with no passing record yet.
func missingExecutedItems(records []record, catalog []catalogItem) []string {
	executed := executedCatalogItems(records)
	var missing []string
	for _, item := range catalog {
		if !executed[item.Item] {
			missing = append(missing, item.Item)
		}
	}
	return missing
}

func cloneRecords(t *testing.T, records []record) []record {
	t.Helper()
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	var out []record
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// TestRecords_BackfillCoversCatalog 只回答「历史是否执行过」：每个目录项至少
// 存在一条 passed 记录；删 subject.items、漏登记、把通过记录改成未验证均判红。
// TestRecords_BackfillCoversCatalog answers only "has this ever run": every
// catalog item needs a passing record; dropped items, missing records and
// not_verified substitutions must fail.
func TestRecords_BackfillCoversCatalog(t *testing.T) {
	catalog := backfillCatalogItems()
	records := publishedRecords(t, stageTestRoot(t))

	t.Run("published_set_is_covered", func(t *testing.T) {
		require.Empty(t, missingExecutedItems(records, catalog),
			"每个冻结目录项至少需要一条 passed 回填记录（视图 1）")
	})

	t.Run("dropped_item_is_red", func(t *testing.T) {
		item := catalog[0].Item
		mutated := cloneRecords(t, records)
		for i := range mutated {
			mutated[i].Subject.Items = slices.DeleteFunc(slices.Clone(mutated[i].Subject.Items),
				func(s string) bool { return s == item })
		}
		require.Contains(t, missingExecutedItems(mutated, catalog), item)
	})

	t.Run("unregistered_record_is_red", func(t *testing.T) {
		item := catalog[len(catalog)/2].Item
		var kept []record
		for _, rec := range records {
			if rec.Subject.Kind == "manual_item" && slices.Contains(rec.Subject.Items, item) {
				continue
			}
			kept = append(kept, rec)
		}
		require.Contains(t, missingExecutedItems(kept, catalog), item,
			"覆盖该编号的记录整条漏登记必须判红")
	})

	t.Run("not_verified_does_not_count", func(t *testing.T) {
		item := catalog[len(catalog)-1].Item
		mutated := cloneRecords(t, records)
		for i := range mutated {
			if mutated[i].Subject.Kind == "manual_item" && slices.Contains(mutated[i].Subject.Items, item) {
				mutated[i].Verdict = "not_verified"
			}
		}
		require.Contains(t, missingExecutedItems(mutated, catalog), item,
			"把唯一通过记录改成未验证后必须不再计入历史曾执行")
	})
}

// manualFixture 描述一条回填记录与其同次报告的最小形态。
// manualFixture describes the minimal shape of a backfill record and its report.
type manualFixture struct {
	Level string
	Item  string
	Check string
	Test  string
	Auth  *authorization
}

// writeManualBinding 生成记录 + 同次报告，供执行绑定的四类反例使用。
// writeManualBinding builds a record plus same-run report for the binding counterexamples.
func writeManualBinding(t *testing.T, spec manualFixture) (string, record) {
	t.Helper()
	root := recordFixtureRoot(t)
	runDir := filepath.Join(root, ".loop", "runs", "run-1")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	logRel := ".loop/runs/run-1/backfill.log"
	require.NoError(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(logRel)),
		[]byte("binary_sha256="+recordTestHash+"\n"), 0o600))
	binary := recordTestHash
	rep := acceptanceReport{
		Version:       1,
		RunID:         "run-1",
		Scope:         "us15-backfill",
		Fingerprint:   "fp-1",
		BaseCommit:    "commit-1",
		Environment:   environment{OS: "linux", Arch: "amd64", GoVersion: "go1.27.2"},
		Authorization: spec.Auth,
		StartedAt:     "2026-10-19T10:00:00Z",
		Status:        "passed",
		Checks: []result{{
			ID:           spec.Check,
			Level:        spec.Level,
			Status:       "passed",
			ExitCode:     0,
			Log:          logRel,
			PassedTests:  []string{spec.Test},
			SkippedTests: []string{},
		}},
	}
	rep.TestedBinarySHA256 = &binary
	reportRel := ".loop/runs/run-1/report.json"
	reportPath := filepath.Join(root, filepath.FromSlash(reportRel))
	require.NoError(t, atomicJSON(reportPath, &rep))
	reportHash, err := fileSHA256(root, reportRel)
	require.NoError(t, err)
	rec := record{
		Version:   1,
		RecordID:  "backfill-run-1",
		RunID:     "run-1",
		RunKind:   "backfill",
		Executor:  "ai",
		StartedAt: "2026-10-19T10:00:30Z",
		Subject: recordSubject{
			Kind:    "manual_item",
			Level:   spec.Level,
			Items:   []string{spec.Item},
			CheckID: spec.Check,
			Test:    spec.Test,
		},
		Criteria: recordCriteria{
			AcceptanceSHA256: recordTestHash,
			Inputs:           []recordInput{{Path: "harness/acceptance.json", SHA256: recordTestHash}},
		},
		Code: recordCode{
			BaseCommit:         "commit-1",
			SourceFingerprint:  "fp-1",
			Dirty:              false,
			TestedBinarySHA256: &binary,
		},
		Environment:   environment{OS: "linux", Arch: "amd64", GoVersion: "go1.27.2"},
		Authorization: spec.Auth,
		Verdict:       "passed",
		Evidence: recordEvidence{
			ReportPath:   reportRel,
			ReportSHA256: reportHash,
			Logs:         []recordArtifact{{Path: logRel, SHA256: recordTestHash}},
		},
		FinishedAt: "2026-10-19T10:00:30Z",
	}
	return root, rec
}

// rewriteReport 改写同次报告并同步记录中的报告哈希，供字段级反例使用。
// rewriteReport rewrites the same-run report and re-hashes it in the record.
func rewriteReport(t *testing.T, root string, rec *record, mutate func(*acceptanceReport)) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rec.Evidence.ReportPath))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var rep acceptanceReport
	require.NoError(t, json.Unmarshal(raw, &rep))
	mutate(&rep)
	require.NoError(t, atomicJSON(path, &rep))
	sum, err := fileSHA256(root, rec.Evidence.ReportPath)
	require.NoError(t, err)
	rec.Evidence.ReportSHA256 = sum
}

// catalogMappingError 核对记录声明的 items 与 check / test 是否与目录逐项一致。
// catalogMappingError checks that the record's items map to its check and test.
func catalogMappingError(rec record) error {
	byItem := catalogByItem()
	for _, item := range rec.Subject.Items {
		entry, ok := byItem[item]
		if !ok {
			return fmt.Errorf("item %s is not in the backfill catalog", item)
		}
		if entry.Check != rec.Subject.CheckID || entry.Test != rec.Subject.Test {
			return fmt.Errorf("item %s maps to %s/%s, record claims %s/%s",
				item, entry.Check, entry.Test, rec.Subject.CheckID, rec.Subject.Test)
		}
	}
	return nil
}

// TestRecords_BackfillBindsToExecution 要求每条 passed / failed 回填记录绑定
// 同一次运行的报告、check、具名 PASS、指纹、平台与二进制哈希；四类反例必须判红。
// TestRecords_BackfillBindsToExecution binds every backfill record to its same-run
// report, check, named pass, fingerprint, platform and binary hash; the four
// counterexamples must fail.
func TestRecords_BackfillBindsToExecution(t *testing.T) {
	root := stageTestRoot(t)
	published := publishedRecords(t, root)
	for _, rec := range published {
		if rec.Subject.Kind != "manual_item" || (rec.Verdict != "passed" && rec.Verdict != "failed") {
			continue
		}
		require.Equal(t, "backfill", rec.RunKind, "%s: 回填记录必须 run_kind=backfill", rec.RecordID)
		require.NoError(t, validateRecord(root, rec), rec.RecordID)
		require.NoError(t, catalogMappingError(rec), rec.RecordID)
	}

	// 每个反例用独立夹具：改写报告会改变哈希，共用会遮掩后续用例的预期错误。
	// Each counterexample gets its own fixture; a rewritten report would otherwise mask later errors.
	newFixture := func(t *testing.T) (string, record) {
		t.Helper()
		return writeManualBinding(t, manualFixture{
			Level: "process",
			Item:  "us1-6-8a",
			Check: "015-backfill",
			Test:  "TestBackfill_Bootstrap/assembly",
		})
	}
	root, base := newFixture(t)
	require.NoError(t, validateRecord(root, base))
	require.NoError(t, catalogMappingError(base))

	t.Run("no_pass_event_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Subject.Test = "TestBackfill_Bootstrap/init_idempotent"
		require.ErrorContains(t, validateRecord(root, rec),
			"report does not show the named test passing in this check")
	})

	t.Run("other_check_test_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Subject.Test = "TestBackfill_Web/listen_addr"
		require.ErrorContains(t, validateRecord(root, rec),
			"report does not show the named test passing in this check")
		require.Error(t, catalogMappingError(rec), "跨 check 的测试必须与目录映射不符")
	})

	t.Run("other_run_report_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.RunID = "run-2"
		require.ErrorContains(t, validateRecord(root, rec),
			"record run_id does not match its report")
	})

	t.Run("rewritten_report_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		path := filepath.Join(root, filepath.FromSlash(rec.Evidence.ReportPath))
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o600))
		require.ErrorContains(t, validateRecord(root, rec),
			"report_sha256 does not match the report file")
	})

	t.Run("fingerprint_mismatch_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Code.SourceFingerprint = "fp-other"
		require.ErrorContains(t, validateRecord(root, rec),
			"record code state differs from its report")
	})

	t.Run("platform_mismatch_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Environment.OS = "windows"
		require.ErrorContains(t, validateRecord(root, rec),
			"record environment differs from its report")
	})

	t.Run("binary_mismatch_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		other := strings.Repeat("f", 64)
		rec.Code.TestedBinarySHA256 = &other
		require.ErrorContains(t, validateRecord(root, rec),
			"tested_binary_sha256 does not match its report")
	})
}

// TestRecords_RealBindsToExecution 要求 external 记录与同次报告授权逐项相等；
// 缺授权、scope 不一致、报告无授权三类反例必须判红。
// TestRecords_RealBindsToExecution requires external records to match their
// report authorization; the three counterexamples must fail.
func TestRecords_RealBindsToExecution(t *testing.T) {
	auth := &authorization{
		Ref:   "user-2026-10-19-real-llm-webhook",
		Scope: "real-llm:openai-compatible; real-webhook:feishu; synthetic-data-only; model-calls<=30; cny<=10; sends<=5",
	}
	root := stageTestRoot(t)
	for _, rec := range publishedRecords(t, root) {
		if rec.Subject.Level != "external" || (rec.Verdict != "passed" && rec.Verdict != "failed") {
			continue
		}
		require.NotNil(t, rec.Authorization, rec.RecordID)
		rep, err := reportForRecord(root, rec)
		require.NoError(t, err, rec.RecordID)
		require.NotNil(t, rep.Authorization, "%s: 报告必须记录运行期授权", rec.RecordID)
		require.Equal(t, *rep.Authorization, *rec.Authorization, "%s: 授权必须逐项相等", rec.RecordID)
	}

	newFixture := func(t *testing.T) (string, record) {
		t.Helper()
		return writeManualBinding(t, manualFixture{
			Level: "external",
			Item:  "us2-6-3a",
			Check: "015-real-model",
			Test:  "TestBackfill_Real/provider_call",
			Auth:  auth,
		})
	}
	root, base := newFixture(t)
	require.NoError(t, validateRecord(root, base))

	t.Run("missing_authorization_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Authorization = nil
		require.ErrorContains(t, validateRecord(root, rec),
			"external records require authorization")
	})

	t.Run("scope_mismatch_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rec.Authorization = &authorization{Ref: auth.Ref, Scope: "narrower"}
		require.ErrorContains(t, validateRecord(root, rec),
			"record authorization differs from its report")
	})

	t.Run("report_without_authorization_is_red", func(t *testing.T) {
		root, base := newFixture(t)
		rec := cloneRecord(t, base)
		rewriteReport(t, root, &rec, func(rep *acceptanceReport) {
			rep.Authorization = nil
		})
		require.ErrorContains(t, validateRecord(root, rec),
			"record authorization differs from its report")
	})
}

// statusRecord 构造一条只用于状态视图计算的人工项记录。
// statusRecord builds a manual-item record used only for status-view tests.
func statusRecord(id, fingerprint, osName, verdict, finishedAt, executor, runKind string) record {
	return record{
		Version:     1,
		RecordID:    id,
		RunID:       "run-" + id,
		RunKind:     runKind,
		Executor:    executor,
		StartedAt:   finishedAt,
		FinishedAt:  finishedAt,
		Subject:     recordSubject{Kind: "manual_item", Level: "process", Items: []string{"us1-6-8a"}},
		Code:        recordCode{SourceFingerprint: fingerprint},
		Environment: environment{OS: osName, Arch: "amd64"},
		Verdict:     verdict,
	}
}

// TestRecords_StatusDoesNotMaskConflicts 覆盖四类遮蔽反例：跨平台、用户复跑、
// 同键冲突与未验证顶替；状态视图必须并列保留并显式置 Conflict。
// TestRecords_StatusDoesNotMaskConflicts covers the four masking counterexamples;
// the status view must keep every key and mark conflicts explicitly.
func TestRecords_StatusDoesNotMaskConflicts(t *testing.T) {
	catalog := []catalogItem{{Item: "us1-6-8a", Check: "015-backfill", Test: "TestBackfill_Bootstrap/assembly", Level: "process"}}
	linuxKey := statusKey{Item: "us1-6-8a", Fingerprint: "fp-1", OS: "linux"}
	windowsKey := statusKey{Item: "us1-6-8a", Fingerprint: "fp-1", OS: "windows"}

	t.Run("cross_platform_pass_does_not_hide_failure", func(t *testing.T) {
		win := statusRecord("win-1", "fp-1", "windows", "failed", "2026-10-19T10:00:00Z", "ai", "backfill")
		linux := statusRecord("linux-1", "fp-1", "linux", "passed", "2026-10-20T10:00:00Z", "ai", "backfill")
		status := computeRecordStatus([]record{win, linux}, catalog)
		require.Len(t, status, 2, "不同平台的结论并列保留,不得按最新时间淘汰")
		require.Equal(t, "failed", status[windowsKey].Verdict, "较新的 Linux PASS 不得盖住 Windows 失败")
		require.Equal(t, "passed", status[linuxKey].Verdict)
		require.True(t, status[windowsKey].Conflict)
		require.True(t, status[linuxKey].Conflict)
	})

	t.Run("user_rerun_enters_status", func(t *testing.T) {
		backfill := statusRecord("bf-1", "fp-1", "linux", "passed", "2026-10-19T10:00:00Z", "ai", "backfill")
		user := statusRecord("user-1", "fp-1", "linux", "failed", "2026-10-20T10:00:00Z", "user", "rerun")
		entry := computeRecordStatus([]record{backfill, user}, catalog)[linuxKey]
		require.Equal(t, "failed", entry.Verdict, "用户复跑进入同一状态视图,不得被 run_kind=backfill 过滤")
		require.True(t, entry.Conflict)
		require.ElementsMatch(t, []string{"user-1", "bf-1"}, entry.RecordIDs)
	})

	t.Run("same_key_conflict_keeps_both", func(t *testing.T) {
		first := statusRecord("a", "fp-1", "linux", "passed", "2026-10-19T10:00:00Z", "ai", "backfill")
		second := statusRecord("b", "fp-1", "linux", "failed", "2026-10-19T11:00:00Z", "ai", "backfill")
		entry := computeRecordStatus([]record{first, second}, catalog)[linuxKey]
		require.True(t, entry.Conflict, "同键相反结论不得静默取最新")
		require.ElementsMatch(t, []string{"a", "b"}, entry.RecordIDs)
		require.Equal(t, "failed", entry.Verdict)
	})

	t.Run("newer_not_verified_does_not_replace_passed", func(t *testing.T) {
		passed := statusRecord("old-pass", "fp-1", "linux", "passed", "2026-10-19T10:00:00Z", "ai", "backfill")
		unverified := statusRecord("new-unverified", "fp-1", "linux", "not_verified", "2026-10-20T10:00:00Z", "ai", "backfill")
		entry := computeRecordStatus([]record{passed, unverified}, catalog)[linuxKey]
		require.Equal(t, "not_verified", entry.Verdict)
		require.True(t, entry.Conflict, "用未验证顶替历史通过必须显式冲突")
		require.ElementsMatch(t, []string{"old-pass", "new-unverified"}, entry.RecordIDs)
	})
}

// TestRecords_PublishedConform 对发布面的每条记录执行完整规范校验；通过 / 失败
// 记录（人工项与运行记录同一口径）的报告必须成对发布在
// records/reports/<record_id>.json，且文件哈希与 report_sha256 相等；人工项的
// evidence.report_path 必须直接指向该副本。
// TestRecords_PublishedConform validates every published record and requires
// every passed/failed report (manual items and run records alike) to be paired
// under records/reports/<record_id>.json with a matching hash; manual items
// must point evidence.report_path at that copy.
func TestRecords_PublishedConform(t *testing.T) {
	root := stageTestRoot(t)
	records := publishedRecords(t, root)
	for _, rec := range records {
		require.NoError(t, validateRecord(root, rec), rec.RecordID)
		if rec.Verdict != "passed" && rec.Verdict != "failed" {
			continue
		}
		want := publishedReportPath(rec.RecordID)
		if rec.Subject.Kind == "manual_item" {
			require.Equal(t, want, rec.Evidence.ReportPath, "%s: 人工项报告必须指向已发布副本", rec.RecordID)
		}
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(want)))
		require.NoError(t, err, "%s: 已发布报告缺失", rec.RecordID)
		hash, err := fileSHA256(root, want)
		require.NoError(t, err, rec.RecordID)
		require.Equal(t, rec.Evidence.ReportSHA256, hash, "%s: 已发布报告与 report_sha256 不符", rec.RecordID)
	}

	root2, base := validRunRecord(t)
	broken := cloneRecord(t, base)
	broken.Executor = ""
	require.ErrorContains(t, validateRecord(root2, broken), "unknown executor")
}

// absoluteWindowsPath 捕获盘符形态的绝对路径。盘符前必须是行首或非字母数字字符：
// URL scheme（`https://` 中的 `s:/`）与仓库测试标识里的示例地址不是绝对路径，
// 发布面与 validateRecord 的 URL 形态扫描同口径。
// absoluteWindowsPath catches drive-letter absolute paths. A real drive letter
// follows a non-alphanumeric boundary; URL schemes such as `s:/` in `https://`
// and example URLs inside repository test identifiers are not absolute paths.
var absoluteWindowsPath = regexp.MustCompile(`(?i)(^|[^a-z0-9])[a-z]:[\\/]`)

// scanEvidence 对记录与已发布报告的原始字节做凭证与绝对路径扫描。
// scanEvidence scans raw record and report bytes for credentials and absolute paths.
func scanEvidence(raw []byte) error {
	text := string(raw)
	for _, pattern := range secretPatterns {
		if pattern.MatchString(text) {
			return errors.New("evidence contains a credential pattern")
		}
	}
	if absoluteWindowsPath.MatchString(text) {
		return errors.New("evidence contains an absolute windows path")
	}
	return nil
}

// TestRecords_NoSecretsOrAbsolutePaths 同扫发布面的记录与报告：零凭证模式、
// 零盘符路径，且报告内日志路径可投影回仓库。
// TestRecords_NoSecretsOrAbsolutePaths scans published records and reports for
// credential patterns and absolute paths.
func TestRecords_NoSecretsOrAbsolutePaths(t *testing.T) {
	root := stageTestRoot(t)
	recordsDir := filepath.Join(root, "harness", "acceptance", "records")
	var files, reportFiles []string
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, filepath.Join(recordsDir, entry.Name()))
		}
	}
	reportsDir := filepath.Join(recordsDir, "reports")
	if reports, err := os.ReadDir(reportsDir); err == nil {
		for _, entry := range reports {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				reportFiles = append(reportFiles, filepath.Join(reportsDir, entry.Name()))
			}
		}
	}
	files = append(files, reportFiles...)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, scanEvidence(raw), filepath.Base(path))
	}
	for _, path := range reportFiles {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var rep acceptanceReport
		require.NoError(t, json.Unmarshal(raw, &rep), filepath.Base(path))
		for _, c := range rep.Checks {
			if c.Log == "" {
				continue
			}
			require.False(t, badPath(c.Log), "%s: 报告日志路径必须仓库相对: %s", filepath.Base(path), c.Log)
		}
	}

	require.ErrorContains(t, scanEvidence([]byte("token sk-"+strings.Repeat("a", 16))), "credential pattern")
	require.ErrorContains(t, scanEvidence([]byte(`{"notes":"D:/logs/run.log"}`)), "absolute windows path")
	require.ErrorContains(t, scanEvidence([]byte(`D:\logs\run.log`)), "absolute windows path")
	require.NoError(t, scanEvidence([]byte(`{"log":"harness/acceptance/records/reports/run-1.json"}`)))
	// URL scheme 与测试标识里的示例地址不是盘符路径；发布面必须能承载它们。
	// URL schemes and example URLs in test identifiers are not drive paths.
	require.NoError(t, scanEvidence([]byte(`{"test":"TestHttpGet_PortFrozenCombinations/放行_http://api.example.com/a"}`)),
		"测试标识中的 URL scheme 不是盘符路径")
	require.NoError(t, scanEvidence([]byte(`{"input":"https://api.example.com:8443/x"}`)),
		"https:// 的 s:/ 不是盘符路径")
}

// ledgerRow 是台账固定列序的一行。
// ledgerRow is one row of the fixed ledger column order.
type ledgerRow struct {
	RecordID string
	Date     string
	Scope    string
	RunKind  string
	Executor string
	Verdict  string
	Link     string
}

// parseLedger 按固定列序解析台账；列数不是 7 即报错。
// parseLedger parses the ledger's fixed column order and rejects other arities.
func parseLedger(raw string) ([]ledgerRow, error) {
	var rows []ledgerRow
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 7 {
			return nil, fmt.Errorf("台账列数应为 7，实际 %d：%q", len(cells), line)
		}
		if strings.EqualFold(cells[0], "record_id") {
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		rows = append(rows, ledgerRow{
			RecordID: cells[0],
			Date:     cells[1],
			Scope:    cells[2],
			RunKind:  cells[3],
			Executor: cells[4],
			Verdict:  cells[5],
			Link:     cells[6],
		})
	}
	return rows, nil
}

// ledgerLink 取出 markdown 链接目标；纯文本路径原样返回。
// ledgerLink extracts a markdown link target or returns a plain path unchanged.
func ledgerLink(cell string) string {
	if i := strings.Index(cell, "]("); i >= 0 {
		rest := cell[i+2:]
		if j := strings.Index(rest, ")"); j >= 0 {
			return rest[:j]
		}
	}
	return cell
}

// checkLedger 要求台账行与发布记录一一对应、链接存在且字段一致。
// checkLedger requires a one-to-one mapping between ledger rows and records.
func checkLedger(root string, rows []ledgerRow, records []record) error {
	byID := make(map[string]record, len(records))
	for _, rec := range records {
		byID[rec.RecordID] = rec
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.RecordID] {
			return fmt.Errorf("台账重复登记 %s", row.RecordID)
		}
		seen[row.RecordID] = true
		rec, ok := byID[row.RecordID]
		if !ok {
			return fmt.Errorf("台账行 %s 没有对应记录", row.RecordID)
		}
		want := "records/" + row.RecordID + ".json"
		got := ledgerLink(row.Link)
		if got != want {
			return fmt.Errorf("台账链接 %q 应为 %q", got, want)
		}
		if _, err := os.Stat(filepath.Join(root, "harness", "acceptance", filepath.FromSlash(got))); err != nil {
			return fmt.Errorf("台账链接 %s 指向的文件不存在: %w", got, err)
		}
		if row.RunKind != rec.RunKind || row.Executor != rec.Executor || row.Verdict != rec.Verdict {
			return fmt.Errorf("台账行 %s 与记录字段不一致", row.RecordID)
		}
		if len(rec.FinishedAt) < 10 || row.Date != rec.FinishedAt[:10] {
			return fmt.Errorf("台账日期 %q 与记录 finished_at %q 不一致", row.Date, rec.FinishedAt)
		}
		if strings.TrimSpace(row.Scope) == "" {
			return fmt.Errorf("台账行 %s 的 scope 为空", row.RecordID)
		}
	}
	for id := range byID {
		if !seen[id] {
			return fmt.Errorf("记录 %s 缺少台账行", id)
		}
	}
	return nil
}

// ledgerFixture 在临时根目录生成记录文件与对应台账行，供反例使用。
// ledgerFixture materialises record files and matching rows under a temp root.
func ledgerFixture(t *testing.T, count int) (string, []ledgerRow, []record) {
	t.Helper()
	root := recordFixtureRoot(t)
	dir := filepath.Join(root, "harness", "acceptance", "records")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	var rows []ledgerRow
	var records []record
	for i := range count {
		id := fmt.Sprintf("run-%d", i)
		rec := record{
			Version:    1,
			RecordID:   id,
			RunID:      id,
			RunKind:    "original",
			Executor:   "ai",
			StartedAt:  "2026-10-20T10:00:00Z",
			FinishedAt: "2026-10-20T10:00:00Z",
			Subject:    recordSubject{Kind: "run", Level: "source"},
			Verdict:    "passed",
		}
		require.NoError(t, atomicJSON(filepath.Join(dir, id+".json"), &rec))
		records = append(records, rec)
		rows = append(rows, ledgerRow{
			RecordID: id,
			Date:     "2026-10-20",
			Scope:    "us15",
			RunKind:  rec.RunKind,
			Executor: rec.Executor,
			Verdict:  rec.Verdict,
			Link:     "[records/" + id + ".json](records/" + id + ".json)",
		})
	}
	return root, rows, records
}

// TestLedger_IndexesAllRecords 解析发布台账并要求与记录一一对应；缺行、悬空
// 链接与链接错指都会判红。
// TestLedger_IndexesAllRecords parses the published ledger and requires a
// one-to-one mapping; missing rows, dangling links and wrong links fail.
func TestLedger_IndexesAllRecords(t *testing.T) {
	root := stageTestRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "harness", "acceptance", "ledger.md"))
	require.NoError(t, err)
	rows, err := parseLedger(string(raw))
	require.NoError(t, err)
	require.NoError(t, checkLedger(root, rows, publishedRecords(t, root)))

	fixtureRoot, fixtureRows, fixtureRecords := ledgerFixture(t, 2)
	require.NoError(t, checkLedger(fixtureRoot, fixtureRows, fixtureRecords))

	t.Run("missing_row_is_red", func(t *testing.T) {
		require.ErrorContains(t, checkLedger(fixtureRoot, fixtureRows[:1], fixtureRecords), "缺少台账行")
	})

	t.Run("dangling_link_is_red", func(t *testing.T) {
		broken := slices.Clone(fixtureRows)
		broken[0].RecordID = "missing"
		broken[0].Link = "[records/missing.json](records/missing.json)"
		require.ErrorContains(t, checkLedger(fixtureRoot, broken, fixtureRecords), "没有对应记录")
	})

	t.Run("wrong_link_is_red", func(t *testing.T) {
		broken := slices.Clone(fixtureRows)
		broken[0].Link = "[records/run-1.json](records/run-1.json)"
		require.ErrorContains(t, checkLedger(fixtureRoot, broken, fixtureRecords), "应为")
	})

	t.Run("field_mismatch_is_red", func(t *testing.T) {
		broken := slices.Clone(fixtureRows)
		broken[1].Verdict = "failed"
		require.ErrorContains(t, checkLedger(fixtureRoot, broken, fixtureRecords), "字段不一致")
	})
}
