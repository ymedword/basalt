# Implementation Plan: US-13 官网展示主页与 README（对外门面）

**Branch**: `013-website-readme` | **Date**: 2026-10-24 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/013-website-readme/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 13` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us13-website-readme.md](../../docs/us/us13-website-readme.md)（R3，R2 于 2026-10-24 经人工 review ① 通过，sha256 `79f53f81736c49e11ff58c9f795610b073dd7cedd616830de9883e1712d4a14a`；R3 按用户 2026-10-24 实施后确认同步，见模块卡修订记录）§2 / §3 / §4 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（本 US **不改契约**）。D-1–D-6 已全部裁决为 A；实施后确认的六类图、单张 README 简图、五层架构与英文终端也已冻结，本 plan 只做落位与验证映射，不引入文档外交付物。

## Summary

交付 Basalt 的对外门面：一个默认英文、完整支持中英文切换的静态官网（首页 / 快速开始 / 架构 / 文档索引四页 × 两种语言）与双语 README；官网只讲已交付能力，首页六项平台能力与架构页五项核心能力分层表达，架构统一采用接入层、应用层、执行引擎层、基础服务层、存储层，安全与审计贯穿各层；架构文档提供完整项目全景与六类专题图中英各一套，两版 README 各只嵌一张本地化简化系统架构图；快速开始写真实链路（最小配置 → 初始化 → 创建 Agent → 启动服务 → 管理台配置模型并启用 → 停止服务 → 首次对话 → 查看会话）；首页演示来自可本地复跑、使用 `read_file` 的脚本化模型，终端展示全英文、明确标注且命令与快速开始逐字同源；仓库提供只手动触发的 Pages 部署工作流（本 US **不执行部署**），并把公开 `docs/Operations.md` 中指向私有文档的链接修正为公开目标。

技术路线的要点：

1. **站点工程**：`website/` 内使用 VitePress 1.6.4 静态构建，`base: '/basalt/'`；Node 24.22.0 仅构建期与维护用，不进产品二进制、不进 `go.mod`。
2. **双语路由**：英文为根路径，中文为 `/zh/` 前缀；统一的语言切换函数保留页面、查询串与锚点，并正确处理 base 前缀。
3. **内容同源**：命令序列、能力卡、页脚目标集中在 `website/.vitepress/data/*.mjs` 纯数据模块；README 的标记块与站点数据逐条比对，首页演示命令必须与快速开始命令逐字相等。
4. **图表**：`website/diagrams/basalt-diagrams.drawio` 为唯一图源，共 12 页，导出六类（项目全景 / 系统架构简图 / 核心执行流 / 模块图 / Go 包依赖 / 部署）× 英中 SVG；每个 SVG 带 `data-diagram-id` / `data-source-commit` / `<title>` / `<desc>` / `viewBox`。README 只使用系统架构简图。
5. **演示夹具**：`website/demo/local-model.mjs` 是只绑定 `127.0.0.1` 的 OpenAI 兼容 stub，首轮调用 `read_file` 读取样例 README 后返回项目摘要；`transcript.json` 由真实 CLI 执行采集，页面明确标注脚本化回复；展示层把工具进度标签译为英文，原始 transcript 与产品 CLI 输出保持原样。
6. **检查先行**：`check-links` / `check-diagrams` 为纯函数 + 可执行入口；`check-scope` 仅 stage 用；Node 内置 `node --test` 承载单测，避免 Vitest 与 Vite 5 的 peer 冲突。
7. **Go 侧唯一改动**：新增 `internal/cli/public_docs_test.go`，扫描公开 README 与公开 `docs/Operations.md`，不扩展生产接口。
8. **CI 与发布边界**：`.github/workflows/ci.yml` 新增独立 `website` job；`.github/workflows/pages.yml` 仅 `workflow_dispatch`，无 secrets、无自动触发、本 US 不执行。
9. **私有边界**：站点、README 与公开运维手册不链接任何未公开路径；私有文档修改（Requirements / DevelopmentLoop / TechnicalDesign §8.2）只在本地，不提交。
10. **收口证据**：stage `harness/stages/us13-website-readme.json` 为 **local-only**（`inputs` 含私有文档）；review ② 前必验项见 §6.2.1，不以"已登记未验证"收口。

## Technical Context

**Language/Version**: Go 1.27.3 构建版本（仅新增一个测试文件；`go.mod` 的 `go` 行写最低要求 `go 1.27`，准确构建版本由 `toolchain` 与 CI 固定——**不得**表述为「锁定 1.27.3」）。站点构建使用 Node 24.22.0（`website/.nvmrc`）与 VitePress 1.6.4；Node 仅站点构建 / 检查、管理台前端重建与演示 transcript 采集，**不是**产品运行时依赖。

**Primary Dependencies**: Go 侧 **零新增坐标**——`go.mod` / `go.sum` 不改。站点侧只新增 `vitepress@1.6.4`（`docs/TechnicalDesign.md` §1.3 已列，2026-10-24 经 npm registry 复核 1.6.4 为 latest）。**不引入** Vitest / jsdom / TypeScript 构建链：VitePress 1.6.4 依赖 Vite `^5.4.14`，Vitest 5.0.5 的 peer 要求 Vite `^6.4.0 || ^7 || ^8`，同包安装会产生双 Vite 或 peer 冲突；站点测试用 Node 24 内置 `node --test`。

**Storage**: **不适用**——无数据库、无迁移、无表变更；站点产物是静态文件，产品 SQLite 与 `schema.sql` 均不改。

**Testing**: 三层——L1 纯函数 / 数据（`node --test "tests/*.test.mjs"`：路由与语言切换、能力数据、命令提取、stub 回复、对比度、检查器夹具）；L2 构建与链接（`npm run build` 后 `npm run check`：链接 / 锚点 / base / 私有路径 / 外部资源 / 六类图 / README 单图）；L3 Go 与人工（`go test ./internal/cli -run TestPublicDocs`；本地预览、三档视口、键盘与语言切换）。**默认层不访问外部网络、不调用真实模型**；stub 只绑定回环。站点检查只在 CI `website` job 与本机 Node 24.22.0 运行；`make check` / `scripts/check-acceptance.*` **不得**调用 Node / npm。覆盖率：本 US 无新增生产 Go 代码，既有阈值不变。

**Target Platform**: 开发机 Windows 11 + Git Bash；静态站点面向现代常青浏览器（桌面与移动），项目页面部署到 GitHub Pages、基路径 `/basalt/`；本 US 只本地预览，不执行线上部署。发布二进制仍为 `CGO_ENABLED=0` 单文件，站点产物**不**参与 `go:embed`。

**Project Type**: 静态站点 + 双语文档 + 一个 Go 回归测试；不新增 Go 包、不改产品运行时。

**Performance Goals**: 无吞吐 / 延迟承诺；判据是**内容与构建正确性**——站点可从 `/basalt/` 基路径完整加载、断网可读、无外部 CDN；对外材料不写未测量的耗时或规模数字。

**Constraints**: **内容同源**——命令序列只由站点数据模块定义，README 标记块与演示 transcript 逐字比对；**隐私边界**——公开材料不链接私有路径，公开 `docs/Operations.md` 只链公开目标；**构建期依赖隔离**——VitePress / Node 不进单二进制、不进 Go 门禁；**无自动部署**——Pages 工作流仅手动触发；**无搜索**——首版不放搜索框、不暗示搜索能力；**事实校准**——`registered=true` 不等于可运行、`limit_reached` 是部分结果结束、不写未交付能力为现状；**格式**——Markdown 改动过 markdownlint，Go 测试文件过 `gofumpt`。

**Scale/Scope**: **工作区 68 个文件**（6 修改 + 62 新增）：公开可提交 65（3 个已跟踪修改 + 62 新增），本地私有修改 3（`docs/Requirements.md`、`docs/DevelopmentLoop.md`、`docs/TechnicalDesign.md`）。站点含 8 个双语页面、六类图中英共 12 个 SVG + 1 个 12 页 drawio 图源、9 个品牌 / 插图资源、5 个 Node 测试文件、3 个检查脚本、2 个演示夹具、1 个 Go 测试文件、2 个工作流。回退变体（历史，不执行）：去掉 `/docs/` 两页与对应图 → 66 / 63；去掉中文 SVG → 59 / 56；两者都去掉 → 57 / 54。

**环境实测读数（2026-10-24，本轮当场命令）**: 分支 `main`；起始 commit `eb97cd5`；工作区 `M docs/Operations.md`（D-6 A 已落地）与 `?? tmp/`；`specs/013-website-readme/` 尚未跟踪。本机 PATH 无 `node`；已在本轮把 Node 24.22.0 官方 zip 解压到 `tmp/us13-lint/` 仅供文档格式检查（临时物，收口前清理）。`vitepress@1.6.4` 经 npm registry 复核存在且为 latest。`docs/TechnicalDesign.md` §8.2 的 US-13 行已改为四页（私有，不提交）。**本轮未跑**站点构建、`node --test`、Go 测试与 `make check`；相关项在实现后由 Claude 验收。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本模块落地判据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | `go.mod` 零 Agent 框架坐标；本 US 不改循环体与工具调度 | 通过 |
| II LLM 客户端只做协议调用 / 单一消息仓储 | 站点 stub 是独立演示夹具，不进入产品运行时；不新增消息追加路径 | 通过 |
| III Provider 显式映射 | 不改 Provider 目录、准入或路由；快速开始只使用已发布映射的管理台能力 | 通过 |
| IV 一个目录 = 一个 Agent / 声明式绑定 | 快速开始只创建示例 Agent；不引入软连接、不新增 `use_skill` | 通过 |
| V 审计 day-one 且 fail-closed | 本 US 无产品运行时；快速开始只消费既有审计与端点，不绕过 | 通过 |
| VI 授权与执行分离 | 不改 Policy / CommandRunner；stub 不是工具执行器 | 通过 |
| VII 同步执行 + context 纪律 | 无新增异步编排、无 `context.WithValue` 业务状态 | 通过 |
| VIII 单二进制纪律 | 站点产物不进 `go:embed`；VitePress / Node 为构建期工具，不进发布二进制；`-race` 与 `CGO_ENABLED=0` 分属不同命令 | 通过 |
| IX 不用 ORM / 只追加迁移 | 无数据库改动、无迁移 | 通过 |
| X 范围卡紧 | 交付物与模块卡 §3 清单逐一对应；真实模型与线上部署明确不执行 | 通过 |
| 架构约束：依赖方向 | 只新增 `internal/cli` 测试；不新增 Go 包、不改 `core` / `app` / `web` / `workspace` / `storage` | 通过 |
| 架构约束：接口定义在消费方 | 无新增生产接口；测试直接读文件，不扩展端口 | 通过 |
| 架构约束：第三方实现放消费方包内或 `adapters` | 站点与 Node 工具链完全在 Go 模块之外 | 通过 |

**Post-Design Re-check**: Phase 1 设计（research / data-model / quickstart）完成后复核，结论不变；无违反项需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/013-website-readme/
├── plan.md              # 本文件
├── spec.md              # 需求规格（含 2026-10-24 澄清）
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── checklists/
│   └── requirements.md  # 规格质量清单（16/16）
├── run-state.md         # 过程状态（/module-dev 建立；非设计输入）
└── tasks.md             # Phase 2 输出（/speckit-tasks，不由本命令创建）
```

**不新建 `specs/013-website-readme/contracts/`**：本 US 不新增或修改任何外部接口；固定字面量仍在 [docs/contracts/](../../docs/contracts/)，站点的 URL / 命令契约在 [data-model.md](./data-model.md) 与模块卡 §3 中定义，复制一份必然漂移。

### Source Code (repository root)

```text
README.md                                  # 新增：英文默认门面
README.zh-CN.md                            # 新增：中文门面

website/
├── package.json                           # 新增：vitepress@1.6.4 + scripts（dev/build/preview/test/check/demo）
├── package-lock.json                      # 新增：npm ci 基线
├── .nvmrc                                 # 新增：24.22.0
├── README.md                              # 新增：站点维护说明
├── .vitepress/
│   ├── config.mts                         # 新增：base / locales / 导航 / 页脚 / 404 文案 / 构建配置
│   ├── theme/
│   │   ├── index.ts                       # 新增：主题入口
│   │   ├── Layout.vue                     # 新增：语言切换与页脚挂载
│   │   ├── custom.css                     # 新增：视觉令牌 / 响应式 / 对比度 / 焦点环
│   │   └── components/                    # 新增：HomeHero / CapabilityGrid / CliDemo / SiteFooter / LanguageSwitch / DiagramFigure
│   └── data/
│       ├── navigation.mjs                 # 新增：导航 / 页脚 / 语言切换纯函数
│       ├── capabilities.mjs               # 新增：六项平台能力、五项核心能力与六类图
│       ├── quickstart.mjs                 # 新增：命令序列唯一来源
│       └── demo.mjs                       # 新增：演示可见行与自动播放判定
├── index.md                               # 新增：英文首页
├── quickstart/index.md                    # 新增：英文快速开始
├── architecture/index.md                  # 新增：英文架构
├── docs/index.md                          # 新增：英文文档索引
├── zh/
│   ├── index.md                           # 新增：中文首页
│   ├── quickstart/index.md                # 新增：中文快速开始
│   ├── architecture/index.md              # 新增：中文架构
│   └── docs/index.md                      # 新增：中文文档索引
├── public/
│   ├── brand/{basalt-logo,favicon,rock-column}.svg
│   ├── illustrations/01-define.svg … 06-self-host.svg
│   └── diagrams/{en,zh}/{project-overview,system-architecture,execution-flow,module-map,package-dependencies,deployment}.svg
├── diagrams/basalt-diagrams.drawio       # 新增：12 页图源
├── demo/
│   ├── local-model.mjs                   # 新增：回环 OpenAI 兼容 stub
│   └── transcript.json                   # 新增：真实采集的演示数据
├── scripts/
│   ├── check-links.mjs                   # 新增：dist HTML 链接 / 锚点 / base / 私有路径
│   ├── check-diagrams.mjs                # 新增：六类图 / 双语 / 图源 / 溯源
│   └── check-scope.mjs                   # 新增：stage 专用 git 范围检查
└── tests/
    ├── routes.test.mjs                   # 新增
    ├── content.test.mjs                  # 新增
    ├── demo.test.mjs                     # 新增
    ├── contrast.test.mjs                 # 新增
    └── checks.test.mjs                   # 新增

internal/cli/public_docs_test.go           # 新增：公开 README + 公开 Operations 的 Go 回归测试
.github/workflows/ci.yml                   # 修改：新增 website job
.github/workflows/pages.yml                # 新增：仅 workflow_dispatch
.gitignore                                 # 修改：忽略 website 构建产物与 node_modules
docs/Operations.md                         # 修改：D-6 A 链接修正（公开）
docs/Requirements.md                       # 修改：§5.8 / §6.1（本地私有，不提交）
docs/DevelopmentLoop.md                    # 修改：§4.5.1（本地私有，不提交）
docs/TechnicalDesign.md                    # 修改：§8.2 四页（本地私有，不提交）
harness/stages/us13-website-readme.json    # 新增：本 US 专属 stage（local-only）
```

**Structure Decision**: 站点完全独立在 `website/`，不进入 Go module、不参与 `go:embed`；四页与数据模块分离，命令只在 `data/quickstart.mjs` 定义一次；主题只做展示与响应式，不在组件里硬编码命令或能力清单。Go 侧只在 `internal/cli` 新增一个测试文件，复用既有包结构。CI 保持 Go job 与站点 job 分离，互不调用。

## Complexity Tracking

无 Constitution Check 违反项，无需登记。

## Phase 0 / Phase 1 产物

- [research.md](./research.md)：关键技术决策与替代方案（四页结构、VitePress / Node、无 Vitest、语言切换、命令同源、图表溯源、本地 stub、检查器、手动部署、Operations 修正）。
- [data-model.md](./data-model.md)：页面 / 路由、能力、图表、命令步骤、演示记录与禁止清单的实体与校验规则。
- [quickstart.md](./quickstart.md)：可执行的验证向导（门禁、专属 check、review ② 必验项与未授权边界）。
