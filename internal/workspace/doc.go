// Package workspace 管理工作区：骨架的幂等创建与只读清点、Agent 目录装载、
// system prompt 第一段拼装，以及写模式命令的工作区实例锁。
// Package workspace manages the workspace: idempotent skeleton creation and
// read-only inventory, agent-directory loading, section-one prompt assembly,
// and the instance lock guarding write-mode commands.
//
// 工作区 = 当前工作目录下的 .basalt/（目录约定见 docs/Compatibility.md §2.1）；
// 本包只碰文件系统，不碰数据库——建库归 storage.Bootstrap。
//
// 包内三块职责：
//   - AgentLoader：把 .basalt/agents/<name>/AGENT.md 经 frontmatter 解析与加载期校验
//     派生成 core.Profile（任一规则不过则整只不注册并点名 WARN）；本包是**唯一**的
//     解析与校验路径，命令面不另立一份。
//   - ContextLoader：按声明面拼装 system prompt 第一段；每次调用重读文件、不缓存，
//     now 由调用方显式传入。
//   - InstanceLock：写给模式命令（create / delete）的准入锁——OS 级独占文件锁，
//     非阻塞获取，诊断记录写在锁定区之外供竞争方读取（契约见 run-and-errors.md §4.3.1）。
//
// The workspace is .basalt/ under the current working directory. This package
// touches the file system only; database bootstrap belongs to storage. Its three
// areas are agent loading (the single parse-and-validate path), section-one
// prompt assembly (stateless, re-reads on every call), and the write-mode
// instance lock.
package workspace
