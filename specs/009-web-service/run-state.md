# US-9 Web Service — 运行状态（run-state）

> 1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入；
> 2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）；
> 3. 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线

| 项 | 值 |
| --- | --- |
| 起始 commit SHA | `1dbeebc41fa12d152e079ba0ceb88ee602985623`（`main`，2026-10-18 18:11:47 +0800） |
| 分支 | `009-web-service`（2026-10-18 自 `main` 创建；仓库未登记 `before_specify` hook，按 module-dev 步骤 1.3 手动建） |
| worktree 路径 | `D:\code\basalt`（主工作区；本 US 无并行 worktree） |
| 开跑时未提交改动 | `M docs/contracts/openapi.yaml`（D-01 已批准契约增补，sha256 `4be307f7f9318712f9629039e74524c3fac68ec603b14fc1c494f0dc2bcb1257`）——**保留，不 stash / 不 checkout 覆盖** |
| 模块卡人工 review ① | **2026-10-18 通过**（用户明确批准，所审路径 `docs/us/us9-web-service.md`，所审版本 SHA-256 `7af94d2f66b315fe37dfce89921ab1680be8783ac25d97a4a106de048076c47b`） |
| 模块卡当前 SHA-256 | `5195dbee0f64ceab9178c4d5158d902a7d4550002a8a7f28dc94bc1c38a4511f`（与所审版本差异仅为人审通过后的状态位修订：通过标记 / D-01 状态 / 自检尾句；设计内容未改） |
| 三件输入 sha256 | 颗粒度文档：`5195dbee0f64ceab9178c4d5158d902a7d4550002a8a7f28dc94bc1c38a4511f`；`spec.md`：`5261c0e245b910dbbe87d6366eee1e3c501632a181a24914d1b54f1d0a3d8db3`（步骤 2 派生 + 步骤 3 clarify 收敛后，2026-10-18）；`plan.md`：`df2765024154251f8c9d59e692518cc22b4e03baecaf42b116e7f80f4986def5`（步骤 4 派生后，2026-10-18） |
| 步骤 4 产物 sha256 | `research.md`：`33d53d30ddc487e3046ba9c993d86670eceda2f7bdd084e5ee39610cd3397702`；`data-model.md`：`561084c34b076b91e98ad0445b17958f95e9ee268a36d7c5751078c54d9e8f47`；`quickstart.md`：`bc4a5d730a0135818cd79108bc19fd70520311185ed50f948c23866a06e0d051`（四份均 markdownlint 0 诊断） |
| tasks 正文指纹 | `d9a87e9b0aae821cdef53390de83a0a917bac204a3ca244569001436d6db4278`（2026-10-18 步骤 5 生成后冻结；生成时全未勾选，无勾选位归一差异） |
| tasks 正文指纹（analyze 修订后） | `c3bba378f7265117c64f21b09e9aa52ed2802fef75ba79b446dc8283876f6c2a`（2026-10-18 步骤 6 analyze findings 处置后重算：T010 端口数措辞 / T015 归档态 400 分支 / T037 §6.1 真实进程项；全未勾选，无勾选位归一差异） |
| 步骤 5 AI 比对 | **齐**：32 文件全覆盖（新增生产 9 / 修改生产 12 / 新增测试 7 / 修改测试 3 / `basalt.yaml.example`）；测试条数 15+61+20+11+5+11+5+5=133 与 §3.2 逐行相加一致；九条专属 check 均可追溯到 §4.4（T036 派生 stage）；R1~R20 落点见下 |
| R1~R20 → 任务 | R1 信封形态 T012；R2 错误类型映射 T012/T021；R3 409 冻结文案 T021；R4 同步屏障 T021；R5 跨源零副作用 T026；R6 32 KiB 字节判定 T016；R7 请求体上限/尾随内容 T016；R8 空串消息与缺失/null 区分 T015/T016；R9 超时 504 T022；R10 状态映射 T022；R11 脱敏六路径 T022；R12 预算传导 T022/T032；R13 固定向量与恶意 channel T003；R14 归档幂等 T018/T005；R15 历史窗口 T016；R16 停机事件序 T026；R17 元数据计数与无凭证 T025/T032；R18 工具来源分类 T032；R19 真实连接写预算 T026；R20 资源释放顺序 T033 |
| R1~R20 → 任务（按模块卡 §4.3 编号订正，2026-10-18） | R1 统一信封 T008/T012；R2 未知路径 / 方法 404 T010/T012；R3 32 KiB T015/T016；R4 409 冻结文案与 429 T008/T010/T021；R5 运行结果映射 T015/T022；R6 无状态 invoke T019/T020；R7 会话标识向量 T003/T004；R8 历史窗口 T005/T013/T014；R9 归档幂等 T006/T017/T018；R10 监听与写预算 T010/T026；R11 panic→500 T011/T026；R12 进程模式 T027/T028/T031/T032/T033；R13 并发上限接线 T029/T030/T032；R14 归档与运行互斥 T021/T023；R15 对外原因脱敏 T008/T022；R16 五阶段停机 T010/T026；R17 资源释放顺序 T033；R18 跨源写保护 T011/T026；R19 真实连接写预算 T026；R20 /tools 来源分类 T024/T025/T032 |
| R21~R24 → 任务（R5 修复轮，2026-10-18） | R21 监听异常退出统一收尾 T039/T040；R22 收尾等待超时不视为已退出 T039/T040；R23 panic 与完成日志共享标识 T041；R24 failed 内部诊断脱敏 T042。四个测试函数同时追加进 `009-web-handlers` / `009-web-server` / `009-web-race` 的 `required_tests` |
| 步骤 6 analyze（实施前） | **CRITICAL 0** / MEDIUM 2（COV-1、INC-1）/ LOW 6（INC-2/3/4、AMB-1、UND-1、COV-2）；2026-10-18 |
| analyze findings 处置 | COV-1 → T037 增补 §6.1 真实进程项；INC-1 → 上方按卡片 §4.3 编号的映射行（旧行保留）；INC-2 → T010「四个 → 五个」；INC-3 → quickstart §5 ① 命令补 `grep -v 'internal/web$'` 并改期望；UND-1 → T015 增补归档态 400 分支；INC-4 → 口径记录：plan 列 §3.5 #6/#10/#16，tasks / 卡片列 #6/#10，无实质冲突，#16 明标等价重构且 T033 已点名，不改冻结卡片；AMB-1 → storage 尾读用例按 1 例计（3+3+4+1=11）；COV-2 → SC-002 / SC-006 时延为结构保证（准入不排队 + `waitInflight` 有界），无墙钟断言，不改判据面 |
| 步骤 6 analyze 复跑 | 修订后逐项复核：COV-1 / INC-2 / INC-3 / UND-1 已修复；INC-1 / INC-4 / AMB-1 / COV-2 已记录口径；**CRITICAL 0，无阻断项**（2026-10-18） |
| 步骤 6.1 stage 派生 | `harness/stages/us9-web.json`（id `us9-web-r1`，2026-10-18）：九个 check 逐字取自模块卡 §4.4；`inputs` = 模块卡 + spec / plan / tasks；`protect` = 五个既有测试文件（`internal/cli/chat_test.go`、`internal/storage/session_store_test.go`、`internal/app/run_test.go`、`internal/cli/config_test.go`、`internal/memory/memory_test.go`）；`tasks` 只绑定「该任务完成后对应 check 转为全绿」的十个任务：T003/T004→009-core-sessionid，T005/T006→009-storage-session-api，T012→009-web-contract，T025→009-web-handlers，T026→009-web-server + 009-web-race，T028→009-cli-serve，T029→009-cli-config，T033→009-app-web；其余任务用包级 `go test` 验证、最终全量验收兜底。装载验证：`go run ./scripts/acceptance --stage harness/stages/us9-web.json --task __load_check__` 返回 `unknown stage task`（stage 与 inputs/protect 均解析通过） |
| 比对结论 | 无多、无缺、无顺序错误（测试与实现成对、关键回归先红后绿）；判据面变更仅 §3.2 三处追加与 §3.5 第 6 / 10 条等价重构，既有断言零删除、零跳过、零放宽 |
| T001 开工复核（2026-10-18） | `go version` = `go1.27.2 windows/amd64`（≥ toolchain 1.27.2）；`git branch --show-current` = `009-web-service`；`GOPROXY=https://goproxy.cn` 由每条命令显式携带；`docs/contracts/openapi.yaml` sha256 = `4be307f7f9318712f9629039e74524c3fac68ec603b14fc1c494f0dc2bcb1257`（与 D-01 已批准增补一致，**保留、不覆盖**）；工作区另有 `?? harness/stages/us9-web.json`（步骤 6.1 派生）与 `?? specs/009-web-service/`（过程工件），均属本 US 允许范围 |
| 判据面基线 | 既有测试零改动；本 US 允许的判据面新增见颗粒度文档 §3.2「既有测试文件的三处追加」与 §3.5 #12~#14；其余判据面变更须用户裁决 |
| 收口复核退回修复轮基线（R5，2026-10-18） | 用户核对收口报告后以四项故障探针复现遗漏（2 HIGH / 2 MEDIUM），授权在 US-9 范围内修复、补具名回归并复跑完整 stage 后再提交 review ②；R13 夹具改为声明 `read_file` 的处置被用户接受（并发判据保留，实际未执行工具）。修复轮输入 sha256：颗粒度文档 `d8f10a11fcefa39a8ec9a55d141c2f53c6be43ea73017d456e372d48c266e00d`、`spec.md` `b98d8a6e40712d1d5c89aba32b758101c670cc5296c8b6f142006490941396c8`、`plan.md` `3d199af23f76a50a24fec4ddd9f6851dbcda5c5199e60d7525ad99c40fd018d8`、`tasks.md` 勾选位归一后 `bc289042e137db8e57fea5c7d066e9a53440e9c4681c96bf50fc9d2be407fbf1`（T039~T044 追加并验收勾选后；归一方法 = 将行首 `- [x]` / `- [X]` 统一替换为 `- [ ]` 再哈希；修订自检两条命令在追加后与勾选后各跑一次，均空）。判据面变更 = **仅新增**四个测试函数与 `required_tests` 追加（用户退回意见明确授权「补模块卡边界和测试」），既有断言零删除、零跳过、零放宽 |
| 独立复核修复轮基线（R6，2026-10-18） | 独立复核（收口三件套第 2 件）结论「带条件通过」：2 MEDIUM（R5R-01 Authorization 投影绕过、R5R-02 R21 判别力）/ 4 LOW（R5R-03 卡片漂移、R5R-04 投影边角、R5R-05 已写头分支、R5R-06 断开诊断）；用户既有授权「修复后复跑完整 stage 再提交 review ②」覆盖本轮处置。修复轮输入 sha256：颗粒度文档 `a2ed7c560466dda36dbdeeefe6b545827596839078c961cc297547becdac9258`、`spec.md` `d72cef05e2e07801038d8bc70ce4ea9153053749e8b6809473d321c290daf885`、`plan.md` `39b09dd49152d0ed88481b4c981db322c2fcd6a2117e1182e22552011bc01aa7`、`tasks.md` 勾选位归一后 `57fd838a197ba2ea39333eabee6e9ab7256617e5f38676ce280fc22d397f7c34`（T045~T049 追加并验收勾选后；归一方法同 R5 行）。判据面变更 = 既有测试函数内**新增夹具与断言**（R15 +2 例、R24 +6 例、R23 +1 子用例），不新增函数名，`required_tests` 不变；既有断言零删除、零跳过、零放宽 |
| 人工 review ② 二次退回小修轮基线（R7，2026-10-18 触发，2026-10-19 复跑） | 用户确认上一轮四项修复有效、源码指纹与 R6 报告一致、19 项全绿，但临时探针复现 1 项 MEDIUM：带引号且含空格的凭证值尾部泄漏到 WARN 日志（`password="prefix SECRET-TAIL-P41"` / `token='prefix SECRET-TAIL-T42'`）；用户授权小修：凭证值整体识别带引号内容（支持空格与反斜杠转义）、无引号回落非空白匹配、未闭合引号过度遮蔽，补双引号 / 单引号 / 转义 / 未闭合四个组合回归（经完整 handler），复跑完整 stage 并更新最终指纹与报告。修复轮输入 sha256：颗粒度文档 `fc7b690e90f42bf0cd05d3ca0651071aadf7b0d2b45b4a8f4f8c281a05525160`、`spec.md` `3d2b1e2779f5aeeffcfb862de07f1280e930626483ebe376328e8e5600b4dde6`、`plan.md` `cbe4d3b668d0e530ec7db8a6413dfb756ea65949a2aee5478f27d93a2f6ddadf`、`tasks.md` 勾选位归一后 `d78df60f70cb961f39a92e0f85437afed2bfab4072014c3c6d2c514d72ef6a97`（T050~T052 追加后；归一方法同 R5 行）。判据面变更 = 既有测试函数 R24 内新增 4 个子用例，不新增函数名，`required_tests` 不变；既有断言零删除、零跳过、零放宽 |

