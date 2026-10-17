package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是三件内置文件工具（技术方案 §4.1.1、契约 §5.1）：read_file / write_file / list_dir。
// 两条纪律贯穿全篇：① 授权只经 **core.Policy 接口**判定——本文件不出现任何具体策略实现
// （授权与执行分离，宪法 VI）；② 路径**不得**从 context、全局变量或进程环境猜"当前是哪个
// Agent"（core.Tool 文档逐字），调用方经 ToolEnv 显式传入，相对路径以**当前 Agent 自有
// 目录**为基准——AGENT.md 正文里的 `scripts/x.py` 因此可移植（技术方案 §4.3）。
// The three builtin file tools of Technical Solution §6.1 / contract §5.1. Two rules run
// through the file: authorization goes through the core.Policy interface only, and every
// path is resolved from the explicitly passed ToolEnv rather than guessed from context.

// maxReadFileBytes 是 read_file 的单文件读取上限（契约 §5.1：256 KB，超出返回可读错误
// 并提示改用 shell 工具）。
// The 256 KB per-file read cap of contract §5.1.
const maxReadFileBytes = 256 * 1024

// workspaceRootPrefixName 是工作区根在**模型可见文本**里的字面前缀：技能卡片与 L1 注入
// 逐字写的就是 `.basalt/skills/<name>/SKILL.md`，与 `basalt init` 落盘的工作区目录名同源。
// 本包按字面量自行定位、**不**反向依赖 `workspace` / `cli` 包（与 agentsDirName 等骨架
// 字面量同一处置）。锚在字面量而非 `filepath.Base(root)` 上是有意的：卡片不随根目录实际
// 名字变化，按 `Base` 匹配会在根目录别名或测试夹具上悄悄失效。
// The literal prefix the L1 disclosure writes; anchoring on it (not on the root's own
// basename) keeps the card form readable regardless of where the root actually lives.
const workspaceRootPrefixName = ".basalt"

// listEmptyContent 是空目录的结果文案：空串与"工具什么都没返回"在模型看来无法区分。
// An explicit empty-directory text: an empty string is indistinguishable from no output.
const listEmptyContent = "(空目录)"

// 三件工具的入参 Schema——**内联 JSON Schema 常量**（颗粒度文档 §3.1：测试逐字钉住），
// 其中 required 数组是执行侧必填校验的**唯一依据**（core 按它判 invalid_input 并仍写审计行）。
// Inline JSON Schema literals; core reads the required array to judge missing input.
const (
	readFileInputSchema  = `{"type":"object","properties":{"path":{"type":"string","description":"要读取的文件路径：绝对路径，或相对当前 Agent 目录的路径；技能卡片给出的 .basalt/skills/... 形态同样可用"}},"required":["path"]}`
	writeFileInputSchema = `{"type":"object","properties":{"path":{"type":"string","description":"目标文件路径：绝对路径，或相对当前 Agent 目录的路径；父目录必须已存在，不会自动创建"},"content":{"type":"string","description":"写入内容（整文件覆盖写）"}},"required":["path","content"]}`
	listDirInputSchema   = `{"type":"object","properties":{"path":{"type":"string","description":"要列出的目录路径：绝对路径，或相对当前 Agent 目录的路径；只列本层，不递归"}},"required":["path"]}`
)

// ReadFileTool 是 read_file：读工作区文件并把内容回填给模型。读取落点同样经
// **os.OpenRoot 的相对根句柄**——可读面（自有目录 / 技能库 / 显式授权的跨 Agent 目录）
// 全在工作区根之内，句柄因此能独立于授权判定把这次读取钉在根之内；投影与读取之间被换成
// 越界软连接的那一段由它兜住（与写入侧同一条纪律）。限流也由读取侧负责：读取**边读边截**
// （见 readWithinRoot），判定之后长大的文件不会先被整份读进内存。
// ReadFileTool reads through a relative root handle as well: every readable scope lives
// under the workspace root, so the handle pins the read inside the root independently of
// the authorization decision.
type ReadFileTool struct {
	policy core.Policy
	root   string
}

var _ core.Tool = (*ReadFileTool)(nil)

