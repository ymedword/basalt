package tool

// 本文件是程序目录可执行件的**执行侧**准入（14D §2.2.2「命令·程序目录」/ D3）：三项
// 条件同时满足才自动放行——① 安装清单登记（含 SHA-256）② `EvalSymlinks` 后的真实路径
// 落在 `<程序目录>/runtime/**` ③ 该路径处于安装器设置的只读保护下。**只有路径前缀相同
// 不算可信**：清单里的名字解析到 runtime/ 下的另一个文件同样拒。
// 授权面（`WhitelistPolicy`）按清单**名字**放行到本层；本层用真实路径与内容哈希把
// "名字对"兑现成"文件对"。未命中清单的可执行件不在这里判——那是显式登记
// （`shell.allowed_commands`）与本层无关的面。
// The execution-side half of the program-directory admission: the policy admits by the
// manifest's *names*; this layer cashes that out against the real path and content hash.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// programRuntimeDirName 是 `<程序目录>` 下承载受保护运行时组件的目录名（设计 §2.1）。
// The runtime directory name under the program directory.
const programRuntimeDirName = "runtime"

// ProgramDirEntry 是安装清单登记的一条程序目录可执行件：相对 `<程序目录>/runtime` 的
// 路径 + 安装时的内容哈希。路径可用 `/` 或 `\` 分隔；哈希为小写十六进制 SHA-256。
// One installation-manifest entry: a runtime-relative path and its installed SHA-256.
type ProgramDirEntry struct {
	Path   string
	SHA256 string
}

// programHash 是一次内容哈希的缓存读数：文件大小与修改时间都没变时沿用旧值——否则每次
// 执行都要把几十 MB 的运行时组件完整读一遍（`node` 一类组件在每次 shell 调用上）。
// 尺寸或修改时间一变就重算，缓存不会掩盖内容变动。
// Cached content hash keyed by size and mtime so a change always triggers a rehash.
type programHash struct {
	size    int64
	modTime int64
	sum     string
}

// toSlashPath 把清单里的相对路径规整成 `/` 分隔的可比对形式（先统一分隔符再 Clean，
// 免得 `./a/b` 与 `a/b` 变成两个键）。
// toSlashPath normalizes a manifest path to slash-separated form.
func toSlashPath(path string) string {
	return filepath.ToSlash(filepath.Clean(strings.ReplaceAll(path, `\`, "/")))
}

// checkProgramDir 复核程序目录可执行件的三项条件（只对"可能通过清单自动放行"的目标判）：
// 真实路径不在 `runtime/` 下的不看；清单既没声明同名基名、也没声明这条路径的不看
// （那是显式登记的目标，与清单无关）。其余一律要求"路径在清单里 + 保护生效 + 哈希一致"。
// checkProgramDir judges the three conditions for executables that could have been
// auto-admitted by the manifest; explicitly registered targets are out of this face.
func (r *LocalRunner) checkProgramDir(cmd core.Command) error {
	if r.programRuntime == "" {
		return nil
	}
	// 解析失败时 spawn 同样会失败，交给执行错误如实归因（不把"没找到"改写成策略拒绝）。
	real, err := r.resolveExecutable(cmd)
	if err != nil || !withinDir(real, r.programRuntime) {
		return nil
	}
	rel, err := filepath.Rel(r.programRuntime, real)
	if err != nil {
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	wantSum, pathDeclared := r.programPaths[foldCase(filepath.ToSlash(rel))]
	if !pathDeclared {
		if _, nameDeclared := r.programNames[foldCase(executableBaseName(real))]; !nameDeclared {
			return nil
		}
		// 名字在清单里、解析出来的却不是清单登记的那个文件：只有路径前缀相同不算可信。
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	info, err := os.Stat(real)
	if err != nil {
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	// 条件 ③：只读保护生效。判据是"没有任何写位"——安装器设的 ACL 在 Go 的 mode 位上
	// 只体现属主写位（Windows 的只读属性同样落在 0200），这里不冒充能读完整 ACL；
	// 保护没生效时降级为需显式登记（页面标注归管理台，不在本层）。
	// Condition ③: no write bits. The mode cannot prove a full ACL; when protection is
	// absent the executable falls back to explicit registration.
	if info.Mode().Perm()&0o222 != 0 {
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	sum, err := r.hashProgram(real, rel, info)
	if err != nil {
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	if sum != wantSum {
		return deny(ruleProgramDirRefused, cmd.Executable, guidanceProgramDirRefused)
	}
	return nil
}

// executableBaseName 取可执行件的基名并去掉可执行后缀（`node.exe` → `node`），与清单
// 声明的名字同一口径——`IsShellName` 的折叠方式与这里一致。
// executableBaseName mirrors the manifest naming rule (basename without suffix).
func executableBaseName(real string) string {
	base := filepath.Base(real)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// hashProgram 返回文件内容的 SHA-256（小写十六进制），按 (大小, 修改时间) 缓存。
// 读取经 `<程序目录>/runtime` 的根句柄 + 清单里的相对路径：要哈希的就是被准入的那一份，
// 根句柄把"只可能是 runtime/ 下这个相对路径"写进代码，而不是只写在注释里。
// hashProgram returns the content hash, cached until size or mtime changes; the read goes
// through a root handle on the runtime directory plus the manifest's relative path.
func (r *LocalRunner) hashProgram(real, rel string, info os.FileInfo) (string, error) {
	key := foldCase(filepath.Clean(real))
	r.programMu.Lock()
	cached, ok := r.programHashes[key]
	r.programMu.Unlock()
	mtime := info.ModTime().UnixNano()
	if ok && cached.size == info.Size() && cached.modTime == mtime {
		return cached.sum, nil
	}
	root, err := os.OpenRoot(r.programRuntime)
	if err != nil {
		return "", fmt.Errorf("打开程序目录运行时根 %s：%w", r.programRuntime, err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(rel)
	if err != nil {
		return "", fmt.Errorf("打开程序目录可执行件：%w", err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	// 读多少算多少：哈希面就是整份文件，不设上限（截断哈希会把"内容已变"报成一致）。
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("读取程序目录可执行件：%w", err)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	r.programMu.Lock()
	r.programHashes[key] = programHash{size: info.Size(), modTime: mtime, sum: sum}
	r.programMu.Unlock()
	return sum, nil
}
