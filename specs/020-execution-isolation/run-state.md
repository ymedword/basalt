# US20 执行状态

本文是过程状态，不替代设计或验收判据。失败台账只追加，不改历史行。未验证项仅在具备日期与证据后从“未验证”转为“已验证”。

## 1. 基线与恢复位置

- 人工 review ①：2026-11-04 用户授权“开始开发 us20…执行到…review2”；WSL 后续授权“Linux可以用wsl来验证”。
- 已审输入 R1.5 SHA-256：`9bd639e9280d81b2e1815ad4f57b22acf69fbeb9f285226faeb965b83e3b1c78`。审批、环境与勘误追加记录后另计算实施指纹。
- HEAD：`7a2c28884c27cb0b3de6cfd61ff332264a406aec`，US19 已合入；分支 `020-execution-isolation`；工作区 `D:/code/basalt`，初始干净。
- 授权范围：已审 US20 实施、测试、修复和 review ② 材料；不自动 commit/push/merge/tag/deploy。
- WSL 授权引用：`user-2026-11-04-us20-wsl`。只操作本 US 临时夹具、测试库及专属标签容器；无真实模型调用，无生产数据。
- 环境预检：WSL2 Ubuntu 26.04 LTS / Linux 6.6.114.1-microsoft-standard-WSL2 / amd64；用户 ym UID 1000；Docker 29.9.2，runc 1.5.2，runsc release-20261019.0 已注册；Go 1.27.3；工具存在，不代表验收已通过。
- 当前（2026-11-04 收尾更新）：已完成实现、原候选公共 full 与三份 US20 stage、独立复核及截图后修正；用户已确认五个已展示页面与七项局部 `#nosec`。整体收口记录尚待完成。
- 恢复动作：先读本文末尾与 acceptance.md，不重新初始化或重建分支；由 Claude 接续当前候选的验证与收口证据核对，不将原候选完整门禁 hash 套给截图修正后的候选，不重复索取七项抑制批准。

## 2. 失败台账（只追加）

| ID | 日期 | 现象与假设 | 处理与证据 | 状态 |
| --- | --- | --- | --- | --- |
| ENV-01 | 2026-11-04 | PowerShell 调 WSL 时内层 Docker Go template 引号丢失，info 报 unclosed action | 改读取完整 docker info，确认 runc/runsc 注册及服务可用；无环境修改 | 已定位 |

## 3. 停止预算与计数

- 同问题修复失败 3 次进入 debug；debug 单问题最多 3 个假设、每 US 最多 2 轮。
- 最终 debug 1 轮；各问题定位后已获有效绿态，没有同问题连续 3 次无效修复。阶段记录保留在后文。
- 实施前与收口 analyze 均已完成，CRITICAL=0；未触发停止预算。

## 4. 未验证清单

| 项 | 状态 | 恢复条件 / 证据 |
| --- | --- | --- |
| US19 接口与迁移漂移全量核对 | 已验证（2026-11-04） | full 的 integration/PG race 通过；旧迁移不变，新增 v9/78 表；见 acceptance.md §1/§4 |
| source / integration / 七目标 / 固定评测 | 已验证（2026-11-04） | us20-public-full-native-r2/report.json，11 checks passed；真实模型可选项按禁止规则跳过 |
| Linux OCI 与 gVisor 行为 | 已验证（2026-11-04） | us20-oci-native / us20-gvisor-native，全 required PASS，候选 SHA 相同 |
| 管理台与官网构建 | 已验证（2026-11-04） | Node24.22.0；console472、website20 测试及构建/check 通过 |
| 逆改判别力、独立复核 | 已验证（2026-11-04） | 11/11 有效红态，4 项末次刷新到冻结源；独立复核记录在 .loop/us20/independent-review-final.md |
| 管理台已展示五个页面及换行修正 | 已验证（2026-11-04） | 用户提供截图并明确反馈“页面没问题了”，见本文末尾人工确认记录 |
| 管理台已启用执行档表格 | 未验证 | 用户截图未展示该表格，不随其他页面确认一并记为通过 |
| 七项局部 nosec 人工豁免确认 | 已验证（2026-11-04） | 用户明确引用 acceptance.md §5.1 七项并回复“我都确认”，已逐项记录批准 |

## 5. 收口状态

