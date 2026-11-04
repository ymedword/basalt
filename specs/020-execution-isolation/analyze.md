# US20 实施前一致性分析

2026-11-04；输入 spec/plan/tasks 已存在，前置脚本检查通过。分析阶段只读，以下为控制侧保存的结论。

| ID | 类别 | 严重度 | 位置 | 发现与处置 |
| --- | --- | --- | --- | --- |
| A01 | 接缝 | HIGH，已处置 | plan Internal clarifications / T014–T015 | 伪 helper 缺 ctx/opts、发布缺 SourceDir/Root；补显式内部输入，实例上限覆盖所有 Agent，不引全局状态 |
| A02 | 安全 | HIGH，已处置 | plan Internal clarifications / T010 | Docker 默认拉镜像、默认共享内存与已审边界不符；共用 argv 强制 pull=never/ipc=none |
| A03 | 兼容 | MEDIUM，已处置 | T005/T016/T018 | 保留 US19 Flow 历史保护和工具白名单；不把普通工具注册推成 Flow 授权 |
| A04 | 事实 | LOW，已处置 | research / run-state | 原 Q4 未授权和 US19 未合入状态已被用户后续指令与实际 HEAD 更新；原记录保留 |

15/15 FR 均映射 tasks 的 coverage 表，5/5 SC 均映射末段；30 项任务均有路径与 DoD，无游离实现任务。测试与实现同任务建立，11 个指定变异面在 T027 必验。source 不伪造 Docker，真实两档未跑不得收口。

宪法十条与架构约束无冲突；core 标准库、两接口、审计唯一、无新依赖、迁移追加、无业务 context 状态、同步执行均有对应任务及回归。当前 CRITICAL=0，未处置 HIGH=0；允许按 tasks 实施，不构成实际验收通过或人工 review ②。

独立只读研究已核对公钥、迁移、发布清单接线及命令边界；最终独立代码复核仍在 T030，不拿本分析替代。

## 2026-11-04 收口复跑

独立上下文 us20_research 对冻结模块卡、spec/plan/tasks、实际生产/测试与三份 stage 重新比对。15/15 FR、5/5 SC 均有任务映射，30 项任务无游离扩展；原 9 个 check 的 93 个 required_tests 条目均有定义且正则覆盖，随后新增 null 拒绝回归也纳入 source core check。恢复测试迁至 app 同包后，两档 stage 均显式执行该包。

| ID | 严重度 | 发现 | 处置与验证入口 |
| --- | --- | --- | --- |
| A05 | MEDIUM | gVisor 秘密测试中间失败可能被末尾成功覆盖 | 改为 `&&` 链；分别用镜像内 `/etc/hosts` 替换两个禁止路径作为负向控制；真实 gVisor stage 复验 |
| A06 | MEDIUM | OCI 取消审计断言误允许闭集外 cancelled | 精确断言既有 `timeout`；OCI lifecycle 复验 |
| A07 | MEDIUM | stage protect 缺模块卡要求的生产安全路径 | 三份 stage 增补 runner/cleanup/ledger/产物/恢复/配置/PG/视图，重新 lint 和冻结 |

以上均落实原判据，不减少断言、不扩大产品范围。独立代码复核另发现 FINAL-01 可选 null 入参问题，已先红后绿修复。当前 CRITICAL=0、未处置 HIGH=0、未处置 MEDIUM=0；源码修复后的真实两档与完整门禁读数在 run-state / acceptance 中另行登记，静态分析不替代运行结果。

恢复夹具实际杀死执行子进程并调用生产恢复；两档各走 shell/run_code，Flow 白名单未扩权。没有新 Go 坐标、历史迁移改写、第二套调用或模型审计路径。CPU/PID/日志的真实证据为 Docker HostConfig 核对，实际耗尽实验覆盖 tmpfs/内存；不得将前者称为逐项耗尽实验。

实施适配：仓库没有 README.zh-CN.md，中文能力说明落在现有 website/zh/architecture/index.md；不为匹配预估文件名新建重复入口。外部 CI 未执行；本机 WSL Linux 已获用户明确授权，替代原环境缺失状态，不能据此宣称 Windows/macOS 原生同等隔离。
