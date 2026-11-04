# US20 验收报告

日期：2026-11-04。状态：实现与自动验收完成，待人工 review ②；未代签 US 收口。review ① 与本机 WSL 授权见 [run-state](run-state.md)。未 commit、push、merge、tag、发布或部署。

本报告下列冻结门禁读数对应最初收口候选。随后用户截图检查触发一处CSS换行修正与预览样例字段修正，新增证据见本文末尾“截图复核补充”；旧报告不改写为新候选的全量通过。

## 1. 门禁与取证基线

基线 HEAD `7a2c28884c27cb0b3de6cfd61ff332264a406aec`，分支 `020-execution-isolation`。四份最终报告均为 `passed / exit 0`，共同源码指纹 `39ccb1d6eb9b9f2e5516cfc0cf95f845f2d7abd13b2668c506c74789a58b2645`。真实两档共用候选 SHA256 `bbe125d34900926ead5aa5fb651c783f3c9a4567e10afcc15693badbf4d9c8f5`。

冻结验证在 WSL 原生工作树 `/tmp/basalt-us20-validation-tucsxg1v` 执行，以避开主树历史 `.loop` 内的残缺 Go 备份。2312 个 Git 可见路径逐字 SHA256 互证；没有改变公共扫描规则或删除旧备份。四份报告的指纹属于该冻结副本，不能冒充主工作区含过程文档的最终指纹。门禁后只整理本 US 验收文档、任务状态及私有文档事实，产品、测试和 stage 未改。

原始报告、record 和日志已复制到主工作区 [.loop/runs](../../.loop/runs/)；摘要与逐文件指纹在 [final-summary.json](../../.loop/us20/final-summary.json)、[snapshot-files.json](../../.loop/us20/snapshot-files.json)。日志保留失败历史，不用后续通过覆盖第一次失败。

| 命令/检查 | 四态 | 本轮读数 | 证据 |
| --- | --- | --- | --- |
| full：fmt | 通过 | 2.263 s / exit 0 | us20-public-full-native-r2/fmt.log |
| full：lint（integration vet/staticcheck） | 通过 | 9.210 s / exit 0 | 同目录 lint.log |
| full：sec | 通过 | 10.666 s / exit 0；7 项局部抑制已获用户确认（2026-11-04） | 同目录 sec.log；§5.1 |
| full：test（race） | 通过 | 65.365 s / exit 0 | 同目录 test.log |
| full：test-integration | 通过 | 415.748 s / exit 0 | 同目录 test-integration.log |
| full：test-pg（race） | 通过 | 257.110 s / exit 0 | 同目录 test-pg.log |
| full：cover | 通过 | 465.174 s，全部有语句包达标 | 同目录 cover.log |
| full：build（CGO_ENABLED=0） | 通过 | 1.641 s，Go 1.27.3 | 同目录 build.log |
| full：eval-build / behavior / injection | 通过 | 2.184 / 3.065 / 2.846 s，均 exit 0；behavior 为 5 PASS / 1 SKIP | 同目录对应 log |
| behavior：outcome-real-model-answer | 跳过 | 未提供 provider，遵守其他 US 自动验收禁止真实模型调用的既有规则；非 US20 必跑项 | behavior.log；EngineeringStandards §2 |
| US20 base | 通过 | 5 checks，68 required entries PASS | us20-base-native/report.json |
| US20 OCI | 通过 | 3 checks，13 required entries PASS | us20-oci-native/report.json |
| US20 gVisor | 通过 | 1 check，13 required entries PASS，48.780 s | us20-gvisor-native/report.json |
| console test / build | 通过 | 39 文件、472 测试；构建 exit 0，dist/integrity 已更新 | 本轮终端读数及 run-state；full 的内嵌资源校验 |
| website test / build / check | 通过 | 20 测试；构建、链接和图源检查 exit 0 | 本轮终端读数及 run-state |
| Windows 六包 TestUS20_ | 通过 | core/tool/cli/isolation/app/web，均 exit 0 | 本轮终端读数及 run-state |
| stage lint | 通过 | exit 0；历史 US8–14 缺名 WARN 保留 | 本轮终端读数及 run-state；不宣称零 WARN |
| 候选 CLI tool_list | 通过 | TestBackfill_Bootstrap/tool_list，精确 12 工具，exit 0 | 最终候选实际执行，含 run_code |
| 候选 CLI isolation check | 通过 | 空工作目录 exit 0，enabled=false，未配置原因 | [cli-smoke.json](../../.loop/us20/cli-smoke.json) |
| 管理台真实截图 | 部分已验证（2026-11-04） | 用户截图及“页面没问题了”确认五个已展示页面；已启用执行档表格未展示 | 本文末尾截图复核补充；DOM 测试不替代截图 |

