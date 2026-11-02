package releasekit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fullTarget() Target {
	for _, target := range DefaultTargets() {
		if target.GOOS == "linux" && target.GOARCH == "amd64" {
			return target
		}
	}
	return DefaultTargets()[1]
}

func windowsFullTarget() Target {
	return DefaultTargets()[0]
}

// 中文验收点：双构建结论只在两份 full 宿主证据都 checked 且 equal 时才汇总为
// checked；缺证据不得冒充通过。
// Reproducibility is checked only when both full-tier host readings were compared
// and equal; missing evidence must not be counted as a pass.
func TestUS17_Evidence_WriteAndAggregate(t *testing.T) {
	in := Inputs{Version: "1.2.3", Mode: ModeRehearsal}
	write := func(t *testing.T, dir string, equal bool) {
		t.Helper()
		for _, target := range []Target{windowsFullTarget(), fullTarget()} {
			repro := &Reproducible{Checked: true, Equal: equal, SecondBuildSHA256: "second-" + target.String()}
			pkg := PackageResult{Target: target, SHA256: "sum-" + target.String()}
			if err := WriteEvidence(dir, in, pkg, repro); err != nil {
				t.Fatalf("写证据 %s：%v", target.String(), err)
			}
		}
	}

	dir := t.TempDir()
	write(t, dir, true)
	aggregate, err := AggregateReproducible(dir)
	if err != nil {
		t.Fatalf("汇总：%v", err)
	}
	if !aggregate.Checked || !aggregate.Equal {
		t.Fatalf("两份证据相等时应 checked+equal：%+v", aggregate)
	}
	if aggregate.SecondBuildSHA256 != "second-linux/amd64" {
		t.Fatalf("second_build_sha256 应取 linux/amd64，实际 %q", aggregate.SecondBuildSHA256)
	}

	if empty, err := AggregateReproducible(t.TempDir()); err != nil || empty.Checked {
		t.Fatalf("无证据应返回零值，实际 %+v, %v", empty, err)
	}

	unequalDir := t.TempDir()
	write(t, unequalDir, false)
	unequal, err := AggregateReproducible(unequalDir)
	if err != nil {
		t.Fatalf("汇总：%v", err)
	}
	if !unequal.Checked || unequal.Equal {
		t.Fatalf("不等时应 checked+not equal：%+v", unequal)
	}

	// 只写 windows 一份：缺 linux 证据不得判 checked。
	partialDir := t.TempDir()
	if err := WriteEvidence(partialDir, in, PackageResult{Target: windowsFullTarget(), SHA256: "w"},
		&Reproducible{Checked: true, Equal: true, SecondBuildSHA256: "w"}); err != nil {
		t.Fatalf("写证据：%v", err)
	}
	partial, err := AggregateReproducible(partialDir)
	if err != nil {
		t.Fatalf("汇总：%v", err)
	}
	if partial.Checked {
		t.Fatalf("缺一份证据不得判 checked：%+v", partial)
	}
}

func TestUS17_Manifest_NotesAndProvenance(t *testing.T) {
	commit := strings.Repeat("a", 40)
	in := Inputs{Version: "1.2.3", Tag: "v1.2.3", Mode: ModeRehearsal, Commit: commit}
	pkg := PackageResult{Name: "basalt-1.2.3-linux-amd64.tar.gz", Target: fullTarget(), SHA256: "abc"}
	manifest, err := BuildReleaseManifest(in, []PackageResult{pkg})
	if err != nil {
		t.Fatalf("生成清单：%v", err)
	}
	if manifest.Schema != 1 || manifest.Version != "1.2.3" || manifest.Commit != commit {
		t.Fatalf("清单字段不符：%+v", manifest)
	}
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].SHA256 != "abc" {
		t.Fatalf("清单制品不符：%+v", manifest.Artifacts)
	}

	dir := t.TempDir()
	if err := BuildProvenance(in, []PackageResult{pkg}, dir); err != nil {
		t.Fatalf("生成 provenance：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "basalt-1.2.3-linux-amd64.provenance.json")); err != nil {
		t.Fatalf("provenance 文件不存在：%v", err)
	}

	notes := filepath.Join(t.TempDir(), "notes.md")
	body := "# Basalt 1.2.3\n\n摘要\n\n变更\n\n升级步骤\n\n已知问题\n\n校验与签名\n\n回滚\n"
	if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
		t.Fatalf("写说明：%v", err)
	}
	if err := CheckNotes(notes, "1.2.3"); err != nil {
		t.Fatalf("完整说明应通过：%v", err)
	}
	if err := CheckNotes(notes, "2.0.0"); err == nil || !strings.Contains(err.Error(), "版本号") {
		t.Fatalf("版本号不符应报错，实际 %v", err)
	}
	missing := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(missing, []byte("摘要 变更 升级步骤 已知问题 校验与签名 1.2.3"), 0o644); err != nil {
		t.Fatalf("写说明：%v", err)
	}
	if err := CheckNotes(missing, "1.2.3"); err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("缺章节应报错，实际 %v", err)
	}
	if err := CheckNotes(filepath.Join(t.TempDir(), "missing.md"), "1.2.3"); err == nil {
		t.Fatal("不存在的说明应报错")
	}
}

