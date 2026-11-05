package workspace_test

// 中文验收点（颗粒度文档 US-21 §3.1.3 / §4.6 `us21-base-workspace`）：候选冻结面的
// 只读读写、来源目录边界拒绝与篡改发现。判据面：合法候选逐字可复算；软链接 /
// 穿越 / 超限 / 哈希不符 / 残留目录逐条拒绝；读取时重跑 `ValidateSkillDraft`。
//
// Acceptance points for the US-21 frozen candidate store: byte-reproducible round
// trip, per-boundary rejection (symlink, traversal, limits, hash mismatch, residues)
// and tamper detection on read.

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// admissionSourceFixture 是一份合法来源声明。工作区层只校验字节与哈希自洽，
// 严格 JSON 解析与保留期判定归 core / app 层（§2.3.2）。
// admissionSourceFixture is a valid source declaration for the workspace layer.
const admissionSourceFixture = `{"kind":"user","reference":"本地手写","retention_until":"2027-01-01T00:00:00Z"}`

// admissionMissingID 是一个合法但从不存在的候选标识（Crockford 26 位载荷）。
// admissionMissingID is a well-formed but never-created candidate id.
const admissionMissingID = "adm_01J9ZQ7K3W8V2N5R6T4YHBCDEF"

// mustAdmissionCandidate 用现有闸门造出自洽候选：内容哈希与清单由
// `ValidateSkillDraft` 生成，来源哈希按 `AdmissionSHA256Hex` 复算——测试不自造
// 哈希口径，否则测的是夹具而不是实现。
// mustAdmissionCandidate builds a self-consistent candidate with the shared gate.
func mustAdmissionCandidate(t *testing.T, name string, files []core.AuthorFile, sourceJSON string) core.AdmissionCandidate {
	t.Helper()
	draft, err := workspace.ValidateSkillDraft(name, files)
	require.NoError(t, err)
	id, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	return core.AdmissionCandidate{
		CandidateID:   id,
		SkillName:     name,
		CandidateHash: draft.ContentHash,
		ManifestJSON:  draft.ManifestJSON,
		SourceHash:    core.AdmissionSHA256Hex([]byte(sourceJSON)),
		SourceJSON:    sourceJSON,
	}
}

// candidatePath 把候选目录的相对路径拼到工作区根上。
// candidatePath resolves a candidate-relative path under the workspace root.
func candidatePath(root, candidateID string, elems ...string) string {
	parts := append([]string{root}, strings.Split(workspace.AdmissionCandidateRelDir(candidateID), "/")...)
	return filepath.Join(append(parts, elems...)...)
}

// freezeTestCandidate 冻结一个含两个文件的默认候选。
// freezeTestCandidate freezes a default two-file candidate.
func freezeTestCandidate(t *testing.T, root, name string) core.AdmissionCandidate {
	t.Helper()
	files := authorFiles(t,
		"SKILL.md", skillMD(name, "准入夹具", "v1"),
		"docs/usage.md", "# 用法\n",
	)
	cand := mustAdmissionCandidate(t, name, files, admissionSourceFixture)
	require.NoError(t, workspace.WriteAdmissionCandidate(root, cand, files, []byte(admissionSourceFixture)))
	return cand
}

// removeAdmissionOnCleanup 在测试清理阶段走补偿删除：既清掉只读位（Windows 下
// `RemoveAll` 删不掉只读文件），也顺带验证删除路径对冻结树有效。
// removeAdmissionOnCleanup removes the frozen tree during cleanup.
func removeAdmissionOnCleanup(t *testing.T, root, candidateID string) {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, workspace.RemoveAdmissionCandidate(root, candidateID))
	})
}

// makeAdmissionWritable 供"篡改"类用例解除只读；生产路径没有这个操作。
// makeAdmissionWritable clears read-only bits for tamper tests only.
func makeAdmissionWritable(t *testing.T, root, candidateID string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(candidatePath(root, candidateID), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o750)
		}
		return os.Chmod(p, 0o644)
	}))
}