覆盖率重点：core 78.20%、tool 78.37%、memory 85.60%、SQLite 谱系 storage 73.87%、postgres 75.66%、新 isolation 89.66%。前五包底线 70%，其余包 50%；无可执行语句包按既有判据跳过。没有降低覆盖率阈值。

`govulncheck` 的实际结论是代码受影响漏洞 0、导入包受影响漏洞 0；另提示所需模块内 2 个当前代码未调用的漏洞。该提示保留在 sec.log，未把它改写成依赖模块完全无漏洞；本 US 没有更改依赖或扫描规则。

失败历史：主树 full 被历史 `.loop` 编译垃圾干扰并取消；第一次 native full 发现前序 11 工具断言；早期 OCI 运行中发生源变更，被 source_changed 与多候选 hash 正确拒绝。以上均不计通过，后续四份冻结报告才是收口读数。

## 2. Harness 与逆改判别力

三份 stage 共 9 个 check、94 个必跑条目、90 个不同测试名。按 report 中 passed_tests 与原始 go-test JSON 核查：94/94 PASS，必跑项零 FAIL/SKIP。逐条映射见本文附录 A；具体 FR/SC → task/check 映射在 [tasks](tasks.md)。

11 项逆改均在仓库外 Go overlay 执行，原仓库文件未改。要求目标测试的功能断言失败，编译失败不算红态；原版同名测试在最终 stage 通过。清理文件末次 Probe 权限调整后，04/05/10/11 又按冻结源刷新变异；其余七项源 hash 原已一致。旧读数保留，当前选用记录见 [mutations/results.json](../../.loop/us20/mutations/results.json)。

| ID | 移除/逆改的保护 | 四态 | 红态证据 |
| --- | --- | --- | --- |
| 01 | network=none | 通过 | OCI 本地监听器变得可达，拒绝断言失败 |
| 02 | read-only | 通过 | `/data` 可写，正反控制组断言失败 |
| 03 | 输出叶目录 0777 改为 0755 | 通过 | 跨 UID roundtrip 写入失败 |
| 04 | 清理权限兜底 | 通过 | 000 子目录无法回收，cleanup_failed 与预期不符 |
| 05 | `..?*` 隐藏目录枚举 | 通过 | `..cache` 遗留，清理断言失败 |
| 06 | unknown+cleaned 释放容量 | 通过 | 恢复后 active 为 2 而非 0 |
| 07 | 发布锁覆盖 check+write | 通过 | race/count=10 中成功发布超过一份 |
| 08 | handled 集合跳过已处理容器 | 通过 | 已 WARN 账本容器二次判死 |
| 09 | tmpfs 上界扣除其他预算 | 通过 | 非法总量被接受，配置反例失败 |
| 10 | helper purpose 标签 | 通过 | cleanup missing 参数断言失败 |
| 11 | rm 后 helper 缺失复查 | 通过 | helper 仍存在却误报 clean，断言失败 |

真实覆盖包括 OCI shell 与 gVisor run_code 两种入口、非 root/跨 UID、取消/超时、网络与只读控制组、秘密不挂载、镜像不批准/篡改、顶层普通文件产物、大小/数量/哈希、000 与隐藏目录、真实子进程崩溃、标签孤儿、预写前目录窗口、同时 N 条占位后的恢复。CPU/PID/日志证据是实际 Docker HostConfig；实际耗尽实验为 tmpfs 和内存，不把参数核对写成所有资源均已做耗尽实验。

小样本计时仅供运维估计：预载镜像时 OCI 首次 Probe 1038–1076 ms、正常全链 932 ms、镜像拒绝 101 ms；gVisor 首次 Probe 1231–1307 ms、正常 roundtrip 985 ms、恶意目录含 helper 清理 2060 ms；单次 remove 102–128 ms。原始日志 [measurements.log](../../.loop/us20/performance/measurements.log)。非吞吐/SLO、无冷镜像拉取；计时发生在最后 Probe 父权限收紧前，最终 stage 另有实际总耗时。