### 本 US 允许触碰的文件范围

来源：颗粒度文档 §3.1 / §3.2 / §3.5。

#### 新增生产代码（9）

- `internal/web/envelope.go`
- `internal/web/server.go`
- `internal/web/middleware.go`
- `internal/web/views.go`
- `internal/web/handlers_meta.go`
- `internal/web/handlers_sessions.go`
- `internal/app/web.go`
- `internal/cli/serve.go`
- `internal/core/sessionid.go`

#### 修改生产代码（12）

- `internal/app/run.go`
- `internal/app/wire.go`
- `internal/cli/root.go`
- `internal/cli/ports.go`
- `internal/cli/deps.go`
- `internal/cli/chat.go`
- `internal/cli/config.go`
- `internal/storage/session_store.go`
- `internal/core/store.go`
- `internal/core/provider.go`
- `internal/memory/memory.go`
- `internal/web/doc.go`

#### 新增测试（7）

- `internal/web/contract_test.go`
- `internal/web/handlers_test.go`
- `internal/web/server_test.go`
- `internal/app/web_test.go`
- `internal/core/sessionid_test.go`
- `internal/storage/session_api_test.go`
- `internal/cli/serve_test.go`

#### 修改测试（3，判据面新增，既有断言不改）

- `internal/app/run_test.go`
- `internal/cli/config_test.go`
- `internal/memory/memory_test.go`

