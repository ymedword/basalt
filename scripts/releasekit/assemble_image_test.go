package releasekit

// 本文件把「镜像记录 → assemble → 发布记录 / 矩阵 / 校验和 → verify」串成一条链。
// 四种默认目标都要真归档，这里用一个最小的假仓库跑真 go build：归档静态校验会读
// 二进制里的 buildinfo 核对 GOOS / GOARCH / CGO_ENABLED，手写文件骗不过去。
// This file drives the chain "image record -> assemble -> record / matrix / checksums ->
// verify". All four default targets need real archives, so a minimal fake repository is
// built with the real go toolchain: the static archive check reads the buildinfo out of
// the binary and compares GOOS / GOARCH / CGO_ENABLED, which hand-written files cannot fake.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// assembleImageVersion 是夹具版本号；镜像记录的 tag 与归档名都由它派生。
// assembleImageVersion is the fixture version; the image tag and archive names derive
// from it.
const assembleImageVersion = "1.2.3"

// writeFakeRepo 造一个可构建的最小仓库与配套 runtime 目录：full 档出包要求
// 清单声明的组件在 runtime 目录里齐全，binary 档不复制它。
// writeFakeRepo builds a minimal buildable repository plus its runtime directory: a
// full-tier package requires every declared component to exist under the runtime
// directory, while a binary-tier package never copies it.
func writeFakeRepo(t *testing.T) (repo, runtimeDir string) {
	t.Helper()
	repo = t.TempDir()
	runtimeDir = t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/basalt-fake\n\ngo 1.27\n",
		"LICENSE": "Apache License\n",
		filepath.Join("cmd", "basalt", "main.go"): "package main\n\nimport \"fmt\"\n\nvar version = \"dev\"\n\n" +
			"func main() { fmt.Println(\"basalt\", version) }\n",
		filepath.Join("cmd", "basalt-verify", "main.go"): "package main\n\nimport \"fmt\"\n\nvar version = \"dev\"\n\n" +
			"func main() { fmt.Println(\"basalt-verify\", version) }\n",
		filepath.Join("release", "runtime-manifest.json"): `{"schema_version":1,"components":[` +
			`{"id":"runtime-smoke","paths":{"windows":"bin/fake.exe","linux":"bin/fake"}}]}` + "\n",
	}
	for name, body := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"fake.exe", "fake"} {
		if err := os.MkdirAll(filepath.Join(runtimeDir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, "bin", name), []byte("runtime"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return repo, runtimeDir
}

// buildFakeArchives 为四个默认目标各出一个归档。BuildTarget 不允许在非匹配宿主上
// 构建 full 档，而夹具里的二进制本来就是对应平台的产物——这里先按 binary 档构建拿到
// 真实二进制，再把档位标回目标，让 full 行带上 runtime/ 与 full 的 MANIFEST。
// buildFakeArchives writes one archive per default target. BuildTarget refuses to build
// a full tier off-host, while the fixture binary is already the right platform: build it
// as binary tier, then re-label the tier so the full row carries runtime/ and a full
// MANIFEST. The buildinfo check still runs per platform inside PackageTarget's caller.
func buildFakeArchives(t *testing.T, repo, runtimeDir string) string {
	t.Helper()
	ctx := context.Background()
	artifactDir := t.TempDir()
	in := Inputs{Version: assembleImageVersion, Tag: "v" + assembleImageVersion, Mode: ModeRehearsal, Repo: repo}
	for _, target := range DefaultTargets() {
		binaryTarget := Target{GOOS: target.GOOS, GOARCH: target.GOARCH, Tier: TierBinary}
		build, err := BuildTarget(ctx, in, binaryTarget, t.TempDir())
		if err != nil {
			t.Fatalf("构建 %s：%v", target.String(), err)
		}
		build.Target = target
		if _, err := PackageTarget(ctx, in, target, build, ReleaseOptions{
			Repo: repo, Version: in.Version, Target: target,
			OutDir: artifactDir, RuntimeDir: runtimeDir,
		}); err != nil {
			t.Fatalf("打包 %s：%v", target.String(), err)
		}
	}
	return artifactDir
}

// writeImageFixture 造一条自洽的镜像记录与它的 SBOM / provenance。
// writeImageFixture writes a self-consistent image record plus its SBOM and provenance.
func writeImageFixture(t *testing.T) string {
	t.Helper()
	// 有检查 ID 的镜像行才是 tested；这里的检查就是本用例本身。
	return writeImageFixtureWithEvidence(t, []string{"TestUS18_Image_AssembleBindsRecord"})
}

// writeImageFixtureWithEvidence 造一条带指定验收证据（可为空）的镜像记录。
// writeImageFixtureWithEvidence writes a record carrying the given evidence (may be
// empty).
func writeImageFixtureWithEvidence(t *testing.T, evidence []string) string {
	t.Helper()
	dir := t.TempDir()
	sbom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5"}` + "\n")
	provenance := []byte(`{"schema":1}` + "\n")
	record := validImageRecord()
	if err := os.WriteFile(filepath.Join(dir, record.SBOMPath), sbom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.ProvenancePath), provenance, 0o644); err != nil {
		t.Fatal(err)
	}
	record.SBOMSHA256 = sha256Hex(sbom)
	record.ProvenanceSHA256 = sha256Hex(provenance)
	record.Evidence = evidence
	return writeImageRecordFile(t, dir, record)
}

// writeNotes 造满足 CheckNotes 的发布说明。
// writeNotes writes release notes that satisfy CheckNotes.
func writeNotes(t *testing.T, version string) string {
	t.Helper()
	body := "# " + version + "\n\n## 摘要\n\n## 变更\n\n## 升级步骤\n\n## 已知问题\n\n## 校验与签名\n\n## 回滚\n"
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// assembleWithImages 用给定镜像记录跑一次完整汇总，返回发布目录与错误。
// assembleWithImages runs a full assembly with the given image records and returns the
// release directory together with the error.
func assembleWithImages(t *testing.T, images []string) (string, error) {
	t.Helper()
	repo, runtimeDir := writeFakeRepo(t)
	artifactDir := buildFakeArchives(t, repo, runtimeDir)
	outDir := filepath.Join(t.TempDir(), "dist")
	err := AssembleRelease(context.Background(), AssembleOptions{
		ArtifactDir: artifactDir,
		EvidenceDir: t.TempDir(),
		Images:      images,
		OutDir:      outDir,
		NotesPath:   writeNotes(t, assembleImageVersion),
		Inputs: Inputs{
			Version: assembleImageVersion, Tag: "v" + assembleImageVersion,
			Mode: ModeRehearsal, Repo: repo,
			StartedAt: time.Now().UTC().Add(-time.Minute),
		},
	})
	return outDir, err
}

// 中文验收点（A1 / A5）：镜像记录经 assemble 进入发布记录、支持矩阵与校验和，
// 且与归档按 (goos, goarch, kind) 分行共存；删掉 images[] 但保留矩阵行时 verify
// 仍必须失败——否则"记录丢了但归档还在"会被读成一致。
// Acceptance (A1 / A5): the image record flows through assemble into the release
// record, support matrix and checksums, coexisting with the archive row per
// (goos, goarch, kind); dropping images[] while keeping the matrix row must still
// fail verification, otherwise "record lost, row kept" would read as consistent.
func TestUS18_Image_AssembleBindsRecord(t *testing.T) {
	recordPath := writeImageFixture(t)
	outDir, err := assembleWithImages(t, []string{recordPath})
	if err != nil {
		t.Fatalf("带镜像记录的汇总必须成功：%v", err)
	}

	record, err := readRecord(filepath.Join(outDir, releaseRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Images) != 1 || record.Images[0].Tag != "basalt:1.2.3-linux-amd64" {
		t.Fatalf("发布记录必须带上镜像记录：%+v", record.Images)
	}
	var imageRow, archiveRow *SupportRow
	for index, row := range record.Evidence {
		switch {
		case row.Kind == kindImage && row.GOOS == "linux" && row.GOARCH == "amd64":
			imageRow = &record.Evidence[index]
		case row.Kind == "" && row.GOOS == "linux" && row.GOARCH == "amd64":
			archiveRow = &record.Evidence[index]
		}
	}
	if imageRow == nil || imageRow.Status != StatusTested || len(imageRow.Checks) == 0 {
		t.Fatalf("矩阵必须含 tested 的镜像行：%+v", imageRow)
	}
	if archiveRow == nil {
		t.Fatal("同平台的归档行必须与镜像行并存")
	}

	sums, err := os.ReadFile(filepath.Join(outDir, checksumsName))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{imageSBOMName, imageProvenanceName, releaseRecordName} {
		if !strings.Contains(string(sums), name) {
			t.Fatalf("%s 必须进入校验和：%s", name, sums)
		}
	}

	// 篡改镜像 SBOM：发布目录内文件的哈希必须被校验和与记录双重拦住。
	if err := os.WriteFile(filepath.Join(outDir, imageSBOMName), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(context.Background(), outDir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("篡改镜像 SBOM 必须失败，got %v", err)
	}
}

// 中文验收点：发布记录里的 images[] 被删而矩阵镜像行还在时，verify 不得放行；
// 反过来矩阵行被删、记录还在同样不行——两处必须一一对应。
// Acceptance: with images[] removed and the matrix image row kept, verification must
// not pass; the reverse (row dropped, record kept) must fail as well — the two sides
// are matched one to one.
func TestUS18_Image_AssembleDetectsOrphan(t *testing.T) {
	recordPath := writeImageFixture(t)
	outDir, err := assembleWithImages(t, []string{recordPath})
	if err != nil {
		t.Fatalf("带镜像记录的汇总必须成功：%v", err)
	}
	recordPathOut := filepath.Join(outDir, releaseRecordName)
	original, err := readRecord(recordPathOut)
	if err != nil {
		t.Fatal(err)
	}

	// 记录侧丢失：删 images[]，保留矩阵行，刷新校验和。
	record := original
	record.Images = nil
	if err := writeJSON(recordPathOut, record); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(outDir); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(context.Background(), outDir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "支持矩阵有") {
		t.Fatalf("矩阵孤儿镜像行必须失败，got %v", err)
	}

	// 矩阵侧丢失：恢复记录、删矩阵镜像行，同样刷新校验和。
	if err := writeJSON(recordPathOut, original); err != nil {
		t.Fatal(err)
	}
	matrixPath := filepath.Join(outDir, "support-matrix.json")
	matrix, err := readMatrix(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	kept := matrix.Rows[:0]
	for _, row := range matrix.Rows {
		if row.Kind != kindImage {
			kept = append(kept, row)
		}
	}
	matrix.Rows = kept
	if err := writeJSON(matrixPath, matrix); err != nil {
		t.Fatal(err)
	}
	// 记录的矩阵哈希指向新文件，否则只会红在哈希上，测不到镜像行核对。
	if err := rewriteMatrixHash(recordPathOut, matrixPath); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(outDir); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(context.Background(), outDir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "kind=image") {
		t.Fatalf("记录孤儿镜像必须失败，got %v", err)
	}
}

// readImageRecord 读回镜像记录（不做文件哈希核对，供夹具改写用）。
// readImageRecord reads an image record back without file-hash checks so the fixture
// can be mutated.
func readImageRecord(path string) (ImageRecord, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 测试夹具
	if err != nil {
		return ImageRecord{}, err
	}
	var record ImageRecord
	err = json.Unmarshal(body, &record)
	return record, err
}

// readMatrix 读回支持矩阵。
// readMatrix reads the support matrix back.
func readMatrix(path string) (SupportMatrix, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 测试夹具
	if err != nil {
		return SupportMatrix{}, err
	}
	var matrix SupportMatrix
	err = json.Unmarshal(body, &matrix)
	return matrix, err
}

// rewriteMatrixHash 把记录里的矩阵哈希重算到当前文件，用于隔离镜像行核对。
// rewriteMatrixHash retargets the record's matrix hash at the current file so the
// image-row check is what fails.
func rewriteMatrixHash(recordPath, matrixPath string) error {
	record, err := readRecord(recordPath)
	if err != nil {
		return err
	}
	sum, err := FileSHA256(matrixPath)
	if err != nil {
		return err
	}
	record.SupportMatrixSHA256 = sum
	return writeJSON(recordPath, record)
}

// 中文验收点（A1 / A5 证据口径）：记录本身不带 evidence、由 `assemble --image-evidence`
// 显式给出的检查 ID 才能让矩阵镜像行成为 tested；记录已带证据时不接受被静默替换，
// 两条来源不一致即失败。夹具直接走 loadAndCopyImages，避免为每条断言重建四平台归档。
// Acceptance (A1 / A5 evidence policy): a record without evidence only becomes a tested
// matrix row through the explicit --image-evidence IDs, and evidence already present in
// the record is never silently replaced — a mismatch fails. The fixture drives
// loadAndCopyImages directly so each assertion does not rebuild all four archives.
func TestUS18_Image_AssembleAppliesEvidence(t *testing.T) {
	evidence := []string{"TestUS18_Image_AssembleAppliesEvidence"}
	recordPath := writeImageFixtureWithEvidence(t, nil)
	outDir := t.TempDir()
	images, err := loadAndCopyImages(AssembleOptions{
		Images:        []string{recordPath},
		ImageEvidence: evidence,
		OutDir:        outDir,
	}, nil)
	if err != nil {
		t.Fatalf("无证据的记录 + --image-evidence 必须成功：%v", err)
	}
	if len(images) != 1 || !slices.Equal(images[0].Evidence, evidence) {
		t.Fatalf("记录必须带上显式给出的证据：%+v", images)
	}
	matrix, err := AppendImageRows(SupportMatrix{Schema: 1}, images)
	if err != nil {
		t.Fatal(err)
	}
	if len(matrix.Rows) != 1 || matrix.Rows[0].Status != StatusTested ||
		!slices.Equal(matrix.Rows[0].Checks, evidence) {
		t.Fatalf("证据必须形成 tested 镜像行：%+v", matrix.Rows)
	}
}

// 中文验收点：记录已带证据时不接受静默覆盖——与命令行给出的证据一致才放行，
// 不一致必须点名冲突；否则“测过什么”会随命令行漂移而记录不变。
// Acceptance: evidence already in the record is not silently overwritten — identical
// CLI evidence passes, differing evidence fails by name; otherwise "what was tested"
// could drift with the command line while the record stays the same.
func TestUS18_Image_AssembleRejectsConflictingEvidence(t *testing.T) {
	recordPath := writeImageFixture(t) // 已带 TestUS18_Image_AssembleBindsRecord
	outDir := t.TempDir()
	if _, err := loadAndCopyImages(AssembleOptions{
		Images:        []string{recordPath},
		ImageEvidence: []string{"TestUS18_Image_AssembleBindsRecord"},
		OutDir:        outDir,
	}, nil); err != nil {
		t.Fatalf("一致的证据必须放行：%v", err)
	}
	_, err := loadAndCopyImages(AssembleOptions{
		Images:        []string{recordPath},
		ImageEvidence: []string{"TestUS18_Image_AssembleRejectsConflictingEvidence"},
		OutDir:        t.TempDir(),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("证据不一致必须失败，got %v", err)
	}
}

// 中文验收点：记录与矩阵里的证据行 kind 只允许空（归档行）与 image（镜像行）；
// 未知取值会被归档核对与镜像核对同时跳过，必须显式拒绝，不能静默放行。
// Acceptance: evidence-row kinds are limited to "" (archive) and image; unknown values
// would be skipped by both the archive and image checks and must be rejected
// explicitly instead of passing silently.
func TestUS18_Image_RejectsUnknownEvidenceKind(t *testing.T) {
	matrix := SupportMatrix{Schema: 1, Rows: []SupportRow{
		{GOOS: "linux", GOARCH: "amd64", Tier: TierBinary, Kind: "docker", Status: StatusNotTested, Reason: "x"},
	}}
	if _, err := VerifyImages(t.TempDir(), ReleaseRecord{}, matrix, VerifyImageOptions{}); err == nil ||
		!strings.Contains(err.Error(), "kind") {
		t.Fatalf("支持矩阵的未知 kind 必须被拒绝，got %v", err)
	}

	dir := t.TempDir()
	matrixBody := []byte(`{"schema":1,"rows":[]}` + "\n")
	notesBody := []byte("# notes\n")
	if err := os.WriteFile(filepath.Join(dir, "support-matrix.json"), matrixBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), notesBody, 0o644); err != nil {
		t.Fatal(err)
	}
	record := ReleaseRecord{
		Limitations:         []string{"无"},
		SupportMatrixSHA256: sha256Hex(matrixBody),
		NotesSHA256:         sha256Hex(notesBody),
		Evidence: []SupportRow{
			{GOOS: "linux", GOARCH: "amd64", Tier: TierBinary, Kind: "docker", Status: StatusNotTested, Reason: "x"},
		},
	}
	if err := crossCheckRecord(dir, record); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("发布记录的未知 kind 必须被拒绝，got %v", err)
	}
}

// 中文验收点：镜像记录自身不自洽时汇总必须在写签名前失败，并把发布目录清空；
// 目录残留会让下一次汇总撞上"非空"，等于把失败变成需要人工清理的事故。
// Acceptance: an inconsistent image record fails assembly before anything is signed and
// leaves the release directory empty; residue would make the next attempt fail with
// "non-empty" and turn a bad record into a manual cleanup.
func TestUS18_Image_AssembleRejectsBadRecord(t *testing.T) {
	recordPath := writeImageFixture(t)
	// 把 SBOM 哈希改成不匹配的值：记录字段本身合法，文件核对才会失败。
	bad, err := readImageRecord(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	bad.SBOMSHA256 = hex64("0")
	if err := writeJSON(recordPath, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageRecords([]string{recordPath}); err == nil || !strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("SBOM 哈希不符必须被拒，got %v", err)
	}

	outDir, err := assembleWithImages(t, []string{recordPath})
	if err == nil {
		t.Fatal("镜像记录不自洽时汇总必须失败")
	}
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("发布目录应存在：%v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("失败后发布目录必须清空，实际残留 %d 个条目", len(entries))
	}
}
