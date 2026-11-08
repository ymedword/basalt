package releasekit

// 本文件覆盖 US-18 的镜像记录、构建上下文守卫与矩阵行派生；真实 docker 构建在
// internal/e2e 的 process 用例里执行，这里只判不需要 docker 的判据。
// This file covers US-18's image record, build-context guard and matrix row
// derivation. The real docker build lives in the internal/e2e process tests; only
// docker-free judgements belong here.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// hex64 造一个 64 位十六进制摘要；fill 必须是一个十六进制字符。
// hex64 builds a 64-char hex digest; fill must be a single hex digit.
func hex64(fill string) string { return strings.Repeat(fill, 64) }

// validImageRecord 是各用例的基准记录：全部字段自洽，改动一项即用来断言拒绝。
// validImageRecord is the baseline record for these tests: every field is
// self-consistent, so a single mutation is enough to assert a rejection.
func validImageRecord() ImageRecord {
	return ImageRecord{
		Name:             "basalt",
		Version:          "1.2.3",
		Platform:         "linux/amd64",
		Tier:             TierBinary,
		Tag:              "basalt:1.2.3-linux-amd64",
		LocalID:          "sha256:" + hex64("a"),
		BaseRef:          "alpine:3.23@sha256:" + hex64("b"),
		BaseDigest:       "sha256:" + hex64("b"),
		BinarySHA256:     hex64("c"),
		DockerfileSHA256: hex64("d"),
		EntrypointSHA256: hex64("e"),
		ConfigSHA256:     hex64("f"),
		SBOMPath:         imageSBOMName,
		SBOMSHA256:       hex64("1"),
		ProvenancePath:   imageProvenanceName,
		ProvenanceSHA256: hex64("2"),
		Scan:             ScanRecord{Status: scanNotRun, Reason: "测试未安装扫描器"},
	}
}

// 中文验收点（A2）：私钥按文件名或内容进入构建上下文都必须被拒，且正常五件套
// 不得误报——否则“全部拒绝”也能让前两层变绿。
// Acceptance (A2): a private key must be rejected by file name or by content, and
// the normal five-file context must pass, otherwise "reject everything" would make
// the first two layers green.
func TestUS18_Image_RejectsPrivateKeyInContext(t *testing.T) {
	// 第一层：文件名命中。暂存目录里放一把“私钥”，守卫必须点名拒绝。
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "release-private.pem"),
		[]byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageContext(stage); err == nil || !strings.Contains(err.Error(), "release-private.pem") {
		t.Fatalf("私钥文件名进入构建上下文时必须拒绝并点名文件，got %v", err)
	}

	// 第二层：内容命中。白名单文件名装私钥内容，同样必须在调用 docker 之前拒绝。
	content := t.TempDir()
	if err := os.WriteFile(filepath.Join(content, "LICENSE"),
		[]byte("-----BEGIN PRIVATE KEY-----\nMIIE...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageContext(content); err == nil || !strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("私钥内容进入构建上下文时必须拒绝，got %v", err)
	}

	// 第三层：正常五件套不得误报，否则上面的红态可能只是“什么都拒绝”。
	ok := t.TempDir()
	for _, name := range []string{"basalt", "Dockerfile", "entrypoint.sh", "basalt.yaml", "LICENSE"} {
		if err := os.WriteFile(filepath.Join(ok, name), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckImageContext(ok); err != nil {
		t.Fatalf("正常五件套不得被拒，got %v", err)
	}
}

// 中文验收点：密钥目录（secrets/、career/、release-keys）与空上下文同样被拒，
// 守卫不能只认扩展名。
// Secrets directories (secrets/, career/, release-keys) are rejected too, so the
// guard cannot rely on the file extension alone.
func TestUS18_Image_RejectsSecretDirectories(t *testing.T) {
	for _, name := range []string{"secrets", "career", "release-keys"} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "payload"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckImageContext(dir); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("目录 %s 进入构建上下文时必须拒绝并点名，got %v", name, err)
		}
	}
	if err := CheckImageContext(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("不存在的构建上下文必须报错")
	}
}