// NewReadFileTool 装配 read_file：policy 做授权判定，root 是工作区根（`.basalt`）。
// NewReadFileTool wires read_file; authorization is delegated to policy.
func NewReadFileTool(policy core.Policy, root string) *ReadFileTool {
	return &ReadFileTool{policy: policy, root: root}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *ReadFileTool) Name() string { return "read_file" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *ReadFileTool) Description() string {
	return "读取文本文件内容（可读范围：当前 Agent 自己的目录与 .basalt/skills；单文件上限 256 KB）"
}

// InputSchema 返回内联 Schema（测试逐字钉住）。
// InputSchema returns the inline schema, pinned verbatim by tests.
func (t *ReadFileTool) InputSchema() json.RawMessage { return json.RawMessage(readFileInputSchema) }

// Execute 读文件：判定 → 投影 → 经根句柄读取。读取落在**投影后**的真实路径上——判定与
// 读取看到的是同一个目标，越界软连接在判定时就被拒（词法比较会被 `ln -s` 绕过）；投影与
// 读取之间的竞争窗口由句柄兜住（与写入侧同一条纪律）。
// Execute authorizes, projects, then reads through the root handle: authorization and the
// actual read see the same target, and the handle closes the swap window between them.
func (t *ReadFileTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeFileToolInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	target := resolveToolPath(t.root, env.AgentDir, args.Path)
	err = t.policy.Check(ctx, core.Action{
		Type:   core.ActionFileRead,
		Target: actionTarget(t.root, target),
		Agent:  env.AgentName,
	})
	if err != nil {
		return core.Result{}, err
	}
	real, err := resolveRealPath(t.root, target)
	if err != nil {
		return core.Result{}, fmt.Errorf("解析读取目标 %s：%w", args.Path, err)
	}
	handle, err := os.OpenRoot(t.root)
	if err != nil {
		return core.Result{}, fmt.Errorf("打开读取根（工作区）：%w", err)
	}
	content, readErr := readWithinRoot(handle, t.root, real, args.Path)
	closeErr := handle.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return core.Result{}, err
	}
	return core.Result{Content: content}, nil
}

// WriteFileTool 是 write_file：把模型给的内容写进当前 Agent 自己的目录。
// 写入落点经 **os.OpenRoot 的相对根句柄**——判定看到的与真正写下去的是同一棵树，
// "判定与写入之间父目录被换成越界软连接"这个窗口由句柄拒绝逃逸路径兜住（颗粒度文档 §3.1；
// `先检查路径、随后普通 open` 证明不了竞争窗口）。
// WriteFileTool writes through a relative root handle: the handle refuses escape paths
// even if the tree is swapped between authorization and the write itself.
type WriteFileTool struct {
	policy   core.Policy
	root     string
	agentDir string
}

var _ core.Tool = (*WriteFileTool)(nil)

// NewWriteFileTool 装配 write_file：policy 做授权判定，root 是工作区根，agentDir 是
// **无运行环境时的回落目录**。正常的注册池是装配期一次构建的全局池（同一实例服务所有
// Agent），Agent 自有目录随每次调用的 ToolEnv 传入并**优先于**构造期取值——两者必须同源，
// 否则"判定的那个 Agent"与"写入的那棵树"会分家。
// agentDir is only the fallback for calls without a runtime environment; the per-call
// ToolEnv.AgentDir wins so that the authorized agent and the written tree stay one.
func NewWriteFileTool(policy core.Policy, root, agentDir string) *WriteFileTool {
	return &WriteFileTool{policy: policy, root: root, agentDir: agentDir}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *WriteFileTool) Name() string { return "write_file" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *WriteFileTool) Description() string {
	return "写入文本文件（整文件覆盖写；可写范围：当前 Agent 自己的目录，脚本目录与技能库只读；父目录必须已存在，不会自动创建）"
}

// InputSchema 返回内联 Schema（测试逐字钉住）。
// InputSchema returns the inline schema, pinned verbatim by tests.
func (t *WriteFileTool) InputSchema() json.RawMessage { return json.RawMessage(writeFileInputSchema) }