五个已展示页面及七项局部静态扫描抑制已获用户明确确认。整体 review ② 收口记录与当前候选证据核对交接 Claude；已启用执行档表格保留未验证，不重复索取已获批准的七项抑制确认。无提交、推送或发布。

### 2026-11-04 输入冻结与 T001

实施前 analyze：CRITICAL=0，未处置 HIGH=0；见 analyze.md。T001 完成，T002 stage 已派生，具名测试存在性 lint 随测试落地验证。

- docs/us/us20-execution-isolation.md SHA256 9671eaa8519cb2856bfb4d7049b755f21577aae04927c4c3b7a353ffd8a58ba6
- specs/020-execution-isolation/spec.md SHA256 c938c869a02dcdc5737b55b9b5bbbc629bc1f25987e62e32a8e9cde1a85bd8dc
- specs/020-execution-isolation/plan.md SHA256 5634b68574ef145de785e956228de0c4b4995a50e55baea91ff4c501abfc0b4d
- tasks 正文归一 SHA256 1b6b2caace1cd77d358e0eefb6c4cf8fd6b31c0c8efcab9d8bf048b834d84209

### 2026-11-04 T003–T006 基础实现与验证

T003–T006 完成：core additive 类型与兼容测试；PG v9 账本及历史保留保护；隔离配置严格解析、显式绑定、正值与溢出、两条临时空间不等式。当前继续 T007 镜像可信清单，T002 的具名测试存在性 lint 待后续测试文件落齐。

- `go test -count=1 ./internal/core -run '^TestUS20_'`：PASS，3 个测试。
- `go test -count=1 ./internal/cli -run '^TestConfig_'`：PASS，旧配置回归。
- `go test -json -count=1 ./internal/cli -run '^TestUS20_Config'`：PASS，10 个具名测试及子用例。
- `wsl -d Ubuntu --cd /mnt/d/code/basalt -- bash dev-env/scripts/with-postgres.sh go test -tags=integration -count=1 ./internal/storage/postgres -run ^TestUS20_`：PASS，4 个真实 PG 集成测试，独立测试 schema；未触碰生产数据。
- 未运行全量、容器行为或变异判别力验证；上述局部通过不替代七目标。

| ID | 日期 | 现象与假设 | 处理与证据 | 状态 |
| --- | --- | --- | --- | --- |
| ENV-02 | 2026-11-04 | 直接 WSL go test 缺 BASALT_TEST_PG_DSN，集成测试拒绝运行 | 改用现有 with-postgres.sh 内存注入凭据后上述 4 测试 PASS，未显示或落盘凭据 | 已解决 |

### 2026-11-04 T007–T009

实现原始字节 Ed25519 验签、schema 2 内容校验、既有发布公钥内嵌；releasekit 在镜像记录之后合并并签名外置清单，镜像清单与发布记录交叉核对。无 RepoDigest 的预演只能产生空 sandbox 段，运行期拒绝；不将 LocalID 冒充仓库 digest。公钥 DER 指纹 `0082e7b53ada39ac3df9a48d57853b4232998c00e3d87c96dff2f904849ac5d5`，未读取生产私钥。

解释器校验提取为共用函数，按基名识别；本机保留受控绝对脚本兼容性，隔离请求在 LocalRunner 入口拒绝。

- `go test -count=1 ./internal/isolation` PASS。
- `go test -count=1 ./scripts/releasekit -run 'TestUS20_ImageLockMerge|TestUS18_Image_LockMatchesSources|TestUS17_Verify'` PASS。
- `go test -count=1 ./scripts/releasekit -run 'TestUS20_|TestUS18_Image_Assemble'` PASS，含真实归档构建与签名链夹具；仅临时测试密钥。
- `go test -count=1 ./internal/tool -run 'TestUS20_|TestLocalRunner|TestRunner|TestShell'` PASS；全包回归留到运行器实现后。

继续 T010–T012。未宣称容器行为、全部回归或独立复核通过。

### 2026-11-04 T010–T013 与接线澄清

Docker CLI、固定安全参数、执行/维护生命周期、清理容器查证、共享准入和账本辅助已实现。`go test -count=1 ./internal/tool` 全包 PASS；WSL `go test -race -count=1 ./internal/tool -run ^TestUS20_` PASS（含并发同时占位断言）。真实容器尚未运行。