// 中文验收点（A1 的记录面）：合法记录通过，任一字段被改坏都必须拒绝——拒绝理由
// 要点名具体字段，不能只给一句“记录非法”。
// Acceptance (record half of A1): a valid record passes and every mutated field is
// rejected with the field named, not with a generic "invalid record".
func TestUS18_Image_RecordValidate(t *testing.T) {
	if err := validImageRecord().Validate(); err != nil {
		t.Fatalf("基准记录必须通过：%v", err)
	}
	cases := []struct {
		name   string
		mutate func(record *ImageRecord)
		want   string
	}{
		{"平台", func(r *ImageRecord) { r.Platform = "linux/arm64" }, "平台"},
		{"档位", func(r *ImageRecord) { r.Tier = TierFull }, "档位"},
		{"tag", func(r *ImageRecord) { r.Tag = "basalt:2.0.0-linux-amd64" }, "tag"},
		{"local_id", func(r *ImageRecord) { r.LocalID = "sha256:xyz" }, "local_id"},
		{"repo_digest", func(r *ImageRecord) { r.RepoDigest = "sha256:abc" }, "repo_digest"},
		{"base_digest", func(r *ImageRecord) { r.BaseDigest = "sha256:" + hex64("9") }, "base_digest"},
		{"binary_sha256", func(r *ImageRecord) { r.BinarySHA256 = "not-a-hash" }, "binary_sha256"},
		{"sbom_sha256", func(r *ImageRecord) { r.SBOMSHA256 = strings.Repeat("g", 64) }, "sbom_sha256"},
		{"sbom_path", func(r *ImageRecord) { r.SBOMPath = "sub/dir.json" }, "sbom_path"},
		{"scan 缺原因", func(r *ImageRecord) { r.Scan = ScanRecord{Status: scanNotRun} }, "原因"},
		{"scan 缺扫描器", func(r *ImageRecord) { r.Scan = ScanRecord{Status: scanPassed} }, "扫描器"},
		{"scan 状态", func(r *ImageRecord) { r.Scan = ScanRecord{Status: "unknown"} }, "scan"},
	}
	for _, testCase := range cases {
		record := validImageRecord()
		testCase.mutate(&record)
		err := record.Validate()
		if err == nil {
			t.Fatalf("%s 被改坏后必须拒绝", testCase.name)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s 的拒绝理由必须点名 %q，got %v", testCase.name, testCase.want, err)
		}
	}
}

