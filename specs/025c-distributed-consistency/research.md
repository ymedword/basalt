# Research: 已发布资产自动分发与跨进程一致性（US-25C）

**输入**：已审模块卡 R8 定稿（R9 仅补适用条件）、`docs/TechnicalDesign.md`、现有资产 / 调度 / 租约 / MCP / 策略代码。全部决策已在模块卡中作结；本文件只记录决策、理由与被否替代方案，不引入新选型。

## R1 分发介质：数据库内容寻址载荷表

- **Decision**: 新建 `agent_bundle_payloads`（`bundle_hash` 主键，`payload bytea`，`payload_sha256`、`size_bytes`、`created_at` 由 Go 写毫秒值），配 `(agent, created_at DESC)` 索引；不引入对象存储、NFS、PVC 或消息队列。
- **Rationale**: 数据库已是共享运行时权威，发布事务能保证「指针存在 ⇒ 载荷存在」；单二进制与 Compose 拓扑不新增基础设施；内容哈希去重天然幂等。
- **Alternatives considered**: 对象存储（MinIO/S3）——增加部署件与一致性边界，首发不支持；文件复制 / rsync——无法在事务内与指针原子，正是本次要修的缺口；消息队列——派生第二套消息路径，违反宪法 VII。

## R2 发布顺序：先载荷后指针；同哈希也补载荷

- **Decision**: `writePayload` 严格先于指针 CAS；载荷写失败调用 `finishFailed(ctx.WithoutCancel)`，指针不动；同哈希发布仍核对载荷存在，缺失时补写载荷但**不移动指针**。
- **Rationale**: 「指针存在 ⇒ 载荷存在」是 Worker 同步的前提；同哈希短路是当前幂等漏洞（恢复短路后载荷表仍为空）。
- **Alternatives considered**: 先移指针后写载荷——失败留下悬空指针，Worker 永久阻塞；发布事务里同时写两者——载荷可能超过事务/内存界，失败撤销成本高；同哈希直接跳过——存量缺口无法自愈。

## R3 同步工作集：四态状态机 + attemptSeq 轮转 + 复合键集

- **Decision**: `pending` / `installed` / `ready` / `blocked` 四态；`installed` 不等于 `ready`——每轮对已安装项重做技能物化与 Entry 构建，注册成功才置 `ready`；单轮按 `attemptSeq` 取至多 `sync_batch` 条，失败退避 `min(2^(failCount-1) × sync_interval, 5m)`、`failCount` 封顶 6；增量发现用 `(updated_at, resource_id) > ($1,$2)` 键集，每页严格前进；每 `full_scan_every` 轮发起一次按 `resource_id` 键集的全量扫描，整趟完成且 `PointerByUID` 回读确认后才移除缺席项；单轮总超时 `sync_timeout`，上一轮未完成不叠加，停机先 join 再关库。
- **Rationale**: 固定「按名字取前 N 条」会让前 N 条永久失败项占满批次；`updated_at >= cursor` 会在同毫秒 201 行时重复第二页并永远漏掉第 201 行；`installed` 当 `ready` 会让一次注册失败永不重试；全量中断即删会让存活 Agent 被移出 readiness。
- **Alternatives considered**: 每轮全量扫描——数据库压力与 O(N) 读放大；失败项直接删除——恢复后无人重试；用 `max(updated_at)` 作游标——同毫秒与迟到提交不可靠。

## R4 钉住领取：领取事务内固定版本 + StartRun 同事务复核执行权