## 3. 交付物核对

逐路径存在性、大小与预期删除见 [deliverables.json](../../.loop/us20/deliverables.json)。以下模块落点均已存在；无名义交付而缺文件的项。

| 交付组 | 主要落点 | 核对结果 |
| --- | --- | --- |
| 核心端口与 additive 契约 | internal/core/isolation.go、action.go、store.go | 通过；零值兼容、单行审计回归 |
| 镜像信任 | internal/isolation/trust.go、release_pubkey.go | 通过；先验签后严格解析，无运行期自供公钥 |
| 运行器与清理 | internal/tool/isolation_runner.go、isolation_cli.go、isolation_cleanup.go | 通过；有界 CLI、探针、清理 helper 全生命周期 |
| 受控命令 | internal/tool/command_guard.go、runner.go、shelltools.go | 通过；共用解释器守卫，不降级 |
| 准入、生成代码与产物 | internal/tool/isolation_ledger.go、runcode.go、runcode_stage.go、runcode_artifacts.go | 通过；共享槽位/发布锁、严格入参、产物回滚 |
| PG 与保留 | internal/storage/postgres/isolation.go、history_retention.go、migrations/0009_sandbox_execution.sql | 通过；v9/78 表、未知保留、清理后释放 |
| 配置/CLI/装配 | internal/cli/config_isolation.go、isolation.go；internal/app/isolation*.go | 通过；fail-closed、只读自检、三类恢复 |
| API 与 console | internal/web/isolation_views.go；assets/console/src/views/isolation.ts、workspace.ts、run-detail.ts | 通过；只读投影，无新页面/写端点 |
| 发布链与镜像挂载点 | scripts/releasekit/imagelock.go、verify_isolation.go；release/image-lock.json；docker/ | 通过；签名清单接线，服务不默认挂 socket |
| 契约 | docs/contracts/isolation.md、isolation.pg.sql、openapi.yaml、run-and-errors.md、agent-md.md | 通过；新 DDL 与迁移逐字相同 |
| 专属 Harness | harness/stages/us20-base.json、us20-oci.json、us20-gvisor.json | 通过；具名测试与保护面已核对 |
| 文档与图 | README.md、basalt.yaml.example、Operations、官网中英文架构页及图源 | 通过；未宣称所有工具隔离或绝对无逃逸 |
| 私有设计/需求/兼容/台账 | docs/TechnicalDesign.md、Requirements.md、Compatibility.md、us/REGISTRY.md | 通过；另按哈希核对，不 force-add |

前序修改单列：core Action/Command/store；tool 原 LocalRunner/ShellTool/Policy；app wire/run/web/工具目录；CLI config/root/ports/deps；PG connect/保留；web 原 Run 与 sandbox 读面；US17 releasekit 与 US18 Docker 配置；原 console/README/官网。旧 dist 三个带 hash 的资源文件随本次构建替换，没有删除产品能力。仓库没有 README.zh-CN.md，沿用现有 website/zh/architecture/index.md，见 analyze 的适配记录。

## 4. 前序回归与判据面

全库普通 race、带 integration 的全部包、PG race、覆盖率和固定行为/注入评测通过；US19 Flow 的历史保留保护与工具白名单不扩权。e2e 不裸跑整个历史外部集合，以免触发其他 US 的真实服务/模型入口；本 US 两档完整具名集合及实际候选 tool_list 已单独验收。

判据面有变更，不能写“零变更”：

- 新增 US20 Go/source/PG/e2e、console 用例和三份 stage；保护面扩到执行、清理、发布、恢复、配置和存储。无必跑 skip。
- 随批准的 additive 契约，将当前工具精确集合 11→12，新增 run_code 及 Resolve 断言；当前 PG 版本 8→9、表数 77→78，保留原表逐项检查；旧历史迁移/Flow 7→8 断言不改。
- 既有控制台夹具补只读 isolation 字段；恢复 e2e 迁到 app 同包以直接测真实私有接缝，stage 同时执行 app/e2e 包，不为测试增加生产导出。
- 收紧取消审计为已有唯一 timeout；秘密脚本改 `&&` 并加入两个独立负向控制；新增 optional null 拒绝反例。均落实既有安全语义，无放宽阈值。
- 未改 Makefile、公共 acceptance.json、公共判卷脚本或 CI；没有因主树旧 `.loop` 备份改变扫描面。局部静态扫描抑制及用户确认记录见下一节。