// 中文验收点（C-15 / FR-21-01）：合法目录逐字冻结、逐字回读，读取路径重跑
// `ValidateSkillDraft` 后哈希与清单完全复算。
func TestUS21_AdmissionCandidate_RoundTrip(t *testing.T) {
	root := initWorkspaceRoot(t)
	files := authorFiles(t,
		"SKILL.md", skillMD("logger", "日志规范", "v1"),
		"docs/usage.md", "# 用法\n",
	)
	cand := mustAdmissionCandidate(t, "logger", files, admissionSourceFixture)
	require.NoError(t, workspace.WriteAdmissionCandidate(root, cand, files, []byte(admissionSourceFixture)))
	removeAdmissionOnCleanup(t, root, cand.CandidateID)

	require.Equal(t, "admissions/candidates/"+cand.CandidateID,
		workspace.AdmissionCandidateRelDir(cand.CandidateID))
	require.True(t, workspace.AdmissionCandidateDirExists(root, cand.CandidateID))
	require.False(t, workspace.AdmissionCandidateDirExists(root, admissionMissingID))

	loaded, loadedFiles, sourceRaw, err := workspace.ReadAdmissionCandidate(root, cand.CandidateID)
	require.NoError(t, err)
	require.Equal(t, cand.CandidateID, loaded.CandidateID)
	require.Equal(t, "logger", loaded.SkillName)
	require.Equal(t, cand.CandidateHash, loaded.CandidateHash)
	require.Equal(t, cand.ManifestJSON, loaded.ManifestJSON)
	require.Equal(t, cand.SourceHash, loaded.SourceHash)
	require.Equal(t, admissionSourceFixture, loaded.SourceJSON)
	require.Equal(t, admissionSourceFixture, string(sourceRaw))

	byPath := make(map[string]string, len(loadedFiles))
	for _, file := range loadedFiles {
		byPath[file.Path] = file.Content
	}
	require.Equal(t, map[string]string{
		"SKILL.md":      skillMD("logger", "日志规范", "v1"),
		"docs/usage.md": "# 用法\n",
	}, byPath, "回读的文件集必须与冻结时逐字一致")

	manifestRaw, err := os.ReadFile(candidatePath(root, cand.CandidateID, "manifest.json"))
	require.NoError(t, err)
	require.Contains(t, string(manifestRaw), cand.CandidateID, "清单必须绑定候选标识")
	require.Contains(t, string(manifestRaw), "logger", "清单必须绑定技能名")

	sourceRawOnDisk, err := os.ReadFile(candidatePath(root, cand.CandidateID, "source.json"))
	require.NoError(t, err)
	require.Equal(t, admissionSourceFixture, string(sourceRawOnDisk))
}

// 中文验收点（C-15）：冻结后目录只读——候选正文与清单都不能再被原地改写。
// 逆改点：去掉 `makeSkillVersionReadOnly` 调用，两条写入断言都会变红。
func TestUS21_CandidateDirReadOnly(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand := freezeTestCandidate(t, root, "logger")
	removeAdmissionOnCleanup(t, root, cand.CandidateID)

	if os.Geteuid() != 0 {
		writeErr := os.WriteFile(candidatePath(root, cand.CandidateID, "files", "SKILL.md"),
			[]byte("tampered"), 0o644)
		require.Error(t, writeErr, "候选冻结后文件必须只读")
		manifestErr := os.WriteFile(candidatePath(root, cand.CandidateID, "manifest.json"),
			[]byte("{}"), 0o644)
		require.Error(t, manifestErr, "候选清单必须只读")
	}
}