// Execute 写文件：判定 → 相对根句柄写入。父目录**不自动创建**：保护规则与"父目录必须
// 已存在"两条由授权判定先行（契约 §5.1），句柄侧只负责把落点钉在写入根之内。
// Execute authorizes first and never creates parent directories; the handle only pins
// the write inside the root.
func (t *WriteFileTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeFileToolInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	base := t.writeBase(env)
	target := resolveToolPath(t.root, base, args.Path)
	err = t.policy.Check(ctx, core.Action{
		Type:   core.ActionFileWrite,
		Target: actionTarget(t.root, target),
		Agent:  env.AgentName,
	})
	if err != nil {
		return core.Result{}, err
	}
	handle, err := os.OpenRoot(base)
	if err != nil {
		return core.Result{}, fmt.Errorf("打开写入根（Agent 自有目录）：%w", err)
	}
	written, writeErr := writeWithinRoot(handle, base, target, args.Path, args.Content)
	closeErr := handle.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return core.Result{}, err
	}
	return core.Result{Content: fmt.Sprintf("已写入 %d 字节：%s", written, args.Path)}, nil
}

// writeBase 定写入根：运行环境给的 Agent 自有目录优先，其次构造期回落值，最后工作区根。
// 落到工作区根只是让流程走下去——授权判定按 Agent 名算自有目录，无 Agent 上下文时写入
// 本就全拒，句柄不会替调用方放宽任何一格。
// The base ladder is runtime agent dir, construction fallback, workspace root; the last
// rung cannot widen access because authorization already refuses agent-less writes.
func (t *WriteFileTool) writeBase(env core.ToolEnv) string {
	switch {
	case env.AgentDir != "":
		return env.AgentDir
	case t.agentDir != "":
		return t.agentDir
	default:
		return t.root
	}
}

// ListDirTool 是 list_dir：只列本层，不递归、不跟随软连接。
// ListDirTool lists one level; it never recurses and never follows symlinks.
type ListDirTool struct {
	policy core.Policy
	root   string
}

var _ core.Tool = (*ListDirTool)(nil)

// NewListDirTool 装配 list_dir：policy 做授权判定，root 是工作区根（`.basalt`）。
// NewListDirTool wires list_dir; authorization is delegated to policy.
func NewListDirTool(policy core.Policy, root string) *ListDirTool {
	return &ListDirTool{policy: policy, root: root}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *ListDirTool) Name() string { return "list_dir" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *ListDirTool) Description() string {
	return "列出目录条目，每行「类型 大小 名字」（类型取 file / dir / link / other）；不递归、不跟随软链接"
}

// InputSchema 返回内联 Schema（测试逐字钉住）。
// InputSchema returns the inline schema, pinned verbatim by tests.
func (t *ListDirTool) InputSchema() json.RawMessage { return json.RawMessage(listDirInputSchema) }

// Execute 列目录：判定 → 投影 → 读一层。条目按名字排序由 os.ReadDir 保证（标准库契约），
// 不额外重排——多一处排序就多一处与平台读数脱钩的机会。
// Execute lists one projected level; name ordering comes from os.ReadDir's contract.
func (t *ListDirTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeFileToolInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	target := resolveToolPath(t.root, env.AgentDir, args.Path)
	err = t.policy.Check(ctx, core.Action{
		Type:   core.ActionFileRead,
		Target: actionTarget(t.root, target),
		Agent:  env.AgentName,
	})
	if err != nil {
		return core.Result{}, err
	}
	real, err := resolveRealPath(t.root, target)
	if err != nil {
		return core.Result{}, fmt.Errorf("解析目录 %s：%w", args.Path, err)
	}
	// 先判存在与类型再列：契约 §5.1 给 list_dir 的失败类别只有 not_found / sandbox_denied，
	// 而直接 `os.ReadDir(文件)` 的平台错误会让同一输入在 Windows 归 not_found、在 Unix 归
	// runtime_error——审计分类按平台分叉对可审计产品是硬伤，故这里自己判、跨平台一致。
	info, err := os.Stat(real)
	if err != nil {
		return core.Result{}, fmt.Errorf("列出目录 %s：%w", args.Path, err)
	}
	if !info.IsDir() {
		return core.Result{}, fmt.Errorf("列出目录 %s：目标不是目录（读文件请用 read_file）：%w", args.Path, fs.ErrNotExist)
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return core.Result{}, fmt.Errorf("列出目录 %s：%w", args.Path, err)
	}
	content, err := formatDirEntries(entries)
	if err != nil {
		return core.Result{}, err
	}
	return core.Result{Content: content}, nil
}

