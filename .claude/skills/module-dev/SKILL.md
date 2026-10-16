---
name: module-dev
description: >-
  按「模块执行颗粒度文档」（模块级设计文档）驱动开发一个模块：输入模块编号/US 编号，自动完成
  备料 → speckit-specify → clarify → plan → tasks（停等确认）→ analyze → implement →
  模块级验收报告，全程施加硬/软两类门禁与测试纪律，保证产出严格贴合项目技术方案。跨项目
  通用：项目相关事实全部在文末「项目配置区」。当用户说「开发模块 N / 实现 US-N /
  颗粒度文档驱动开发」时使用。
argument-hint: "模块编号或 US 编号（合法值见项目配置区的分流规则），如：1"
user-invocable: true
---

# module-dev — 模块执行颗粒度文档驱动的 Spec-Kit 开发编排

## User Input

```text
$ARGUMENTS
```

## 适用范围

本 skill 是跨项目通用的**编排层**：流程、门禁、DoD 对所有项目相同；与项目绑定的
事实（文档目录与**US 台账指针**、技术栈句、门禁命令、全局不变量）全部收敛到
文末「项目配置区」——**逐 US 的章节对照与模块落位不在本 skill 内**，在项目台账
（配置区给出路径与取数规则），登记由 `/us-register N` 做。**复制本 skill 到新项目
时，只改写项目配置区，其余章节不动。**

开工前必读项目根 CLAUDE.md——宪法、质量门禁、陷阱表以它为准；本 skill 的配置区
若与 CLAUDE.md 冲突，以 CLAUDE.md 为准并停下报告。

## 总则

1. 可机器判定的检查项必须由门禁强制执行，不得以人工检查替代；机器无法判定的
   分歧必须停下等用户裁决，不得自行决定。
2. 软门禁：遇到下列任一情况，**立即停下、向用户报告、等确认后继续**：
   1. 需要创建"交付物清单"之外的任何对外概念（导出类型、配置键、数据表、
      REST 路径、`AGENT.md` frontmatter 字段）；
   2. 需要修改任何已定字面量（类型名、方法签名、配置键、表列名、端点路径）；
   3. 模块执行颗粒度文档与技术方案冲突（裁决：以项目技术方案为准，见配置区）；
   4. 需要修改前序模块交付的公共接口（当模块文档明确列为"改造点"的除外）；
   5. 第三方 API 在本地依赖中核实不到；
   6. 需要新增 plan 未列明的第三方依赖（**例外**：配置区「固定技术栈句」与本 US
      颗粒度文档已列明、且已经用户裁决的依赖**直接落地**，不重复询问；版本变更
      仍按新增处理）。
3. 测试纪律：不得删断言、不得无理由跳过测试（`t.Skip` 必须写明理由）、不得放宽
   阈值让测试变绿。实现错误修实现；认为测试错误则停下报告。未全绿不得宣称完成。
4. 硬门禁：测试、配置区「门禁命令」的全量构建、交付物存在性核对——不通过不
   放行，不得绕过。
5. 全程**不自动 commit / push / 部署**，同步时机由用户决定。

## 步骤 0：输入校验与模块类型分流

解析输入编号，按项目配置区「输入与模块类型分流」的规则分流；表外输入直接报错退出。

- 开发类模块 → 走完整流程（步骤 1~7）
- 评审类模块 → 拒绝并说明：本模块不产码，是下一模块的 specify 素材
- 串联类 / Demo 类模块等特殊模式 → 按配置区该行的处理方式执行

**续跑检查（分流后执行）**：检查 specs 目录下是否已存在该模块的 feature 目录
（如 `specs/{NNN}-<slug>/`，以 `.specify/feature.json` 为准）且 spec/plan 已
锁定。已存在 → 跳过步骤 2~4（specify/clarify/plan 已完成），从步骤 5
（/speckit-tasks）继续；只存在 spec 无 plan → 从步骤 4（/speckit-plan）继续。

## 步骤 1：上下文加载与依赖检查

1. **读取以下三类输入**（完整读取，不得凭记忆）：
   - 当前模块执行颗粒度文档（从 US 台账的文档列取；文件缺失 → 软门禁停下，
     先用 `/module-doc-gen` 生成）；
   - 技术方案对应章节（按配置区取数规则；本项目从 US 台账的对应列取）；
   - 前序各模块颗粒度文档的"交付物清单"节。
