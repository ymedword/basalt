# D4 docs-diff —— 文档差异报告（只读 MCP + 记忆）

每天对比一份**只读**文档库与上次基线：给出**新增 / 修改 / 删除**三个集合，并为每份
文档记录**内容哈希**（未变的哈希一致、变更的哈希不同）。首跑只记基线、不编造变更；
没有变化也照样出报告。跨会话的「上次报了什么」来自**记忆**，不靠对话历史。

这个 Demo 的看点是：**变更判定只认内容哈希**——`modified_at` 和文件大小只用来做
「两次读取是否同源」的一致性检查，不参与差异判定；换台机器、换个目录、改 JSON 缩进
都不会改变报告结论。差异由 Python 脚本算，模型只负责取数、解释与写报告。

> 档位：Demo（不是模板）。**业务脚本 22 个单测 / 夹具 / `docs-readonly` 单测 / 真实
> MCP → 脚本三轮链路（本机 Windows）已验证**；**真实模型驱动的完整 Agent
> Run 未验证**（本轮无模型授权）；**真实通知接收未验证**；**自动触发 / 人工补跑未在
> 该实例取证**。证据档位见文末。

## 业务目标

回答「文档库这一轮到底改了什么」。要点：

- 差异只看**内容哈希**：`tooling.md` 只改了一个空格也算修改；`modified_at` 变了但内容
  没变仍判「未变」——时间戳不参与判定，只在两份快照之间做同源核对；
- 三个集合互斥且**守恒**：新增 + 修改 + 未变 = 本次文档数；删除 + 修改 + 未变 = 基线
  文档数。任何一边对不上都说明输入有问题，脚本直接报错，不写半截结果；
- 首跑 `baseline_missing=true`：只记录基线，**不**出现 `changes_detected` 字段，更不会
  把被记录的文档写成「新增」；
- 无变化也出报告：`changes_detected=false` 时照样写 `changes.json` 与 `changes.md`，
  message 写明「本次无变化」；
- 删除的文档**不读正文**（它已经不在了），只列路径与旧哈希；只读正文的范围限定在
  新增 / 修改的文档。

## 工具清单与 `tools:` 声明

| 工具 | 来源 | 用途 |
| --- | --- | --- |
| `list_documents` | `docs-readonly` MCP（自研） | 列出只读库全部 Markdown：`{path,size,modified_at}` |
| `document_metadata` | `docs-readonly` MCP（自研） | 逐份取 `{path,size,modified_at,sha256}` |
| `read_document` | `docs-readonly` MCP（自研） | 读新增 / 修改文档的正文（纯文本，不是 JSON） |
| `recall_memory` | 内置 | 取跨会话记忆基线（`query` = `docs-diff`） |
| `save_memory` | 内置 | 每次运行追加一条 `docs-diff 基线 …` 记忆 |
| `read_file` | 内置 | 读脚本产物 `changes.json` |
| `write_file` | 内置 | 落盘 `documents.json` / `metadata.json` / `changes.md` |
| `shell` | 内置 | 运行 `scripts/docs_diff.py`（解释器受 `shell.interpreters` 约束） |

`AGENT.md` 的声明与上表一致；`notify` **不在**声明里——基线 `notify_mode: none`，
工具面也不会补。`SETUP.md` 第 7 节开启通知后，保存响应里的 `changes.tools_added`
会出现 `notify`，由装配路径加入，不要手改 `AGENT.md`。

## 内置域名白名单

**无**。本 Demo 不声明 `http_get`，Basalt 自身不发任何出站 HTTP 请求；文档来自本地
只读目录，不需要在「执行环境」页添加任何域名。

## MCP 自身出网范围

`docs-readonly` 是**自研、纯标准库、零依赖**的最小 MCP server，代码里没有任何网络
调用（不 import `net` / `net/http`），只读一个固定根目录。它作为子进程运行，理论上的
出网能力**不受** Basalt 的域名白名单约束——Basalt 只约束自己发出的 HTTP 请求，不代管
子进程的网络策略。用第三方 MCP（如官方 Filesystem）时请把这条记在心里：只读要靠
server 的**能力边界**保证，而不是只靠 Agent 的 `tools:` 声明。

## 启动程序与版本

MCP 目录里没有这条配方（自研素材，随仓库交付）。`basalt demo install --demo docs-diff`
会把源码导出到 `<工作区>/.basalt/demos/src/docs-readonly` 并打印构建命令：

```sh
go -C <工作区>/.basalt/demos/src/docs-readonly build -o <absolute-path-to-docs-readonly-binary> .
```

导出目录自带安装器生成的最小 `go.mod`；从源码检出构建时改用
`go build ./examples/shared/mcp/docs-readonly`。

- `serverVersion` 是 `0.1.0`，`initialize` 回显 `2024-11-05`；
- 已实测（本机 Windows）：`initialize` → `tools/list` → `tools/call` 全链路通过，
  `tools/list` 恰为 `list_documents` / `read_document` / `document_metadata` 三个；
