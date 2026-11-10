package assets

// 本文件是 US-25C T005 / FR-25C-04 的载荷编解码验收面（模块卡 §3.1.1 / §3.4）：
//   - 编码确定性：路径排序、固定 mode、零 mtime、不跟随符号链接——同一内容无论来自
//     哪个构建目录、源文件时间与权限如何变化，载荷必须逐字节相同；
//   - 解码 fail-closed：绝对路径、`..`、符号链接、非普通文件、超限、tar 之外的垃圾
//     一律拒绝，且路径穿越不产生任何目标目录之外的写入；
//   - 复算判据：解码后按 `bundle.json` 复算的 BundleHash 必须等于 `wantHash`；
//     内容篡改即使骗过解码，也必须被后续 Verify 拒绝。
//
// 判据落在字节与文件系统上，而不是"函数返回了 nil"——确定性由逐字节相等证明，
// 拒绝由"目标目录零写入 / 目录外零文件"证明。
// The acceptance surface for the bundle payload codec: byte-identical determinism and
// fail-closed decoding, proven on bytes and on the filesystem rather than on err == nil.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// payloadLimit 是测试用的载荷上限（远大于夹具，边界用例单独收紧）。
// payloadLimit is the generous default cap; the boundary case tightens it explicitly.
const payloadLimit int64 = 1 << 24