2. **依赖存在性检查**：对前序每个模块交付物清单里的核心类型 / 函数，在代码库里
   grep/Glob 确认存在。任何缺失 → 停下报告"先做第 X 个模块"，不得跳模块自造。
3. **分支**：确认当前在该模块的 feature 分支上（speckit-specify 的
   before_specify hook 会建分支；若 hook 未配置，手动
   `git checkout -b {NNN}-<slug>`），不在主干上直接开发。

## 步骤 2：组装并执行 /speckit-specify

用 Skill 工具调用 `speckit-specify`，参数按此骨架从**当前颗粒度文档的第 1、2
节**提炼（只写 WHAT/WHY，不带类型名和技术栈）：

```text
第{N}模块需求：<模块名>——<一句话定位>
背景与价值。<从文档"模块概述"提炼>
用户场景。<2~3 个具体场景>
功能需求。FR1~FRn <从文档"设计要点与约束"提炼，每条可测试>
明确不做（边界）。<文档"范围边界"逐项照搬>
验收标准。可自动化部分由文档"验收测试（Harness）"测试套件承载（测试全绿即
  通过），关键回归点：<列出文档中写出代码的那几个测试的守点>；
  人工项见文档"验证与验收"。
依赖与假设。<指向前序模块交付物；外部依赖>
```

## 步骤 3：/speckit-clarify

调用 `speckit-clarify`。有问题答问题（答案只从颗粒度文档和技术方案找，找不到 →
软门禁停下问用户）；无问题继续。

## 步骤 4：组装并执行 /speckit-plan

参数 = 固定技术栈句 + 本模块落位 + 测试策略句 + 语法禁区（均来自项目配置区）：

- **固定技术栈句**（配置区原文照抄）
- **模块落位**（按配置区取数规则取本模块落位，细节以技术方案模块章 / 该 US
  颗粒度文档 §3 为准）
- **测试策略句**（从文档"验收测试（Harness）"抄）：`测试策略按文档"验收测试
  （Harness）"执行：<测试类清单>（覆盖 <关键回归点>），单测默认执行、集成冒烟
  环境守卫 CI 跳过；实现完成的定义是门禁全绿。`
- **语法禁区句**（配置区原文；若配置区写"无"，本句省略）

## 步骤 5：/speckit-tasks + 固定软停点

1. 调用 `speckit-tasks`。
2. **自动比对**：任务清单 ↔ 当前颗粒度文档"交付物清单"（代码/测试/配置/表逐
   项），并确认测试任务先于或伴随对应实现任务（harness 先行）。
3. 输出比对结果（齐 / 缺什么 / 多什么），**停下等用户确认**后才进入下一步。
   这是流程中唯一的固定停点，不得跳过。

## 步骤 6：/speckit-analyze（实施前**阻断**检查）+ /speckit-implement

`tasks` 停点确认之后、进 implement 之前**必须**先跑一次 `/speckit-analyze`：
**CRITICAL 问题清零前不得进入 implement**（修 spec / plan / tasks，不得稀释原则，
也不得自行改宪法）。analyze 结论贴进验收报告。US 收口**再跑一次**（核对实现与
文档一致性，证据进七项 DoD）。

implement 期间逐任务执行，附加门禁：

- **写前（H3）**：涉及第三方 API 的任务，先在本地依赖核实方法存在；核实不到 →
  软门禁。