// fileToolInput 是三件工具共用的入参形态：path 三件都必填，content 只 write_file 用。
// 字段**是否出现**由各工具 Schema 的 required 判（执行侧据此回 invalid_input 并写审计行），
// 本结构体只承载解出的值。
// The shared input shape; presence is judged by each tool's schema, not here.
type fileToolInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// decodeFileToolInput 解出共用入参，错误里点名工具——失败信息要能让模型知道改哪一处。
// **字段类型**不符是调用方的入参问题，不是本仓缺陷：失败载体是 `*core.InvalidInputError`，
// 执行器据此分类 `invalid_input`；文案与改造前逐字一致（只换承载类型）。
// decodeFileToolInput names the tool in the error so the model knows what to fix; a type
// mismatch is an invalid input, never a defect of this repository.
func decodeFileToolInput(toolName string, input json.RawMessage) (fileToolInput, error) {
	var args fileToolInput
	if err := json.Unmarshal(input, &args); err != nil {
		return fileToolInput{}, &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("解析 %s 入参：%s", toolName, err.Error()),
		}
	}
	return args, nil
}

// check 兜"路径为空"：`{"path":""}` 在 Schema 上合法（键存在），但它没有可解释的目标，
// 放它进解析只会拿基准目录当目标，得到与模型意图无关的结果。取值问题与解码失败**同类**
// （`*core.InvalidInputError`）——执行器入口的必填校验与这里的两处判别产出同一个 `error_kind`。
// An empty path passes the schema (the key exists) but has no interpretable target; it is
// the same invalid-input class as a decode failure.
func (a fileToolInput) check(toolName string) error {
	if a.Path == "" {
		return &core.InvalidInputError{Tool: toolName, Reason: fmt.Sprintf("%s 的 path 不能为空", toolName)}
	}
	return nil
}

// resolveToolPath 把模型给的路径解析成**投影前**的字面绝对路径。基准顺位：
// ① 绝对路径原样；② 相对路径以 base（本次调用所属 Agent 的自有目录）为基准——工具拿到的
// 环境必须显式传入（core.Tool 文档），"当前 Agent 目录"正是 AGENT.md 正文里相对路径的
// 基准（技术方案 §4.3）；③ base 为空（无 Agent 上下文）时回落工作区根——契约 §5.1
// 「相对工作区根」的字面口径；④ 首段是工作区根字面前缀（`.basalt`）的形态按**工作区根**
// 解析：技能卡片逐字注入的就是这个形态，read_file 必须能吃下（US-3 §2.2 约束 6）。
// 纯词法：不碰文件系统——投影与授权分别是 resolveRealPath 与 Policy 的事。
// Lexical only: projection and authorization belong to resolveRealPath and Policy.
func resolveToolPath(root, base, raw string) string {
	cleaned := filepath.Clean(raw)
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	workspaceRoot := filepath.Clean(root)
	first, rest, found := strings.Cut(cleaned, string(filepath.Separator))
	if found && first == workspaceRootPrefixName {
		if rest == "" {
			return workspaceRoot
		}
		return filepath.Join(workspaceRoot, rest)
	}
	if base == "" {
		return filepath.Join(workspaceRoot, cleaned)
	}
	return filepath.Join(filepath.Clean(base), cleaned)
}

// actionTarget 把解析后的绝对路径折成 Policy 的 Target 形态：同卷时给**相对工作区根**的
// 形态——拒绝文案里因此不出现绝对路径（技术方案 §6.2.2：错误文案不得回显绝对路径），
// 越界目标则以 `..` 起头的形态暴露给模型；跨卷（Rel 报错）只能原样给绝对路径。
// The workspace-relative form keeps absolute paths out of refusal copy.
func actionTarget(root, target string) string {
	rel, err := filepath.Rel(filepath.Clean(root), target)
	if err != nil {
		return target
	}
	return rel
}