// TestEncodeBundle_Deterministic 判 FR-25C-04 的编码一侧：
//   - 同一目录重复编码、同一来源在另一个目录重建、源文件 mode / mtime 被改动之后，
//     载荷都必须逐字节相同（固定 mode、零 mtime、路径排序的合并判据）；
//   - 返回的 sha 必须是载荷字节的 sha256（`payload_sha256` 口径，与 BundleHash 的
//     目录内容口径不同）；
//   - 载荷条目是相对路径且按路径升序，普通文件的 mode 一致、mtime 全为零；
//   - 载荷包含完整 bundle（bundle.json + agent/** + skills/**），解码后 Verify 能还原
//     同一份清单与文件内容；
//   - 上限语义是 `size_bytes <= max_bytes`：恰等于上限放行，超出报错。
//
// 逆改点：让 mode / mtime 取自磁盘 → 改动源文件后字节不再相同；按 map 遍历写 tar →
// 条目顺序断言红；漏掉 bundle.json → 解码后 Verify 报缺清单；上限判断用 `<` →
// 恰等于上限的用例红。
// Reverse-edit points: disk-derived mode/mtime breaks byte identity after chmod/chtimes;
// unsorted walk breaks the ordering assertion; a missing manifest fails Verify after decode.
func TestEncodeBundle_Deterministic(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")
	work := t.TempDir()

	first := filepath.Join(work, "build-1")
	manifest, err := Build(ctx, fixture.spec(first))
	require.NoError(t, err)

	payload, sha, err := EncodeBundle(ctx, first, payloadLimit)
	require.NoError(t, err)
	require.NotEmpty(t, payload, "载荷不得为空")
	require.True(t, validHash(sha), "返回的 sha 必须是 64 位小写十六进制")
	require.Equal(t, sha256Hex(string(payload)), sha,
		"返回的 sha 必须是载荷字节的 sha256（payload_sha256 口径，不是 BundleHash）")

	// ① 同一目录重复编码：逐字节相同。
	again, againSha, err := EncodeBundle(ctx, first, payloadLimit)
	require.NoError(t, err)
	require.Equal(t, payload, again, "同一目录两次编码必须逐字节相同")
	require.Equal(t, sha, againSha)

	// ② 同一来源在另一个构建目录重建：绝对路径与目录 mtime 不得进入载荷。
	second := filepath.Join(work, "build-2")
	rebuiltManifest, err := Build(ctx, fixture.spec(second))
	require.NoError(t, err)
	require.Equal(t, manifest.BundleHash, rebuiltManifest.BundleHash, "同一来源两次构建必须先得到同一 BundleHash")
	rebuiltPayload, rebuiltSha, err := EncodeBundle(ctx, second, payloadLimit)
	require.NoError(t, err)
	require.Equal(t, payload, rebuiltPayload, "两个构建目录的载荷必须逐字节相同（绝对路径 / mtime 不得泄漏）")
	require.Equal(t, sha, rebuiltSha)

	// ③ 源文件 mode 与 mtime 被改动后仍逐字节相同：固定 mode、零 mtime 的合并判据。
	// 先把可写位改掉再编码；Cleanup 恢复可写，避免临时目录在 Windows 上无法删除。
	fixed := time.Unix(1_700_000_000, 0)
	require.NoError(t, filepath.WalkDir(first, func(candidate string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if err := os.Chtimes(candidate, fixed, fixed); err != nil {
			return err
		}
		return os.Chmod(candidate, 0o444)
	}))
	t.Cleanup(func() {
		// 清理失败不影响判据：临时目录随后会整体移除，这里只恢复可写位。
		_ = filepath.WalkDir(first, func(candidate string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			return os.Chmod(candidate, 0o644)
		})
	})
	touched, touchedSha, err := EncodeBundle(ctx, first, payloadLimit)
	require.NoError(t, err)
	require.Equal(t, payload, touched, "源文件 mode 与 mtime 不得进入载荷（固定 mode / 零 mtime）")
	require.Equal(t, sha, touchedSha)

	// ④ tar 结构判据：相对路径、路径升序、零 mtime、普通文件 mode 一致、无链接条目。
	entries := readPayloadEntries(t, payload)
	names := make([]string, 0, len(entries))
	fileMode, dirMode := int64(-1), int64(-1)
	for _, entry := range entries {
		names = append(names, entry.name)
		// "零 mtime"允许两种等价表示：零值或 Unix epoch；两者都不携带源文件时间。
		require.True(t, entry.modTime.IsZero() || entry.modTime.Equal(time.Unix(0, 0)),
			"载荷条目 %s 的 mtime 必须是零值或 Unix epoch（不得携带源文件时间）", entry.name)
		require.False(t, strings.HasPrefix(entry.name, "/"), "载荷条目必须是相对路径：%s", entry.name)
		require.False(t, strings.Contains(entry.name, `\`), "载荷条目必须用正斜杠分隔：%s", entry.name)
		for _, segment := range strings.Split(entry.name, "/") {
			require.NotEmpty(t, segment, "载荷条目不得含空段：%s", entry.name)
			require.NotEqual(t, ".", segment, "载荷条目不得含 . 段：%s", entry.name)
			require.NotEqual(t, "..", segment, "载荷条目不得含 .. 段：%s", entry.name)
		}
		switch entry.typeflag {
		case tar.TypeReg:
			if fileMode < 0 {
				fileMode = entry.mode
				continue
			}
			require.Equal(t, fileMode, entry.mode, "普通文件的 mode 必须固定（%s）", entry.name)
		case tar.TypeDir:
			if dirMode < 0 {
				dirMode = entry.mode
				continue
			}
			require.Equal(t, dirMode, entry.mode, "目录的 mode 必须固定（%s）", entry.name)
		default:
			require.Failf(t, "载荷含非普通文件条目", "条目 %s 的 typeflag=%d", entry.name, entry.typeflag)
		}
	}
	require.GreaterOrEqual(t, fileMode, int64(0), "载荷必须至少包含一个普通文件")
	require.True(t, slices.IsSorted(names), "载荷条目必须按路径升序（排序是确定性的一部分）")
	for _, want := range []string{
		manifestFileName,
		path.Join(configDirName, "AGENT.md"),
		path.Join(skillsDirName, "tender", "SKILL.md"),
	} {
		require.Contains(t, names, want, "载荷必须包含 %s（解码后要能通过 Verify）", want)
	}

	// ⑤ 解码往返：解出的目录必须与构建目录同清单、同内容。
	dest := filepath.Join(work, "decoded")
	require.NoError(t, DecodeBundle(ctx, payload, dest, manifest.BundleHash, payloadLimit))
	decoded, err := Verify(ctx, dest)
	require.NoError(t, err, "解码结果必须是可校验的 bundle")
	require.True(t, sameManifest(decoded, manifest), "解码后的清单必须逐项等于构建时的清单")
	require.Equal(t, fixture.ConfigFile["AGENT.md"], readFile(t, filepath.Join(dest, configDirName, "AGENT.md")))
	require.Equal(t, fixture.SkillFiles["SKILL.md"], readFile(t, filepath.Join(dest, skillsDirName, "tender", "SKILL.md")))

	// ⑥ 上限边界：恰等于上限放行（size_bytes <= max_bundle_bytes），少一个字节报错。
	exact, exactSha, err := EncodeBundle(ctx, first, int64(len(payload)))
	require.NoError(t, err, "载荷大小恰等于上限必须放行（size_bytes <= max_bundle_bytes）")
	require.Equal(t, payload, exact)
	require.Equal(t, sha, exactSha)
	_, _, err = EncodeBundle(ctx, first, int64(len(payload))-1)
	require.Error(t, err, "超过上限必须返回明确错误，而不是截断产出")
	err = DecodeBundle(ctx, payload, filepath.Join(work, "over-limit"), manifest.BundleHash, int64(len(payload))-1)
	require.Error(t, err, "解码超限载荷必须拒绝")

	// ⑦ 不跟随符号链接：目录里多一个指向 AGENT.md 的链接，载荷要么拒绝编码，要么
	// 不含该条目、不含链接类型；任何情况下都不得把链接目标的内容当成新文件打进载荷。
	// 当前平台无法创建符号链接时只记录环境限制（不上报 skip：本用例是 stage 的
	// required test，允许跳过会削弱判据）。
	symlinkDir := filepath.Join(work, "build-link")
	_, err = Build(ctx, fixture.spec(symlinkDir))
	require.NoError(t, err)
	linkTarget := filepath.Join(symlinkDir, configDirName, "AGENT.md")
	linkPath := filepath.Join(symlinkDir, configDirName, "extra-link.md")
	if linkErr := os.Symlink(linkTarget, linkPath); linkErr != nil {
		t.Logf("当前环境不允许创建符号链接（%v）——编码侧“不跟随符号链接”分支未在本平台执行", linkErr)
	} else {
		linkedPayload, _, encodeErr := EncodeBundle(ctx, symlinkDir, payloadLimit)
		if encodeErr == nil {
			for _, entry := range readPayloadEntries(t, linkedPayload) {
				require.NotEqual(t, byte(tar.TypeSymlink), entry.typeflag, "载荷不得含符号链接条目")
				require.NotEqual(t, byte(tar.TypeLink), entry.typeflag, "载荷不得含硬链接条目")
				require.NotEqual(t, path.Join(configDirName, "extra-link.md"), entry.name,
					"符号链接来源不得进入载荷（不跟随）")
			}
		}
	}
}

// TestDecodeBundle_Rejects 判 FR-25C-04 的解码一侧：恶意与损坏的载荷一律 fail-closed，
// 路径穿越零写入；复算哈希不等于 `wantHash` 即失败；内容篡改即使解出也不得成为可用
// bundle。
//
// 逆改点：允许 `..` → 穿越用例在目标目录之外看到 evil.txt；解出后不比对哈希 →
// wantHash 不符用例拿到 nil；先解压后校验符号链接 → 链接条目落地。
// Reverse-edit points: accepting `..` leaves evil.txt outside the destination; skipping
// the hash comparison returns nil for a foreign payload; extracting before validating
// materializes a symlink entry.
func TestDecodeBundle_Rejects(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-technical")
	work := t.TempDir()

	bundleDir := filepath.Join(work, "build")
	manifest, err := Build(ctx, fixture.spec(bundleDir))
	require.NoError(t, err)
	payload, _, err := EncodeBundle(ctx, bundleDir, payloadLimit)
	require.NoError(t, err)

	// ① 拒绝清单：绝对路径、`..`（含嵌套）、符号链接、非普通文件。
	rejects := []struct {
		name    string
		entries []payloadFixture
		outside string // 目标目录之外不得出现的文件（相对 work）
	}{
		{"绝对路径", []payloadFixture{{name: "/etc/passwd", data: []byte("root:x:0:0")}}, ""},
		{"路径穿越", []payloadFixture{{name: "../evil.txt", data: []byte("escape")}}, "evil.txt"},
		{"嵌套穿越", []payloadFixture{{name: "sub/../../evil.txt", data: []byte("escape")}}, "evil.txt"},
		{"符号链接条目", []payloadFixture{{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}}, ""},
		{"非普通文件", []payloadFixture{{name: "fifo", typeflag: tar.TypeFifo}}, ""},
	}
	for index, reject := range rejects {
		t.Run(reject.name, func(t *testing.T) {
			dest := filepath.Join(work, "reject-"+string(rune('a'+index)))
			err := DecodeBundle(ctx, buildTarPayload(t, reject.entries), dest, manifest.BundleHash, payloadLimit)
			require.Error(t, err, "%s 载荷必须被拒绝", reject.name)
			requireNoEntries(t, dest, "被拒绝的解码不得留下任何条目（路径穿越零写入）")
			if reject.outside != "" {
				require.NoFileExists(t, filepath.Join(work, reject.outside),
					"路径穿越不得在目标目录之外写出文件")
			}
		})
	}

	// ② 不是 tar：垃圾字节与空载荷都必须拒绝，而不是"尽力理解"。
	t.Run("非 tar 载荷", func(t *testing.T) {
		dest := filepath.Join(work, "reject-garbage")
		require.Error(t, DecodeBundle(ctx, []byte("not a tar archive"), dest, manifest.BundleHash, payloadLimit))
		requireNoEntries(t, dest, "非法载荷不得留下任何条目")
		require.Error(t, DecodeBundle(ctx, nil, filepath.Join(work, "reject-empty"), manifest.BundleHash, payloadLimit))
	})

	// ③ 复算哈希不符：载荷属于另一个版本（wantHash 指向别处）必须拒绝。
	t.Run("wantHash 不符", func(t *testing.T) {
		dest := filepath.Join(work, "reject-hash")
		err := DecodeBundle(ctx, payload, dest, strings.Repeat("0", hashHexLength), payloadLimit)
		require.Error(t, err, "解码后复算的 BundleHash 不等于 wantHash 时必须失败")
	})

	// ④ 内容篡改：解码可以拒绝，也可以解出——但结果绝不能通过 Verify（安全判据是
	// "不可用"，不是"在哪一层被拦下"）。
	t.Run("内容篡改不得产出可用 bundle", func(t *testing.T) {
		tampered := tamperPayloadFile(t, payload, path.Join(configDirName, "AGENT.md"))
		dest := filepath.Join(work, "reject-tampered")
		if err := DecodeBundle(ctx, tampered, dest, manifest.BundleHash, payloadLimit); err != nil {
			return // 解码层即拒绝同样满足 fail-closed
		}
		_, err := Verify(ctx, dest)
		require.ErrorIs(t, err, core.ErrAssetVersionMismatch, "内容篡改必须被复算校验拒绝")
	})
}

// payloadFixture 是一份手工构造的 tar 条目（用于恶意载荷）。
// payloadFixture is one hand-crafted tar entry for hostile-payload cases.
type payloadFixture struct {
	name     string
	data     []byte
	typeflag byte
	linkname string
}

// payloadEntry 是读回的一条 tar 条目（名字 / 权限 / 时间 / 类型 / 内容）。
// payloadEntry is one decoded tar entry.
type payloadEntry struct {
	name     string
	mode     int64
	modTime  time.Time
	typeflag byte
	linkname string
	data     []byte
}

// buildTarPayload 按给定条目写一份 tar（typeflag 缺省为普通文件；目录与链接的
// Size 一律为 0）。
// buildTarPayload serializes hand-crafted entries; links and directories carry size 0.
func buildTarPayload(t *testing.T, entries []payloadFixture) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o600,
			Typeflag: typeflag,
			Linkname: entry.linkname,
			ModTime:  time.Time{},
		}
		if typeflag == tar.TypeReg {
			header.Size = int64(len(entry.data))
		}
		require.NoError(t, writer.WriteHeader(header))
		if header.Size > 0 {
			_, err := writer.Write(entry.data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

// readPayloadEntries 读回载荷的全部 tar 条目（供结构与篡改用例复用）。
// readPayloadEntries decodes every entry of a payload for structural assertions.
func readPayloadEntries(t *testing.T, payload []byte) []payloadEntry {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(payload))
	var entries []payloadEntry
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "载荷必须是可解析的 tar")
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		entries = append(entries, payloadEntry{
			name:     header.Name,
			mode:     header.Mode,
			modTime:  header.ModTime,
			typeflag: header.Typeflag,
			linkname: header.Linkname,
			data:     data,
		})
	}
	return entries
}

// tamperPayloadFile 改掉载荷里指定文件的首字节再原样重打包（模拟传输 / 存储篡改）。
// tamperPayloadFile flips the first byte of one regular file inside the payload.
func tamperPayloadFile(t *testing.T, payload []byte, target string) []byte {
	t.Helper()
	entries := readPayloadEntries(t, payload)
	fixtures := make([]payloadFixture, 0, len(entries))
	replaced := false
	for _, entry := range entries {
		data := entry.data
		if entry.name == target {
			require.NotEmpty(t, data, "被篡改的文件不得为空")
			data = bytes.Clone(entry.data)
			data[0] ^= 0xff
			replaced = true
		}
		fixtures = append(fixtures, payloadFixture{
			name: entry.name, data: data, typeflag: entry.typeflag, linkname: entry.linkname,
		})
	}
	require.True(t, replaced, "载荷里必须存在待篡改文件 %s", target)
	return buildTarPayload(t, fixtures)
}

// requireNoEntries 断言目录不存在或为空（被拒绝的解码不得留下任何条目）。
// requireNoEntries asserts the destination is absent or empty after a rejected decode.
func requireNoEntries(t *testing.T, dir, message string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, entries, "%s：%s", message, dir)
}