#### 配置与文档

- `basalt.yaml.example`（新增 `server:` 段）
- `docs/TechnicalDesign.md` §6.2.1（§3.5 #15 文档同步）
- `docs/contracts/openapi.yaml`（§3.5 #17，D-01 **已批准**；已在基线改动中）
- `harness/stages/us9-web.json`（本 US 专属 stage，步骤 6 派生）
- `specs/009-web-service/**`（spec / plan / tasks / run-state 等过程工件）

**范围纪律**：`go.mod` / `go.sum` 零 diff、`schema.sql` 零 diff、零迁移；除上列文件外**不得**触碰其他生产 / 测试文件。

### 1.1 实施期进度（2026-10-18 起，只追加）

| 任务 | 读数 |
| --- | --- |
| T027 / T028 | `internal/cli/serve.go` + `serve_test.go` + `ports.go` Server 端口 + `deps.go` Server 字段 + `root.go` 注册两条命令；gofumpt 干净；`go test -run 'TestServeCommand_\|TestGatewayCommand_\|TestRootCommand_RegistersServeAndGateway' ./internal/cli` ok；stage `sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json --task T028` → `passed fmt` / `passed 009-cli-serve`，exit 0，report `accept-20261018T132937.982782300` |
| T029 | `ServerConfig.MaxConcurrentRuns` + `Config.Server` 落位；负数拒载点名键；gofumpt 干净；`go test -run '^TestConfig_RejectsNegativeMaxConcurrentRuns$' ./internal/cli` ok；stage `--task T029` → `passed fmt` / `passed 009-cli-config`，exit 0，report `accept-20261018T133038.868827700` |
| T030 / T031 / T032 / T033 | `wire.go` 运行期保留 `cfg` / `entries` / `mem` / `mcpConns` 并接线 `MaxConcurrent`；`web.go` 投影与三个适配器；`web_test.go` 六条（11 例）；`run.go` 重路径扩员 + serve 兜底初始化 + `Deps.Server` 注入 + `runWithRuntime` 抽取（先 exec 后 Close）；`run_test.go` +3；gofumpt 干净；`go vet ./internal/app/... ./internal/cli/...` 干净；`go test -run '^(TestNewWebServer_\|TestWebProviders_\|TestNewToolCatalogPort_\|TestEnsureWorkspaceForServe\|TestNeedsServer\|TestNewRuntime_MaxConcurrentRuns\|TestRun_HeavyCommandSplit\|TestRunWithRuntime_)' ./internal/app` ok（1.653s）；stage `--task T033` → `passed fmt` / `passed 009-app-web`，exit 0，report `accept-20261018T134158.866175700` |
| 夹具差异记录（T032 派生） | 卡片 R13 原文 `f.writeAgent(t, "ops-agent")` 触发既有夹具零工具分支的前言拼接缺陷（`wire_test.go` 的 `"tools: []"` 后缺换行，与收尾围栏粘连为 `tools: []---` → 整只未注册，运行在 Chat 前返回）。`wire_test.go` 不在本 US 允许触碰清单内，用例改为 `f.writeAgent(t, "ops-agent", "read_file")`；卡片断言逻辑逐字保留、运行路径不变（首个响应无工具调用）；诊断探针文件用毕即删 |
| T034 | `basalt.yaml.example` 在 `http:` 之后新增 `server:` 段（`max_concurrent_runs: 32`，注释注明缺省 / 0 = 32 与 429）；键名与 `docs/contracts` / `cli.ServerConfig` 一致 |
| T035 | 核对 `docs/TechnicalDesign.md` §6.2.1：`WriteTimeout: 130 * time.Second` 及预算依据、`http.CrossOriginProtection`、宽限耗尽后的取消 / 关闭 / 有界收尾 / 释放共享资源三处均已就位（无需改动）；`docs/contracts/openapi.yaml` 的 D-01 增补在册（POST /sessions `400`、DELETE /sessions/{id} `409`），`handlers_sessions.go:214-220` 的归档拒绝映射 409 / `errSessionBusy`，与契约一致；未新增契约变更 |
| T036 | `harness/stages/us9-web.json` 已在步骤 6.1 派生并装载验证（§1 基线）；本轮 `--task T028 / T029 / T033` 三次实跑均按该 stage 解析执行，九个 check 与 task 映射生效 |
| T037 / T038 完整验收与真实进程（2026-10-18） | 全量 stage `accept-20261018T134633.615168300` 19 项全 PASS、exit=0；`make check` 七目标逐项 passed、exit=0（fmt / lint / sec / test / test-integration / cover / build），`make cover` 全包达标（`internal/web` 87.41%、`internal/core` 86.06%、`internal/storage` 79.71%）。真实进程（仓库外临时工作区 `%TEMP%\tmp.BqwVYoMY8D`，用毕已删；二进制 `CGO_ENABLED=0` 临时构建）：`basalt serve` 子进程 PID 25856 → 重启 PID 3244，`netstat -ano` 读数 `127.0.0.1:8080 LISTENING`（非 0.0.0.0，默认监听成立）；首启 Init `created=[skills memory logs archive sessions AGENTS.md SOUL.md USER.md mcp_servers.yaml memory/MEMORY.md] kept=[agents]`，二启 `created=[]` 全 kept（幂等、逐字节不动）；`POST /sessions` 两次同 `session_id=web-22c66f19af7a4d7d`、同 `created_at`；DELETE 响应无 `data` 字段、归档后 `status=archived`、重复 DELETE 200、归档后发消息 400「会话已归档，不可追加消息」；强制终止并重启后 GET 仍 `status=archived`；`POST /agents/demo/invoke` 返回 200、`session_id=""`、`status=failed`、`reason="运行失败"`（探针 provider 不可达，脱敏文案生效）且会话 `message_count` 保持 0；跨源 POST 403「跨源请求被拒绝」、跨源 GET 200 且无 `Access-Control-*` 头；SIGTERM：MSYS `kill -TERM` 后进程消失但无任何停机日志（未触发 Go 信号处理器）→ 真实进程优雅停机在 Windows 记未验证（§4 1b）。§8 清理：临时工作区 / 临时二进制 / `bin/basalt`（`make build` 产物）已删，8080 无监听、无 basalt 进程残留 |
| 收口全量复跑（2026-10-18） | 最终树（tasks 38/38 全勾、quickstart / run-state 定稿后）复跑 `sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json`：公共 10 项 + 本 US 九 check 共 **19 项全 PASS、exit=0**，report `accept-20261018T135716.818593600`；与 T037 首跑 `accept-20261018T134633.615168300` 结果一致（两次均无 `source_changed`） |
| T038 §5 九条 grep（2026-10-18） | ① 依赖方向：`go list -deps ./internal/web` 仅 `internal/core` 一项；② 单一写入路径零命中；③ `context.WithValue` 仅 `internal/web/middleware.go:71` 的 `requestIDKey{}`——追踪标识，宪法 VII 允许（quickstart ③ 已按同日实测改为「仅追踪标识并逐处确认」）；④ `go.mod` / `go.sum` 零 diff、框架零命中；⑤ `schema.sql` 零 diff、迁移目录 0 文件；⑥ 凭证标签零命中；⑦ 抑制注释零命中；⑧ 时区禁用词零命中；⑨ `heavyCommands` 含 `serve` / `gateway`。§8 清理见 T037 收尾记录 |
| T039 / T040（R5 缺陷 1 / 2，2026-10-18） | `server.go` 新增 `stopServing`（draining 置位 → `cancelBase` → `srv.Close()` → `waitInflight(5s)`，超时记 ERROR 后继续 `inflight.Wait()`）、`lifecycleMu` + `draining` 与在途登记互斥、`drainTimeoutOverride` 接缝；信号路径与 `Serve` 错误路径共用收尾。`TestServer_ServeErrorUnifiedCleanup` / `TestServer_ShutdownDrainTimeoutWaitsForHandler` 定向 PASS；`go test ./internal/web -count=1` PASS；`go test ./internal/web -count=1 -race` PASS。红态来自用户探针（§2 台账），本轮未另造本方红态 |
| T041（R5 缺陷 3，2026-10-18） | 请求标识改在链首 `recoverMiddleware` 生成并经 context 注入；完成日志移入 `defer`（panic 未写头记 500、已写头记真实状态、不 recover）。`TestServer_PanicLogSharesRequestID` 定向 PASS（完整 `handler()` 链；panic 与完成日志共享非空 `request_id`），包级与 race 复跑同上 |
| T042（R5 缺陷 4，2026-10-18） | `envelope.go` 新增 `sanitizeRunReason`（凭证 / Bearer / 引号字面量 / URL / 绝对路径 / SQL 正文 → 占位，截断 256 字符）；`writeRunResult` 对 failed 记 WARN（`run_id` / `request_id` / `session_id` / `agent` / `diagnostic`），原始原因不落日志。`TestMessages_FailedRunDiagnosticLogged` 定向 PASS，既有 `TestMessages_FailureReasonRedaction` 仍 PASS，包级与 race 复跑同上 |
| T043（R5 文档与 stage 同步，2026-10-18） | 模块卡升 R5：约束 20 / 21 / 23、§6.3 #21 / #22 / #24、§3.2 计数（132 + 5 = 137，race 并集 32）、§4.2~§4.4 映射与 R21~R24；spec FR-025 / FR-028 / FR-029 / SC-006 与边界情形、plan 技术上下文、research D9、quickstart 计数同步；`harness/stages/us9-web.json` 的 `required_tests` 已扩、approval 已刷新。`tasks.md` 追加 T039~T044（快照 `tasks.md.pre-revision`），两条修订自检命令：① 无白名单外删除行（grep 空、exit=1）、② 被删未勾选正文缺失集为空（exit=0） |
| T044（R5 全量复跑，2026-10-18） | 修复后首跑 `accept-20261018T142525.836769200`（指纹 `1d46ed1e…`）19 项全 PASS；随后 tasks 勾选与 stage approval 定稿，**最终树**复跑 `sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json` → **19 项全 PASS、exit=0**，report `accept-20261018T143209.849873900`，源码指纹 `e52c59fa3e2833424bac36647e4123e27a8adbd3490b760ec7bf9d1767b65b92`（收口报告以此为准）。四个新增具名测试在对应 check 日志中均出现（handlers 1 / server 3 / race 并集 4）。`make check` 七目标逐项 passed、exit=0；`make cover` 全部包达标（`internal/web` 88.13%）；`make build` / stage build 产物 `bin/basalt` 用毕已删。真实模型与真实进程信号项保持 §4 原状态 |
| T045（R6 R5R-01 / R5R-04，2026-10-18） | `envelope.go` 新增 `runReasonAuth`（Authorization 连 scheme 与凭证整体投影，先于通用键值规则）；SQL 跨行到分号或串尾；路径允许空格并覆盖盘符 / UNC；引号支持反斜杠转义。R15 扩 Bearer / Basic 两例、R24 扩同两例与多行 SQL / 含空格路径 / UNC / 转义引号四例（均在既有函数内，不新增函数名）。仓库外 `go test -overlay` 五处逆改各自命中逐字断言；`go test ./internal/web` 与 `-race` PASS；stage `--task T045` → `passed fmt` / `009-web-handlers` / `009-web-race`，report `accept-20261018T145522.246467100` |
| T046（R6 R5R-02 / R5R-05，2026-10-18） | R21 改为 handler 收取消后继续阻塞（`drainTimeoutOverride=50ms`，300ms 不返回窗口，`srv.Close` 以客户端错误判别）；R23 增「panic 已写头」子用例（完整链、逐字 200 与单份信封、完成行 `status=200`、共享 `request_id`）。逆改「删 `Close`」「超时即返回」均使 R21 红；包级与 `-race` PASS；stage `--task T046` → `passed fmt` / `009-web-server` / `009-web-race`，report `accept-20261018T145534.917174300` |
| T047（R6 R5R-06，2026-10-18） | 两条客户端断开分支先 `logFailedRun` 再 `logClientGone`；R24 增「客户端断开仍留 failed 诊断」子用例（诊断存在、`run_id` 在场、敏感标记不落日志、断开日志仍在）。逆改（断开分支不调 `logFailedRun`）→ 找不到 `diagnostic=` 行，断言红；stage `--task T047` → `passed fmt` / `009-web-handlers` / `009-web-race`，report `accept-20261018T145547.250284800` |
| T048（R6 文档与 stage 同步，2026-10-18） | 模块卡升 R6：新增「独立复核发现处置（R6）」、§2.3 伪代码、§3.1 声明与函数清单、§3.2 计数（140 + 5 = 145；R15 8 例 / R24 7 例）、§4.3 R15 / R16 / R21 / R23 描述；spec FR-029 扩脱敏面；plan / quickstart 计数同步；`harness/stages/us9-web.json` approval 刷新为 R6 输入指纹（`required_tests` 不新增函数名），新增 T045~T047 任务绑定。`tasks.md` 追加 T045~T049（快照 `tasks.md.pre-revision`），两条修订自检在追加后与勾选后各跑一次：① 空白名单外删除行（grep 空、exit=1）、② 被删未勾选正文缺失集为空（exit=0） |
| T049（R6 全量复跑，2026-10-18） | **最终树**（T045~T049 全勾、approval 与约束 23 定稿后）复跑 `sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json` → 公共 10 项 + 本 US 九 check 共 **19 项全 PASS、exit=0**，report `accept-20261018T150755.950665800`，源码指纹 `e7e5406a6bcc307757c87dd05e462aaae34e87df5023f8855e1b447591de2b6c`（收口报告以此为准）。`make check` 七目标逐项 passed、exit=0；`make cover` 全部包达标（`internal/web` 88.04%）。stage / make 产物 `bin/basalt` 用毕已删。真实模型与真实进程信号项保持 §4 原状态 |
| T050（R7-01，2026-10-18/19） | `envelope.go` 的 `runReasonSecret` 凭证值改为「双引号整体识别（支持 `\\.` 转义）→ 单引号整体识别 → 无引号 `\S+` 回落 → 未闭合引号按「吞到串尾」遮蔽」；R24 增双引号 / 单引号 / 转义 / 未闭合四个组合子用例。红态：修复前四例逐字失败（`diagnostic="failed password=<redacted> SECRET-TAIL-P41\""` 等）；修复后四例全 PASS。`go test ./internal/web` 与 `-race` 均 PASS；`gofumpt -w` 仅作用于本轮两个文件 |
| T051（R7 文档与 stage 同步，2026-10-18/19） | 模块卡升 R7：新增「人工 review ② 退回与修复（R7）」、约束 23 脱敏面、§3.2 计数（144 + 5 = 149；R24 11 例）、§4.3 行 16 与 §4.4 自检；spec FR-029 扩「带引号凭证值整体识别 / 未闭合遮蔽到串尾」；plan / quickstart 计数同步，并修正 quickstart §2 三处陈旧具名数（handlers 15→16、server 8→11、race 28→32）。`harness/stages/us9-web.json` approval 刷新为 R7 输入指纹，新增 T050 任务绑定；approval 中反斜杠导致 JSON 非法一次，改为纯文本后装载验证通过。`tasks.md` 追加 T050~T052（快照 `tasks.md.pre-revision`），两条修订自检在追加后与勾选后各跑一次：① 空白名单外删除行（grep 空、exit=1）、② 被删未勾选正文缺失集为空（exit=0） |
| T052（R7 全量复跑，2026-10-18/19） | **最终树**（T050~T052 全勾、approval 定稿后）复跑 `sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json` → 公共 10 项 + 本 US 九 check 共 **19 项全 PASS、exit=0**，report `accept-20261018T160540.441629000`，源码指纹 `538e3ba427bf46880dc02f5f4e4e9f1d3936b5331b1b2b33f0734be64ce395cf`（收口报告以此为准）。四个新子用例在 `009-web-handlers` 日志中逐个 PASS（R24 共 11 例）；`SECRET-TAIL` 在日志零命中。`make check` 七目标逐项 passed、exit=0；`make cover` 全部包达标（`internal/web` 88.04%）。stage / make 产物 `bin/basalt` 用毕已删。真实模型与真实进程信号项保持 §4 原状态 |

