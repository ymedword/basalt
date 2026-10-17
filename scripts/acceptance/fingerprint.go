package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func ignored(path string) bool {
	p := filepath.ToSlash(path)
	for _, prefix := range []string{".git", ".loop", ".basalt", "bin", "evals/results"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	b := filepath.Base(path)
	// run-state.md 是过程状态（失败台账只追加、勾选随实现翻转），不是源码/判据输入。
	// run-state.md is process state, so it must not churn source or control fingerprints.
	return b == "run-state.md" || b == "basalt.yaml" || b == ".env" || strings.HasSuffix(b, ".local.env") || strings.HasPrefix(b, ".tmp-")
}

// taskCheckbox 只归一化 tasks.md 的勾选位：勾选是过程状态，正文变化仍会改变指纹。
// Only the checkbox marker is normalized; any other edit to tasks.md still changes the hash.
var taskCheckbox = regexp.MustCompile(`(?m)^(\s*- \[)[ xX]\]`)

func normalizeTaskCheckboxes(raw []byte) []byte {
	return taskCheckbox.ReplaceAll(raw, []byte("$1 ]"))
}

// hashPaths 记录路径、内容与权限；缺失路径也纳入指纹，不跟随符号链接越界。
// hashPaths includes names, bytes and permission bits; missing paths are explicit.
func hashPaths(root string, paths []string) (string, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	h := sha256.New()
	slices.Sort(paths)
	for _, rel := range slices.Compact(paths) {
		info, err := handle.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(h, "%s\x00missing\x00", rel)
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\x00", filepath.ToSlash(rel), info.Mode().String())
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink not supported in harness inputs: %s", rel)
		}
		if info.IsDir() {
			directory, err := handle.Open(rel)
			if err != nil {
				return "", err
			}
			entries, err := directory.ReadDir(-1)
			closeErr := directory.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
			children := make([]string, 0, len(entries))
			for _, e := range entries {
				child := filepath.Join(rel, e.Name())
				if !ignored(child) {
					children = append(children, child)
				}
			}
			childHash, err := hashPaths(root, children)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(h, childHash)
		} else {
			f, err := handle.Open(rel)
			if err != nil {
				return "", err
			}
			if filepath.Base(rel) == "tasks.md" {
				raw, readErr := io.ReadAll(f)
				if err := errors.Join(readErr, f.Close()); err != nil {
					return "", err
				}
				_, _ = h.Write(normalizeTaskCheckboxes(raw))
			} else if _, copyErr := io.Copy(h, f); copyErr != nil {
				_ = f.Close()
				return "", copyErr
			} else if err := f.Close(); err != nil {
				return "", err
			}
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sourceFingerprint(root string) (string, error) {
	// Git includes tracked edits and non-ignored untracked files, not only HEAD.
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		return "", errors.New("source fingerprint requires a Git worktree")
	}
	var paths []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" && !ignored(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return "", errors.New("empty source fingerprint")
	}
	return hashPaths(root, paths)
}