- WSL 命令传正则时普通 `--` 经 bash 二次解析括号，失败一次；改 `--exec` 直接 argv 后通过。这是命令包装纠正，不是产品修复。
- 源码刷新发现模块卡把 Root 混称为工作目录与 `.basalt` 工作区根。实现统一使用既有 `a.wsRoot()` 的实际工作区根（默认 `<工作目录>/.basalt`），故 staging 为 `<Root>/sandbox/<run>/<inv>`，不重复嵌套 `.basalt`，也不通过取父目录扩大挂载范围。输入副本放 `<stage>/task/input`，与 `task/main.sh` 同处只读 `/task` 挂载内。对外物理路径与既有工作区自定义能力不变；属于内部参数澄清。

当前进入 T014–T016。未宣称全部 stage、真实 Linux 两档或 review ② 通过。

| ID | 日期 | 现象与假设 | 处理与证据 | 状态 |
| --- | --- | --- | --- | --- |
| IMPL-01 | 2026-11-04 | 产物正向及并发测试失败，真实 Policy 拒绝尚未创建的父目录；不是并发竞态 | 先授权已有 Agent 下的 artifacts 根，再在根句柄内建固定目录，逐文件授权后才写入；不改变 Policy 的父目录规则。首次修复待重验 | 修复中 |

### 2026-11-04 T014–T016 验证

IMPL-01 首次修复通过：`go test -count=1 ./internal/tool -run '^TestUS20_'` PASS。随后 `go test -count=1 ./internal/tool` 全包 PASS。生成代码使用真实 Policy 判定、Root 句柄暂存；脚本字节进入输入预算；产物类型/哈希/累计总量与实例锁判定已落地；OCI shell 绑定共享账本与准入。

- `go test -count=1 ./internal/core -run '^TestUS20_'` PASS，包含真实 ToolExecutor → RunCodeTool 的单行审计、Policy 拒绝及审计预写失败零执行。
- WSL `go test -race -count=10 ./internal/tool -run ^TestUS20_ArtifactPublishSerialized$` PASS，12 并发发布只能成功一份，总量不超。
- 内部 ShellIsolationOptions 显式增加 MaxTimeout，用于把已审 profile.timeout_seconds 传到 shell 调用，保持与 Run/env 超时取最小；没有新增配置键。
- 继续 T017–T020：装配、启动恢复、CLI 只读探针与管理投影。容器真实行为、变异红态、全量门禁仍未验证。

### 2026-11-04 T017–T020 与执行链定向复核

启动探针/三类恢复、共享装配、只读 CLI、管理 API 投影完成；`go test -count=1 ./internal/app ./internal/web ./internal/cli` 全包 PASS（23.515s / 5.845s / 17.520s）。T017–T020 已勾选，真实容器行为仍待验证。

| ID | 日期 | 现象与假设 | 处理与证据 | 状态 |
| --- | --- | --- | --- | --- |
| FIXTURE-01 | 2026-11-04 | app 用例误用 FlowAdapterFor 参数及仅 integration 可见的 MCP 助手；编译失败 | 改为实际接口与 source 层同包假件；未改变断言，上述全包 PASS | 已解决 |
| FIXTURE-02 | 2026-11-04 | CLI JSON 夹具混 stderr，契约夹具缺 request_id / 使用不存在的 TriggerUser | 分开 stdout/stderr、填实际合法 RunRecord；全包 PASS | 已解决 |
| REVIEW-01 | 2026-11-04 | Probe 的 set -e 会在跨 UID chmod /output EPERM 时提前停止 | 清理段 set +e，结束恢复 set -e 并断言 locked 与 ..cache 消失；source PASS，真实跨 UID 待验 | 待真实验证 |
| REVIEW-02 | 2026-11-04 | 最后产物复制期间取消仍可返回成功，collector 丢失取消错误链 | 每块读写及 EOF 检查 ctx、最终提交前检查；错误链保留并归类 canceled/timeout；新增末次写取消与 RunCode 清理期间取消测试 | 已解决（source） |
| REVIEW-03 | 2026-11-04 | OCI shell profile 上限低于 env 时截断无标记 | ContainerRunner 补统一 core.TruncationMarker，run_code 不重复；新增 profile 限额回归 | 已解决 |

只读独立审查由 us20_tool_review 执行，1 P1 + 2 P2 与先前 Probe 问题均已响应；不替代最终 review② 独立复核。修复后 `go test -count=1 ./internal/tool ./internal/core -run TestUS20_` PASS。当前 T021 管理台读面，后续 T022–T026 契约与真实 WSL 容器。