func TestUS17_Checksum_VerifyFileHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	if err := WriteChecksums(dir); err != nil {
		t.Fatalf("写校验和：%v", err)
	}
	if err := VerifyFileHash(dir, "a.txt"); err != nil {
		t.Fatalf("未篡改应通过：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatalf("篡改：%v", err)
	}
	if err := VerifyFileHash(dir, "a.txt"); err == nil || !strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("篡改应报哈希不符，实际 %v", err)
	}
	if err := VerifyFileHash(dir, "missing.txt"); err == nil || !strings.Contains(err.Error(), "不在") {
		t.Fatalf("未列文件应报错，实际 %v", err)
	}
}

func TestUS17_Sign_Fingerprint(t *testing.T) {
	dir := t.TempDir()
	privateKey := filepath.Join(dir, "release.key")
	publicKey := filepath.Join(dir, "release.pub")
	if err := GenerateKeyPair(privateKey, publicKey); err != nil {
		t.Fatalf("生成密钥：%v", err)
	}
	fingerprint, err := Fingerprint(publicKey)
	if err != nil {
		t.Fatalf("计算指纹：%v", err)
	}
	if len(fingerprint) != 64 {
		t.Fatalf("指纹长度 %d，期望 64", len(fingerprint))
	}
	if _, err := Fingerprint(filepath.Join(dir, "missing.pub")); err == nil {
		t.Fatal("不存在的公钥应报错")
	}
}

func TestUS17_Record_ReadReleaseRecord(t *testing.T) {
	dir := t.TempDir()
	body := `{"schema":1,"version":"1.2.3","record_id":"1.2.3-final-0123456789ab-20260101T000000Z"}`
	if err := os.WriteFile(filepath.Join(dir, "release-record.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("写记录：%v", err)
	}
	record, err := ReadReleaseRecord(dir)
	if err != nil {
		t.Fatalf("读记录：%v", err)
	}
	if record.Version != "1.2.3" || record.Schema != 1 {
		t.Fatalf("记录字段不符：%+v", record)
	}
	if err := os.WriteFile(filepath.Join(dir, "release-record.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("写坏记录：%v", err)
	}
	if _, err := ReadReleaseRecord(dir); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := ReadReleaseRecord(t.TempDir()); err == nil {
		t.Fatal("缺记录应报错")
	}
}

func TestUS17_Release_PrepareAndCleanup(t *testing.T) {
	out := filepath.Join(t.TempDir(), "release")
	if err := prepareReleaseDir(out); err != nil {
		t.Fatalf("空目录应可准备：%v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写陈留文件：%v", err)
	}
	if err := prepareReleaseDir(out); err == nil || !strings.Contains(err.Error(), "非空") {
		t.Fatalf("非空目录应拒绝，实际 %v", err)
	}
	removeDirEntries(out)
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 0 {
		t.Fatalf("清理后应为空目录：%d, %v", len(entries), err)
	}
	body, err := MarshalIndentJSON(map[string]string{"a": "b"})
	if err != nil || !strings.Contains(string(body), "\n") {
		t.Fatalf("稳定 JSON 应以换行结尾：%q, %v", body, err)
	}
}
