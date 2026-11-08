//go:build e2e

package e2e

// US-25 容器验收 · 种子归档（FR-25-17）。
//
// 用例把一份**构造出来的**工作区交给镜像里真实的 `docker/seed-workspace.sh`：导出 →
// 校验 → 篡改 / 截断 / 路径穿越 / 符号链接 / 清单外文件逐项拒绝 → 正常导入。所有判据
// 都跑在容器内，因为脚本依赖 busybox 的 tar / awk / sha256sum；宿主机只提供目录与
// 期望值。
//
// The case drives the shipped seed script inside the image: export, verify, then refuse
// every tampering shape, and finally import the genuine archive.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// us25SeedSourceFiles 是种子必须带走的文件（名册、Agent 正文、资产清单、一段大正文）。
// The files the seed must carry.
var us25SeedSourceFiles = map[string]string{
	".basalt/instance.json":                            `{"version":1,"instance_id":"inst_0123456789abcdef"}` + "\n",
	".basalt/control/agents/bid-requirements/AGENT.md": "---\nname: bid-requirements\n---\n正文\n",
	".basalt/assets/bundle/manifest.json":              `{"files":[]}` + "\n",
	// 8 KB 的正文让"截断一半"必然落在某个条目的数据里，而不是落在归档末尾的
	// 填充块中——否则半截归档仍可能是一个合法的 tar，截断判据就成了空判。
	// The 8 KB payload makes "cut in half" land inside member data rather than in the
	// trailing padding, where the half archive would still be a valid tar.
	".basalt/assets/big.bin": strings.Repeat("basalt-seed-payload\n", 400),
}

// us25SeedExcludedPaths 是 C-10 的排除项：每一条都必须不出现在归档里。
// Every C-10 exclusion, none of which may appear in the archive.
var us25SeedExcludedPaths = []string{
	".basalt/basalt.db",
	".basalt/basalt.db-wal",
	".basalt/basalt.lock",
	".basalt/logs/app.log",
	".basalt/runs/r1/record.json",
	".basalt/agents/bid/state/state.json",
	".basalt/agents/bid.staging/AGENT.md",
	".basalt/notes.tmp",
	".basalt/scrap.part",
}

// us25ZeroManifest 是一个合法的“零文件”清单：用它把“清单外文件 / 链接”的判据与
// “清单本身不合法”的判据分开，避免后者掩盖前者。
// A valid zero-entry manifest, so the extra-file and symlink verdicts cannot be
// shadowed by a malformed manifest.
const us25ZeroManifest = `{
  "format_version": 1,
  "created_at": "2026-11-08T00:00:00Z",
  "source_instance_id": "inst_0123456789abcdef",
  "files": [
  ]
}
`

