# US20 最终独立复核

日期：2026-11-04。审阅者：独立子上下文 `us20_tool_review`。结论：在下述范围内，未发现仍未处置的产品 P0/P1/P2；实现与自动验收证据足以提交人工 review ②。七处局部静态检查抑制尚须用户逐项确认，管理台真实视觉检查尚未完成；本结论不代签人工收口。

## 范围与方法

对照已审模块卡 R1.5、`specs/020-execution-isolation/{spec,plan,research,tasks,run-state}.md`，先后只读审阅执行器、命令守卫、准入与账本、产物校验/发布、镜像验签及 releasekit，随后审阅 app/CLI 装配、三类启动恢复、Web/console 只读投影、契约及新增/适配的测试与阶段判据。最后读取四份冻结报告、record、原始日志及变异文件，核对证据链；不以作者摘要代替原始结果。

本次最终证据复核没有启动容器、真实模型或读取生产私钥，没有修改产品或判据。此前曾独立运行五项 source 回归：`TestUS20_RunCodeOptionalNullRejected`、`TestUS20_RunCodeArtifactCancellation`、`TestUS20_ArtifactCancelFinalWrite`、`TestUS20_ContainerTruncationMarker`、`TestUS20_RecoveryManyEmptyRuns`，tool/app 均通过。真实 WSL 行为依据作者执行并保存的原始日志与候选关联证据复核，不冒充审阅者另跑了一遍。

## 发现与处置

| ID | 严重性/类别 | 触发条件与事实 | 要求/定位 | 最终处置 |
| --- | --- | --- | --- | --- |
| FINAL-01 | P2，已修复 | `inputs:null` 或 `timeout_seconds:null` 原可越过参数类型约束并执行 | FR-20-03；`internal/tool/runcode.go:84`、`runcode_test.go:106` | 显式拒绝 null；冻结 source 回归 PASS，拒绝时零执行 |
| PRE-01 | P1/P2，已修复 | 产物最后一次写入时取消原可能仍发布；收集层原可能抹去取消原因 | FR-20-07/09、SC-003；`internal/tool/runcode_artifacts.go:248`、`runcode_artifacts_test.go:23`、`runcode_test.go:148` | 末端检查取消并回滚，保持取消分类；两项冻结回归 PASS |
| PRE-02 | P2，已修复 | OCI stdout 被截断时原缺少明确截断标志 | FR-20-06；`internal/tool/isolation_runner_test.go:63` | 与既有受控输出语义一致，冻结回归 PASS |
| PRE-03 | P1，已修复 | 正常执行遗留空 run 父目录，累计后启动清扫触及数量上限 | FR-20-08/14；`internal/app/isolation_test.go:22` | 正常路径回收空父目录、恢复分批有界清扫；大量空目录回归 PASS |
| PRE-04 | P2，已修复 | 未完成或未知执行原显示默认退出码 0，误导为成功退出 | FR-20-11；`assets/console/src/views/isolation.ts:50` | 仅 finished 展示退出码，其余明确未知 |
| EVID-01 | P2，证据缺口已修复 | 04/05/10/11 初版变异使用末次 Probe 权限修订前的 cleanup.go | SC-005；`.loop/us20/mutations/results.json` | 四项按冻结源复跑，原记录保留于 history-before-final-refresh；新红态与基线 hash 已逐项核对 |
| HUMAN-01 | 人工门禁，待裁决 | 本轮七处新增 `#nosec` 尚无逐项用户确认 | `docs/EngineeringStandards.md:19`；下表 | 理由与前置约束成立，但机器 sec 通过不等于人工豁免批准；须 review ② 明确裁决 |
| LIMIT-01 | 未验证面 | 尚无本轮管理台真实页面截图/视觉实测 | FR-20-11；`acceptance.md` §6 | 单测/构建不替代视觉确认，明确留给人工 review ② |

此前 Probe 的跨 UID 清理脚本问题也已在最终代码中处理：父目录保持私有，叶目录显式 0777；清理段允许修复任务自建子项，之后恢复严格模式并断言 locked 与 `..cache` 均消失。最终两档真实 Probe 及恶意目录用例通过，不保留早期失败的错误通过口径。

## 冻结证据核对

四份记录均为 passed / exit 0，且各 check 为 passed / exit 0：

| 报告目录 | 范围 | 必跑条目 |
| --- | --- | --- |
| `.loop/runs/us20-public-full-native-r2` | full 的 11 checks：fmt、lint、sec、test、test-integration、test-pg、cover、build、eval-build、behavior、injection | 全部门禁通过；行为评测跳过单列如下 |
| `.loop/runs/us20-base-native` | 5 个 source checks，含并发 race | 68/68 exact PASS |
| `.loop/runs/us20-oci-native` | 3 个真实 Linux OCI checks | 13/13 exact PASS |
| `.loop/runs/us20-gvisor-native` | 1 个真实 Linux gVisor check | 13/13 exact PASS |

