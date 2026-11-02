// Package privatebackup 备份与恢复未入库私有资产。
// Package privatebackup snapshots and restores private assets that never enter Git.
package privatebackup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	manifestName = "manifest.json"
	filesPrefix  = "files/"
	schema       = 1
)

// Entry 是快照中的一个文件。
// Entry is one file in a snapshot.
type Entry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

// Manifest 是私有资产快照清单。
// Manifest is the private snapshot inventory.
type Manifest struct {
	Schema    int       `json:"schema"`
	CreatedAt time.Time `json:"created_at"`
	Entries   []Entry   `json:"entries"`
}

// CreateSnapshot 把私有路径打包为确定性 tar.gz，并写入 manifest.json；排除规则里的
// 路径自动纳入，输出必须落在工作区之外（C-12）。
// CreateSnapshot packs private paths into a deterministic tar.gz with a manifest;
// paths listed in .git/info/exclude are included automatically, and the output must
// live outside the worktree.
func CreateSnapshot(repo, out string, includes []string) (Manifest, error) {
	if out == "" {
		return Manifest{}, errors.New("必须提供 --out")
	}
	if err := checkOutputOutside(repo, out); err != nil {
		return Manifest{}, err
	}
	excluded, err := excludeEntries(repo)
	if err != nil {
		return Manifest{}, err
	}
	includes = append(append([]string{}, includes...), excluded...)
	entries, err := collect(repo, includes)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{Schema: schema, CreatedAt: time.Now().UTC(), Entries: entries}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return Manifest{}, err
	}
	file, err := os.Create(out) // #nosec G304 -- out 由维护者用 --out 显式给出，不是外部输入
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = file.Close() }()
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return Manifest{}, err
	}
	writer := tar.NewWriter(gzipWriter)
	if err := writeManifest(writer, manifest); err != nil {
		return Manifest{}, err
	}
	for _, entry := range entries {
		if err := writeFile(writer, entry, filepath.Join(repo, filepath.FromSlash(entry.Path))); err != nil {
			return Manifest{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return Manifest{}, err
	}
	if err := gzipWriter.Close(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// checkOutputOutside 拒绝把快照写进工作区：快照包含私有资产，落在库内会被误提交。
// checkOutputOutside rejects snapshots written inside the worktree: they hold private
// assets and would be committed by accident.
func checkOutputOutside(repo, out string) error {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	// 只认 `..` 或 `..<分隔符>` 开头的相对路径：单看前缀 ".." 会把工作区内的
	// `..snap/` 之类目录当成工作区之外，快照就落回了仓库里。
	rel, err := filepath.Rel(repoAbs, outAbs)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return fmt.Errorf("快照输出 %s 必须位于工作区 %s 之外", outAbs, repoAbs)
	}
	return nil
}

// excludeEntries 读取 .git/info/exclude 里实际存在的工作区路径；注释、否定规则与
// 通配符按 git 语义跳过或展开，不存在或无命中的条目静默忽略。
// excludeEntries reads the paths listed in .git/info/exclude that exist in this
// worktree; comments and negations are skipped and wildcards are expanded.
func excludeEntries(repo string) ([]string, error) {
	path := filepath.Join(repo, ".git", "info", "exclude")
	body, err := os.ReadFile(path) // #nosec G304 -- 仓库内固定路径
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	var entries []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" || !worktreeRelative(line) {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			matches, err := filepath.Glob(filepath.Join(repo, filepath.FromSlash(line)))
			if err != nil {
				continue
			}
			for _, match := range matches {
				if rel, err := filepath.Rel(repo, match); err == nil {
					entries = append(entries, filepath.ToSlash(rel))
				}
			}
			continue
		}
		if _, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(line))); err != nil { // #nosec G703 -- line 已由 worktreeRelative 限定为工作区相对路径
			continue
		}
		entries = append(entries, line)
	}
	return entries, nil
}

// worktreeRelative 只接受工作区内的相对路径：排除清单由用户维护，`..` 或绝对路径
// 会把工作区外的文件卷进私有快照。
// worktreeRelative accepts only worktree-relative entries; absolute paths and `..`
// would pull files from outside the worktree into the snapshot.
func worktreeRelative(entry string) bool {
	if filepath.IsAbs(entry) || strings.HasPrefix(entry, "/") {
		return false
	}
	for _, part := range strings.FieldsFunc(entry, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return false
		}
	}
	return true
}

func collect(repo string, includes []string) ([]Entry, error) {
	if len(includes) == 0 {
		return nil, errors.New("必须至少列出一个私有路径")
	}
	seen := map[string]bool{}
	var entries []Entry
	for _, value := range includes {
		relative, err := cleanRelative(value)
		if err != nil {
			return nil, err
		}
		root := filepath.Join(repo, filepath.FromSlash(relative))
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("读取 %s：%w", relative, err)
		}
		if info.IsDir() {
			err = filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(repo, current)
				if err != nil {
					return err
				}
				entry, err := makeEntry(repo, filepath.ToSlash(rel))
				if err != nil {
					return err
				}
				if !seen[entry.Path] {
					seen[entry.Path] = true
					entries = append(entries, entry)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		entry, err := makeEntry(repo, relative)
		if err != nil {
			return nil, err
		}
		if !seen[entry.Path] {
			seen[entry.Path] = true
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func makeEntry(repo, relative string) (Entry, error) {
	full := filepath.Join(repo, filepath.FromSlash(relative))
	info, err := os.Stat(full)
	if err != nil {
		return Entry{}, err
	}
	if !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("%s 不是普通文件", relative)
	}
	sum, err := hashFile(full)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Path: relative, SHA256: sum, Size: info.Size(), Mode: uint32(info.Mode().Perm())}, nil
}