// TestUS25_Seed_ManifestAndTamper 判 FR-25-17：归档含清单（size 与 sha256）；篡改一
// 字节、截断、路径穿越、符号链接、清单外文件都拒绝；排除项不进归档；导入先校验后落盘。
//
// 逆改点：去掉清单核对、只判 tar 能否解压、或让导入在校验前写目标目录 → 对应断言红。
func TestUS25_Seed_ManifestAndTamper(t *testing.T) {
	s := us25NewStack(t, "seed")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	src := t.TempDir()
	out := t.TempDir()
	for path, body := range us25SeedSourceFiles {
		us25WriteHostFile(t, src, path, body)
	}
	for _, path := range us25SeedExcludedPaths {
		us25WriteHostFile(t, src, path, "must-not-be-seeded\n")
	}
	// 工作区根的配置不在 `.basalt/` 下：种子只认 `.basalt/` 前缀，配置由入口脚本
	// 按角色从镜像模板播种，因此它必须不在归档里。
	us25WriteHostFile(t, src, "basalt.yaml", "database:\n  dsn: ${BASALT_DATABASE_DSN}\n")

	seedScript := func(scriptArgs ...string) (string, error) {
		args := append([]string{
			"run", "--rm", "--user", "0",
			"-v", src + ":/src:ro", "-v", out + ":/seed",
			"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
		}, scriptArgs...)
		return us25DockerCombined(ctx, args...)
	}
	archiveRun := func(tarArgs ...string) (string, error) {
		args := append([]string{
			"run", "--rm", "--user", "0",
			"-v", out + ":/seed:ro", "--entrypoint", "tar", s.image,
		}, tarArgs...)
		return us25DockerCombined(ctx, args...)
	}
	// requireVerifyRefusal 判一次 `verify` 必须失败且点名给定原因。
	// requireVerifyRefusal requires a refusal that names the expected reason.
	requireVerifyRefusal := func(archiveName, want string) {
		t.Helper()
		text, err := seedScript("verify", archiveName)
		us25RequireSeedRefusal(t, text, err, want)
	}

	// ① 导出并校验：真实脚本、真实 tar。
	if text, err := seedScript("export", "/src", "/seed/seed.tar"); err != nil {
		t.Fatalf("导出种子失败：%v（%s）", err, us18Tail(text, 3000))
	}
	if text, err := seedScript("verify", "/seed/seed.tar"); err != nil {
		t.Fatalf("校验种子失败：%v（%s）", err, us18Tail(text, 3000))
	}

	// ② 归档成员：恰好是三个白名单文件 + 清单；每条排除项都缺席。
	listing, err := archiveRun("-tf", "/seed/seed.tar")
	if err != nil {
		t.Fatalf("列出归档失败：%v（%s）", err, us18Tail(listing, 2000))
	}
	got := us18NonEmptyLines(listing)
	sort.Strings(got)
	want := []string{
		".basalt/assets/big.bin",
		".basalt/assets/bundle/manifest.json",
		".basalt/control/agents/bid-requirements/AGENT.md",
		".basalt/instance.json",
		"seed-manifest.json",
	}
	requireStringList(t, "归档成员", got, want)
	for _, excluded := range us25SeedExcludedPaths {
		for _, entry := range got {
			if entry == excluded || strings.HasPrefix(entry, strings.TrimSuffix(excluded, "/")+"/") {
				t.Fatalf("归档含排除项：%s", entry)
			}
		}
	}
	for _, entry := range got {
		if entry == "basalt.yaml" {
			t.Fatalf("归档含工作区根的配置：%s", entry)
		}
	}

	// ③ 清单逐项对源文件：size 与 sha256 都要能复算。
	manifestText, err := archiveRun("-xOf", "/seed/seed.tar", "seed-manifest.json")
	if err != nil {
		t.Fatalf("读取清单失败：%v（%s）", err, us18Tail(manifestText, 2000))
	}
	var manifest struct {
		FormatVersion    int    `json:"format_version"`
		SourceInstanceID string `json:"source_instance_id"`
		Files            []struct {
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(manifestText), &manifest); err != nil {
		t.Fatalf("解析清单失败：%v（%s）", err, us18Tail(manifestText, 2000))
	}
	if manifest.FormatVersion != 1 {
		t.Fatalf("清单 format_version=%d（要求 1）", manifest.FormatVersion)
	}
	if manifest.SourceInstanceID != "inst_0123456789abcdef" {
		t.Fatalf("清单 source_instance_id=%q", manifest.SourceInstanceID)
	}
	if len(manifest.Files) != len(us25SeedSourceFiles) {
		t.Fatalf("清单列了 %d 个文件（要求 %d）：%+v",
			len(manifest.Files), len(us25SeedSourceFiles), manifest.Files)
	}
	for _, entry := range manifest.Files {
		body := []byte(us25SeedSourceFiles[entry.Path])
		sum := sha256.Sum256(body)
		if entry.Size != int64(len(body)) {
			t.Fatalf("%s 的 size=%d（源 %d）", entry.Path, entry.Size, len(body))
		}
		if entry.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s 的 sha256 与源文件不符", entry.Path)
		}
	}

	// ④ 篡改一字节（长度不变 → 必须被 sha256 判据抓住，而不是只靠大小）。
	entries := us25ReadTar(t, filepath.Join(out, "seed.tar"))
	tampered := false
	for i := range entries {
		if entries[i].Header.Name == ".basalt/control/agents/bid-requirements/AGENT.md" {
			entries[i].Body[0] ^= 0xff
			tampered = true
		}
	}
	if !tampered {
		t.Fatalf("归档里找不到用于篡改的 Agent 正文条目")
	}
	us25WriteTar(t, filepath.Join(out, "tampered.tar"), entries)
	requireVerifyRefusal("/seed/tampered.tar", "sha256 不符")

	// ⑤ 截断。
	raw, err := os.ReadFile(filepath.Join(out, "seed.tar"))
	if err != nil {
		t.Fatalf("读取归档失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "truncated.tar"), raw[:len(raw)/2], 0o644); err != nil {
		t.Fatalf("写截断归档失败：%v", err)
	}
	requireVerifyRefusal("/seed/truncated.tar", "截断")

	// ⑥ 路径穿越 / 绝对路径 / 符号链接 / 清单外文件：四种恶意归档逐项拒绝，
	//    且每个失败都不得在导入目标里留下任何东西（先校验、后落盘）。
	manifestEntry := us25TarRegular("seed-manifest.json", us25ZeroManifest)
	cases := []struct {
		name    string
		entries []us25TarEntry
		want    string
	}{
		{
			name: "traversal",
			entries: []us25TarEntry{
				manifestEntry,
				us25TarRegular("../evil", "escaped\n"),
			},
			want: "越界",
		},
		{
			name: "absolute",
			entries: []us25TarEntry{
				manifestEntry,
				us25TarRegular("/tmp/evil", "escaped\n"),
			},
			want: "越界",
		},
		{
			name: "symlink",
			entries: []us25TarEntry{
				manifestEntry,
				{Header: &tar.Header{Name: ".basalt/control/", Mode: 0o755, Typeflag: tar.TypeDir}, Body: nil},
				{Header: &tar.Header{
					Name:     ".basalt/control/link",
					Mode:     0o777,
					Typeflag: tar.TypeSymlink,
					Linkname: "/etc/passwd",
				}, Body: nil},
			},
			want: "链接",
		},
		{
			name: "extra-file",
			entries: []us25TarEntry{
				manifestEntry,
				us25TarRegular(".basalt/control/extra.md", "not-in-manifest\n"),
			},
			want: "清单外文件",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(out, tc.name+".tar")
			us25WriteTar(t, path, tc.entries)
			requireVerifyRefusal("/seed/"+tc.name+".tar", tc.want)

			target := t.TempDir()
			text, err := us25DockerCombined(ctx, "run", "--rm", "--user", "0",
				"-v", out+":/seed:ro", "-v", target+":/data",
				"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
				"import", "/seed/"+tc.name+".tar", "/data")
			if err == nil {
				t.Fatalf("恶意归档 %s 被导入了：%s", tc.name, us18Tail(text, 2000))
			}
			remaining, readErr := os.ReadDir(target)
			if readErr != nil {
				t.Fatalf("读取导入目标失败：%v", readErr)
			}
			if len(remaining) != 0 {
				names := make([]string, 0, len(remaining))
				for _, item := range remaining {
					names = append(names, item.Name())
				}
				t.Fatalf("校验失败的归档在导入目标里留下了 %v（要求零落盘）", names)
			}
		})
	}

	// ⑦ 正常导入：目标目录拿到名册、正文与归档根上的清单；排除项不落地。
	target := t.TempDir()
	// 导入按真实路径以 root 执行并把属主改成 10001:10001；宿主用户删不掉那棵子树，
	// 不先以 root 清掉的话 t.TempDir 的 RemoveAll 会以 permission denied 失败。
	us25RemoveRootOwned(t, s.image, target)
	if text, err := us25DockerCombined(ctx, "run", "--rm", "--user", "0",
		"-v", out+":/seed:ro", "-v", target+":/data",
		"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
		"import", "/seed/seed.tar", "/data"); err != nil {
		t.Fatalf("导入种子失败：%v（%s）", err, us18Tail(text, 3000))
	}
	for _, path := range []string{
		".basalt/instance.json",
		".basalt/control/agents/bid-requirements/AGENT.md",
		"seed-manifest.json",
	} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(path))); err != nil {
			t.Fatalf("导入后缺少 %s：%v", path, err)
		}
	}
	for _, path := range us25SeedExcludedPaths {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(path))); err == nil {
			t.Fatalf("导入后出现了排除项 %s", path)
		}
	}
	// 目标已有名册时再导入必须拒绝（不覆盖）。
	text, err := us25DockerCombined(ctx, "run", "--rm", "--user", "0",
		"-v", out+":/seed:ro", "-v", target+":/data",
		"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
		"import", "/seed/seed.tar", "/data")
	if err == nil {
		t.Fatalf("已有名册的工作区被再次导入了：%s", us18Tail(text, 2000))
	}
	if !strings.Contains(text, "已有实例名册") {
		t.Fatalf("重复导入的失败信息没有点名名册：%s", us18Tail(text, 3000))
	}
}