### 2026-11-04 T021 与 T023/T024 准备

管理台只读执行档/资源/镜像、Run 执行与清理投影已完成。Windows Node v24.22.0，`npm test` 39 文件 / 466 测试 PASS；`npm run build` PASS，dist/integrity 已更新。Vite 仍提示主 chunk 大于 500kB（体积提示，不影响构建；未放宽阈值）。既有两个 SandboxView 测试夹具补新增必填字段后 typecheck 通过。

Linux `CGO_ENABLED=0 go build -o /tmp/basalt-us20-candidate ./cmd/basalt` PASS。生产 Dockerfile 预建 /task 与 /output，构建本地测试镜像 `basalt:us20-test`，ID `sha256:32c7ea41a1bce7c55a4fc1b650b589fb4a46b144c9bf2c6b14af9b0538ac5a54`；上下文 `/tmp/basalt-us20-build.eDwPkd`（实际路径由构建输出核查）。不推送、不签生产清单、不读生产私钥。测试信任材料用内存生成的临时密钥；本地 Image ID 只用于测试清单，不冒充发布 RepoDigest。

已创建专属 tmpfs `/tmp/basalt-us20-output.FaoGup`，64MiB、UID/GID1000、mode0700、nodev/nosuid；WSL 默认用户 sudo 无免密，因此仅挂载动作使用 WSL root 入口，测试服务仍 UID1000。该授权归 user-2026-11-04-us20-wsl，收尾卸载并回收。T024 先运行两档真实 Probe / 生命周期 / 跨 UID RoundTrip；尚未宣称 PASS。

| ID | 日期 | 现象与假设 | 处理与证据 | 状态 |
| --- | --- | --- | --- | --- |
| REAL-01 | 2026-11-04 | 两档真实 Probe 首次均失败 docker --host exit 1；source 假件未暴露 CLI/daemon 拒绝原因 | 进入 debug 根因调查，先临时记录子命令/有界 stderr，尚无修复尝试；不得修改隔离断言 | 调查中 |

REAL-01 根因已定位：Docker local 日志驱动默认压缩开启，而固定 `max-file=1` 被 daemon 拒绝（start: `compression cannot be enabled when max file count is 1`）。最小修复为显式 `compress=false`，保留 local/max-file=1/max-size，不放宽容量。临时详细 argv/stderr 诊断立即撤回；第 1 次修复待重验。

REAL-01 修复后两档已越过 Docker start，证明 compress=false 修复有效；新失败为维护容器自身退出 1（REAL-02）。OCI 留下 mode000 的 locked 子目录，初步怀疑 BusyBox chmod 在 /output 属主不同时未向下恢复权限。先采集固定 Probe 脚本的逐命令读数，未尝试更改清理语义。

REAL-02 逐命令证据确认：BusyBox 对服务 UID 所有的 /output 执行 chmod -R 时未修复 000 子项，随后 rm 后 locked 仍在。最小修复为在逐项 for 内对容器自己创建的每个子项 chmod -R 再 rm，保留三段通配及不跟随 symlink 的删除。第 1 次修复待验。另 REAL-03 gVisor 64MiB 夹具在 tini exec /bin/sh 时 Cannot allocate memory；属于资源不足被探针正确拒绝，夹具按已审示例使用 256MiB，不修改产品内存校验或安全断言。临时 trace/日志读取撤回。

REAL-02 第 1 次修复已通过 OCI 真实跨 UID Probe，生命周期经 ToolExecutor → PG → runc PASS。夹具 SQL 对 error_reason 可空字段补 COALESCE，属于读数夹具修复。

REAL-03 原“内存不足”假设已被控制组推翻：同样 256MiB、全部安全参数无 bind 时成功；增加一个 /output bind 后 pids=32 复现 ENOMEM，仅将 pids 改为 64 后成功。证据表明该环境 gVisor 的宿主线程/挂载辅助开销使 32 pid 预算不足；采用模块卡示例 256MiB / 64pids 进行有界验收。产品仍按配置原值限制，不提高默认或自动降级；低配置由真实 Probe 拒绝。控制容器均 --rm，无外网。