## 2. 失败台账

| 日期 | 问题（测试名或首错位置 + 症状短语） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-18 | `TestNewRuntime_MaxConcurrentRuns` 卡在 `<-blocker.started`（90s 超时；首个运行在 Chat 前返回） | `ops-agent` 整只未注册——夹具零工具分支把 `tools: []` 与收尾围栏拼成 `tools: []---` | 用例改声明内置工具 `read_file`（断言未改）；修复后绿，见 §1.1 | 1 |
| 2026-10-18 | **用户探针**：注入永久 `Accept` 错误后 `Server.Run` 已返回，正在执行的 Run 上下文仍未取消（`server.go` 的 Serve 错误分支直接返回） | 收尾逻辑只接在信号路径上 | 两条退出路径统一走 `stopServing`；R21 绿（用户探针红态，见 §1.1 T039/T040） | 1 |
| 2026-10-18 | **用户探针**：运行收到取消后延迟清理，5s 收尾等待超时后 `Server.Run` 仍返回、handler 仍存活 | 等待超时被当作「已退出」 | 超时只记 ERROR 并继续 `inflight.Wait()`；draining 与在途登记互斥；R22 绿（用户探针红态） | 1 |
| 2026-10-18 | **用户探针**：panic 日志 `request_id=""`、请求完成日志缺失（内部请求标识非空） | 标识在内层 `r.WithContext` 注入、外层 recover 读原请求看不到；完成日志写在 `next.ServeHTTP` 之后 | 标识改在链首生成；完成日志移入 `defer`；R23 绿（用户探针红态） | 1 |
| 2026-10-18 | **用户探针**：failed 运行只得到「运行失败」响应与一条 HTTP 200 请求日志，具体原因消失 | `core` 快照读取失败发生在 Run 账本创建之前，原因不入库 | failed 记脱敏 WARN（run / request / session / agent）；R24 绿（用户探针红态） | 1 |
| 2026-10-18 | **独立复核 R5R-01**：`Authorization: Bearer SUPERSECRET-TOKEN-abc123` 投影为 `Authorization=<redacted> SUPERSECRET-TOKEN-abc123`；Basic 同样泄漏 | secret 正则含 `authorization` 且先于 bearer 执行，scheme 被 `\S+` 吃掉 | 新增 `runReasonAuth` 专用规则整体投影；R15 / R24 各增 Bearer / Basic 夹具，逆改后红、修复后绿 | 1 |
| 2026-10-18 | **独立复核 R5R-02**：删 `stopServing` 的最终 `inflight.Wait()` → R21 连续 30 次全绿；再删 `srv.Close()` → 四用例全绿 | R21 只断言 `run-cancelled` 与事后 `waitInflight`，未覆盖阻塞 handler 与活动连接关闭 | R21 改为阻塞 handler + `drainTimeoutOverride` + `srv.Close` 客户端错误判别；逆改均红、修复后绿 | 1 |
| 2026-10-18 | **独立复核 R5R-03**：卡片 §2.3 / §3.1 仍写修复前伪代码与缺项；§3.2 行 9 监听器描述与实际不符 | R5 代码已改、文档未同步 | §2.3 / §3.1 / §3.2 行 9 同步到 R5/R6 实现（静态核对，无测试红态） | 1 |
| 2026-10-18 | **独立复核 R5R-04**：多行 SQL 只遮蔽首行；含空格路径被截成多段；UNC 原样保留；转义引号留下后半截 | 正则按单行 / 无空格 / 无转义编写 | SQL 跨行、路径含空格与 UNC、引号转义；R24 增四例具名夹具，逆改后红 | 1 |
| 2026-10-18 | **独立复核 R5R-05**：「panic 前已写头 → 完成日志记真实状态」无具名用例；逆改为固定 500 不会被发现 | R23 只覆盖 panic 未写头分支 | R23 增「panic 已写头」子用例（完整链、逐字断言、共享标识）；逆改后红 | 1 |
| 2026-10-18 | **独立复核 R5R-06**：客户端断开分支在 `writeRunResult` 前返回，`logClientGone` 不带诊断；「断开 + failed」不记 failed WARN | 断开分支未调用 failed 诊断出口 | 两条断开分支先 `logFailedRun` 再 `logClientGone`；R24 增断开子用例，逆改后红 | 1 |
| 2026-10-18 | **用户探针（review ② 二次退回）**：`password="prefix SECRET-TAIL-P41"` 经完整 handler 后诊断行仍含 `SECRET-TAIL-P41`；单引号 `token='prefix SECRET-TAIL-T42'` 同样 | `runReasonSecret` 的 `\S+` 只吃到开头与左引号，后续引号规则无法识别剩余串 | 凭证值改为「引号整体识别（支持空格 / 转义）→ 无引号 `\S+` 回落 → 未闭合引号遮蔽到串尾」；R24 增四个组合子用例，修复前逐字红、修复后绿 | 1 |

