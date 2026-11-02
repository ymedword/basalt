package privatebackup

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Verify 只读校验快照清单与每个文件的哈希。
// Verify checks the manifest and every file hash without writing anything.
func Verify(archive string) (Manifest, error) {
	reader, err := openArchive(archive)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = reader.Close() }()
	manifest, err := readManifest(reader.Reader)
	if err != nil {
		return Manifest{}, err
	}
	expected := map[string]Entry{}
	for _, entry := range manifest.Entries {
		expected[entry.Path] = entry
	}
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		if header.Name == manifestName {
			continue
		}
		if !strings.HasPrefix(header.Name, filesPrefix) {
			return Manifest{}, fmt.Errorf("快照包含未知条目 %q", header.Name)
		}
		rel := strings.TrimPrefix(header.Name, filesPrefix)
		entry, ok := expected[rel]
		if !ok {
			return Manifest{}, fmt.Errorf("快照包含未登记文件 %q", rel)
		}
		sum, err := hashReader(reader)
		if err != nil {
			return Manifest{}, err
		}
		if sum != entry.SHA256 {
			return Manifest{}, fmt.Errorf("%s 哈希不符：%s", rel, sum)
		}
		seen[rel] = true
	}
	for rel := range expected {
		if !seen[rel] {
			return Manifest{}, fmt.Errorf("快照缺少 %q", rel)
		}
	}
	return manifest, nil
}

// RestoreSnapshot 先校验再解包到 dest；拒绝绝对路径、.. 与符号链接。
// RestoreSnapshot verifies first, then extracts into dest; absolute paths, .. and
// symlinks are rejected.
func RestoreSnapshot(archive, dest string) (Manifest, error) {
	manifest, err := Verify(archive)
	if err != nil {
		return Manifest{}, err
	}
	reader, err := openArchive(archive)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = reader.Close() }()
	if _, err := readManifest(reader.Reader); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return Manifest{}, err
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return manifest, nil
		}
		if err != nil {
			return Manifest{}, err
		}
		if header.Name == manifestName {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return Manifest{}, fmt.Errorf("快照条目 %q 不是普通文件", header.Name)
		}
		rel := strings.TrimPrefix(header.Name, filesPrefix)
		target, err := safeJoin(dest, rel)
		if err != nil {
			return Manifest{}, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return Manifest{}, err
		}
		if err := writeExtracted(target, reader, header.FileInfo().Mode().Perm()); err != nil {
			return Manifest{}, err
		}
	}
}