REAL-03 正向重验 PASS：gVisor 使用 256MiB / 64pids，真实 Probe 与 ArtifactRoundTrip（服务 UID1000 ≠ 任务 UID65534、/output=777、内容和 SHA256、目录与容量回收）均通过。

定向复核追加 REVIEW-04：正常任务遗留空 Run 父目录，超过 1024 后恢复拒绝；REVIEW-05：前端把 unknown/running 的 COALESCE 退出码 0 展示为事实。当前修复：空父目录回收及带截止时间的分批枚举，不降低真实孤儿上限；前端仅 finished 展示退出码。先补反例再重验。

T025/T026 第一批真实行为 17 个测试 PASS（81.741s），包括无网正反控制组、只读根正反控制组、秘密缺失、OCI 取消超时、资源实际配置与 tmpfs/内存耗尽、两种恶意目录清理。真实恢复用例放 internal/app 同包 e2e（不新增生产导出），由杀真实执行子进程制造残留，stage 将同时包含 app/e2e 包。新增独占恢复 tmpfs `/tmp/basalt-us20-app-output.WBKvXN` 64MiB，隔离于工具用例输出根；收尾需卸载。

FIXTURE-03：恢复夹具首轮用了非实例名册格式 `us20_recovery_*`，生产门禁正确拒绝 not_ready；改为实际 `inst_<16hex>` 格式。没有修改产品身份校验，待重验。

### 2026-11-04 最终复核与门禁进展

FIXTURE-03 重验 PASS：真实 OCI/GVisor 崩溃残留、无账本容器与目录恢复、同时占用 N 条后恢复并再次准入 N 条全部通过（internal/app，14.477s）。REVIEW-04 空父目录源测试与 REVIEW-05 退出码未知展示（9 用例）已通过；console 全量 39 文件/472 测试、构建，以及 website 20 测试/构建/链接与图源检查通过。

LINT-01：首次静态门禁报 isolation_cli.go 两处 Docker 开头错误文本违反 ST1005；改为小写 docker，未改变错误类别与行为。复跑进行中。

变异证据：前 5 项 /tmp/us20-mutations-1793770980221068703；中间 4 项另一个 us20-mutations 目录；后 2 项 /tmp/us20-mutations-1793771410704854533。11 项均已出现指定测试断言红态，未改仓库生产文件。两次脚本定位串命中两处而在变异前退出（Count SQL、helper 标签），收窄到精确语句后继续；不是产品修复或通过证据。最终归档将逐份核对。

FINAL-01：独立复核发现可选 inputs/timeout_seconds 的显式 null 被误当缺省；新增 Execute 反例先红（calls=1, records=1），再增加 null 检查，缺省与正常值仍接受。FINAL-02：真实取消审计断言收紧为契约唯一值 timeout。FINAL-03：stage protect 补齐模块卡要求的生产安全路径。FINAL-04：gVisor 秘密测试改为 && 链，使用 /etc/hosts 分别替换两个禁止路径的负向控制，防止中间失败被末尾覆盖。以上均为落实既有判据，不改变批准范围；正在重验。

当前停止预算：debug 1 轮（真实 Docker/清理/gVisor 前置根因调查）；各定位均已有有效读数和最小修复，未出现同问题连续 3 次无效修复。不存在需停止的预算触发。继续整体验收与 review②证据，不代签收口。

GATE-01：全量 cover 首轮失败，明确发现前序当前版本断言仍为 PG8/77表（实际批准为 PG9/78表），已更新5处测试中的当前版本/清单，保持旧表54+观测6+Flow17逐项断言，并显式断言 sandbox_executions；历史 0001–0008 零改动。定向6项 integration 回归 PASS（app 2.225s / PG 0.606s），包括此前 WorkerStopsOnCancel 3秒进入 sender 超时项；该时间阈值未改，整体验收仍需复验。

GATE-02：OCI 三组行为全部 PASS，但运行期间我修订了受保护输入，验收器正确拒绝 source_changed / 不同 binary_sha256，未生成有效收口记录。保留 .loop/runs/us20-oci-final 失败报告；冻结后用同一候选重跑。一次并行启动 base 被独占 Harness 锁拒绝，未删除锁，后续串行执行。

stage lint 已退出0（stage lint: ok）。历史 US8–14 stage 有既有缺名 WARN（这些不是当前 US20 映射）；不改写历史 stage、不把 WARN 写成零。US20 具名映射已由独立上下文核对，当前随后新增1条 null 用例；最终 stage 自身 required 判卷继续确认。