## 5. 全局不变量与人工豁免

机器清单：[invariants.json](../../.loop/us20/invariants.json)、[global-invariants.json](../../.loop/us20/global-invariants.json)。

| 不变量 | 四态 | 本轮读数 |
| --- | --- | --- |
| 无 Agent 框架/无 ORM | 通过 | go.mod 两类坐标零命中；go.mod/go.sum 字节未变 |
| context.WithValue 仅追踪 | 通过 | 仅 web middleware 两处 requestID；business_auth 一处为禁止性注释 |
| 调用审计唯一、pending 先写 | 通过 | storage 外生产审计 SQL 零命中；AuditSingleRow/预写失败零执行回归 PASS |
| 凭证不落盘 | 通过 | 代码/配置密钥形状扫描零命中；PG 凭据仅既有 wrapper 内存注入 |
| 前向迁移 | 通过 | 0001–0008 零改写；新增 0009 与契约 DDL 逐字相同 |
| 单二进制 | 通过 | full 静态构建及额外 CGO_ENABLED=0 go build ./... 均 exit 0 |
| 显式时区 | 通过 | 生产零命中；全树两处命中均为既有测试注释/逆改说明 |
| 安全随机源/无点导入 | 通过 | 生产两类扫描零命中 |
| core 标准库 | 通过 | go list 实际导入均为标准库；没有项目包/第三方包 |
| LLM 审计入口唯一 | 通过 | core 生产 `.BeginCall(` 零命中 |
| 前端不进入 Go 七目标 | 通过 | Makefile/check-acceptance 两脚本 node/npm 零命中 |
| SQLite 不入运行期装配 | 通过 | app/web 生产导入零命中；既有测试夹具不算运行期 |
| 宪法未变 | 通过 | SHA256 与 HEAD 字节相同，无自行修宪 |

### 5.1 七处局部静态扫描抑制：用户已确认（2026-11-04）

[EngineeringStandards §1](../../docs/EngineeringStandards.md) 要求：`#nosec` 抑制“确有理由须逐条写明、经用户确认、进验收报告”。2026-11-04，用户明确引用“验收报告 §5.1 的七处局部 `#nosec` 仍需用户逐项确认，不将页面确认视为豁免批准”，并回复“我都确认”。该明确批准覆盖下表七项，依据为本次专门确认，而非此前的开发授权或页面确认。当前 sec 的通过是保留以下局部注释时的机器读数；无全局关闭规则，批准范围限于表中具体位置、规则及理由。

| 位置 | 规则 | 具体理由与约束 | 人工状态 |
| --- | --- | --- | --- |
| internal/isolation/trust.go:109 | G304 | 路径来自管理员配置的签名清单；读取限 1 MiB，先验原始字节签名再解析；不来自任务输入 | 用户已确认（2026-11-04） |
| internal/tool/isolation_cleanup.go:144 | G302 | 跨 UID 探针单次输出叶目录按批准设计需 0777；父目录仍私有 0700 | 用户已确认（2026-11-04） |
| internal/tool/isolation_cleanup.go:156 | G304 | 只读取本函数创建目录内固定 probe-result，不接受调用方文件名 | 用户已确认（2026-11-04） |
| internal/tool/isolation_cli.go:62 | G204 | 部署装配指定 Docker CLI，参数逐项 argv，固定子命令与本机 endpoint，不经 shell，有截止时间及输出上界 | 用户已确认（2026-11-04） |
| internal/tool/isolation_cli.go:217 | G204 | 同一 CLI 的固定 logs 操作，容器 id 单独 argv，输出持续排空且有界 | 用户已确认（2026-11-04） |
| internal/tool/runcode_stage.go:102 | G302 | 仅单次 OutputRoot 子目录允许服务 UID 与非特权容器 UID 交换产物，显式 chmod 抵消 umask | 用户已确认（2026-11-04） |
| internal/app/isolation_linux.go:40 | G115 | Bsize 正值且 Blocks ≤ MaxInt64/Bsize 后才转换与相乘，前置检查保证不溢出 | 用户已确认（2026-11-04） |