- **Decision**: `ClaimDueTriggerPinned` 在既有领取事务内 `SELECT … FOR SHARE` 锁 `agent_bundle` 指针行并核对哈希；指针缺失 / 已删 → `ErrAssetNotReady`，哈希不符 → `ErrAssetMoved`，两者零写入（不写执行行、不推进 `next_fire_at`）；成功时把 `expectHash` 写入同一事务的 started 执行行 `pinned_bundle_hash`。Run 准入在 `StartRun` 同一事务、固定锁序（生命周期行 → 执行租约 `FOR SHARE` → 执行行普通读）内复核：租约 owner / epoch / `released_at IS NULL` / `expires_at > db_now`，执行行 status / `run_id` / Agent / 哈希；任一不符 → `ErrClaimPinInvalid` → 503 `ASSET_UNAVAILABLE`，零新 Run；`Pin == nil` 的普通 Run 保持当前指针判据。
- **Rationale**: 「查指针 → 发布 → 领取 → 准入」窗口会消耗触发且永久丢一次到期；只凭调用方传入哈希则不校验，Entry 与资源可能分叉；`status='started'` 不证明执行权，领取后暂停、租约被回收器接管（epoch+1）或自然过期时旧 Worker 恢复仍可能创建新 Run——必须与执行租约同事务核对。
- **Alternatives considered**: 准入再读当前指针——发布窗口破坏在途版本；只核对执行行状态——接管后旧方仍通过；新契约错误码——无必要，复用 `ASSET_*`；自动重放失败触发——副作用未知，违反 VII。

## R5 调度扫描：两段键集轮转 + 每圈冻结边界

- **Decision**: `SweepBounds{Cutoff, NullUpperTaskID}` 每圈由一次只读 `ReadSweepBounds` 冻结（数据库时间 + NULL 段最大 `task_id`）；单拍只取一页，NULL 段 `task_id > $nullAfter AND task_id <= $nullUpper ORDER BY task_id`，非 NULL 段 `next_fire_at <= $sweepCutoff [AND (next_fire_at, task_id) > ($afterAt,$afterID)] ORDER BY next_fire_at, task_id`；`AfterNextFireAt == nil` 首屏省略元组下界；两段都到达冻结边界才算一圈，短页不作结束判据；只有 `Start` 清空游标与边界，`SetRegistered` / `ReplaceEntries` 不清空；missed 逐行求值先于就绪门。
- **Rationale**: 每拍从段首取 → 前 N 条不可领取占满批次，健康任务永不到达；NULL 整页复位 → 后续健康任务永不执行；高频到期使每页始终取满，一圈永不结束，恢复任务与新增 NULL 行长期不被考察；首屏拿 NULL 做元组比较恒为 NULL，非 NULL 段首屏零行。
- **Termination bound**: 正常时钟下每圈查询次数 `ceil(N/n) + ceil(S/n) + 1`，其中 N 为 NULL 段行数、S 为非 NULL 段到期行数、n 为单拍容量；仅当「所有移除路径都严格前进 `next_fire_at`（`max(old, dbNow)` 派生）、数据库时钟不回拨、外部对 `next_fire_at` 的重写有限」时成立；圈内新到期行归下一圈，不延长本圈。
- **Alternatives considered**: 无边界全量游标——高频到期下一圈永不结束；读不满页即结束——持续到期时误判结束；`OFFSET` 分页——删除与更新下重复 / 漏行。

## R6 策略刷新：各服务锁内应用并自取摘要；过期用最近成功核对时间

- **Decision**: 四个策略服务各自实现 `RefreshFromStore(ctx) (changed, digest, err)`：持自己的 `mu` 读表、应用，并只用**这份实际应用的行**计算子摘要；`appliedDigest = sha256(fixed-order four sub-digests)`；刷新单飞；`lastVerifiedAt` 与内容版本分开，读失败才推进过期；准入返回 `POLICY_STALE`（503）。
- **Rationale**: 用 `max(updated_at)` 漏删除；发现阶段另一次读的摘要与应用内容可分叉——应用 B 却落 A 的摘要，撤权用例永久漏刷新；用内容版本判过期会让长期无变化实例每 5 分钟 503。
- **Alternatives considered**: 单事务四读快照——仍与逐服务锁内应用分叉；定时重建整个策略服务——影响在跑 Run 且代价高；把过期判定并入内容摘要——空闲误判。

## R7 技能联动：锁内复核 + 发布前复查 + 运行期有界修复