// us25TarEntry 是一个待写 tar 条目（Header 只有 Name/Mode/Typeflag/Linkname 有意义）。
// us25TarEntry is one tar member to write.
type us25TarEntry struct {
	Header *tar.Header
	Body   []byte
}

// us25TarRegular 造一个普通文件条目。
// us25TarRegular builds one regular-file member.
func us25TarRegular(name, body string) us25TarEntry {
	return us25TarEntry{
		Header: &tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg},
		Body:   []byte(body),
	}
}

// us25ReadTar 把一个 tar 归档读成条目表（保留头与正文）。
// us25ReadTar reads every member of an archive into memory.
func us25ReadTar(t *testing.T, path string) []us25TarEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取归档 %s 失败：%v", path, err)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	var entries []us25TarEntry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析归档 %s 失败：%v", path, err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("读取归档 %s 的 %s 失败：%v", path, header.Name, err)
		}
		frozen := *header
		entries = append(entries, us25TarEntry{Header: &frozen, Body: body})
	}
	return entries
}

// us25WriteTar 把条目表写成一个新归档；Size 一律按正文长度重算。
// us25WriteTar writes the members back out, recomputing Size from the body.
func us25WriteTar(t *testing.T, path string, entries []us25TarEntry) {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		header := *entry.Header
		header.Size = int64(len(entry.Body))
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatalf("写归档条目 %s 失败：%v", header.Name, err)
		}
		if len(entry.Body) > 0 {
			if _, err := writer.Write(entry.Body); err != nil {
				t.Fatalf("写归档条目 %s 正文失败：%v", header.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭归档失败：%v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写归档 %s 失败：%v", path, err)
	}
}

// us25RequireSeedRefusal 判一次种子脚本调用**必须失败**且输出点名给定原因。
// us25RequireSeedRefusal requires a refusal that names the expected reason.
func us25RequireSeedRefusal(t *testing.T, text string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("种子脚本意外成功（要求拒绝）：%s", us18Tail(text, 3000))
	}
	if !strings.Contains(text, want) {
		t.Fatalf("拒绝信息没有包含 %q：%s", want, us18Tail(text, 3000))
	}
}