- **写中（H1/H5）**：只创建交付物点名的对外概念；已定字面量逐字保真；错误不吞
  （`err` 必落审计/日志或上抛；**不得**用 `_ =` 静默丢弃）；**可观测性双轨**：
  失败路径审计落库与 WARN 日志两者都要（只落审计不记日志 = 线上排障黑洞）；
  不建文档外抽象层；注释只写"为什么"。**静态门禁修复模式**（Go 形态，核心是
  **修实现不改判据**）：①门禁（gofmt / gofumpt / go vet / staticcheck / gosec /
  govulncheck）报出的问题一律改代码；抑制手段（`//nolint` / `#nosec` /
  `//lint:ignore`）**默认不得**使用，确有理由必须逐条写清理由、经用户确认、
  并进验收报告；②格式类一律 `gofumpt -w <本轮改动的文件>` 修（严格超集），禁手改
  格式；全仓格式检查（`gofumpt -l .` / `make fmt`）保持**只读**——**不得**顺手重排
  无关文件（避免无关 diff；全仓初始格式化的唯一例外是 `/init-foundation`）；③vet /
  staticcheck 的真问题按语义修——不得用 `_ =` 把"未使用"糊过去（先判断该不该
  处理）；④安全类（gosec）报警按编程指南 §7 陷阱表对应条目修（如 `http.Server`
  四超时必设、路径白名单必须真实路径投影后再比前缀）；⑤不为"消灭告警"牺牲可观测
  性（错误路径 WARN 日志与审计落库两者都要）。**案例不在此内嵌**——复核方式一律
  指 `docs/AiProgrammingGuide.md` §7 陷阱表（55 条，每条带"怎么发现"与"怎么
  复核"）。核心逻辑实现必须与颗粒度文档第 2 节的「核心逻辑」流程图/伪代码
  **逐块对照落地**（同构性核对，签名逐字一致）。**注释语言**（遵循项目 CLAUDE.md
  约定）：正文中文为主，英文标识符/术语保留原文不翻译；**关键注释（包级 /
  导出符号级 / 复杂逻辑段）中英并列**，中文在前、英文在后。**测试函数名必须是
  英文**（`TestXxx_yyy` 形态），不得用中文函数名；文档 harness 里若给出中文用例名，
  翻译成语义等价的英文名落地，并以注释保留文档原文以便对号。
- **写后**（任务级 DoD）：实现与测试一起落地，跑该模块测试，失败即时修复，不
  累积到任务末尾；同一问题修复 3 次不收敛 → 停下走 `/debug` 四阶段根因定位
  （**不得**第 4 次盲修）。
- 文档"验收测试（Harness）"里**写出代码的关键回归测试必须原样落地**（断言逻辑
  逐条保真；函数名按上条规则译成英文，文档原文以注释保留）。

## 步骤 7：模块级收尾——七项证据 DoD

全部满足才可宣布本模块完成，逐项把证据写进验收报告：

1. 门禁命令全绿（配置区），贴关键输出；
2. 文档 harness 映射表的每个测试类存在且非空，关键回归测试逐个对号；
3. "交付物清单"逐项 ls/grep 存在性核对；
4. **前序模块全部测试回归绿**（跨模块契约证据）；
5. **全局不变量逐条自查**（配置区清单，机器可判）；
6. 验收报告收尾：以上证据 + 文档"验证与验收"的**剩余人工项清单**（真模型 /
   真 webhook / 冒烟等），明确告知用户"harness 已判卷，这几项等用户人工执行"；
7. **变更总结（给 reviewer 的导读，直接输出在对话里，不另开文件）**——以
   `git status --short` / `git diff --stat` 实测为准，三段固定结构：
   - **改动点**：按包分组列新增 / 移动（改名）/ 修改 / 删除的文件，每处一句话
     说明动机；前序模块文件被本模块触碰的（哪怕只改 import）单独标出；
   - **重点 review 清单**：按风险排序 3~6 条——架构决策（依赖方向、契约变化）
     优先，其次文档交付物同构性（逐行对照点）、跨模块契约兼容性、静态门禁妥协点；
     每条给出文件行级定位；
   - **如何验证**：可直接复制执行的命令块（全量门禁、只跑本模块测试、关键回归
     单测、依赖方向 grep 等），每条命令注明预期结果；最后重复剩余人工项。

报告完停止——commit / push / 部署由用户决定。

---

## 项目配置区（复制本 skill 到新项目时，只改写本节）

> 本节是唯一允许因项目而异的部分；上文所有章节跨项目不动。
> 本实例按 Basalt 预填，事实来源：`docs/TechnicalSolution.md`（最权威）、
> 项目根 `CLAUDE.md`、`docs/AiProgrammingGuide.md`、`docs/EngineeringStandards.md`、
> `docs/Roadmap.md`。
> 术语：**模块执行颗粒度文档** = 模块级设计文档（本项目统一用该称法）。

### 输入与模块类型分流（规则式——**不在本 skill 内维护 US 清单**）

- 输入**必须**是整数 N（US 编号）；非整数 → 报错退出
- **取数**：读 `docs/us/REGISTRY.md`（**US 台账**，唯一登记处）找 `US-N` 行：
  - 台账**无**该行 → 报错退出：`US-N 未登记——先跑 /us-register N`（**不得**凭记忆造事实）
  - 台账**有**该行 → 按其「类型」列分流：