## 6. 复核结论与剩余人工项

先完成 [analyze 收口复跑](analyze.md)，再由独立上下文核查源码、判据面和原始证据，最后整理本报告。最终独立复核记录见 [review.md](review.md)，与审阅者原始文件逐字相同。结论：无未处置产品 P0/P1/P2，可提交 review ②。T001–T030 已完成；其中 T030 的交付是人审材料就绪，不是用户已批准。不把作者自检称为独立复核，不将机器通过写成人工收口。

人工 review ②：§5.1 七项抑制已获用户明确批准；五个已展示页面及换行修正已获用户视觉确认。已启用执行档表格仍未展示。后续收尾需按当前候选核对验证与证据，并完成本模块实现/契约与共享输出总量上限 v1 边界的整体收口记录；不再重复索取本表七项批准。

真实 WSL Linux 已完成，原 Q4 环境缺失不再阻断。仍不声称已验证：外部 CI、Windows/macOS 原生同等隔离、生产签名密钥与正式发布制品、完整服务容器升级部署、所有 CPU/PID/磁盘攻击强度、绝对无逃逸。真实模型/第三方 webhook 不属本 US 验收且未调用；生产发布留在既有发布流程。

## 7. 复跑与交接

可复制命令、专用 tmpfs/PG/候选前提见 [quickstart](quickstart.md)。最终对话提供按包改动、风险行号、判据面变化和验证入口四段 reviewer 导读。

本轮保留原生验证工作树、候选、夹具镜像及日志供查证；已确认本 US 标签容器零残留、卸载两个专用 tmpfs 并移除空挂载点，预览服务也已回收，见 [cleanup-final.json](../../.loop/us20/cleanup-final.json)。任务状态和失败历史保留，不替用户提交。运行期输出区按本实例独占；共享输出没有逐任务硬配额，其他任务可能受总量耗尽影响，已在契约和运维说明明确。

## 附录 A：逐项必跑读数

下表由冻结三份 stage 的 required_tests 与最终报告交叉生成；每行均对应报告原始 JSON PASS，重复名在不同 check 中分别判卷。