GATE-03：gosec ./... 扫入既有 `.loop/obs-*` / `retention-*` 残缺备份及 us19 多个 main 探针，产生非产品编译错误；另有探针父目录两处 G301/G302。父目录无需向任务开放，已保持 MkdirTemp 私有权限，叶目录先0700后按已审要求显式0777；未新增抑制。source TestUS20_ 已通过。主工作区 public full 已发送 SIGTERM 正常取消并保留 error 报告，不改扫描规则、不移除历史备份。

改用 WSL 原生临时 worktree /tmp/basalt-us20-validation-tucsxg1v（detached HEAD仍7a2c288）验收同一未提交源码。git可见2312个路径逐文件复制并 SHA256互证，清单 .loop/us20/snapshot-files.json；无旧.loop备份，无新依赖，无commit。完整full原命令在该副本执行，完成后归档其日志与记录到主工作区。差异只在宿主文件模式/路径，字节清单可核查；代码修复必须回主工作区并同步互证，不接受副本独有修复。

变异红态已归档 .loop/us20/mutations/results.json，11项均具备实际失败断言（不是编译失败）。Windows 六包 TestUS20_ 全绿。额外仅测试夹具 overlay 计时，不改生产或判据：OCI首次Probe1038–1076ms、正常含审计清理932ms、镜像拒绝101ms；gVisor首次Probe1231–1307ms、正常RoundTrip985ms、恶意000目录含helper清理2060ms；单次docker remove102–128ms。样本少、镜像已存在，不是吞吐/SLO或冷镜像拉取数据；原始日志 .loop/us20/performance/measurements.log。

浏览器视觉检查未完成：Chrome连接可列出，但创建/选取本地页持续报 timeoutMs 读取错误，未取得页面截图。临时Vite与夹具文件已回收；不把DOM单测当截图。管理台472项测试与构建通过，实际视觉可作为review②人工查看项。

GATE-04：native首轮fmt/lint/sec通过；普通race测试发现docs_14d仍要求11工具。同步检查又发现两个integration装配精确集及CLI bootstrap仍为11。主树已按US20批准的12工具扩展逐字集合，并新增run_code Resolve断言，保留既有测试函数名以不破坏历史stage锚点；Requirements当前正文同改十二工具并补run_code行。定向app/cli三项PASS（1.411s/0.054s）；旧“第11个web_search”历史记载不改。native首轮保留失败报告，后续integration主动停止，避免重复跑已知旧断言。

同步后2312文件再次逐字SHA256互证，原清单留带时间备份，新清单 .loop/us20/snapshot-files.json。正在native-r2 full→base→OCI→gVisor串行运行；两档共用full构建的同一候选二进制，防止跨次自动重构造成hash漂移。完整脚本在 .loop/us20/final-suite.sh。脚本首行PowerShell UTF8 BOM产生一条无效shebang提示，后续set -e及各原命令已运行；该提示不是任何判据通过读数，最终以逐个Harness report/record为准。

### 2026-11-04 最终读数与 review ② 交接

公共 `us20-public-full-native-r2` 全部 11 checks passed；base 5、OCI 3、gVisor 1 check 全 passed。共同源码 fingerprint 为 `39ccb1d6eb9b9f2e5516cfc0cf95f845f2d7abd13b2668c506c74789a58b2645`；两档均使用 full 构建候选 `bbe125d34900926ead5aa5fb651c783f3c9a4567e10afcc15693badbf4d9c8f5`。94 个 required entries / 90 个不同测试名全部 PASS、零必跑 SKIP；公共 behavior 的可选真实模型项按既有禁止规则 SKIP，实际为 5 PASS / 1 SKIP，不冒充所有用例均运行。

最终候选实际 `TestBackfill_Bootstrap/tool_list` PASS（精确12工具），`isolation check` 在空目录退出0并显示未配置、本机执行与 run_code 拒绝。没有对候选进行正式签名或发布。两档任务测试从测试二进制进入生产组件，不能仅凭 binary_sha256 标记宣称完整发布镜像/服务部署已验收。

独立证据核对发现4条清理变异仍用旧源码 hash，已仅重跑04/05/10/11，全部功能断言红；11条当前变异源 hash 均对齐冻结文件。旧四条日志和结果留在 `.loop/us20/mutations/history-before-final-refresh`，最新原始目录 `/tmp/us20-mutations-1793774126615712850`。没有改原版代码或断言。