| 类型 | 处理 |
| --- | --- |
| 开发类 | 走完整流程（步骤 1~7） |
| 评审类 | 拒绝并说明：本模块不产码，是下一模块的 specify 素材 |
| 串联类 / Demo 类 | 本项目暂无此类；遇到时停下问用户 |

- **US-1 特殊说明**：工程地基的**装法**参照 `/init-foundation`（runbook：
  go.mod 骨架 / 版本核实 / Makefile 七目标（含覆盖率阈值判卷）/ CI 双 job / 配置与 `${ENV_VAR}` 校验 /
  slog 双档日志 / `basalt init` 工作区 / SQLite schema / 契约 YAML 单测）；
  **流程与验收仍以 US-1 颗粒度文档为准**（三步链照走），不另立流程
- US 是**开放序列**（US-15 起顺延，编号只增不改），见 `docs/Roadmap.md` §4.1

### 颗粒度文档与技术方案（取数规则）

- 文档路径 = 台账「模块执行颗粒度文档」列；文件缺失 → 软门禁停下，先跑
  `/module-doc-gen N`
- 技术方案：`docs/TechnicalSolution.md`（项目最权威文档）；需求文档：
  `docs/DemandAnalysis.md`。文档与其冲突时以技术方案为准（软门禁报告）
- 章节对照与模块落位**不在本 skill 内逐 US 登记**：
  - 技术方案 / 需求文档对应章节 = 台账「技术方案章节 / 需求文档章节」列
  - 台账该列写「**无对应章节**」时：以该 US 颗粒度文档的**差异裁决注**为主源、
    列出的对照段作对照。**不得**因"技术方案没有章节"就跳过冲突检查，也**不得**
    据此改写技术方案原文
  - 模块落位 = 台账「模块落位（摘要）」列；细节以该 US 颗粒度文档 §3（交付物
    清单 + 改造点）为准，改造点属软门禁，逐条停下报告
- 兼容承诺（条件触发）：US 触及 NivroOS 兼容承诺时（`docs/Compatibility.md`
  §2 对照矩阵行 / §5 共享验收样例）→ 对应节为**附加事实源**，§5 样例逐条作
  验收判据；无涉及不读
- US-15 起（开放序列）：章节来源按 `docs/Roadmap.md` §4.2 取对应方向节
  （颗粒度文档头部已注明），台账列取 `/us-register` 的派生值

### 编码规范（取数规则）

`docs/EngineeringStandards.md` 是本项目的工程规范（Go / 测试 / 数据库 / 日志 /
提交；§1 以 Go Code Review Comments 为基线，机器可判子集由 staticcheck
`-checks` 判卷）：

- **步骤 6 编码前**：**必须**读与本模块相关的节——Go 代码 §1、测试 §2、
  SQL / 迁移 §3、日志 / 错误 §4（改动涉及哪块读哪块）
- **步骤 7 收尾**：全局不变量自查按本节清单逐条执行；实测新坑按陷阱表惯例
  拟条（带「怎么发现」与「怎么复核」），**经用户确认后**入库
- 提交信息口径 §5（模板 `.gitmessage`；AI 不自动 commit / push / tag）

### 模块结构（单 Go module + `internal/` 十包为起点——**包数不冻结**）

逐 US 的落位见台账「模块落位（摘要）」列（取数规则见上节）；**不得**为"导航方便"
另建同义第二套包、**不得**让 `core` 反向依赖任何内部包。纪律是
`docs/TechnicalSolution.md` §10.1 的**三条包纪律**：

1. **依赖方向**：`channel` / `web` / `cli` → `core` ← `provider` / `memory` /
   `tool` / `storage` / `workspace`；`app` 是唯一允许"什么都知道"的装配层；
   `core` 只依赖标准库（不得 import 其他 `internal` 包）
2. **接口定义在消费方**：`core` 声明它需要的接口（如 `TaskStore` / `MemoryStore` /
   `Policy`），实现放各自的包——Go 的隐式接口让这成为自然写法（依赖倒置）