- **Decision**: 技能发布成功后调用 `republishDependentBundles`：逐 Agent 取 `AgentGate.Lock`，锁内重读指针与 `PointerVersion` 作为当前版本；从**已安装 bundle** 重建（`rebuildBundleWithSkill`），校验 `ConfigFiles` 与原清单逐项相等；发布前再次确认 `PointerVersion`，不符记 `superseded`；结果 `{ok, skipped, superseded, missing, failed}` 原样返回、请求内不重试；控制面 `SkillRepairer.RepairOnce` 批量 5、按名轮转、退避、连续 8 次转 `blocked`、下一轮全量扫描复位；Worker 的 `Repair` 为 nil。
- **Rationale**: 从工作副本重建会把未批准改动带进新 bundle；不复查会让晚到的旧版本覆盖新版本；失败只靠重启恢复违背「运行期自愈」验收。
- **Alternatives considered**: 发布请求内同步重试——长请求阻塞管理台且仍可能失败；后台无限重试——无界资源；Worker 侧重发——违反「控制面定义权威」。

## R8 MCP 准备：`PrepareAtApplied` P1–P6 + 换入前复核

- **Decision**: `MCPSupervisor.PrepareAtApplied` 只处理 applied 连接：`entry.mu` 内取 P1–P6 快照（目标版本、凭据代次、epoch）后立即解锁；锁外构建候选；按 `ResourceGate → entry.mu` 提交，`Commit: nil`、`OldLegal: nil`；换入前在同一临界区由只读 `Verify` 复核目标版本、`StillCurrent` 与凭据代次，失败 `superseded` 不换入；P1（连接 live + 版本相符 + 凭据代次相符）零副作用复用；既有管理 apply（`Commit != nil`）保持事务校验 + CAS 成功后立即换入，不插入提交后检查，构造期拒绝 `Commit` + `Verify` 组合。
- **Rationale**: 每轮 `RebuildAtApplied` 会周期性 `seal` / `Close` / `Connect` / `epoch++`；只比版本 ID 会在凭据轮换后继续用旧连接；把复核放到提交后会出现「库已 applied、工具仍旧版」。
- **Alternatives considered**: 持 `entry.mu` 调 `Replace`——锁序倒置与并发超时；准入期惰性补建——范围外且掩盖缺项；未 applied 连接隐式 apply——越权写入。

## R9 概览：单条 SQL、只读三态、按行阈值

- **Decision**: `ReadWorkerOverview(ctx, limit)` 用**一条 SQL 语句**（四段 `MATERIALIZED` CTE：bounds → scored → counted → page + 外层 LEFT JOIN）返回全库三类计数、活跃租约数、`as_of` 与有界明细（上限 200，常量）；状态按行 `max(3 × heartbeat_interval_seconds, 30s)` 以 `now()` 判定 `active` / `stale` / `stopped`；旧行 NULL 用控制面 `worker.heartbeat_interval_seconds`（默认 10s）兜底并在详情标注；同名不同 `instance_id` 不合并；读取失败整组不可用，不补 0；状态只作显示。
- **Rationale**: 平铺全量与「N 个 Worker」文案让 37 行登记被误读为 37 台机器；计数由明细条数推算在截断时失真；两条查询会在中途插入新行时计数与明细不一致；浏览器时钟与 `clock_timestamp()` 会让状态漂移。
- **Alternatives considered**: 应用层分组——多一次往返且快照不一致；按 `worker_name` 合并——隐藏实例与接管事实；可配置上限 / 配置键——无必要，常量足够。

## R10 角色门：control / worker 能力分表，Worker 只跳过共享初始化写

- **Decision**: `newRuntime(ctx, role)` 由子命令决定（`serve` = control，`gateway` = worker；不新增 `role` 配置键）；Worker 跳过基线回填、管理操作回填、定义协调、启动绑定重建、MCP applied 写（改 `PrepareAtApplied`）、密钥 `Ensure`、策略种子写、开箱安装、本地资产补建、调度定义列写（`DefinitionAuthority=false` 时 `SetRegistered` / `ReconcileAgent` 失败关闭）、MCP 来源补记、学习回填、知识代际 `Ensure`；保留只读知识装配与合法协调写（`next_fire_at`、`last_run_at` / `last_status`、执行行、租约 / 心跳、Run / 审计）。
- **Rationale**: 旧种子 Worker 启动会把共享定义列与绑定投影写回旧值；整段跳过知识面会让知识绑定 Agent 在 Worker 上检索 / 记忆失效；禁掉协调写会让领取与租约停摆。
- **Alternatives considered**: Worker 读旧种子但不连库——无法参与调度；用配置开关显式声明角色——两份事实源易漂移，子命令已是事实。
