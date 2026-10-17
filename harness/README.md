# Basalt 开发验收 Harness

公共验收规则与机器结果在此维护；专属验收语义写在每份 US 颗粒度文档。
[agent-harness](../.claude/skills/agent-harness/SKILL.md) 负责判据设计、验证和收口方法，
[loop-programming](../.claude/skills/loop-programming/SKILL.md) 由当前 Agent 驱动实现、
验收与修复。Basalt 绑定见 [project.md](project.md)。验收工具不启动编码 Agent。

## 使用入口

Claude 中使用 `/loop-programming <已审 US 或任务>`；`module-dev` 在完成派生与分析后
进入同一 skill。固定人工 review 为 US 颗粒度文档生成后和 US 收口时。

```powershell
# Windows：完整公共验收 / 仅固定评测 / 当前任务 / 完整 US
.\scripts\check-acceptance.ps1
.\scripts\check-acceptance.ps1 --profile evals
.\scripts\check-acceptance.ps1 --stage harness/stages/<阶段>-r1.json --task T001
.\scripts\check-acceptance.ps1 --stage harness/stages/<阶段>-r1.json
```

```sh
# Linux / macOS：相同参数与退出码
sh scripts/check-acceptance.sh
sh scripts/check-acceptance.sh --profile ci --out .loop/runs/ci-local
sh scripts/check-acceptance.sh --stage harness/stages/<阶段>-r1.json --task T001
sh scripts/check-acceptance.sh --stage harness/stages/<阶段>-r1.json
```

示例中的 `<阶段>` 替换为从 [stage.example.json](stage.example.json) 派生并写入 `harness/stages/`
的文件名；仓库不预置具体阶段文件。

`make acceptance` / `make evals` 调用相同验收入口。完成的工程门禁仍为 `make check`
七目标；完整公共验收在此基础上加入固定行为 / 注入评测。任务检查或 evals-only 不替代完整验收。
`--out` 必须是 `.loop/runs/` 下全新的单层目录；默认带时间戳，不能覆盖旧证据。
包装器编译后运行验收程序，保留 0/1/2；不要用 `go run` 的退出码区分内部失败类别。

| 退出码 | 含义 | skill 后续动作 |
| --- | --- | --- |
| 0 | 所选全部检查通过，允许的真模型项可按规则 SKIP | 保存证据并推进；任务通过仍须最终完整验收 |
| 1 | 检查 / 断言失败（含非评测命令退出 2，如 make recipe 失败） | 定位失败并按既有预算修复 |
| 2 | 参数 / 配置 / 工具缺失、超时 / 中断、报告写失败、验收期间源码变化等 | 保存现场、处理前置或执行错误后重验 |

修复预算和无进展停止由 skill 管理，不是验收程序的退出码或自动重试功能。

## 公共检查与必跑规则

[acceptance.json](acceptance.json) 是公共可执行清单的唯一事实源。
`full` / `ci` = fmt、lint、sec、test、test-integration、cover、build、eval-build、
behavior、injection；`task` = fmt，再加任务专属检查；`evals` 只运行构建和两组固定评测。
`--task` 未显式给 profile 时选 task；`--stage` 不选 task 时默认 full，并加入该阶段
所有 checks（含未绑定单个任务的整合断言）。任务绑定用于定向选择，不能缩减最终验收。

| 类别 | 必须实际 PASS 的 case ID |
| --- | --- |
| 固定行为 | tool-selection-read-file、outcome-list-and-read、outcome-iteration-limit、injection-read-outside、usage-token-accounting |
| 固定注入 | injection-cross-agent-read、injection-protected-agent-md-write、injection-scripts-readonly-write、injection-write-outside-agent |

唯一允许 SKIP：`outcome-real-model-answer`，须由用例声明真实模型条件、报告写明原因，
且不属于 required_cases。必跑项缺失、SKIP、重复 ID、报告与退出码矛盾都失败。
固定替身验证确定行为与权限边界，不能证明真实模型效果或抗注入泛化。
专属 Go 检查使用 `go test -json -count=1` 和 `required_tests`：具名测试及其子测试
不能 SKIP，空匹配不能通过。检查日志、JSON 和实际退出码一起读。

## 接入一个 US 或扩展阶段

1. 人审模块卡后，AI 自动完成登记、spec / plan / tasks / analyze；CRITICAL 清零。
2. 从模块卡的公共引用及专属断言派生 [stage.example.json](stage.example.json)，写入
   `harness/stages/<阶段>-r1.json`。stage 只是验收映射：批准引用、inputs、protect、
   checks 和 task → checks；不含 Agent CLI、prompt 或重试预算，不另立需求事实源。
3. `approval` 引用人工 review ① 的真实决定及所审版本；派生输入指纹由 AI 留证，
   不宣称 tasks 另经人审。替换所有占位路径和测试名；模板自身不授予批准。
4. 控制侧冻结判据、取得有效反例，Loop skill 在批准范围内实现和修复。每个命令
   显式指定 1～7200 秒超时。先定向验收，最后运行完整 stage 与适用的整链路检查。
5. 独立 AI 复核与收口证据完成后，进入人工 review ②。未来 US 编号无上限，新的
   沙箱、存储或分布式检查按真实交付接入，不把尚无环境的检查算作通过。

`inputs` / `protect` 声明范围并校验路径，skill 按批准基线核对变更；它们不是写权限
隔离或执行器自动回滚。验收源码指纹覆盖跟踪改动及未忽略的新文件，检查期间变化
退出 2。`tasks.md` 仅归一化勾选位，正文变化仍改变指纹；`run-state.md`、`.loop/`、
凭证与生成目录排除。报告不能证明未纳入指纹的状态，制品与外部服务需各自留证。

## 结果、恢复与清理

每次验收输出 `.loop/runs/<run>/report.json`、逐检查日志与适用的评测结果。
报告含 version、scope、fingerprint、started_at、status、exit_code、checks。
检查状态为 passed / failed / error / not_run；running 报告只表示进度，不能授权推进。
CI 上传完整 runs 目录（含失败与控制台日志）；保留原 job 名称及既有平台检查。

`specs/<feature>/run-state.md` 是 Loop 过程记录：当前任务、输入基线、尝试前记账、
失败累计、报告位置和下一步。恢复读取此记录，先验证可能已完成的工作，再决定修复。
skill 依赖当前 Agent 和工具执行，不能保证进程被强杀前尚未写出的状态，也不会自动
唤醒新会话。旧 `.loop/state/` 如存在仅保留历史证据，不再读写机器 checkpoint。

验收用 `.loop/lock` 避免同工作区同时检查；强杀可能遗留锁，核实 PID、旧进程与
子进程已退出后仅移除该锁。命令超时 / 取消清理本轮子进程。不要删除历史记录清零
预算，不操作共享或生产资源，不自动提交 / 推送 / 发布。

哈希、protect 和同权限的角色分工是协作守卫。只有可信验收策略与执行权限实际隔离
的外部 CI 才提供相应的权限保护；远端配置未核实时如实记未验证。