// 中文验收点（C-15）：既存正式目录与 `.staging` / `.backup` 残留一律拒绝，拒绝时
// 原物原样保留、不产生新的 staging 残留（候选 ID 是新铸的，出现同名目录就是孤儿）。
func TestUS21_AdmissionCandidate_RefusesResidueAndExisting(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
	}{
		{"staging", ".staging"},
		{"backup", ".backup"},
		{"existing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initWorkspaceRoot(t)
			files := authorFiles(t, "SKILL.md", skillMD("logger", "残留", "v1"))
			cand := mustAdmissionCandidate(t, "logger", files, admissionSourceFixture)

			blocked := candidatePath(root, cand.CandidateID) + tc.suffix
			require.NoError(t, os.MkdirAll(blocked, 0o750))
			marker := filepath.Join(blocked, "marker.txt")
			require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o644))

			err := workspace.WriteAdmissionCandidate(root, cand, files, []byte(admissionSourceFixture))
			require.Error(t, err)
			require.Contains(t, err.Error(), "已存在")

			raw, readErr := os.ReadFile(marker)
			require.NoError(t, readErr)
			require.Equal(t, "keep", string(raw), "拒绝路径不得改动既存目录")

			_, statErr := os.Lstat(candidatePath(root, cand.CandidateID) + ".staging")
			if tc.suffix == ".staging" {
				require.NoError(t, statErr, "既存 staging 目录原样保留")
			} else {
				require.ErrorIs(t, statErr, os.ErrNotExist, "拒绝时不得留下新的 staging 残留")
			}
		})
	}
}

// 中文验收点（C-03）：写入前候选必须自洽——内容哈希、来源哈希或来源字节任一
// 不一致都拒绝，且零写入。
func TestUS21_AdmissionCandidate_WriteRejectsInconsistentRequest(t *testing.T) {
	root := initWorkspaceRoot(t)
	files := authorFiles(t, "SKILL.md", skillMD("logger", "不一致", "v1"))
	cand := mustAdmissionCandidate(t, "logger", files, admissionSourceFixture)

	badHash := cand
	badHash.CandidateHash = strings.Repeat("0", 64)
	err := workspace.WriteAdmissionCandidate(root, badHash, files, []byte(admissionSourceFixture))
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
	require.False(t, workspace.AdmissionCandidateDirExists(root, cand.CandidateID), "拒绝时零写入")

	badSourceHash := cand
	badSourceHash.SourceHash = strings.Repeat("0", 64)
	err = workspace.WriteAdmissionCandidate(root, badSourceHash, files, []byte(admissionSourceFixture))
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)

	err = workspace.WriteAdmissionCandidate(root, cand, files, []byte("{}"))
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch,
		"来源参数字节与候选字段不一致必须拒绝")
	require.False(t, workspace.AdmissionCandidateDirExists(root, cand.CandidateID))
}

// 中文验收点（C-03）：回读重跑 `ValidateSkillDraft`——候选正文被改、清单被换都
// 必须报 `CANDIDATE_MISMATCH`，不得静默采用被改动的字节。
func TestUS21_AdmissionCandidate_TamperDetected(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		root := initWorkspaceRoot(t)
		cand := freezeTestCandidate(t, root, "logger")
		removeAdmissionOnCleanup(t, root, cand.CandidateID)
		makeAdmissionWritable(t, root, cand.CandidateID)

		require.NoError(t, os.WriteFile(candidatePath(root, cand.CandidateID, "files", "SKILL.md"),
			[]byte(skillMD("logger", "被改过的描述", "v2")), 0o644))
		_, _, _, err := workspace.ReadAdmissionCandidate(root, cand.CandidateID)
		require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
	})

	t.Run("manifest", func(t *testing.T) {
		root := initWorkspaceRoot(t)
		cand := freezeTestCandidate(t, root, "logger")
		removeAdmissionOnCleanup(t, root, cand.CandidateID)
		makeAdmissionWritable(t, root, cand.CandidateID)

		manifestPath := candidatePath(root, cand.CandidateID, "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		require.NoError(t, err)
		require.Contains(t, string(raw), cand.CandidateHash)
		tampered := strings.Replace(string(raw), cand.CandidateHash, strings.Repeat("0", 64), 1)
		require.NoError(t, os.WriteFile(manifestPath, []byte(tampered), 0o644))

		_, _, _, err = workspace.ReadAdmissionCandidate(root, cand.CandidateID)
		require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
	})

	t.Run("manifest-unknown-key", func(t *testing.T) {
		root := initWorkspaceRoot(t)
		cand := freezeTestCandidate(t, root, "logger")
		removeAdmissionOnCleanup(t, root, cand.CandidateID)
		makeAdmissionWritable(t, root, cand.CandidateID)

		manifestPath := candidatePath(root, cand.CandidateID, "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		doc["injected"] = true
		tampered, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(manifestPath, tampered, 0o644))

		_, _, _, err = workspace.ReadAdmissionCandidate(root, cand.CandidateID)
		require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch,
			"清单出现未知键必须按篡改拒绝")
	})
}