- stdio 启动同样要过命令授权：`mcp_servers.yaml` 里的 `command` 必须**逐字**出现在
  `shell.allowed_commands` 里（按整串比对，路径写法不一致就不放行）；
- 根目录用 `-root <绝对路径>` 显式指定；不传时取 `$DOCS_READONLY_ROOT`，再缺省为
  进程当前目录（不建议依赖缺省，容易把根指到意料之外的地方）。

## 凭据

**无**。本地只读目录 + 自研 server，不需要令牌或密码。`SETUP.md` 里也不会出现任何
明文凭据；`shell.env_passthrough` 保持空数组。

## 依赖与固定版本

| 依赖 | 版本 | 用途 |
| --- | --- | --- |
| Python | 3.12（标准库即可） | 运行 `scripts/docs_diff.py`（`shell.interpreters`） |
| Go | 1.27.2（构建版本） | 构建 `examples/shared/mcp/docs-readonly`（主 module 内、零第三方依赖；安装器导出源码时生成 `go.mod`） |
| Basalt | v0.1.0 | 运行底座（调度、MCP、记忆、审计） |

脚本零第三方依赖：`docs_diff.py` 只用 `argparse` / `hashlib` / `json` / `os` / `sys` /
`tempfile` / `datetime` / `pathlib`。`docs-readonly` 是纯标准库 Go 程序（主 module 内
`package main`，随根 `go test ./...` 与 `make example-tests` 一起测；导出目录里的
`go.mod` 由安装器生成）。

## 读写文件范围

| 路径 | 谁写 | 说明 |
| --- | --- | --- |
| `documents.json`（Agent 根目录） | 模型（`write_file`） | `list_documents` 响应**原样**落盘，**不得**重排或改字段 |
| `metadata.json`（Agent 根目录） | 模型（`write_file`） | `{"documents":[...]}`，每项是 `document_metadata` 的结果**原样** |
| `runs/<时间戳>/changes.json` | 脚本（原子替换） | 机器可读差异结果；父目录由脚本创建 |
| `runs/<时间戳>/changes.md` | 模型（`write_file`） | 差异报告，数字全部取自 `changes.json` |
| `state/baseline.json` | 脚本（原子替换） | 跨运行基线（每份文档的内容哈希 + 大小）；**不提交**，已在 `.gitignore` |
| `scripts/` | 只读区 | 模型改不动脚本；这是「哈希由脚本算」的前提 |

`write_file` **不自动创建父目录**：根目录的 `documents.json` / `metadata.json` 没有这个
问题；`changes.md` 写在脚本刚建好的 `runs/<时间戳>/` 里（目录名取自 `report.path`）。

`state/baseline.json` 只保存 `{path: {sha256, size}}` 与观察时间，不保存 `modified_at`
——基线不把时间戳当事实，避免「拷来拷去 mtime 变了就误报修改」。

## 通知路径

- 基线 `notify_mode: none`：**零通知**，工具面里也没有 `notify`；报告只留在
  `runs/<时间戳>/changes.md` 与运行记录里。
- 按 `SETUP.md` 第 7 节改为 `allowlist` 后：每次运行（**含无变化**）发**恰好一条**
  `notify`，内容是结论 message + 计数 + 报告路径。D4 的验收口径是「一次 `notify`」；
  出现多条、或只在有变化时才发，都不符合。
- 通知送达不等于端到端恰好一次；真实接收方未授权，本轮只做本地接收器。

## 生效方式

改 `AGENT.md`、`mcp_servers.yaml` 或 `shell.*` 白名单后**必须重启** Basalt；后两类
不热加载。管理台显示「待发布 / 待重启」不等于已生效。

## 固定输入

只读库是 **`examples/shared/fixtures/docs/`**（4 份虚构文档，脱敏，不含真实人员 / 账号 /
凭据）：

| 文件 | 内容 | 大小 | sha256 |
| --- | --- | --- | --- |
| `handbook.md` | 员工手册 | 1717 | `0f2f958431bbb0cb986959051eecd246634a6b3a7ad2d99c2ae94c221eb123bd` |
| `security-basics.md` | 信息安全基线 | 1898 | `8b28568a6d7b175709fb06c8408d7789c49c62702520ec674fab1d59bc6a9058` |
| `teams/data-access.md` | 数据访问与权限申请 | 1641 | `a3341425435fbaeb736c6166aef8e66fcf6ba68e4289a719c7cc375e99658d35` |
| `tooling.md` | 研发工具与账号开通 | 1677 | `51c0ff04d9b4e89ad41b21c8bcf71e5197cc8f6b5aa25151bbded3a76c1483cd` |

`SETUP.md` 会把这份目录**复制**到仓库外当只读根（例如 `D:/basalt-samples/docs`）：
复制只改 `modified_at`，不改内容哈希，所以报告的结论与哈希不受影响。

本目录的 `fixtures/docs/a.md`（值班手册，719 字节，sha256 `8826189d…`）与
`fixtures/docs/b.md`（发布检查单，565 字节，sha256 `3dd24d0a…`）是**单测夹具**：
22 个单测直接读它们的真实字节，保证断言与夹具不会各自漂移；其中一条把 `a.md` 的末尾
换行换成空格（字节数不变、内容不同）也必须判「修改」——变更只认哈希。它们不是运行时只读根。