// 中文验收点：记录旁的 SBOM / provenance 被篡改或缺失时，CheckImageRecordFiles 与
// LoadImageRecords 都必须失败，且重复平台的记录不得同时读入。
// A tampered or missing SBOM / provenance must fail both CheckImageRecordFiles and
// LoadImageRecords; two records on the same platform must not load together.
func TestUS18_Image_RecordFilesAndLoad(t *testing.T) {
	dir := t.TempDir()
	record := validImageRecord()
	sbom := []byte(`{"bomFormat":"CycloneDX"}` + "\n")
	provenance := []byte(`{"schema":1}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, record.SBOMPath), sbom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.ProvenancePath), provenance, 0o644); err != nil {
		t.Fatal(err)
	}
	record.SBOMSHA256 = sha256Hex(sbom)
	record.ProvenanceSHA256 = sha256Hex(provenance)
	if err := CheckImageRecordFiles(record, dir); err != nil {
		t.Fatalf("文件自洽时必须通过：%v", err)
	}
	loaded, err := LoadImageRecords([]string{writeImageRecordFile(t, dir, record)})
	if err != nil || len(loaded) != 1 || loaded[0].Tag != record.Tag {
		t.Fatalf("记录应可读入：%+v, %v", loaded, err)
	}

	if err := os.WriteFile(filepath.Join(dir, record.SBOMPath), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageRecordFiles(record, dir); err == nil || !strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("SBOM 篡改必须按哈希不符报错，got %v", err)
	}
	if _, err := LoadImageRecords([]string{writeImageRecordFile(t, dir, record)}); err == nil {
		t.Fatal("篡改后 LoadImageRecords 必须失败")
	}
	if err := os.WriteFile(filepath.Join(dir, record.SBOMPath), sbom, 0o644); err != nil {
		t.Fatal(err)
	}

	second := validImageRecord()
	second.Version = "1.2.4"
	second.Tag = "basalt:1.2.4-linux-amd64"
	secondPath := writeImageRecordFile(t, dir, second)
	firstPath := writeImageRecordFile(t, dir, record)
	if _, err := LoadImageRecords([]string{firstPath, secondPath}); err == nil ||
		!strings.Contains(err.Error(), "重复") {
		t.Fatalf("同平台两条记录必须被拒，got %v", err)
	}
}

// writeImageRecordFile 把记录写成 JSON 文件；文件名带计数避免同目录覆盖。
// writeImageRecordFile writes the record to JSON; the name carries a counter so a
// second record in the same directory does not overwrite the first.
func writeImageRecordFile(t *testing.T, dir string, record ImageRecord) string {
	t.Helper()
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// tag 里的冒号在 Windows 上不是合法文件名字符，写盘前换成连字符。
	path := filepath.Join(dir, strings.ReplaceAll(record.Tag, ":", "-")+".json")
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 中文验收点：镜像 SBOM 必须列出 apk 包（含 PURL），许可证未知时写 NOASSERTION
// 而不是猜；缺条目直接报错，不写半份清单。
// The image SBOM must list apk packages with PURLs, write NOASSERTION instead of
// guessing a license, and reject incomplete entries rather than emit half a list.
func TestUS18_Image_SBOMIncludesOSPackages(t *testing.T) {
	record := validImageRecord()
	body, err := BuildImageSBOM(record, []OSPackage{
		{Name: "musl", Version: "1.2.5-r0"},
		{Name: "ca-certificates", Version: "20250619-r0", Licenses: []string{"MPL-2.0"}},
	})
	if err != nil {
		t.Fatalf("生成镜像 SBOM：%v", err)
	}
	text := string(body)
	for _, want := range []string{
		`"bomFormat": "CycloneDX"`,
		"pkg:apk/musl@1.2.5-r0",
		"pkg:apk/ca-certificates@20250619-r0",
		`"MPL-2.0"`,
		`"NOASSERTION"`,
		`"name": "basalt"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("镜像 SBOM 缺少 %q：%s", want, text)
		}
	}
	if _, err := BuildImageSBOM(record, []OSPackage{{Name: "musl"}}); err == nil {
		t.Fatal("缺版本的 apk 包必须报错")
	}
	if _, err := BuildImageSBOM(ImageRecord{}, nil); err == nil {
		t.Fatal("缺 name / version 的记录必须报错")
	}
}

// 中文验收点：`apk info -v` 的行必须拆成包名与完整版本（含 -r 修订号）；包名里的
// 连字符与数字不能把拆分位置带偏，解析不出的行如实记为 NOASSERTION 并去重。
// The apk version lines split into name and full version (revision included); hyphens
// and digits inside the name must not shift the split point.
func TestUS18_Image_ParseAPKPackages(t *testing.T) {
	parsed := parseAPKPackages(strings.Join([]string{
		"musl-1.2.5-r21",
		"ca-certificates-bundle-20250619-r0",
		"libssl3-3.5.1-r0",
		"musl-1.2.5-r21",
		"weird-package",
		"",
	}, "\n"))

	want := []struct{ name, version string }{
		{"musl", "1.2.5-r21"},
		{"ca-certificates-bundle", "20250619-r0"},
		{"libssl3", "3.5.1-r0"},
		{"weird-package", "NOASSERTION"},
	}
	if len(parsed) != len(want) {
		t.Fatalf("解析出 %d 个包，期望 %d 个：%+v", len(parsed), len(want), parsed)
	}
	for index, entry := range want {
		if parsed[index].Name != entry.name || parsed[index].Version != entry.version {
			t.Fatalf("第 %d 条 = %q@%q，期望 %q@%q",
				index, parsed[index].Name, parsed[index].Version, entry.name, entry.version)
		}
		if len(parsed[index].Licenses) != 1 || parsed[index].Licenses[0] != "NOASSERTION" {
			t.Fatalf("第 %d 条的许可证 = %v，期望 [NOASSERTION]", index, parsed[index].Licenses)
		}
	}
}

