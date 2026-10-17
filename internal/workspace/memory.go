package workspace

// 本文件是工作区记忆路径字面量的**唯一事实源**（颗粒度文档 §3.1 注 4）：`memory`
// 包的两档载体都经 `MemoryFilePath` 取路径，路径字面量此后只此一处。既有的另外两处
// （`templates.go` 的模板与目录清单、`tool/policy.go` 的 `memoryDirName`）**不顺手
// 改写**——那是范围外重构；漂移由 R7 守住：`write_file` 写 `memory/<任意路径>` 必须
// 被拒，`policy.go` 的字面量一漂，R7 当场红。
// The single source of truth for workspace memory path literals (note 4).

// memoryRootDir 是记忆目录名（相对工作区根）。
// memoryRootDir is the memory directory name relative to the workspace root.
const memoryRootDir = "memory"

// MemoryFilePath 返回某一层长期记忆的 markdown 载体路径——**相对工作区根、无前导
// 斜杠、恒为正斜杠**（两档平台同一字面量；**不得**改用 `filepath.Join`，它在
// Windows 上会产出反斜杠，与契约字面量不符）。
// MemoryFilePath returns the markdown carrier path of one memory layer, relative
// to the workspace root, always slash-separated.
//
// `agent == ""` 即全局层（`core.GlobalScope`）→ `memory/MEMORY.md`；否则为 Agent 层
// → `memory/agents/<agent>/MEMORY.md`。两层都**不在此处建目录**：全局层文件随工作区
// 骨架生成，`memory/agents/` 有意不预建、Agent 层文件在首次写入时按需创建。
// An empty agent name is the global layer. Neither path is created here: the
// global file comes with the workspace skeleton and the agent layer appears on
// first write.
//
// 路径逃逸不做本地校验（签名无错误返回值，§3.1 逐字）——越界由 `os.Root` 兜底。
// Escapes are left to os.Root rather than validated here.
func MemoryFilePath(agent string) string {
	if agent == "" {
		return memoryRootDir + "/MEMORY.md"
	}
	return memoryRootDir + "/agents/" + agent + "/MEMORY.md"
}