// us25RemoveRootOwned 在用例清理之前，用 root 容器删掉导入留下的属主不属于宿主用户的
// 子树：导入以 root 运行并把内容 chown 到 10001:10001，宿主用户只能删自己拥有的目录项，
// `t.TempDir` 的 RemoveAll 会因 permission denied 失败（实测：unlinkat ... permission denied）。
// us25RemoveRootOwned deletes the import tree as root before TempDir cleanup, because the
// import chowns to 10001:10001 by design and the host user cannot unlink inside it.
func us25RemoveRootOwned(t *testing.T, image, dir string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = us25DockerCombined(ctx, "run", "--rm", "--user", "0",
			"-v", dir+":/data", "--entrypoint", "rm", image,
			"-rf", "/data/.basalt", "/data/seed-manifest.json")
	})
}

// us25WriteHostFile 在宿主目录里写一个相对路径文件（自动建父目录）。
// us25WriteHostFile writes a relative-path file under root.
func us25WriteHostFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建 %s 的父目录失败：%v", rel, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 %s 失败：%v", rel, err)
	}
}

// us25DockerCombined 跑一条 docker 命令并合并 stdout / stderr——种子脚本的拒绝原因
// 走 stderr，`us18DockerOutput` 成功时只回 stdout，判据会读不到。
// us25DockerCombined merges both streams: the seed script writes refusals to stderr.
func us25DockerCombined(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) // 参数由夹具构造，不含外部自由文本
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}