// 中文验收点：provenance 必须拒绝零值与倒序时间戳，避免把未知的构建时长写成事实。
// Provenance must reject zero or reversed timestamps so an unknown build window is
// never written as fact.
func TestUS18_Image_ProvenanceRejectsBadTimestamps(t *testing.T) {
	record := validImageRecord()
	finished := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	in := ImageInputs{Repo: ".", Version: record.Version, BaseRef: record.BaseRef}
	body, err := BuildImageProvenance(in, record, finished.Add(-time.Minute), finished)
	if err != nil {
		t.Fatalf("正常构建窗口应可生成 provenance：%v", err)
	}
	if !strings.Contains(string(body), record.Tag) {
		t.Fatalf("provenance 必须记录 tag：%s", body)
	}
	if _, err := BuildImageProvenance(in, record, finished, finished.Add(-time.Minute)); err == nil {
		t.Fatal("倒序时间戳必须报错")
	}
	if _, err := BuildImageProvenance(in, record, time.Time{}, finished); err == nil {
		t.Fatal("零值开始时间必须报错")
	}
}

// 中文验收点：矩阵镜像行由记录派生——有检查 ID 才是 tested，没有证据的行必须带
// 原因；任一行不合法则整次追加不生效，矩阵保持原样。
// Matrix image rows derive from the record: evidence makes a row tested, a row
// without evidence needs a reason, and one invalid row aborts the whole append.
func TestUS18_Image_AppendImageRowsValidation(t *testing.T) {
	archiveRow := SupportRow{
		GOOS: "linux", GOARCH: "amd64", Tier: TierBinary,
		Status: StatusCompiledOnly, Reason: "交叉编译目标",
	}
	matrix := SupportMatrix{Schema: 1, Rows: []SupportRow{archiveRow}}

	tested := validImageRecord()
	tested.Evidence = []string{"TestUS18_Image_BuildAndRecord"}
	updated, err := AppendImageRows(matrix, []ImageRecord{tested})
	if err != nil {
		t.Fatalf("合法记录应可追加：%v", err)
	}
	if len(updated.Rows) != 2 {
		t.Fatalf("归档行与镜像行必须并存，实际 %d 行", len(updated.Rows))
	}
	imageRow := updated.Rows[1]
	if imageRow.Kind != kindImage || imageRow.Status != StatusTested || len(imageRow.Checks) != 1 {
		t.Fatalf("镜像行派生不符：%+v", imageRow)
	}
	if updated.Rows[0].Kind != "" {
		t.Fatalf("归档行不得被改写：%+v", updated.Rows[0])
	}
	if len(matrix.Rows) != 1 {
		t.Fatal("追加不得就地修改入参矩阵")
	}

	// 没有 e2e 证据的镜像行降级为 not-tested，原因取记录里的扫描说明；这里用基准
	// 记录（scan=not-run 且带原因）验证该分支可达。
	notTested := validImageRecord()
	updated, err = AppendImageRows(matrix, []ImageRecord{notTested})
	if err != nil {
		t.Fatalf("无证据但带原因的记录应可追加：%v", err)
	}
	if row := updated.Rows[1]; row.Status != StatusNotTested || row.Reason == "" {
		t.Fatalf("无证据的镜像行必须写 not-tested 并带原因：%+v", row)
	}

	emptyEvidence := validImageRecord()
	emptyEvidence.Evidence = []string{"  "}
	if _, err := AppendImageRows(matrix, []ImageRecord{emptyEvidence}); err == nil ||
		!strings.Contains(err.Error(), "检查 ID") {
		t.Fatalf("tested 空证据必须被拒，got %v", err)
	}
	noReason := validImageRecord()
	noReason.Scan = ScanRecord{Status: scanPassed, Scanner: "trivy"}
	if _, err := AppendImageRows(matrix, []ImageRecord{noReason}); err == nil ||
		!strings.Contains(err.Error(), "原因") {
		t.Fatalf("not-tested 无原因必须被拒，got %v", err)
	}
	if _, err := AppendImageRows(updated, []ImageRecord{tested}); err == nil ||
		!strings.Contains(err.Error(), "重复") {
		t.Fatalf("同 (goos,goarch,kind) 重复行必须被拒，got %v", err)
	}
	bad := validImageRecord()
	bad.Platform = "linux/arm64"
	if _, err := AppendImageRows(matrix, []ImageRecord{bad}); err == nil {
		t.Fatal("非法记录必须被拒")
	}
}