// 中文验收点（C-03）：缺失与篡改分开报——候选目录 / 清单 / 来源 / 文件目录缺失
// 归 `CANDIDATE_MISSING`，调用方据此决定"从未写入"还是"被改动"。
func TestUS21_AdmissionCandidate_MissingArtifacts(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		root := initWorkspaceRoot(t)
		_, _, _, err := workspace.ReadAdmissionCandidate(root, admissionMissingID)
		require.ErrorIs(t, err, core.ErrAdmissionCandidateMissing)
	})

	for _, artifact := range []string{"manifest", "source", "files"} {
		t.Run(artifact, func(t *testing.T) {
			root := initWorkspaceRoot(t)
			cand := freezeTestCandidate(t, root, "logger")
			removeAdmissionOnCleanup(t, root, cand.CandidateID)
			makeAdmissionWritable(t, root, cand.CandidateID)

			switch artifact {
			case "manifest":
				require.NoError(t, os.Remove(candidatePath(root, cand.CandidateID, "manifest.json")))
			case "source":
				require.NoError(t, os.Remove(candidatePath(root, cand.CandidateID, "source.json")))
			case "files":
				require.NoError(t, os.RemoveAll(candidatePath(root, cand.CandidateID, "files")))
			}

			_, _, _, err := workspace.ReadAdmissionCandidate(root, cand.CandidateID)
			require.ErrorIs(t, err, core.ErrAdmissionCandidateMissing)
		})
	}
}

// createSymlinkOrJunction 优先创建真实符号链接；Windows 普通用户缺少
// SeCreateSymbolicLinkPrivilege 时回退为目录联接（`cmd /c mklink /J`）。两条路径
// 都会被实现拒绝，但分类不同：真实符号链接命中 ModeSymlink 分支，目录联接被 Go
// 报告为 ModeIrregular（2026-11-05 本机实测 mode=?rw-rw-rw-、symlink=false），走
// "非目录 / 非普通文件"的拒绝分支。返回值为该对象应携带的消息片段，供调用方断言；
// 两条路都失败才 Skip（写清理由）。目标必须是已存在的目录。
//
// createSymlinkOrJunction prefers a real symlink and falls back to a Windows
// directory junction. Both are rejected, though a junction surfaces as
// ModeIrregular ("特殊文件"), not ModeSymlink. The returned fragment is the
// message the rejection must carry; the test skips only when neither link type
// can be created.
func createSymlinkOrJunction(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return "符号链接"
	}
	if runtime.GOOS == "windows" {
		// mklink 要求链接的父目录已存在；目录联接只能指向目录。
		cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr == nil {
			return "特殊文件"
		}
		t.Skipf("本机无法创建符号链接或目录联接，跳过软连接拒绝用例：%v：%s", cmdErr, out)
	}
	t.Skipf("本平台不允许创建符号链接，跳过软连接拒绝用例")
	return ""
}