3. **第三方实现放消费方包内或 `internal/adapters/`**：MCP SDK 封装在 `tool`、
   SQLite 驱动封装在 `storage`；**cron 适配放独立内部包**（`internal/adapters/cron`），
   `core` 保留接口与纯业务逻辑、由 `app` 完成装配——新增一个第三方实现**不得**
   迫使其他包跟着改。⚠ 此处按**宪法「架构约束」**的加固口径执行：技术方案 §10.1
   同时写着「`core` 只依赖标准库」与「cron 调度封装在 `core`」，两句自相矛盾；
   冲突以 2026-10-15 经用户确认的宪法为准

**无环**由 CI 用 `go list` 复核。现有 10 包是**起点**：确实容纳不下时允许按需新增包
（前提是三条纪律仍然成立）；Spec-Kit 生成的 plan 必须与台账该列及该 US 颗粒度
文档 §3 一致。

### 分支口径

**仓库已初始化**（2026-10-15：`git init -b main`、首个 commit、推送 `origin/main`；
授权与分工记录见 `docs/GettingStarted.md` §1.4）。仓库无 `.specify/extensions.yml`
（即无 `before_specify` hook）→ speckit-specify 不会建分支，按步骤 1.3 **手动建**：
`git checkout -b {NNN}-<slug>`（如 `001-foundation`；NNN 取 specs 目录序号，slug 取
颗粒度文档短名）。合回 main 的口径由用户定（建议快进合回，保持线性历史）；
**AI 不自动 commit / merge / push / tag**——每个 US 的 commit 由用户执行。

### 固定技术栈句

`Go 1.27.2 构建版本（go.mod 的 go 行写最低版本要求 go 1.27，准确版本由 toolchain 指令与 CI 固定）+ 单 Go module（cmd/basalt + internal/ 十包）+ spf13/cobra v1.10.2 + goccy/go-yaml v1.19.2 + sashabaranov/go-openai v1.43.0（只做协议调用，禁自动工具循环）+ modernc.org/sqlite v1.60.1 + robfig/cron/v3 v3.0.1（6 段：cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)，每任务时区经 CRON_TZ= 前缀）+ modelcontextprotocol/go-sdk v1.8.0 + fsnotify v1.10.1 + 标准库 net/http + ServeMux + log/slog + testify v1.12.1；凭证 ${ENV_VAR} 占位；不用 ORM（手写 SQL + schema.sql 幂等）；CGO_ENABLED=0；本技术栈句与已批准计划内的依赖直接落地、表外新增坐标或版本变更先问用户；GOPROXY=https://goproxy.cn；写坐标前先 go list -m -versions 核实`

版本坐标明细见 `docs/TechnicalSolution.md` §1.2（2026-10-14 经模块代理实测）；
落地后以 `go.mod` / `go.sum` 为唯一事实源。追加段（如 US-11 起的官网展示前端）
由 `/us-register` 按能力追加机制在对应 US 登记时补入——**本批不预写**。

### 门禁命令

- 全量门禁：`make check`（= `fmt lint sec test test-integration cover build` 七目标
  聚合；**Makefile 由 US-1 交付**——未落地前按 `docs/AiProgrammingGuide.md` §3.2
  的命令序直跑：gofmt → gofumpt → go vet → staticcheck → gosec → govulncheck →
  `go test -race -cover ./...` → `go test -tags=integration ./...` →
  覆盖率阈值判卷（`make cover` 的等价脚本）→ `CGO_ENABLED=0 go build ./...`）
- **三条实测纪律**（判卷顺序写错会让门禁"报绿而实际没检"，见编程指南 §3.2）：
  1. `gofmt -l` / `gofumpt -l` 发现格式差异**不会自动失败**（退出码 0、只把文件
     打印到 stdout）——判卷**必须先查退出码、再判输出**（语法错误时退出码非 0
     而输出为空）；两个格式工具各查一遍
  2. `go test -race` 需要 CGO 与 C 编译器，与 `CGO_ENABLED=0` 发布构建**要求的
     CGO 状态相反**——两者**必须**分属不同命令 / job
  3. build tag 隔离的集成测试默认不执行——`go test ./...` 不碰它们，**必须**有
     独立的 `go test -tags=integration ./...` 步骤
- 格式修复只许 `gofumpt -w <本轮改动的文件>`（gofumpt 是 gofmt 的严格超集，一次
  到位），禁手改格式；全仓格式检查（`gofumpt -l .`）**只读**——**不得**顺手重排
  无关文件（全仓初始格式化的唯一例外是 `/init-foundation`）