## 3. 停止预算与计数

- 同一问题修复 **3 次**不收敛 → 停下走 `/debug`（不得第 4 次盲修）
- `/debug` 内同一问题累计 **3 个**假设不成立 → 停下讨论设计 / 架构
- 每个 US 的 `/debug` 轮次预算 **≤ 2**（超出 → 停下请裁）
- **无进展**（相邻两次尝试无新读数 / 新假设 / 新定位）→ 立即停，不等计数
- 预算到期 = **停下报告请裁**（继续 / 降档 / 换方案）——不得草率收尾、不得自动放弃

当前计数：本会话尚未发生修复尝试；`/debug` 轮次 0。

当前计数（2026-10-18 收口复核修复轮）：四个独立缺陷（R5-01~R5-04）各 1 次修复尝试、均一次收敛；`/debug` 轮次 0。红态来源为用户故障探针，非本方独立红态运行。

当前计数（2026-10-18 独立复核修复轮 R6）：R5R-01 / R5R-02 / R5R-04 / R5R-05 / R5R-06 各 1 次修复尝试、均一次收敛；R5R-03 为文档同步 1 次；`/debug` 轮次 0。红态来源为独立复核探针与本方仓库外 overlay 逆改；逐项计数见 §2。

当前计数（2026-10-18 触发 / 2026-10-19 复跑，人工 review ② 二次退回小修轮 R7）：R7-01 一次修复尝试即收敛；`/debug` 轮次 0。红态来源为本方新增四个子用例在修复前的逐字失败复现（`diagnostic="failed password=<redacted> SECRET-TAIL-P41\""` 等），非 overlay 逆改。

