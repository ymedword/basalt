package tool

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 本文件是路径白名单的**投影层**：每个文件动作在授权判定之前都要先问"这东西的真身在哪"。
// 只做词法规范化是不够的——`..` 与软连接都能把词法上"在目录内"的路径指到目录外
// （开发操作 §7 陷阱表第 7 条），故判定一律在**真实路径投影**之后做，投影不出来就拒。
// This file projects a requested path onto its real location before any authorization
// decision. Lexical normalisation alone is bypassable, so the projection is mandatory
// and any unresolvable target fails closed.

// resolveRealPath 把 root 相对（或绝对）的 target 投影成真实路径。
// 不可解析（悬空链接 / 链接成环 / 中间目录无权限）→ 返回错误，由调用方**失败关闭**。
// Resolve-and-fail-closed; the caller refuses whenever the projection fails.
func resolveRealPath(root, target string) (string, error) {
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, target)
	}
	return evalRealPath(filepath.Clean(abs))
}

// evalRealPath 逐段投影真实路径；尚不存在的**尾段**按字面保留（write_file 可能新建文件），
// 但若那个名字本身已存在（含悬空链接）则必须投影成功，否则就是断链 → 失败关闭。
// Existing components must resolve; a missing tail is kept verbatim only when the name
// itself does not exist — an existing name that fails to resolve is a broken link.
func evalRealPath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		// 已到卷根仍不存在：路径本身有问题（盘符 / 挂载点不存在）。
		return "", err
	}
	realParent, parentErr := evalRealPath(parent)
	if parentErr != nil {
		return "", parentErr
	}
	candidate := filepath.Join(realParent, filepath.Base(path))
	if _, lstatErr := os.Lstat(candidate); lstatErr == nil {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil {
			return "", resolveErr
		}
		return resolved, nil
	} else if !errors.Is(lstatErr, fs.ErrNotExist) {
		return "", lstatErr
	}
	return candidate, nil
}

// withinDir 判定 real 是否落在 dir 之内（含 dir 自身）——比较双方都必须是已投影的真实路径。
// Windows 上按折叠大小写比较（NTFS 不区分大小写，逐字比较会把同一目录判成两个）。
// withinDir compares already-projected paths, folding case on Windows where the
// filesystem itself is case-insensitive.
func withinDir(real, dir string) bool {
	real, dir = foldCase(filepath.Clean(real)), foldCase(filepath.Clean(dir))
	rel, err := filepath.Rel(dir, real)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// foldCase 在 Windows 上折叠大小写，其余平台原样返回（大小写敏感文件系统上折叠会放宽判定）。
// foldCase lowercases only on Windows: folding elsewhere would loosen the judgement.
func foldCase(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
