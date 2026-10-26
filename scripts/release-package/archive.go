package main

// 本文件实现文件树复制、压缩与摘要。发行包里既有普通文件也有符号链接（Linux 的
// Node / Python 发行版用链接共享可执行件），复制与压缩都必须保留链接本身，不能把
// 它展开成字节——展开会让 `python3` 变成一份独立副本，升级时两处版本可能漂移。
// This file copies trees, zips them and hashes files. Symlinks are preserved as links
// so the Unix runtime keeps its shared executables instead of silently duplicating them.

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// zipFixedTime 是压缩包条目的固定时间戳：输入相同则归档逐字节相同，复核 SHA-256
// 时不因墙钟时间变化。
// zipFixedTime keeps archives byte-identical across runs for identical inputs.
var zipFixedTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

type treeStats struct {
	bytes int64
	files int
}

// copyFile 复制一个普通文件并保留权限位。
// copyFile copies one regular file and preserves its permission bits.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyFileMode(src, dst, info.Mode())
}

// copyFileMode 以指定权限复制文件；目标已存在时截断重写。
// copyFileMode copies a file with an explicit mode.
func copyFileMode(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- 路径来自清单或已验证的运行时目录，不是请求输入
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()) // #nosec G302,G304 -- 发行包文件需要组/其他可读；路径来自清单
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyTree 递归复制目录，保留目录权限与符号链接；链接不会跟随到目标。
// copyTree copies a tree, preserving directory modes and symlinks without following them.
func copyTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstRoot, info.Mode().Perm())
		}
		target := filepath.Join(dstRoot, rel)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 发行包目录对所有用户可读
				return err
			}
			return os.Symlink(link, target) // #nosec G122 -- 打包输入是维护者预置的运行时目录，不是请求输入
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 发行包目录对所有用户可读
				return err
			}
			return copyFileMode(path, target, info.Mode())
		}
	})
}

// zipTree 把 srcRoot 压成 zip（相对路径、固定时间戳），返回压缩前的总字节与文件数。
// zipTree writes srcRoot into a deterministic zip and returns the pre-compression stats.
func zipTree(srcRoot, archivePath string) (treeStats, error) {
	var stats treeStats
	archive, err := os.Create(archivePath) // #nosec G304 -- 输出路径由调用方显式给出
	if err != nil {
		return stats, err
	}
	writer := zip.NewWriter(archive)
	walkErr := filepath.WalkDir(srcRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		switch {
		case entry.IsDir():
			header := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: zipFixedTime}
			header.SetMode(info.Mode())
			if _, err := writer.CreateHeader(header); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header := &zip.FileHeader{Name: name, Method: zip.Store, Modified: zipFixedTime}
			header.SetMode(info.Mode())
			// 链接内容是目标字符串，不是被链接文件的内容——解包方据此还原链接。
			// A symlink entry stores its target string, never the linked file's bytes.
			entryWriter, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(entryWriter, target); err != nil {
				return err
			}
			stats.files++
		default:
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = name
			header.Method = zip.Deflate
			header.Modified = zipFixedTime
			entryWriter, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			src, err := os.Open(path) // #nosec G304,G122 -- 路径来自维护者预置的待打包文件树，不是请求输入
			if err != nil {
				return err
			}
			written, copyErr := io.Copy(entryWriter, src)
			closeErr := src.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			stats.bytes += written
			stats.files++
		}
		return nil
	})
	if walkErr != nil {
		_ = writer.Close()
		_ = archive.Close()
		return stats, walkErr
	}
	if err := writer.Close(); err != nil {
		_ = archive.Close()
		return stats, err
	}
	if err := archive.Close(); err != nil {
		return stats, err
	}
	return stats, nil
}

// sha256File 返回文件的十六进制 SHA-256。
// sha256File returns the hex SHA-256 of a file.
func sha256File(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- 路径由调用方显式给出
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// writeReadme 按模板渲染包内 README（只替换版本与平台标签两个占位）。
// writeReadme renders the in-package README from the template.
func writeReadme(templatePath, programDir, version, label string) error {
	raw, err := os.ReadFile(templatePath) // #nosec G304 -- 模板路径由调用方显式给出
	if err != nil {
		return fmt.Errorf("读取 README 模板：%w", err)
	}
	text := string(raw)
	text = strings.ReplaceAll(text, "{{VERSION}}", version)
	text = strings.ReplaceAll(text, "{{PLATFORM}}", label)
	target := filepath.Join(programDir, "README.txt")
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil { // #nosec G306,G703 -- 包内公开说明；目标名固定、目录来自打包暂存区
		return fmt.Errorf("写出包内 README：%w", err)
	}
	return nil
}

// platformLabel 返回文档里展示的平台名；架构取实际构建架构，不假定 amd64。
// platformLabel is the human-readable platform label; the arch comes from the build.
func platformLabel(goos, goarch string) string {
	switch goos {
	case "windows":
		return "Windows " + goarch
	case "linux":
		return "Linux " + goarch
	default:
		return goos + " " + goarch
	}
}