// 中文验收点（C-15）：候选文件树里被塞入软链接（冻结后被篡改）→ 回读按篡改拒绝，
// 且补偿删除仍能把整棵只读树删干净（不跟随链接、不动链接目标）。
func TestUS21_AdmissionCandidate_RejectsSymlinkInFiles(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand := freezeTestCandidate(t, root, "logger")
	removeAdmissionOnCleanup(t, root, cand.CandidateID)
	makeAdmissionWritable(t, root, cand.CandidateID)

	// 链接目标取目录：Linux 走真实符号链接，Windows 回退目录联接；两者在冻结树
	// 回读时都必须被拒绝（符号链接 / 特殊文件分支），断言只看拒绝与错误类别。
	linkPath := candidatePath(root, cand.CandidateID, "files", "docs", "link.md")
	createSymlinkOrJunction(t, t.TempDir(), linkPath)

	_, _, _, err := workspace.ReadAdmissionCandidate(root, cand.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
}

// 中文验收点（C-15）：来源目录根是软链接、或目录内条目是软链接 → 拒绝且归入
// IMPORT_REJECTED；文档原文用例名 `TestUS21_Propose_RejectsSymlinkSource`。
func TestUS21_Propose_RejectsSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(source, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte(skillMD("logger", "软链接", "v1")), 0o644))

	rootLink := filepath.Join(dir, "root-link")
	wantKind := createSymlinkOrJunction(t, source, rootLink)
	_, err := workspace.ReadSkillSourceDir(rootLink)
	requireImportRejected(t, err, wantKind)

	// 指向来源目录之外的目录：Linux 符号链接 / Windows 目录联接都必须被拒绝。
	outside := filepath.Join(dir, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o750))
	entryLink := filepath.Join(source, "docs", "linked")
	require.NoError(t, os.MkdirAll(filepath.Dir(entryLink), 0o750))
	entryKind := createSymlinkOrJunction(t, outside, entryLink)

	_, err = workspace.ReadSkillSourceDir(source)
	requireImportRejected(t, err, entryKind)
}

// 中文验收点（C-15）：来源目录里的隐藏段、`.staging` / `.backup` 后缀与"根不是
// 目录"逐条拒绝；文档原文用例名 `TestUS21_Propose_RejectsTraversal`。
func TestUS21_Propose_RejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		relPath string
		want    string
	}{
		{"hidden-dir", ".git/config", "点开头"},
		{"hidden-file", ".env", "点开头"},
		{"staging-suffix", "notes.staging", ".staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "src")
			require.NoError(t, os.MkdirAll(source, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"),
				[]byte(skillMD("logger", "穿越", "v1")), 0o644))
			target := filepath.Join(source, tc.relPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
			require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

			_, err := workspace.ReadSkillSourceDir(source)
			requireImportRejected(t, err, tc.want)
		})
	}

	t.Run("root-not-dir", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "SKILL.md")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
		_, err := workspace.ReadSkillSourceDir(file)
		requireImportRejected(t, err, "不是目录")
	})
}

// 中文验收点（C-15 / 约束 24）：来源目录复用技能导入的三条上限与文本闸门。
func TestUS21_Propose_EnforcesImportLimits(t *testing.T) {
	newSource := func(t *testing.T) string {
		t.Helper()
		source := filepath.Join(t.TempDir(), "src")
		require.NoError(t, os.MkdirAll(source, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"),
			[]byte(skillMD("logger", "上限", "v1")), 0o644))
		return source
	}

	t.Run("file-size", func(t *testing.T) {
		source := newSource(t)
		big := bytes.Repeat([]byte("a"), 256*1024+1)
		require.NoError(t, os.WriteFile(filepath.Join(source, "big.md"), big, 0o644))
		_, err := workspace.ReadSkillSourceDir(source)
		requireImportRejected(t, err, "上限")
	})

	t.Run("total-size", func(t *testing.T) {
		source := newSource(t)
		chunk := bytes.Repeat([]byte("a"), 250*1024)
		for i := 0; i < 9; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(source, "chunk"+string(rune('0'+i))+".md"),
				chunk, 0o644))
		}
		_, err := workspace.ReadSkillSourceDir(source)
		requireImportRejected(t, err, "总量")
	})

	t.Run("file-count", func(t *testing.T) {
		source := newSource(t)
		for i := 0; i < 200; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(source, "f"+strconv.Itoa(i)+".md"),
				[]byte("x"), 0o644))
		}
		_, err := workspace.ReadSkillSourceDir(source)
		requireImportRejected(t, err, "数量")
	})

	t.Run("non-utf8", func(t *testing.T) {
		source := newSource(t)
		require.NoError(t, os.WriteFile(filepath.Join(source, "bin.md"), []byte{0xff, 0xfe, 0xfd}, 0o644))
		_, err := workspace.ReadSkillSourceDir(source)
		requireImportRejected(t, err, "UTF-8")
	})

	t.Run("nul", func(t *testing.T) {
		source := newSource(t)
		require.NoError(t, os.WriteFile(filepath.Join(source, "nul.md"), []byte("a\x00b"), 0o644))
		_, err := workspace.ReadSkillSourceDir(source)
		requireImportRejected(t, err, "NUL")
	})
}