| check | 测试名 | 四态 |
| --- | --- | --- |
| us20-base-config | `TestUS20_Config_OldConfigUnchanged` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_UnknownKeyRejected` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_RunCodeMustBeGvisor` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_ImageMustBePinned` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_ImageMustBeApproved` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_ReleaseManifestRequired` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_IsolationDoesNotUpgradeProfile` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_TmpfsUpperBound` | 通过（PASS） |
| us20-base-config | `TestUS20_Config_TempFormulaBytes` | 通过（PASS） |
| us20-base-config | `TestUS20_IsolationCLI_Check` | 通过（PASS） |
| us20-base-core | `TestUS20_LocalRunnerRejectsIsolation` | 通过（PASS） |
| us20-base-core | `TestUS20_ContainerRunnerRequiresIsolation` | 通过（PASS） |
| us20-base-core | `TestUS20_ImageNotApprovedRejects` | 通过（PASS） |
| us20-base-core | `TestUS20_BuildCreateArgs` | 通过（PASS） |
| us20-base-core | `TestUS20_OutputDirOutsideRootRejects` | 通过（PASS） |
| us20-base-core | `TestUS20_InterpreterAbsolutePathStillValidated` | 通过（PASS） |
| us20-base-core | `TestUS20_TrustManifestSignature` | 通过（PASS） |
| us20-base-core | `TestUS20_RunCode_PolicyDenied` | 通过（PASS） |
| us20-base-core | `TestUS20_RunCode_PolicyAllowed` | 通过（PASS） |
| us20-base-core | `TestUS20_NoFallback_Unconfigured` | 通过（PASS） |
| us20-base-core | `TestUS20_ArtifactScanner_RejectsSymlink` | 通过（PASS） |
| us20-base-core | `TestUS20_ArtifactScan_Directory` | 通过（PASS） |
| us20-base-core | `TestUS20_ArtifactLimits` | 通过（PASS） |
| us20-base-core | `TestUS20_ArtifactPublishSerialized` | 通过（PASS） |
| us20-base-core | `TestUS20_ReleaseStage` | 通过（PASS） |
| us20-base-core | `TestUS20_RemoveOutputDirNoop` | 通过（PASS） |
| us20-base-core | `TestUS20_RemoveOutputDirArgv` | 通过（PASS） |
| us20-base-core | `TestUS20_RemoveOutputDirCreateArgs` | 通过（PASS） |
| us20-base-core | `TestUS20_RemoveOutputDirCleanupLeftover` | 通过（PASS） |
| us20-base-core | `TestUS20_PublishMuPrivateWhenNil` | 通过（PASS） |
| us20-base-core | `TestUS20_InstanceTotal` | 通过（PASS） |
| us20-base-core | `TestUS20_CreateUnknownNoRetry` | 通过（PASS） |
| us20-base-core | `TestUS20_SandboxLedger_Order` | 通过（PASS） |
| us20-base-core | `TestUS20_SandboxLedger_FinishUnknown` | 通过（PASS） |
| us20-base-core | `TestUS20_SandboxLedger_StoreFailureWarns` | 通过（PASS） |
| us20-base-core | `TestUS20_AuditSingleRow` | 通过（PASS） |
| us20-base-core | `TestUS20_ErrorKindMapping` | 通过（PASS） |
| us20-base-core | `TestUS20_PolicyDenialReasonUnchanged` | 通过（PASS） |
| us20-base-core | `TestUS20_CommandZeroValue` | 通过（PASS） |
| us20-base-core | `TestUS20_ShellLocalUnchanged` | 通过（PASS） |
| us20-base-core | `TestUS20_OCI_RejectsInlineCode` | 通过（PASS） |
| us20-base-core | `TestUS20_OCI_RejectsOutOfTaskScript` | 通过（PASS） |
| us20-base-core | `TestUS20_OCI_TranslateScriptArg` | 通过（PASS） |
| us20-base-core | `TestUS20_AdmissionStoreFailureDenies` | 通过（PASS） |
| us20-base-core | `TestUS20_AdmissionDoesNotDoubleCountLedger` | 通过（PASS） |
| us20-base-core | `TestUS20_MixedTierAdmission` | 通过（PASS） |
| us20-base-core | `TestUS20_PinnedSlotStaysOccupied` | 通过（PASS） |
| us20-base-core | `TestUS20_RunCodeArtifactCancellation` | 通过（PASS） |
| us20-base-core | `TestUS20_ArtifactCancelFinalWrite` | 通过（PASS） |
| us20-base-core | `TestUS20_ContainerTruncationMarker` | 通过（PASS） |
| us20-base-core | `TestUS20_RunCodeOptionalNullRejected` | 通过（PASS） |
| us20-base-race | `TestUS20_ConcurrentAdmissionRace` | 通过（PASS） |
| us20-base-race | `TestUS20_AdmissionDoesNotDoubleCountLedger` | 通过（PASS） |
| us20-base-race | `TestUS20_MixedTierAdmission` | 通过（PASS） |
| us20-base-race | `TestUS20_PinnedSlotStaysOccupied` | 通过（PASS） |
| us20-base-race | `TestUS20_ArtifactPublishSerialized` | 通过（PASS） |
| us20-base-app | `TestUS20_Probe_RuntimeMissingRefusesStartup` | 通过（PASS） |
| us20-base-app | `TestUS20_CleanupOrphans` | 通过（PASS） |
| us20-base-app | `TestUS20_CleanupOrphansSkipsHandled` | 通过（PASS） |
| us20-base-app | `TestUS20_CleanupOrphansFailClosed` | 通过（PASS） |
| us20-base-app | `TestUS20_CleanupOrphansListFailure` | 通过（PASS） |
| us20-base-app | `TestUS20_DeclaredToolsMatchRegistry` | 通过（PASS） |
| us20-base-app | `TestUS20_BuiltinCatalogIncludesRunCode` | 通过（PASS） |
| us20-base-app | `TestUS20_MCPBoundaryUnchanged` | 通过（PASS） |
| us20-base-app | `TestUS20_RecoveryManyEmptyRuns` | 通过（PASS） |
| us20-base-web | `TestUS20_ConsoleReadOnly` | 通过（PASS） |
| us20-base-web | `TestUS20_OpenAPIContract` | 通过（PASS） |
| us20-base-web | `TestUS20_RunDetailProjection` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_CodeExecution` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_NoSecrets` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_NoNetwork` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ReadOnlyRoot` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ResourceExhaustion` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_CrashResidue` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ImageTamper` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_MCPBoundary` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ArtifactRejectsSymlink` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ArtifactLimits` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ArtifactRoundTrip` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ArtifactNestedDirCleanup` | 通过（PASS） |
| us20-gvisor | `TestUS20_GVisor_ArtifactOddNameCleanup` | 通过（PASS） |
| us20-oci-lifecycle | `TestUS20_OCI_ShellLifecycle` | 通过（PASS） |
| us20-oci-lifecycle | `TestUS20_OCI_CancelTimeout` | 通过（PASS） |
| us20-oci-lifecycle | `TestUS20_OCI_Lifecycle` | 通过（PASS） |
| us20-oci-security | `TestUS20_OCI_NoNetwork` | 通过（PASS） |
| us20-oci-security | `TestUS20_OCI_NoHostSecrets` | 通过（PASS） |
| us20-oci-security | `TestUS20_OCI_ReadOnlyRoot` | 通过（PASS） |
| us20-oci-security | `TestUS20_OCI_ResourceLimits` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_OrphanCleanup` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_OrphanDirSweep` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_CrashResidue` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_RecoveryReleasesCapacity` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_ImageDigestMismatch` | 通过（PASS） |
| us20-oci-cleanup | `TestUS20_OCI_ImageNotApproved` | 通过（PASS） |

