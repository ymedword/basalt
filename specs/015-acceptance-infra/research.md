# Research — US-15 工程验收基础设施

**输入**：模块卡 `docs/us/us15-acceptance-infra.md` §3/§4（人审 ① 版本）、`spec.md`、`harness/acceptance.json`、`scripts/acceptance/*`（现状）、`.github/workflows/ci.yml`、`docs/DevelopmentLoop.md` §5.1 / §9.3。

无 NEEDS CLARIFICATION 项：模块卡与待决事项 D-01~D-12 已冻结全部关键选择；下列决策逐条记录来源、理由与被否决方案。

## R-01 记录载体与发布方式

- **Decision**: 每次运行在运行目录写 `record.json`；收口时把记录与同次 `report.json` 成对复制到 `harness/acceptance/records/` 与 `records/reports/`，台账追加一行；原始日志留在 `.loop/runs/`、不入库。
- **Rationale**: 记录只追加、可跨 clone 审计；报告原件是执行绑定的唯一凭据，不能只靠记录字段自证。
- **Alternatives considered**: 只写记录不存报告——绑定校验无原件；把原始日志入库——体积大且可能含敏感输出；新增 `--publish` 命令——扩大工具面与测试面（D-03 否决）。

## R-02 层级由 check 声明决定，不从日志反推

- **Decision**: `check.level` ∈ `source` / `process` / `external`，缺省 `source`；`process` / `external` 的日志必须含唯一 `binary_sha256=<64 位小写十六进制>`；缺失或出现多个不同哈希即基础设施错误（退出 2，不生成记录）。
- **Rationale**: 若按"日志里有没有哈希"反推层级，删掉标记即可把 process 降级为 source 并放行，证据层级失效（C-14）。
- **Alternatives considered**: 自动推断层级——可被空日志绕过；只要求"至少一个哈希"——多哈希说明构建不一致，必须拒绝。

## R-03 报告路径投影（执行期绝对、序列化时仓库相对）

- **Decision**: 运行期用绝对路径读写日志；写 `report.json` 前把每个 check 的 `log` 投影为仓库相对路径并统一 `/`；`--out` 解析后必须位于仓库根内，否则退出 2、不写报告、不生成记录。
- **Rationale**: 同时满足"报告原样拷贝"与"记录/报告零绝对路径"；越界目录无法投影，按基础设施错误处理，不把 `..` 或盘符写入证据。
- **Alternatives considered**: 执行期就用相对路径——子进程 cwd 不同易错；发布时重写拷贝——破坏"原样"与哈希一致性。

## R-04 stage lint 只读预检

- **Decision**: 从 check argv 去掉执行与计数参数（`-json` / `-count` / `-run` / `-v` 等）、保留 build tag 与包参数，追加或替换为 `-list`；`required_tests` 的 `Parent/sub` 截断为 `Parent`、去重并保持声明顺序；现行 stage 失配使 lint 退出非 0，历史 stage 只 WARN。
- **Rationale**: lint 不执行测试，只回答"测试名是否仍存在"；`go test -list` 只列顶层名，必须截断子测试。
- **Alternatives considered**: 用 `go test -run` 实跑预检——违反"lint ≠ 执行"（C-03）；用 `go list` 解析——拿到包而非测试名。

## R-05 历史失配以显式替代映射登记

- **Decision**: `stage.superseded_checks` 为 `旧 ID → 新 ID`；校验只要求替代目标存在、不自我替代，且替代目标 `required_tests` 精确等于冻结值 `["TestToolListerPort_HeavyAssemblyRegistersTenTools"]`；非替代项仍与历史定义逐字段相等。
- **Rationale**: 旧测试已不存在，逐字段比较必然失配；只校验"目标存在 + 测试名精确相等"能防替代目标被稀释，同时不伪造旧定义。
- **Alternatives considered**: 改写历史 stage——违反 C-01；静默删 check——漂移不可见；只校验目标存在——`required_tests` 被改宽也能通过。

## R-06 回填目录与冻结全集双向相等

- **Decision**: `backfillCatalog` 按 item / check / test / level 记录；`requiredBackfillItems`（`source` + `process`）与 `requiredRealItems`（`external`）是独立冻结全集；两个方向都要求相等；目录项与各 stage 的 `required_tests` 按 check 逐字一致；同时删目录项与其 required_test 仍必须红。
- **Rationale**: 单方向校验可在"目录与 required_tests 一起漏项"时放行（C-11）；冻结全集以模块卡 §2.6 / §4.5 为转写源，独立于目录。
- **Alternatives considered**: 只校验 required_tests——目录漏项不可见；从目录反推全集——同源漏项无法发现。

## R-07 三视图状态与冲突显式