// readWithinRoot 经相对根句柄读一个**已投影**的普通文件。三件事与写入侧同源：先算 target
// 相对 base 的形态，再让 os.Root 自己拒绝逃逸（`..` / 绝对路径 / 软连接跳出根）——投影与
// 这次读取之间被换掉的那一段由句柄兜住，`先检查路径、随后普通 open` 兜不住。契约三件事
// 照旧：UTF-8 宽容解码（非法字节以替换字符呈现，**不**因编码拒绝读取）、单文件 256 KB
// 上限（超出给可读错误并提示改用 shell 工具）、目录拒读并指路 list_dir。
// 读取**边读边限流**：最多多读 1 字节用于判溢出——判定之后长起来的大文件不会先被整份
// 读进内存（`先 Stat 再整份 ReadFile` 只是在读完之后才发现它已经很大）。
// Reads are streamed through a limit: at most one byte past the cap is ever buffered.
func readWithinRoot(handle *os.Root, base, target, display string) (string, error) {
	rel, err := filepath.Rel(filepath.Clean(base), target)
	if err != nil {
		return "", fmt.Errorf("读取文件 %s：目标不在读取根所在卷上", display)
	}
	info, err := handle.Stat(rel)
	if err != nil {
		return "", fmt.Errorf("读取文件 %s：%w", display, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("读取文件 %s：目标是目录，列目录请用 list_dir", display)
	}
	if info.Size() > maxReadFileBytes {
		return "", fileTooLargeError(display, info.Size())
	}
	file, err := handle.Open(rel)
	if err != nil {
		return "", fmt.Errorf("读取文件 %s：%w", display, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxReadFileBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", fmt.Errorf("读取文件 %s：%w", display, err)
	}
	// 判定与读取之间文件长大了：同一个上限对两处读数都要成立。超限文案里的字节数取
	// **重新读到的实际大小**（取值失败时退回落下来的读数——它是"至少这么多"的下界）。
	if len(data) > maxReadFileBytes {
		size := int64(len(data))
		if grown, statErr := handle.Stat(rel); statErr == nil && grown.Size() > size {
			size = grown.Size()
		}
		return "", fileTooLargeError(display, size)
	}
	return strings.ToValidUTF8(string(data), string(utf8.RuneError)), nil
}

// fileTooLargeError 组装超限的可读错误（契约 §5.1 点名提示改用 shell 工具处理大文件）。
// fileTooLargeError carries the contract-mandated hint to use the shell tool instead.
func fileTooLargeError(display string, size int64) error {
	return fmt.Errorf("读取文件 %s：大小 %d 字节，超过单文件上限 %d 字节；请改用 shell 工具处理大文件",
		display, size, maxReadFileBytes)
}

// writeWithinRoot 经相对根句柄写入：先算 target 相对 base 的形态，再让 os.Root 自己拒绝
// 逃逸（`..` / 绝对路径 / 软连接跳出根）。逃逸时句柄报错——这就是"句柄才是保证"的落点：
// 授权判定与这次写入之间被换掉的那一段由它兜住，`先检查路径、随后普通 open` 兜不住。
// The handle is the actual guarantee: it refuses escape paths even when the tree was
// swapped between the authorization check and this write.
func writeWithinRoot(handle *os.Root, base, target, display, content string) (int, error) {
	rel, err := filepath.Rel(filepath.Clean(base), target)
	if err != nil {
		return 0, fmt.Errorf("写入文件 %s：目标不在写入根所在卷上", display)
	}
	file, err := handle.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("写入文件 %s：%w", display, err)
	}
	written, writeErr := file.WriteString(content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return written, fmt.Errorf("写入文件 %s：%w", display, err)
	}
	return written, nil
}

// formatDirEntries 按契约把条目渲染成逐行「类型 大小 名字」文本：类型取 file / dir /
// link / other，大小只对普通文件有意义（其余按 `-` 占位——"0 字节"与"没有大小"是两回事）。
// One line per entry; sizes are meaningful for regular files only.
func formatDirEntries(entries []fs.DirEntry) (string, error) {
	if len(entries) == 0 {
		return listEmptyContent, nil
	}
	var builder strings.Builder
	for _, entry := range entries {
		kind, size, err := describeEntry(entry)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&builder, "%s %s %s\n", kind, size, entry.Name())
	}
	return builder.String(), nil
}

// describeEntry 取一个条目的类型名与大小文本。类型与大小都取自 DirEntry.Info——它**不跟随
// 软连接**（返回链接自身的信息），故指向别处的链接只会以 `link` 出现在本层，不会让被指向的
// 东西冒充本层条目；取不到信息即失败上抛，不静默降级成占位值。
// DirEntry.Info describes the link itself, so symlinks are reported rather than followed.
func describeEntry(entry fs.DirEntry) (string, string, error) {
	info, err := entry.Info()
	if err != nil {
		return "", "", fmt.Errorf("取条目 %s 的信息：%w", entry.Name(), err)
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "link", "-", nil
	case info.IsDir():
		return "dir", "-", nil
	case info.Mode().IsRegular():
		return "file", strconv.FormatInt(info.Size(), 10), nil
	default:
		return "other", "-", nil
	}
}