// 中文验收点（FR-25-32）：构建上下文必须带齐 8 个文件，Worker 模板与两个运维脚本
// 按 Dockerfile 期望的权限位暂存；缺任一 Worker 工件必须在 docker build 之前报错，
// 旧发布记录（三项摘要为空）仍可通过校验，新形态摘要必须是小写 64 位十六进制。
// Acceptance (FR-25-32): the context carries all eight files with the modes the
// Dockerfile expects; a missing worker asset fails before docker build; old records
// (empty digests) still validate while the new digest shape must be lowercase hex.
func TestUS25_Image_ContextStagesWorkerAssets(t *testing.T) {
	fixture := t.TempDir()
	sources := map[string]string{
		"basalt":               "binary",
		"Dockerfile":           "FROM scratch\n",
		"entrypoint.sh":        "#!/bin/sh\n",
		"basalt.yaml":          "server: {}\n",
		"LICENSE":              "license\n",
		"basalt.worker.yaml":   "server: {}\n",
		"export-master-key.sh": "#!/bin/sh\n",
		"seed-workspace.sh":    "#!/bin/sh\n",
	}
	for name, body := range sources {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := ImageInputs{
		Repo:             "repo",
		Version:          "1.2.3",
		Target:           Target{GOOS: "linux", GOARCH: "amd64", Tier: TierBinary},
		Binary:           filepath.Join(fixture, "basalt"),
		BaseRef:          "alpine:3.23@sha256:" + hex64("b"),
		Dockerfile:       filepath.Join(fixture, "Dockerfile"),
		Entrypoint:       filepath.Join(fixture, "entrypoint.sh"),
		ConfigFile:       filepath.Join(fixture, "basalt.yaml"),
		License:          filepath.Join(fixture, "LICENSE"),
		WorkerConfigFile: filepath.Join(fixture, "basalt.worker.yaml"),
		ExportKeyScript:  filepath.Join(fixture, "export-master-key.sh"),
		SeedScript:       filepath.Join(fixture, "seed-workspace.sh"),
		OutDir:           t.TempDir(),
	}
	contextDir, cleanup, err := in.stageImageContext()
	if err != nil {
		t.Fatalf("暂存构建上下文失败: %v", err)
	}
	entries, err := os.ReadDir(contextDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(sources) {
		t.Fatalf("构建上下文必须恰好暂存 %d 个文件，got %d", len(sources), len(entries))
	}
	modes := map[string]os.FileMode{
		"basalt":               0o755,
		"Dockerfile":           0o644,
		"entrypoint.sh":        0o755,
		"basalt.yaml":          0o644,
		"LICENSE":              0o644,
		"basalt.worker.yaml":   0o644,
		"export-master-key.sh": 0o755,
		"seed-workspace.sh":    0o755,
	}
	for _, entry := range entries {
		want, ok := sources[entry.Name()]
		if !ok {
			t.Fatalf("构建上下文出现预期外的文件 %s", entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(contextDir, entry.Name())) // #nosec G304 -- 测试自建的暂存目录
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("%s 的暂存内容与源文件不一致", entry.Name())
		}
		// Windows 上 POSIX 权限位不具意义，权限断言只在类 Unix 宿主执行。
		// POSIX mode bits are meaningless on Windows, so the mode check is Unix-only.
		if runtime.GOOS != "windows" {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != modes[entry.Name()] {
				t.Fatalf("%s 的暂存权限是 %#o，期望 %#o", entry.Name(), got, modes[entry.Name()])
			}
		}
	}
	cleanup()
	if _, err := os.Stat(contextDir); !os.IsNotExist(err) {
		t.Fatalf("cleanup 必须删除整个构建上下文，stat 返回 %v", err)
	}

	// 缺任一 Worker 工件都必须在 docker build 之前失败：validate 是 BuildImage 的
	// 第一条语句，因此 BuildImage 返回该错误本身就证明 docker 没有被调用。
	// A missing worker asset must fail before docker build: validate is BuildImage's
	// first statement, so getting this error back proves docker was never invoked.
	for _, item := range []struct {
		label string
		name  string
		set   func(*ImageInputs, string)
	}{
		{"WorkerConfigFile", "basalt.worker.yaml", func(x *ImageInputs, path string) { x.WorkerConfigFile = path }},
		{"ExportKeyScript", "export-master-key.sh", func(x *ImageInputs, path string) { x.ExportKeyScript = path }},
		{"SeedScript", "seed-workspace.sh", func(x *ImageInputs, path string) { x.SeedScript = path }},
	} {
		missing := in
		item.set(&missing, filepath.Join(fixture, "absent-"+item.name))
		if err := missing.validate(); err == nil || !strings.Contains(err.Error(), item.name) {
			t.Fatalf("缺 %s 必须点名拒绝，got %v", item.name, err)
		}
		if _, err := BuildImage(context.Background(), missing); err == nil ||
			!strings.Contains(err.Error(), item.name) {
			t.Fatalf("BuildImage 缺 %s 必须在 docker build 之前报错，got %v", item.name, err)
		}
	}
	blank := in
	blank.SeedScript = ""
	if err := blank.validate(); err == nil || !strings.Contains(err.Error(), "SeedScript") {
		t.Fatalf("空 SeedScript 必须点名拒绝，got %v", err)
	}

	// 记录面兼容：空摘要的旧记录仍通过；非空摘要必须是小写 64 位十六进制。
	// Record compatibility: old empty digests still pass; a non-empty digest must be
	// 64 lowercase hex chars.
	if err := validImageRecord().Validate(); err != nil {
		t.Fatalf("旧记录（三项摘要为空）必须仍然可校验：%v", err)
	}
	filled := validImageRecord()
	filled.WorkerConfigSHA256 = hex64("a")
	filled.ExportKeySHA256 = hex64("b")
	filled.SeedScriptSHA256 = hex64("c")
	if err := filled.Validate(); err != nil {
		t.Fatalf("带三个小写摘要的记录必须通过：%v", err)
	}
	for _, item := range []struct {
		label  string
		mutate func(record *ImageRecord)
	}{
		{"worker_config_sha256", func(r *ImageRecord) { r.WorkerConfigSHA256 = strings.ToUpper(hex64("a")) }},
		{"export_key_sha256", func(r *ImageRecord) { r.ExportKeySHA256 = "not-a-hash" }},
		{"seed_script_sha256", func(r *ImageRecord) { r.SeedScriptSHA256 = hex64("c") + "0" }},
	} {
		record := filled
		item.mutate(&record)
		if err := record.Validate(); err == nil || !strings.Contains(err.Error(), item.label) {
			t.Fatalf("%s 被改坏后必须点名拒绝，got %v", item.label, err)
		}
	}
}