全局不变量检查已执行：框架/ORM/业务 context/明文密钥形状/非安全随机/点导入/生产本地时区/core LLM 审计重复入口/运行期 SQLite 均无新增违反；core 导入全为标准库，`CGO_ENABLED=0 go build ./...` 退出0。全树时区字面扫描两处既有命中都是测试注释，已逐行核查；未改公共判据。旧迁移、go.mod/go.sum、宪法字节不变，新增迁移与契约同源。

本轮新增7项局部 `#nosec` 已在 acceptance.md §5.1 逐项列出（不是此前粗数的6项）；独立复核认为理由在现有边界下成立，但 EngineeringStandards §1 明确要求用户确认，故仍待 review② 裁决，不写成已批准。真实页面截图仍未验证，浏览器工具连续 timeoutMs 错误已登记。

资源收尾：Docker 查询本实例标签容器零残留；已核对挂载类型、路径及64MiB限额后卸载两个专用 tmpfs 并删除空挂载点。记录 `.loop/us20/cleanup-final.json`。预览服务/临时页面已回收，保留原生验证工作树、候选、夹具镜像、overlay和日志供复核；原有 US18 demo、PG 与 kind 资源未改。

原始报告/record/log 已复制到主树 `.loop/runs`。产品、测试、stage 字节与冻结副本一致；随后只更新 acceptance、quickstart、run-state、任务勾选等收口文档。`markdownlint` 12文件0 issues；最后文档更新后再查一次。恢复位置：待人工 review ②，先看 acceptance.md §5.1/§6；不自动提交或把本记录当用户收口决定。

最终独立回执已收到：无未处置产品 P0/P1/P2，94/94 required 与11项变异复核通过。原始回执83行、SHA256 `63e72ebec8c828e9ed817e255b6b9f1dcea1124bdcd7358f86de5abf87afafbc`；逐字副本保存到 `specs/020-execution-isolation/review.md`，随过程工件保留。T001–T030 全部勾选，完成的是实施与人审材料准备；七项局部抑制、真实视觉和 US 最终收口仍由用户裁决。

### 2026-11-04 用户人工查看：8080 临时服务

用户要求启动8080并提供检查步骤。已使用相同最终候选，在 WSL 新工作区 `/tmp/basalt-us20-preview-0g_x1a43` 与独立测试 schema `basalt_us20_preview_1f4e995a887cba82` 启动服务（PID2739912）。先在18080确认启动，再临时停止占用8080的 `basalt-us18-demo-basalt-1` 并切到8080；旧演示数据/PG容器未改。配置凭据仅占位及进程环境，管理面 local_trust、仅127.0.0.1，无真实模型。

最小配置准备中先后修正了缺失的入口限流段、空 providers 流式序列被管理配置加载器拒绝两处夹具问题；没有修改产品。当前 Windows 实测 console200、最终前端 bundle匹配、info/sandbox及四条Run API均200。四条Run为明确标注的UI人工样例（成功、清理失败、unknown、空态），不能计作真实执行证据；此实例隔离仍为未启用，不冒充正式签名镜像运行期装配。

检查步骤在 `.loop/us20/preview/CHECKLIST.md`，状态和API查证在同目录；停止/恢复脚本 `.loop/us20/stop-preview.py` 仅针对上述PID、可执行路径和工作目录，恢复原US18演示。服务保持运行供用户查看。视觉结果尚待用户反馈，七项局部抑制仍未确认。

### 2026-11-04 截图复核与预览修正

用户提供了执行环境及成功/清理失败/unknown/空态五张截图。可确认任务隔离未启用说明、清理失败警告、unknown 的退出码未知、空集合文案均实际显示，截图范围内未见遮挡；成功行 cleaned 被断为 cleane/d，摘要在成功和空态下也错误出现“已记录失败”。不能把这轮截图无条件记为全视觉通过；已启用执行档表格仍未展示。