## 4. 未验证清单

| # | 项 | 状态 | 证据 / 说明 |
| --- | --- | --- | --- |
| 1a | 真实进程验收（`basalt serve` 子进程、监听地址、`netstat -ano`、归档后重启保持 `status=archived`、工作区幂等初始化） | 已验证（2026-10-18） | 仓库外临时工作区实测：`netstat -ano` = `127.0.0.1:8080 LISTENING`（PID 25856；重启后 3244）；首启 Init 建齐骨架（`kept=[agents]`），二启 `created=[]` 全 kept；会话 `web-22c66f19af7a4d7d` 归档后终止进程并重启仍 `status=archived`；完整读数见 §1.1「T037 / T038」行 |
| 1b | SIGTERM 优雅停机（真实进程五阶段事件序） | 未验证 | 2026-10-18 Windows 实测：MSYS `kill -TERM` 后进程消失但无任何停机日志——未触发 Go 信号处理器（等价直接终止）；Windows 的 `Process.Signal` 不支持 SIGTERM。五阶段事件序由 `009-web-server` 进程内用例判卷；真实进程面按颗粒度文档 §6.1 记未验证 |
| 2 | Demo 四 / 五（curl 全链路、并发 409、无状态 invoke、监听可见性） | 未验证 | 2026-10-18 真实进程已读到：建会话幂等 / 历史读取 / 归档 / invoke 无状态（`session_id=""`、消息计数不变）/ 跨源拒绝；**仍未验证**：真实模型两轮工具（本机无模型凭证）、并发 409 / 429 的独立进程复现（`009-web-handlers` 已判卷）。待人工执行 |
| 3 | 飞书 / webhook 真实外部投递 | 不适用（本 US 无外部投递面） | US-9 无出站 webhook；HTTP 面以进程内真实回环连接验收（R19） |