- 四份 report/record 的共同源码指纹为 `39ccb1d6eb9b9f2e5516cfc0cf95f845f2d7abd13b2668c506c74789a58b2645`，基线 HEAD 为 `7a2c28884c27cb0b3de6cfd61ff332264a406aec`。依据实际 fingerprint.go 的排除规则、权限、tasks 勾选归一化，在 native 工作树独立重算 2012 个计入指纹的路径，结果完全一致。
- 逐个复算 record 中的 report SHA256 与全部日志 SHA256，均匹配。按 required-tests.json 对照原始 go-test JSON 的 `Action:pass`，94 条必跑条目全部精确命中、零 fail/skip；去重为 90 个测试名。没有仅凭 `go test` 退出码或正则匹配推断测试已运行。
- 两档 report、record、原始日志中的 binary SHA256，及 native `bin/basalt` 实际文件 hash，均为 `bbe125d34900926ead5aa5fb651c783f3c9a4567e10afcc15693badbf4d9c8f5`。
- 原始日志记录 service UID 1000，任务 UID 65534（只读根控制用例为 10001），runc/runsc 与固定测试镜像；因此确有跨 UID 和两档实际行为证据。未将默认本机或协议桩执行冒充容器执行。
- snapshot-files.json 的 2312 个路径逐字对照主树与 native；冻结产品、测试、stage 均一致。三个旧 dist hash 文件的 null 是预期删除。主树后续 run-state、acceptance、quickstart 等文档整理不冒充已冻结源码指纹；新文档不影响上述产品字节一致性。
- full 的行为评测为 **5 PASS / 1 SKIP**。跳过的 `outcome-real-model-answer` 因未提供 provider，遵守后续 US 禁止自动真实模型调用的规则，非 US20 required；不能宣称全库所有用例零跳过。注入评测 4 PASS / 0 SKIP。详见 `us20-public-full-native-r2/behavior.log:30`。
- full 覆盖率、race 与 CGO_ENABLED=0 构建证据存在；未降低覆盖率阈值。go.mod/go.sum/宪法与 HEAD 无差异，历史迁移无改写；新增 0009 与 isolation.pg.sql 的实际 SHA256 完全相同。私有事实文档 hash 与 invariants.json 一致。

## 逆改判别力

11 条当前记录全部 exit 1 且有目标测试的功能断言失败；逐份检查了 red.log、Go overlay、变异文件与冻结原文件 hash。没有 `[build failed]`、未定义符号或语法错误冒充有效红态。原版同名测试均在最终 stage 中 PASS。

| ID | 实际失败断言 |
| --- | --- |
| 01-network | 去掉 network=none 后本地监听器可达，预期非零退出却得到 0 |
| 02-readonly | 去掉 read-only 后写 `/data` 成功，拒绝断言失败 |
| 03-output-mode | 0777 改为 0755 后实际权限为 755，跨 UID report.txt 写入被拒 |
| 04-cleanup-fallback | 移除权限兜底后 000 子目录遗留，预期 artifact_rejected 实际 cleanup_failed |
| 05-odd-name | 不枚举 `..?*` 后 `..cache` 遗留，清理断言失败 |
| 06-capacity | unknown+cleaned 仍计容量后 active 为 2，预期为 0 |
| 07-publish-race | 锁不覆盖 check/write 后 10 轮 race 测试均出现多份成功发布，预期仅 1 |
| 08-handled | 去掉 handled 跳过后账本清理失败被二次当作孤儿致命错误 |
| 09-temp-budget | 不扣除其他预算后接受非法 tmpfs 总量，配置反例失败 |
| 10-helper-label | purpose 改错后缺少 basalt.purpose=cleanup 参数，固定断言失败 |
| 11-helper-absence | 跳过缺失复查后 helper 重现仍被报 clean，断言失败 |

04/05/10/11 的最终 overlay 来自 `/tmp/us20-mutations-1793774126615712850`，cleanup.go 原文件 hash 为 `b6fc6b2d46d9d9fd51d991cc0601b42234e753c0813dd9c90475f0999d8b7473`，与冻结源码一致；其余七项原文件 hash 同样匹配。

## 七项局部抑制的独立判断

规范原文：`#nosec` 抑制“确有理由须逐条写明、经用户确认、进验收报告”（EngineeringStandards.md:19）。下列理由与源码约束一致，没有发现为压掉真实缺陷而添加的全局禁用或无关抑制；全部仍为 **待用户确认**。

| 位置 | 规则 | 独立判断及边界 |
| --- | --- | --- |
| `internal/isolation/trust.go:109` | G304 | 管理员指定发布清单，限长读取，原字节先验签；任务不能供应路径或信任公钥 |
| `internal/tool/isolation_cleanup.go:144` | G302 | 单次 Probe 输出叶目录跨 UID 写入所需，显式 0777；父目录保持 MkdirTemp 私有权限 |
| `internal/tool/isolation_cleanup.go:156` | G304 | 内部新建探针目录下固定 probe-result，只用于自检结果 |
| `internal/tool/isolation_cli.go:62` | G204 | 受信装配的 Docker CLI 与独立 argv，不经 shell；固定操作及有界上下文/输出 |
| `internal/tool/isolation_cli.go:217` | G204 | 同一 CLI 的固定 logs 操作，独立容器参数与有界排空 |
| `internal/tool/runcode_stage.go:102` | G302 | 仅每次输出叶目录显式 0777，符合已批准跨 UID 产物交换设计；不扩到 output_root/staging |
| `internal/app/isolation_linux.go:40` | G115 | Bsize > 0 且 Blocks ≤ MaxInt64/Bsize 的前置检查证明转换及乘法安全 |

## 未覆盖面与结论边界

未进行真实管理台视觉确认、外部 CI、生产密钥/正式发布制品验证、完整容器部署升级、Windows/macOS 原生同等隔离或穷尽逃逸测试。CPU/PID/日志限制有实际 HostConfig 证据；不把参数读取写成全部资源的真实耗尽攻击。共享输出区只有实例总量硬限与单任务事后校验，单任务相互争用风险是已批准 v1 边界。MCP/HTTP 等外部工具服务不被本模块自动隔离。

`acceptance.md` 已准确区分机器通过、人工待确认和未验证面。建议进入人工 review ②；用户确认七项局部抑制并完成/裁决管理台视觉检查后，再按流程决定 US 收口。未自动 commit/push/merge/tag/发布/部署。