- 新增第三方坐标前：`GOPROXY=https://goproxy.cn go list -m -versions <module>`
  核实（软门禁；**已裁决且已批准计划内的依赖直接落地**，不重复询问）；交付收尾时
  `gosec -quiet ./...` 与 `govulncheck ./...` 须零命中
- 覆盖率底线（宪法开发工作流，**必须有可执行判据**）：`core` / `tool`（沙箱）/
  `memory` / `storage` ≥ 70%，其余 ≥ 50%。`go test -cover` 只打印百分比、**不会**
  因低于阈值而失败——**必须**由 US-1 交付的 `make cover`（纳入 `make check`）按包
  解析结果并判失败；判定口径：**有可执行语句但无测试文件的包**（`no test files`）
  计 0%、未达标即失败（不得豁免），**无可执行语句的包**（`[no statements]`）跳过；
  安全关键路径（沙箱三档、凭证红线、审计落库）必须有专项测试，不靠覆盖率兜底

### 全局不变量（收尾逐条自查，机器可判）

1. 坐标零 Agent 框架：`grep -nE "eino|langchaingo|adk-go|trpc-agent-go" go.mod` 零命中
2. `context.WithValue` 逐处审查——**只允许追踪标识**：`grep -rn "context.WithValue" --include='*.go' cmd internal` 的每处命中人工确认（业务状态一律走显式类型化参数）
3. 审计两表 day-one 且写路径唯一：审计 SQL 只出现在 `internal/storage`，其余包经 `core` 定义的仓储接口调用；工具执行与模型调用**先插 `pending`、插入失败即拒绝发出**，返回后更新终态
4. 凭证零明文：配置与代码中的凭证一律 `${ENV_VAR}` 占位，加载期未解析残留即拒（点名配置键）；`grep -rnE "sk-[A-Za-z0-9]{16,}" --include='*.go' --include='*.yaml' .` 零命中
5. 无 ORM / 无自动迁移：`grep -nE "gorm|entgo|ent\.io|xorm" go.mod` 零命中；建表与迁移只在 `schema.sql` + `schema_version`（已发布迁移只追加、不改写）
6. 单二进制：`CGO_ENABLED=0 go build ./...` 通过（`-race` 不在该命令内）
7. 时区禁用词：`grep -rnE "time\.Local|[.]Local[(][)]|time\.Now[(][)]\.Location[(][)]" --include='*.go' .` 零命中（调度 zone 缺省 UTC、Agent 配置显式 zone；**`.Local()` 是同一条禁令的另一种写法**——对 `time.Time` 值调 `.Local()` 与写 `time.Local` 一样语义上依赖进程本地时区，只是躲过了字面 grep：2026-10-16 实测 `internal/cli/session.go` 的渲染路径就是这种漏网写法）
8. 随机源红线：`grep -rn '"math/rand' --include='*.go' cmd internal` 零命中——密钥 / 令牌 / 随机标识一律 `crypto/rand`（EngineeringStandards §1.8；gosec G404 复核）
9. 点导入零命中：`grep -rnE '^import[[:space:]]+\.|^[[:space:]]+\.[[:space:]]*"' --include='*.go' cmd internal` 零命中——`import .` 只在测试的循环依赖特例允许（EngineeringStandards §1.3）
10. `core` 只依赖标准库：`GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` 只出标准库包名（零第三方路径、零 `github.com/ymedword/basalt/internal/...`；登记读数 `[context errors time]`）——`core` 自 US-4 起首次有可执行语句，架构约束「核心抽象与引擎只依赖标准库」自该 US 起按此判据复核（技术方案 §10.1 三条包纪律之一）
11. LLM 审计入口唯一：`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中（登记读数：零命中）——两阶段审计（`pending` 预写 / 终态回填）只由 `internal/provider` 的 `ProviderService` 调用；`core` 只**声明** `LLMCallStore` 接口、**不得**出现调用点（宪法 II / V：审计写入路径唯一、运行引擎不自建第二套）

### 语法禁区

无——本项目的静态检查（`go vet` / staticcheck / gosec）支持 Go 1.27 全部语法形态。
若后续某工具版本对某语法 / 依赖形态报误，在此列明（附复核命令）。
