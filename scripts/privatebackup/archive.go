package privatebackup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func writeManifest(writer *tar.Writer, manifest Manifest) error {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	header := &tar.Header{
		Name: manifestName, Mode: 0o600, Size: int64(len(body)),
		ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err = writer.Write(body)
	return err
}

func writeFile(writer *tar.Writer, entry Entry, source string) error {
	file, err := os.Open(source) // #nosec G304 -- 显式清单路径
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	header := &tar.Header{
		Name: filesPrefix + entry.Path, Mode: int64(entry.Mode), Size: entry.Size,
		ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}

func writeExtracted(target string, reader io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304 -- 已校验路径
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, reader); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readManifest(reader *tar.Reader) (Manifest, error) {
	for {
		header, err := reader.Next()
		if err != nil {
			return Manifest{}, errors.New("快照缺少 manifest.json")
		}
		if header.Name != manifestName {
			continue
		}
		var manifest Manifest
		if err := json.NewDecoder(io.LimitReader(reader, 4<<20)).Decode(&manifest); err != nil {
			return Manifest{}, err
		}
		if manifest.Schema != schema {
			return Manifest{}, fmt.Errorf("不支持的快照 schema %d", manifest.Schema)
		}
		return manifest, nil
	}
}

type archiveReader struct {
	*tar.Reader
	file *os.File
	gzip *gzip.Reader
}

func (a *archiveReader) Close() error {
	if err := a.gzip.Close(); err != nil {
		_ = a.file.Close()
		return err
	}
	return a.file.Close()
}

func openArchive(archive string) (*archiveReader, error) {
	file, err := os.Open(archive) // #nosec G304 -- 用户指定快照
	if err != nil {
		return nil, err
	}
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &archiveReader{Reader: tar.NewReader(gzipReader), file: file, gzip: gzipReader}, nil
}

func cleanRelative(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") {
		return "", fmt.Errorf("路径 %q 必须使用正斜杠且非空", value)
	}
	cleaned := path.Clean(value)
	if cleaned != value || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("路径 %q 不是安全的相对路径", value)
	}
	return cleaned, nil
}

func safeJoin(root, relative string) (string, error) {
	cleaned, err := cleanRelative(relative)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(cleaned))
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if absolute != base && !strings.HasPrefix(absolute, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径 %q 逃逸出目标目录", relative)
	}
	return absolute, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- 显式清单路径
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	return hashReader(file)
}

func hashReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