- **Decision**: 状态在读取时计算，不落盘：①历史曾执行（任一 `passed` 回填记录）；②`(item, source_fingerprint, environment.os)` 键内按 `finished_at` 取最新（同刻取 `record_id` 字典序最大）；③本次回归由 current / linux 运行承担。同键或跨键结论相反置 `Conflict = true`，保留两条记录 ID，不取"通过优先"。
- **Rationale**: 历史证据按自身指纹归档，不能用旧版本通过遮蔽新版本失败；用户复跑必须进入视图（C-13）。
- **Alternatives considered**: 全局最新记录——跨平台互相遮蔽；把状态写进记录——记录不可变原则被破坏。

## R-08 进程级回填独立包与 build tag 分层

- **Decision**: 新增 `internal/e2e`；`doc.go` 无 tag；平台中立测试 `e2e`，Linux 信号 `e2e && linux`，慢速 `e2e && slow`，真实外发 `e2e && real`；`TestMain` 构建 `CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=us15-e2e"`，helper 子进程与 `-list` 模式不构建。
- **Rationale**: 真实二进制场景不应拖慢 `make test` / `make test-integration`，也不改变既有判据面；`real` 再隔离一层，CI 与日常 e2e 不会触发真实费用。
- **Alternatives considered**: 复用 `integration` tag——混入真实调用语义；放进现有包——启动构建成本污染组件测试。

## R-09 真实项 external 与授权绑定

- **Decision**: `015-real-model` / `015-real-feishu` 声明 `level = external`，只在 `us15-real.json`；执行时 CLI 必须同时给 `--authorization-ref` / `--authorization-scope`，未选 external 时给旗标同样退出 2；记录与报告授权逐项相等；四个 `BASALT_REAL_*` 环境变量只经执行 shell 注入，缺失即测试失败（不 SKIP）；模型调用 ≤30、飞书发送 ≤5，只允许合成数据。
- **Rationale**: 一次性授权必须可追溯、不可复用；缺环境不能冒充通过；stage 文件不得携带授权值或凭证（C-15 / D-12）。
- **Alternatives considered**: 授权写进 stage——固化为常设批准；缺环境 SKIP——无法区分"没跑"与"通过"；复用 `ANTHROPIC_*`——拿 Agent 自身凭证做被测系统调用。

## R-10 CI 三步与运行目录唯一化

- **Decision**: 在既有 `test` job 的 Unified acceptance 与 Upload acceptance evidence 之间插入 lint-stages、current、us15-linux 三步；所有 `--out`（含既有固定 `ci`）改为 `<name>-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`，控制台日志同后缀；不新增 job、不改 job 名；`us15-real` 不接入 CI。
- **Rationale**: 重试（`run_attempt` 递增）不得复用目录，否则 `accept` 拒绝已存在目录、`record_id` 无法发布；真实外发不能每轮依赖凭证与费用。
- **Alternatives considered**: 只带 `GITHUB_RUN_ID`——同一 run 重试碰撞；新增 job——改变 job 结构，超出已审范围。

## R-11 记录规范载体：Go 结构体 + README 字段表

- **Decision**: 不新增 JSON Schema 文件；`record` 等类型用 Go 结构体表达，字段表写进 `harness/README.md`；校验只用标准库。
- **Rationale**: 避免两份定义漂移，也避免为校验引入第三方依赖（D-11 / FR-024）。
- **Alternatives considered**: JSON Schema + 校验库——新增坐标与维护双份定义；只写文档不写结构体——无法机器判卷。

## R-12 历史 stage 钉住与指纹排除

- **Decision**: `TestStages_HistoricalFilesPinned` 钉住 `us8-notify-r1.json` / `us9-web.json` / `us10-scheduler.json` 三个文件（固定集合，不 glob）的 LF 归一哈希；`sourceFingerprint` 的忽略面增加 `harness/acceptance/records/` 与 `harness/acceptance/ledger.md`。
- **Rationale**: 历史文件零改写需要机器判据；发布记录若计入指纹，证据自身会改变其指纹，自相矛盾（C-01 / D-05）。
- **Alternatives considered**: glob 历史目录——`current.json` 与 `us15-*.json` 会被误判为历史；按 mtime 判定——checkout / clone 会改变时间戳。

## R-13 判据面保护

- **Decision**: `acceptance_test.go` 仅做签名适配 + 两个新增用例；既有断言逐条保留；`harness/acceptance.json`、`Makefile`、`go.mod` 不变；新增测试覆盖模块卡 §4.2 的测试到验收点映射与逆改点表。
- **Rationale**: 本 US 只新增测试与检查，判据面零删除、零跳过、零放宽（模块卡 §3.4 末段）。
- **Alternatives considered**: 把 `015-e2e-vet` 并入 `make lint`——改变七目标含义；在 `make check` 里跑 e2e——拖慢公共门禁并改变超时语义。