## 5. 收口结论

- AI 验收：**通过（2026-10-18）**——最终树全量 stage `accept-20261018T135716.818593600` 公共 10 项 + 本 US 九 check 共 19 项全 PASS、exit=0；`make check` 七目标全绿（含 `make cover` 全包达标）；真实进程项读数见 §1.1「T037 / T038」行；未验证项见 §4（1b SIGTERM、2 人工 Demo）。
- 独立复核：**通过（2026-10-18，独立上下文子代理）**——按冻结规格（模块卡 §3/§4/§6.1 + spec FR/SC + contracts）复核 A~J 十项全部符合，**0 CRITICAL / 0 HIGH / 0 MEDIUM**；6 条 LOW 逐条处置：**O-1** `/profiles` problems 判据比措辞窄（生产路径 `Registered=false` 必带 `RejectReason`，行为符合意图）→ 记录不改；**O-2** DeadlineExceeded 优先于类型错误（约束 22 未定义同时命中的优先级，真实路径超时优先合理）→ 记录、不补未定义判据；**O-3** 显式空串 channel / user_id 按「未提供」回填缺省（FR-016 未区分缺省与空串，无用例）→ 记录；**O-4** 新建会话 `status` 由 handler 置 active、INSERT 依 schema `DEFAULT 'active'` 回读（卡片固定 SQL 本身省略 status，行为正确）→ 记录，不改冻结 SQL；**O-5** 编码失败兜底信封无 timestamp（当前 DTO 全可编码，分支不可达）→ 记录；**O-6** §3.2「例」计数与运行时 `t.Run` 事件数不是同一计数轴（表驱动用例不产生子测试；逐例断言覆盖完整、required_tests 全通过）→ 记录口径，不改冻结卡片与测试。复核未覆盖面（未跑测试/未起真实进程/未按 diff 审 TechnicalDesign 等）由控制侧门禁与真实进程项补足。
- 收口复核（人工 review ② 前置检查）：**退回（2026-10-18）**——用户核对收口报告确认 19 项检查通过、源码指纹一致，但四项故障探针复现 2 HIGH / 2 MEDIUM 遗漏（详见模块卡「收口复核退回与修复（R5）」与 §2 台账）。用户在退回意见中授权本 US 范围内修复、补具名回归并复跑完整 stage 后重新提交。
- 修复轮状态（2026-10-18）：代码与具名回归已落（T039~T044 未勾选，待全量 stage 复跑判定）；模块卡 / spec / plan / research / quickstart / stage 已同步；**未**宣称修复完成，真实模型与真实进程信号项保持 §4 原状态。
- 人工 review ②：**已提交并退回（2026-10-18，四项发现）**；修复后待重新提交（未收到新决定前不得写成人工接受）。
- 四态明细：指向本轮修复后的最终验收报告 `accept-20261018T143209.849873900`（19 项全 PASS，源码指纹 `e52c59fa…`）与 §1.1；本文件只保留未验证项与结论。
- 修复后 AI 验收（2026-10-18）：**通过**——四个具名回归在最终树 stage 中逐条执行；`make check` 七目标全绿；未验证项与修复前一致（§4 1b SIGTERM、2 真实模型 / 人工 Demo），未因本轮修复翻转。
- 独立复核（R6，2026-10-18）：**带条件通过**——0 CRITICAL / 2 MEDIUM（R5R-01 凭证投影绕过、R5R-02 R21 判别力）/ 4 LOW（R5R-03 卡片漂移、R5R-04 投影边角、R5R-05 已写头分支、R5R-06 断开诊断）；R5R-07~R5R-09 通过。三项条件（修复 R5R-01、补强 R21、同步卡片 §2.3 / §3.1）与三项 LOW 均已处置，记录见模块卡「独立复核发现处置（R6）」。
- R6 修复轮状态（2026-10-18）：T045~T049 已完成；最终树全量 stage `accept-20261018T150755.950665800` 19 项全 PASS、exit=0，源码指纹 `e7e5406a6bcc307757c87dd05e462aaae34e87df5023f8855e1b447591de2b6c`；`make check` 七目标全绿；判据面变更仅为既有测试函数内的新增夹具与断言（不新增函数名，`required_tests` 不变）。
- 人工 review ②：**已退回一次**（用户四项故障探针，R5 处置）；R5 修复后按收口三件套执行独立复核，复核带条件通过并进入 R6 退修轮；R6 完成后**待重新提交**（未收到新决定前不得写成人工接受）。
- R7 修复轮状态（2026-10-19）：T050~T052 已完成；最终树全量 stage `accept-20261018T160540.441629000` 公共 10 项 + 本 US 九 check 共 19 项全 PASS、exit=0，源码指纹 `538e3ba427bf46880dc02f5f4e4e9f1d3936b5331b1b2b33f0734be64ce395cf`；四个组合子用例在 `009-web-handlers` 日志中逐个 PASS（R24 共 11 例），`SECRET-TAIL` 日志零命中；`make check` 七目标全绿（`internal/web` 覆盖 88.04%）。判据面变更仅为既有 R24 函数内新增 4 个子用例，不新增函数名，`required_tests` 不变；既有断言零删除、零跳过、零放宽。
- 人工 review ②：**累计退回两次**（2026-10-18 四项探针 → R5/R6 处置；2026-10-18 带引号凭证尾部泄漏 1 MEDIUM → R7 处置）。R7 修复已按退回意见收敛并完成全量复跑，**待重新提交**（未收到新决定前不得写成人工接受）；§4 未验证项（1b SIGTERM、2 真实模型 / 人工 Demo）保持原状态，未因本轮翻转。
- 人工 review ②：**通过（2026-10-19）**——用户复核 R7 修复包后明确「可以提交推送了，没问题就开始 us10 的颗粒度文档」；AI 按该授权执行提交、推送 US 分支并快进合回 `main`。§4 未验证项保持原状态。