## 期望业务结果

以下三轮是**实测**（本机 Windows，真实 `docs-readonly` MCP 产出输入 + `docs_diff.py`
执行；`--now` 是脚本参数，用来固定 `observed_at`）：

| 轮次 | 场景 | `mode` | 集合 | 报告 sha256 |
| --- | --- | --- | --- | --- |
| 首跑 | 空 `state/`，4 份文档 | `baseline_created` | 全部记为 `baseline`，`changes_detected` **不存在** | `584b663845a98314aa2d2278505e73cae8b6fa51fd477a3937b2c205d1e06110` |
| 无变化 | 同内容再跑一次 | `compared` | 未变 4 | `7a345be32bd62ec225a3533cba3dc60b06f346f7e0061969219aec4afecdb681` |
| 变更演练 | 追加 `tooling.md`、删 `teams/data-access.md`、加 `teams/faq.md` | `compared` | 新增 `["teams/faq.md"]`、修改 `["tooling.md"]`、删除 `["teams/data-access.md"]`、未变 `["handbook.md","security-basics.md"]` | `c4671cc8c9cc83082acfd727f100e144b0e4bff27bafa9ebb54105ccb2c8fb30` |

首轮的状态文件 sha256 是 `fa0b484f4471be5d434c68955b7d67174274274b0937476742a7f69ec4935554`；
首跑 message 是「无历史基线：本次记录 4 份文档的内容哈希作为基线，不判定新增 / 修改 /
删除」，无变化轮是「本次无变化：4 份文档的内容哈希与基线一致（基线观察时间
2026-10-25T09:15:00+08:00）」。

**复现口径**：报告只装业务事实——不含输入文件路径、不含输入文件哈希、不含
`modified_at`。因此**同一份内容 + 同一个 `--now` ⇒ 逐字节相同的 `changes.json`**，
与目录位置、文件 mtime、JSON 缩进无关；`--now` 不同则 `observed_at` 不同，报告自然
不同。输入 / 报告 / 背景的哈希都在脚本 `stdout`（`input.*.sha256`、`report.sha256`、
`state.sha256`），要核对就抄 stdout，不要从报告里找。

## 失败样例

**只读 MCP 的工具级拒收**（`isError=true`，原文实测）：

| 调用 | 返回 |
| --- | --- |
| `read_document` `../outside.md` | `path 不得包含 ..（越界拒绝）: ../outside.md` |
| `read_document` `teams/../tooling.md` | `path 不得包含 ..（越界拒绝）: teams/../tooling.md` |
| `read_document` `missing.md` | `文档不存在: missing.md` |
| `document_metadata` `../handbook.md` | `path 不得包含 ..（越界拒绝）: ../handbook.md` |

**业务脚本的输入拒收**（`rc=1`，`stderr` 原文实测，全部零写入）：

```text
docs_diff: metadata 缺少文档的元信息：teams/data-access.md, tooling.md（list_documents 有、document_metadata 没有；本 Run 没取全？）
docs_diff: metadata 多出清单外的文档：extra.md（不在 list_documents 里；是不是混入了上一次的元信息？）
docs_diff: 文档 handbook.md 的 size 在 list 与 metadata 之间不一致：1717 != 1718（两次读取之间文件变了？整轮重取）
docs_diff: document_metadata 文档 handbook.md 的 sha256 不是 64 位小写十六进制：'ABC'
docs_diff: 状态文件 schema_version 必须是 1，实际 2
docs_diff: list_documents 第 1 项的 path 含空段 / . / ..：'../a.md'
```

脚本失败时**什么都不写**：不生成 `changes.json`、不创建 `runs/` 父目录、不动
`state/baseline.json`。报告先写、状态后写——万一只剩状态写失败，下一轮会重算出同一份
报告（幂等）。

## 证据档位

| 项 | 状态 | 证据 |
| --- | --- | --- |
| 脚本单测 | **已验证** | `py -3 -B -m unittest discover -s scripts`：`Ran 22 tests`，`OK` |
| `docs-readonly` 单测 | **已验证** | `go -C examples/shared/mcp/docs-readonly test ./...` 绿 |
| 真实 MCP → 脚本链路 | **已验证（本机 Windows）** | 三轮报告哈希见上表；`document_metadata` 的 sha256 与磁盘 `sha256sum` 逐份相等 |
| 只读边界 | **已验证（工具级）** | `..` 与缺失路径的 `isError` 原文见上表 |
| 软链接拒收 | **未在本机手工验证** | 由 `main_test.go` 的注入点覆盖（Windows 构造软链接需额外权限） |
| 真实模型驱动的完整 Run | **未验证** | 本轮无模型授权；AGENT.md 的步骤顺序与报告模板未实跑 |
| 真实通知接收 | **未验证** | 无外部接收方；只用本地回环接收器 |
| 自动触发 / 人工补跑 | **未在该实例取证** | 需要用户环境按 `SETUP.md` 接一次 |