## 附录 B：私有工件指纹

私有文档不在 Git diff 中；以下是本轮最终文件 SHA256，原模块卡审批 hash 仍保留在 run-state。

| 文件 | SHA256 |
| --- | --- |
| docs/TechnicalDesign.md | `e303aa5bc8db2ce75228ffe93225858e741fe965a9619e7029a5bd1b299916ec` |
| docs/Requirements.md | `c5cf915644eea14761732bfc5dcb03712f3a14a4b7d747463080bda6d3b5280f` |
| docs/Compatibility.md | `17a857a5a9b0f245f674f29008e4bd9a9867a0a217e2c88d525d2780029d9e7b` |
| docs/us/REGISTRY.md | `9c084b9cb44129a16e6af38873eb789a12e961779741843f85f9687ff56a6185` |
| docs/us/us20-execution-isolation.md | `9671eaa8519cb2856bfb4d7049b755f21577aae04927c4c3b7a353ffd8a58ba6` |

## 截图复核补充（2026-11-04）

用户提供五张实际页面截图，确认未启用提示、清理失败警告、unknown 退出码未知及空态实际显示。发现两项：四条预览样例的 runs.reason 被作者错误填入样例说明，导致 unclassified；成功行 cleaned 被 anywhere 样式拆行。

第一项只修正独立预览数据及生成器，不改诊断投影、公开原因闭集或产品代码；四条实际 API 断言先红后绿。第二项仅将隔离表格 td 的 overflow-wrap 改为 break-word，保留长 digest 换行和表格滚动。472项前端测试、前端构建、Go Web全包和静态构建通过；当前8080候选 SHA256 为 `5730318da8890bcf24dd1d13a453093af7b4edffa2bfff2e23a23eb7098b67a6`。原冻结工作树/报告未改，不宣称旧完整门禁与容器候选hash是新UI候选的hash。

截图修订后的实际视觉与已启用执行档表格仍待确认；七项局部抑制与最终人审结论不变。定位、原始读数和预览状态见 run-state 末节及 `.loop/us20/preview/`。

后续用户明确反馈“页面没问题了”：已展示的未启用/成功/清理失败/unknown/空态页面及换行修正的人工视觉检查现记为通过。已启用执行档表格仍未展示，七项局部抑制及整体review②不因此自动通过。us20-ui-sample仅为运行样例数据中的名称，不是已创建或注册的Agent。

## 局部抑制批准补充（2026-11-04）

用户随后专门引用 §5.1 七处局部 `#nosec` 的待确认事项并回复“我都确认”。本次已将 §5.1 七行逐项标为用户已确认，并同步 run-state 当前状态；此前待确认的过程记录和独立复核原文保留。七项批准不再是收尾待决项，后续由 Claude 接续最终验证与收口记录。