| ID | 日期 | 假设与根因 | 验证与处置 | 同题计数 |
| --- | --- | --- | --- | --- |
| UI-01 | 2026-11-04 | 样例生成器把“UI 检查样例，不代表实际执行”写进四条 runs.reason；obsRunReason 将任意原文投影为 unclassified，前端按该固定分类展示失败兜底 | 原始API四条均 unclassified，check-summary.py before 四条断言红；只修本预览schema及生成器：成功/空态 reason 空、清理失败 runtime_error、未知 unknown，after 四条绿。样例身份保留在 agent/user_id/result，不改产品诊断逻辑或闭集 | 假设1成立，修复1通过 |
| UI-02 | 2026-11-04 | `.isolation-table td { overflow-wrap: anywhere }` 降低短英文状态词的最小内容宽度，截图出现 cleaned 中间拆行 | 改为 break-word，保留 digest anywhere 与既有 table-wrap 横向滚动；不改标签语义。CSS 只读核对及新资源HTTP查证通过，修正后的实际视觉仍待用户刷新确认 | 修复1，待视觉复看 |

本轮 console39文件/472测试通过、前端构建退出0、Go internal/web 全包通过（4.635s）、CGO_ENABLED=0 构建通过。当前8080预览 PID2747201，候选为 `/tmp/basalt-us20-preview-0g_x1a43/basalt-ui`，SHA256 `5730318da8890bcf24dd1d13a453093af7b4edffa2bfff2e23a23eb7098b67a6`；Windows实际读取新HTML/CSS均200，新bundle与break-word规则匹配。原四份full/OCI/gVisor报告和冻结候选保留，不能把旧hash套给该UI候选；本轮未重跑无改动的容器行为或完整Go七目标，针对CSS与样例数据运行相应回归。测试日志、前后API读数在 `.loop/us20/preview/`。

因此“没有安全分类”不是已发现安全事故，而是对不能安全公开原文的错误原因使用固定兜底；本次由作者误用样例字段触发。没有扩大公开原因闭集、没有直接向页面暴露任意错误原文。用户最终review②和七项局部抑制仍未裁决。

### 2026-11-04 已展示页面的人工确认

用户反馈“页面没问题了”，据此将上述五个已展示页面及UI-02修正后的视觉复看记为已验证（用户截图与本次明确反馈为证据）。已启用执行档表格尚未展示，不能连带记为通过；七项局部抑制与整体US收口仍待裁决。

用户另问 Agent 列表为何没有 us20-ui-sample：它仅是预览运行记录中人工写入的样例名称，没有对应工作区 Agent 目录/AGENT.md，也没有注册实际 Agent。运行历史不自动派生 Agent 实体，本轮没有执行该样例 Agent。已解释此数据来源；未为消除疑问偷偷创建实体或把UI样例改称真实运行。

### 2026-11-04 七项局部静态扫描抑制的用户批准

用户原文：“验收报告 §5.1 的七处局部 `#nosec` 仍需用户逐项确认，不将页面确认视为豁免批准。   我都确认”。该明确回复覆盖 acceptance.md §5.1 的全部七项具体抑制；已逐行更新为“用户已确认（2026-11-04）”，同步当前恢复位置与未验证清单。批准依据是本次专门回复，不是此前“页面没问题了”。

此前待确认的历史记录、失败台账与独立 review.md 原文保留。此次仅更新审批与交接文档，不改产品代码或验收判据；七项抑制批准不再阻塞后续收尾。Claude 可接续当前候选验证、证据核对与整体收口记录，无需再次询问这七项。

### 2026-11-04 人工 review ② 通过与提交授权

用户明确回复“US20 进入最终收尾，不再扩大设计或复核范围。人工review也通过；可以进行提交和跟踪CI最后合并到main”。据此：review ② 记为通过，§5.1 七项局部抑制与五个已展示页面的人工视觉确认随本次决定收口；已启用执行档表格未展示的事实保留，不改写为已验证。

授权本轮执行：提交 → 推送 `020-execution-isolation` 分支 → 跟踪分支 CI → 通过后 fast-forward 合入 `main` 并推送 main；不 tag、不发布、不部署，发布仍归用户。提交候选含截图复核后的 console 修正（CSS break-word、dist 重建），其验证口径为 console 472 测试、前端构建、Go internal/web 全包与 CGO_ENABLED=0 构建；容器行为与完整门禁证据对应冻结源码指纹 `39ccb1d6eb9b9f2e5516cfc0cf95f845f2d7abd13b2668c506c74789a58b2645`，不把旧 hash 套到新 UI 候选，最终分支 CI 以提交树重新覆盖。