// 中文验收点（FR-21-14）：`--source-file` / `--decision-file` 只收 ≤64 KiB 的
// 普通文件，软链接与超限拒绝；来源归 IMPORT_REJECTED、决策归 INVALID_INPUT。
func TestUS21_ReadAdmissionInputFiles_Bounds(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.json")
	require.NoError(t, os.WriteFile(sourceFile, []byte(admissionSourceFixture), 0o644))
	raw, err := workspace.ReadAdmissionSourceFile(sourceFile)
	require.NoError(t, err)
	require.Equal(t, admissionSourceFixture, string(raw))

	oversize := filepath.Join(dir, "oversize.json")
	require.NoError(t, os.WriteFile(oversize, bytes.Repeat([]byte("a"), 64*1024+1), 0o644))
	_, err = workspace.ReadAdmissionSourceFile(oversize)
	requireImportRejected(t, err, "上限")

	_, err = workspace.ReadAdmissionDecisionFile(oversize)
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)

	decisionFile := filepath.Join(dir, "decision.json")
	require.NoError(t, os.WriteFile(decisionFile, []byte(`{}`), 0o644))
	raw, err = workspace.ReadAdmissionDecisionFile(decisionFile)
	require.NoError(t, err)
	require.Equal(t, "{}", string(raw))

	link := filepath.Join(dir, "link.json")
	wantKind := createSymlinkOrJunction(t, t.TempDir(), link)
	_, err = workspace.ReadAdmissionSourceFile(link)
	requireImportRejected(t, err, wantKind)
	_, err = workspace.ReadAdmissionDecisionFile(link)
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
}

// 中文验收点（§3.1.3）：非法候选标识在四个入口都按 INVALID_INPUT 拒绝，绝不参与
// 路径拼接（`../evil` 之类穿越在解析阶段就被挡下）。
func TestUS21_AdmissionCandidate_RejectsInvalidID(t *testing.T) {
	root := initWorkspaceRoot(t)
	files := authorFiles(t, "SKILL.md", skillMD("logger", "非法标识", "v1"))
	cand := mustAdmissionCandidate(t, "logger", files, admissionSourceFixture)
	cand.CandidateID = "adm_short"

	err := workspace.WriteAdmissionCandidate(root, cand, files, []byte(admissionSourceFixture))
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)

	_, _, _, err = workspace.ReadAdmissionCandidate(root, "../evil")
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)

	require.False(t, workspace.AdmissionCandidateDirExists(root, "../evil"))
	require.ErrorIs(t, workspace.RemoveAdmissionCandidate(root, "../evil"), core.ErrAdmissionInvalidInput)
}

// 中文验收点（§3.1.3）：补偿删除对冻结树有效且幂等——只读位不阻止删除，重复删除
// 按成功处理。
func TestUS21_RemoveAdmissionCandidate_ClearsFrozenTree(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand := freezeTestCandidate(t, root, "logger")

	require.NoError(t, workspace.RemoveAdmissionCandidate(root, cand.CandidateID))
	require.False(t, workspace.AdmissionCandidateDirExists(root, cand.CandidateID))
	require.NoError(t, workspace.RemoveAdmissionCandidate(root, cand.CandidateID), "重复删除必须幂等")
}
