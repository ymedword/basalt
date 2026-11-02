package releasekit

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const checksumsName = "SHA256SUMS"

// ChecksumEntry 是 SHA256SUMS 的一行。
// ChecksumEntry is one line of SHA256SUMS.
type ChecksumEntry struct {
	SHA256 string
	Name   string
}

// WriteChecksums 生成 SHA256SUMS，覆盖除自身与自身签名外的全部文件；按文件名排序。
// WriteChecksums writes SHA256SUMS over every file except itself and its signature,
// sorted by relative name.
func WriteChecksums(dir string) error {
	var entries []ChecksumEntry
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == checksumsName || rel == checksumsName+".sig" {
			return nil
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return err
		}
		entries = append(entries, ChecksumEntry{SHA256: sum, Name: rel})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	var body strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&body, "%s  %s\n", entry.SHA256, entry.Name)
	}
	// #nosec G306 -- SHA256SUMS 是发布目录里的公开校验清单，必须对所有下载者可读
	return os.WriteFile(filepath.Join(dir, checksumsName), []byte(body.String()), 0o644)
}

// ParseChecksums 读取并校验校验和清单。
// ParseChecksums parses and validates the checksum file.
func ParseChecksums(path string) ([]ChecksumEntry, error) {
	file, err := os.Open(path) // #nosec G304 -- 发布文件路径
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var entries []ChecksumEntry
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 || strings.ContainsAny(fields[1], `\`) {
			return nil, fmt.Errorf("SHA256SUMS 行格式非法：%q", line)
		}
		if seen[fields[1]] {
			return nil, fmt.Errorf("SHA256SUMS 重复文件：%s", fields[1])
		}
		seen[fields[1]] = true
		entries = append(entries, ChecksumEntry{SHA256: fields[0], Name: fields[1]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("SHA256SUMS 为空")
	}
	return entries, nil
}

// VerifyFileHash 按清单核对单个文件。
// VerifyFileHash checks one file against its checksum entry.
func VerifyFileHash(dir, name string) error {
	entries, err := ParseChecksums(filepath.Join(dir, checksumsName))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		got, err := FileSHA256(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if got != entry.SHA256 {
			return fmt.Errorf("%s 哈希不符：清单 %s，实际 %s", name, entry.SHA256, got)
		}
		return nil
	}
	return fmt.Errorf("%s 不在 SHA256SUMS 内", name)
}
