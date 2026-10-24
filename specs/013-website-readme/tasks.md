# Tasks: US-13 官网展示主页与 README（对外门面）

**Input**: Design documents from `specs/013-website-readme/`
**Prerequisites**: [plan.md](plan.md)（必需）、[spec.md](spec.md)（必需）、[research.md](research.md)、[data-model.md](data-model.md)、[quickstart.md](quickstart.md)
**模块卡（R3）**: [docs/us/us13-website-readme.md](../../docs/us/us13-website-readme.md) §3 / §4 / §6（R2 于 2026-10-24 review ① 通过，sha256 `79f53f81736c49e11ff58c9f795610b073dd7cedd616830de9883e1712d4a14a`；R3 按用户 2026-10-24 实施后确认同步）

**Tests**: 本 US 要求检查与内容测试伴随实现（模块卡 harness 先行口径）。Node 测试用 `node --test`；Go 测试函数名英文 `TestXxx_yyy`，中文验收点以注释保留。

**Organization**: 任务按依赖顺序分六阶段；Phase 3 起每阶段对应 spec 的一个用户故事，可独立验收。专属 check 标注 `check:`；check ID 以模块卡 §4.4 的四个为唯一枚举：`go-docs-consistency`、`website-build`、`workflow`、`scope`。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1–US4），仅故事阶段使用
- 每个任务给出精确文件路径

## Path Conventions

- 仓库根：`D:\code\basalt`；站点工程：`website/`
- Go 测试：`internal/cli/public_docs_test.go`
- Harness 工件：`harness/stages/us13-website-readme.json`（local-only）
- 私有文档修改：`docs/Requirements.md`、`docs/DevelopmentLoop.md`（不提交）

---

## Phase 1: Setup（共享前置）

**Purpose**: 站点工程骨架、主题与构建配置；此阶段不交付任何用户故事。

- [x] T001 新建 `website/package.json`（`private: true`、`type: module`、`engines.node >=24.22.0`；devDependency 只含 `vitepress@1.6.4`；scripts：`dev` / `build` / `preview` / `test`=`node --test "tests/*.test.mjs"` / `check`=`node scripts/check-links.mjs && node scripts/check-diagrams.mjs` / `demo`=`node demo/local-model.mjs`）与 `website/.nvmrc`（`24.22.0`）；执行 `npm install` 生成 `website/package-lock.json` 并提交（不得引入 Vitest / jsdom / TypeScript 构建链）
- [x] T002 [P] 新建 `website/.vitepress/config.mts`：`base: '/basalt/'`、`lang: 'en'`、`appearance: false`、locales `root` / `zh`（含每语言 nav、outline 与 404 文案：默认英文，切换语言后回目标语言首页）、`head` favicon、本地字体与无外部 CDN；不配置站内搜索
- [x] T003 [P] 新建 `website/.vitepress/theme/index.ts`、`website/.vitepress/theme/Layout.vue`、`website/.vitepress/theme/custom.css`：`extends: DefaultTheme`、注册全局组件、群青蓝页脚、浅色 CLI 动画样式、对比度令牌、焦点环、三档响应式（1440×900 / 768×1024 / 390×844）、`prefers-reduced-motion` 降级
- [x] T004 [P] 修改 `.gitignore`：新增 `website/node_modules/`、`website/.vitepress/dist/`、`website/.vitepress/cache/`；不忽略 `package-lock.json`、`public/`、`tests/`

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: 站点数据模块、品牌资产、六类图与共享组件；所有用户故事依赖此层。

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事阶段。

- [x] T005 [P] 新建 `website/.vitepress/data/navigation.mjs`：`BASE`、`REPO_URL`、`ISSUES_URL`、`OPERATIONS_URL`、`ROUTES`（四页 × 英中）、`localePath`、`switchLocalePath`（保留查询串 / 哈希 / base）、`resolveNav`、`resolveFooter`；页脚只保留真实目标（部署指南 / CLI 演示 / 架构 / 配置 / CLI 命令 / 工具与 MCP / Operations / GitHub / Issues）
- [x] T006 [P] 新建 `website/.vitepress/data/capabilities.mjs`：首页六项平台能力（含插图路径与双语 `alt`）与架构页五项核心能力（Provider / ReAct / Memory / Tool / Web Service）、`DIAGRAMS`（六类固定 ID，含项目全景）与 `CURRENT_SCOPE_NOTE`；每项带 `evidence` 文件或机制；五层架构与当前 / 未来分离文案从数据模块取
- [x] T007 [P] 新建 `website/.vitepress/data/quickstart.mjs`：`QUICKSTART_COMMANDS`（`basalt init` / `basalt profile create demo` / `basalt profile list` / `basalt serve` / `basalt chat --agent demo --message "Hello"` / `basalt session list`）、`TOKEN_GENERATION`（Windows / POSIX）、`MINIMAL_CONFIG`、`PREREQUISITES`（五行）、`STEPS`、`TROUBLESHOOTING`、`DURATION_MEASURED = false`、`extractBasaltCommands`、`assertQuickstartParity`；命令唯一事实源，不含 `--profile` / `profile enable`
- [x] T008 [P] 新建 `website/.vitepress/data/demo.mjs`：`visibleLines(transcript, reducedMotion)`、`shouldAutoplay(reducedMotion)` 与演示标注文案；不复制 stub 回复正文
- [x] T009 [P] 新建 `website/public/brand/`（`basalt-logo.svg`、`favicon.svg`、`rock-column.svg`）与 `website/public/illustrations/01-define.svg` … `06-self-host.svg`：本地、无文字、无外部引用
- [x] T010 [P] 新建 `website/diagrams/basalt-diagrams.drawio`（未压缩 XML，恰好 12 页：六类 × 英中）并导出 `website/public/diagrams/{en,zh}/{project-overview,system-architecture,execution-flow,module-map,package-dependencies,deployment}.svg`；每个 SVG 带 `data-diagram-id`、`data-source-commit`、`<title>`、`<desc>`、`viewBox`；执行流 `<desc>` 含四个终态与失败 / 拒绝 / 取消 / 恢复分支；禁止脚本 / `foreignObject` / 外部引用
- [x] T011 新建共享组件 `website/.vitepress/theme/components/HomeHero.vue`、`CapabilityGrid.vue`、`SiteFooter.vue`、`LanguageSwitch.vue`、`DiagramFigure.vue`：只消费数据模块与路由函数，不硬编码命令或能力清单；插图双语 `alt`；外链 `rel="noopener noreferrer"`

**Checkpoint**: 数据、资产、图与共享组件就绪；用户故事可开始。

---

## Phase 3: User Story 1 - 访客读懂 Basalt 并在中英文之间切换 (Priority: P1) 🎯 MVP

**Goal**: 四页 × 英中可访问；首页六卡、架构页五项能力、五层架构与六类图完整；语言切换保留页面与锚点；无搜索入口。

**Independent Test**: 打开八个地址；在架构页锚点切换语言；检查六卡与五项能力；只用键盘可达主导航与语言切换。

- [x] T012 [P] [US1] 新建 `website/index.md` 与 `website/zh/index.md`：首屏文案取确认稿（无第二组标语）、Get started / Watch demo CTA、六项平台能力卡片、默认英文 / 中文完整；公开标题 `Platform capabilities / 平台能力`；定位段含至少两个不同用途 demo Agent 示例
- [x] T013 [P] [US1] 新建 `website/architecture/index.md` 与 `website/zh/architecture/index.md`：五项核心能力、完整项目全景 + 六类图、五层架构（接入 / 应用 / 执行引擎 / 基础服务 / 存储；调度归应用层，安全与审计贯穿）、`#tools-and-mcp` 锚点、当前单节点实现与未来规划分开（区分多 Agent 托管 / 协作 / 多节点分布式，本版本只做托管；含两个不同用途 Agent 示例）
- [x] T014 [P] [US1] 新建 `website/docs/index.md` 与 `website/zh/docs/index.md`：Get started / Understand / Operate / Project 四组；只链真实目标；低调说明无站内搜索，不放搜索框
- [x] T015 [P] [US1] 新建 `website/tests/routes.test.mjs`：八个地址、语言切换（锚点 / 查询串 / base / 404 回退）、404 文案（默认英文；切换语言后回目标语言首页）、导航与页脚目标集合、每个 `ROUTES` 锚点在页面源文件有显式 `{#anchor}`
- [x] T016 [P] [US1] 新建 `website/tests/contrast.test.mjs`：从 `custom.css` 读取颜色令牌，正文 / 背景、页脚文字 / 背景达到 WCAG AA；焦点环颜色存在
- [x] T017 [US1] 新建 `website/tests/content.test.mjs`：内容检查实现为可注入文件映射的纯函数（默认读仓库文件），四页齐备（缺页失败，不跳过）、首页六卡与证据文件、架构页五项能力、未交付能力禁用词、私有路径、"已上线 / 已验证"零出现、未测量时长；每页唯一 title / description
- [x] T018 [US1] 本地预览走查：`npm run dev`（或 `npm run preview`）检查三档视口、六卡 3 / 2 / 1、键盘与焦点可见、语言切换、减少动态效果与断网（无外部请求）；证据写入 run-state，不以截图替代命令输出；`check: website-build`

**Checkpoint**: US1 可独立演示——四页双语与视觉基线完整。

---

## Phase 4: User Story 2 - 新用户按 README 从干净环境跑通首次对话 (Priority: P1)

**Goal**: 双语 README 与站点快速开始使用同一条真实命令链；`registered=true` 说明正确；Windows token 方法可用。

**Independent Test**: 干净目录按 README 执行到 `session list`；核对三处命令逐字一致；确认无 `--profile`。

- [x] T019 [P] [US2] 新建 `README.md`：英文默认门面；定位、使用价值、单张系统架构简图、快速开始（真实链路）、配置、控制台、依赖边界（预构建运行不需 Go / Node；源码构建需 Go；Node 仅维护）、安全、文档；顶部中文链接；**只嵌一张**本地化 `system-architecture.svg`，完整全景与专题图链接架构文档；无 LICENSE 声明、无线上 Pages URL
- [x] T020 [P] [US2] 新建 `README.zh-CN.md`：与英文同结构 / 事实 / 命令 / 依赖边界，**只嵌一张**本地化系统架构简图；顶部英文链接；代码片段不翻译
- [x] T021 [US2] 新建 `website/quickstart/index.md` 与 `website/zh/quickstart/index.md`：锚点 `{#config}` `{#console}` `{#commands}` `{#deploy}` 与 `#first-chat` / `#verify` / `#troubleshoot`；五条运行前置区分预构建 / Go 源码构建 / 模型 / SQLite / Agent 外部依赖；`serve` 只监听 `127.0.0.1` 并指向 Operations 的远程访问说明；管理台标签双语注释与占位符 / disabled 说明；完整链路（含 `basalt profile list`）与故障表
- [x] T022 [US2] 修改 `website/tests/content.test.mjs`：快速开始命令与 `QUICKSTART_COMMANDS` 逐字一致（用 `extractBasaltCommands` / `assertQuickstartParity`）；README 双语命令一致、互链与最小配置事实；无 `--profile` / `profile enable`；`registered=true` 说明、模型占位符、Windows token 方法、依赖措辞（预构建 / 源码构建 / Node 仅维护）存在
- [x] T023 [US2] 新建 `internal/cli/public_docs_test.go`：`TestPublicDocs_UseAgentFlag` 的 `public`（双语 README）与 `requirements-local`（私有 requirements，缺失时带理由 `t.Skip`）子测试；不扩展生产接口，`check: go-docs-consistency`
- [x] T024 [US2] 私有文档修订：`docs/Requirements.md` §5.8（`chat --agent`、`profile delete` 归档语义）与 §6.1（四页、双语、命令以实跑为准、六卡与五项分层）；`docs/DevelopmentLoop.md` §4.5.1（首页六卡、架构五项、真实链路、依赖措辞、文件量）；**不提交**
- [x] T025 [US2] 按公开 README 在干净临时工作区执行完整快速开始（含管理台实际配置与启用点击），到 `session list` 看到会话；证据写入 run-state；`check: go-docs-consistency`

**Checkpoint**: US2 可独立演示——README 单文件能带新用户跑通。

---

## Phase 5: User Story 3 - 首页演示可复跑且与命令同源 (Priority: P2)

**Goal**: 首页 CLI 演示来自本地脚本化模型，明确标注；命令与快速开始逐字一致；可重采。

**Independent Test**: 对比演示命令与快速开始；重跑 stub 采集；改 stub 回复不更新 transcript 时检查失败。

- [x] T026 [P] [US3] 新建 `website/demo/local-model.mjs`：只绑定 `127.0.0.1` 的 OpenAI 兼容 stub；首轮返回 `read_file` 工具调用读取样例 README，收到结果后返回固定项目摘要；忽略但要求非空 API key；无外部网络
- [x] T027 [US3] 新建 `website/.vitepress/theme/components/CliDemo.vue` 并接入首页：浅色终端动画、重播按钮、减少动态效果 / 键盘降级、脚本化标注可见；只消费 `.vitepress/data/demo.mjs`
- [x] T028 [US3] 采集 `website/demo/transcript.json`：用真实 `basalt chat --agent demo --message "Hello"` 与 stub 运行，字段 `capturedAt` / `source` / `stub` / `command` / `lines` / `exitCode`；`command` 与 `QUICKSTART_COMMANDS` 逐字相等
- [x] T029 [P] [US3] 新建 `website/tests/demo.test.mjs`：stub 先 tool call 后最终答复；减少动态效果直接显示；transcript 字段齐全且 `lines` 与 stub 最终文案一致
- [x] T030 [US3] 修改 `website/tests/content.test.mjs`：演示命令与快速开始命令逐字一致；脚本化标注存在；终端展示内容全英文且不随语言切换；`DURATION_MEASURED=false` 时禁用时长词

**Checkpoint**: US3 可独立演示——首页动画可复跑、可失败。

---

## Phase 6: User Story 4 - 维护者更新内容并手动决定是否发布 (Priority: P3)

**Goal**: 检查器能发现缺页 / 缺图 / 断链 / 私有路径 / 命令漂移；CI 与手动部署分离；Operations 只链公开目标。

**Independent Test**: 逐类制造漂移确认检查失败；确认部署工作流仅手动；确认 Operations 无私有链接。

- [x] T031 [P] [US4] 新建 `website/scripts/check-links.mjs`：扫描 `dist/**/*.html` 的内部链接 / 锚点 / base / 空链接 / `javascript:` / 私有路径 / 外部 allowlist；导出 `checkLinks({distDir, base})` 与 `main()`；`check: website-build`
- [x] T032 [P] [US4] 新建 `website/scripts/check-diagrams.mjs`：六类固定 ID、双语成对、文件存在、`data-diagram-id` / `data-source-commit` 祖先 / `<title>` / `<desc>` / `viewBox` 一致、无外部引用、执行流分支；drawio 恰好 12 页；两版 README 各恰好一张 `system-architecture` 简图；导出 `checkDiagrams` 与 `main()`；`check: website-build`
- [x] T033 [P] [US4] 新建 `website/scripts/check-scope.mjs`：解析 `git status --porcelain`，only allow §3 公开文件、`specs/013-*`、`harness/stages/us13-website-readme.json`、开工前已有 `tmp/`；越界非零退出；`check: scope`
- [x] T034 [P] [US4] 新建 `website/tests/checks.test.mjs`：用临时夹具验证 `checkLinks` 抓缺失目标 / 私有路径 / 根路径硬编码，`checkDiagrams` 抓缺对 / 外部引用 / 缺源码标记，内容检查纯函数抓缺页 / 命令漂移 / 私有路径 / 演示不一致；工作流断言（CI `website` job 含四条命令且 `fetch-depth: 0`；Pages 仅 `workflow_dispatch`，拒绝 `push` / 数组 / secrets）；`check: workflow`
- [x] T035 [US4] 修改 `.github/workflows/ci.yml`：新增独立 `website` job（checkout `fetch-depth: 0` → `setup-node`（`.nvmrc`）→ `npm ci` → `npm test` → `npm run build` → `npm run check` → 上传 `website-dist`）；不改 Go job，Go job 不调用 Node；`check: workflow`
- [x] T036 [US4] 新建 `.github/workflows/pages.yml`：仅 `workflow_dispatch`；权限 `contents: read` / `pages: write` / `id-token: write`；`environment: github-pages`；`configure-pages` → `upload-pages-artifact` → `deploy-pages`；无 secrets、无自动触发；本 US 不执行；`check: workflow`
- [x] T037 [US4] 修改 `internal/cli/public_docs_test.go`：新增 `operations-links` 子测试，扫描公开 `docs/Operations.md`，四份私有文档链接零出现；核对 D-6 A 已落地的链接修正（README 外链 + 去链接），不改运维行为；`check: go-docs-consistency`
- [x] T038 [US4] 新建 `website/README.md`：Node 版本、安装 / 构建 / 测试 / 检查命令、stub 启动与 transcript 采集、drawio 导出、README 命令标记块、公共 / 私有边界；不复制产品运维手册

**Checkpoint**: US4 可独立演示——漂移会被检查器抓住，部署边界清楚。

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: 全量验证、阶段工件与收口证据。

- [x] T039 运行 `cd website && npm ci && npm test && npm run build && npm run check`，零失败；保存日志作为 `website-build` 证据；无 Node 时不得记为通过
- [x] T040 运行 `GOPROXY=https://goproxy.cn go test ./internal/cli -run TestPublicDocs` 与 `make check` 七目标；`gofumpt -w internal/cli/public_docs_test.go` 后复跑；`check: go-docs-consistency`
- [x] T041 新建 / 更新 `harness/stages/us13-website-readme.json`（控制侧；收口时把双语 README、站点关键文件、`public_docs_test.go`、`pages.yml` 等**逐个文件**补入 `inputs` 并复跑，不得用目录）：local-only；`checks` 为 `go-docs-consistency` / `website-test` / `website-build` / `website-check` / `workflow` / `scope`；`approval` 记录 D-1–D-6 全部 A、R2 哈希、用户 2026-10-24 实施后确认（六类图 / 单张 README 简图 / 五层架构 / 英文终端）与 local-only 原因（**Claude 执行**）
- [x] T042 运行 `node website/scripts/check-scope.mjs` 与 `git status --porcelain`：只允许 §3 公开文件与过程工件；私有文档不出现在 `git status`；无 `git add -f`；`check: scope`
- [x] T043 完成 review ② 必验项（§6.2.1）：站点安装 / 测试 / 构建、管理台实际配置与启用点击、完整快速开始、三档视口、语言切换与键盘操作；证据写入 run-state；真实模型与线上 Pages 保持未授权 / 未执行
- [x] T044 清理临时物（`tmp/us13-lint` 等），确认 `tmp/` 不进入提交；报告如实写明未验证项；**Claude 执行**

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始
- **Foundational (Phase 2)**: 依赖 Setup；阻塞全部用户故事
- **US1 (Phase 3)**: 依赖 Foundational；MVP
- **US2 (Phase 4)**: 依赖 Foundational；与 US1 可并行，但都改 `content.test.mjs`，实际按 US1 → US2 串行编辑
- **US3 (Phase 5)**: 依赖 Foundational 与 US2 的命令源；演示命令必须与快速开始一致
- **US4 (Phase 6)**: 依赖 US1–US3 的产物（检查器要扫描最终页面与图）；`operations-links` 依赖 T023 的 Go 测试文件
- **Polish (Phase 7)**: 依赖全部用户故事

### User Story Dependencies

- **US1**: 无故事内依赖；产出页面、图与视觉基线
- **US2**: 复用 US1 的页面骨架与组件；命令源在 Foundational
- **US3**: 复用 US2 的 chat 命令；演示数据独立
- **US4**: 复查全部产物；不改变页面语义

### Parallel Opportunities

- T002 / T003 / T004 可并行
- T005–T010 可并行（不同数据与资产文件）
- T012–T014 可并行（不同页面）；T015–T016 可并行（不同测试文件）
- T019 / T020 可并行（双语 README）
- T026 / T029 可并行（stub 与 demo 测试）
- T031–T034 可并行（检查器与夹具测试，除 `checks.test.mjs` 依赖前两者成品）
- T035 / T036 可并行（两个工作流文件）

---

## Parallel Example: User Story 1

```bash
# 页面与测试可并行起草：
Task: "Create website/index.md and website/zh/index.md"
Task: "Create website/architecture/index.md and website/zh/architecture/index.md"
Task: "Create website/docs/index.md and website/zh/docs/index.md"
Task: "Create website/tests/routes.test.mjs"
Task: "Create website/tests/contrast.test.mjs"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 Phase 1 Setup
2. 完成 Phase 2 Foundational
3. 完成 Phase 3 US1
4. 本地预览验证四页双语与视觉基线
5. 继续 US2 → US3 → US4

### Incremental Delivery

1. Setup + Foundational → 站点可构建
2. US1 → 对外门面可读（MVP）
3. US2 → README 能带新用户跑通
4. US3 → 首页演示可复跑
5. US4 → 漂移检查与手动发布边界
6. Polish → 全量门禁与 review ② 证据

### 实现分工

- 实现由 **Codex** 执行；Codex **不得** commit / push / tag / 改验收判据。
- Claude 负责 stage、门禁执行、独立复核与 evidence；Codex 自报不作为证据。
- 私有文档修改只在本地，不提交；`docs/Operations.md` 的 D-6 A 修正已在 review ① 收口时落地，T037 只做核对与回归测试。

---

## Notes

- [P] 任务 = 不同文件、无未完成依赖
- [Story] 标签映射到 spec 的用户故事
- 每个用户故事可独立验收
- 实现前先确认测试会失败（检查器夹具、缺页 / 缺图用例）
- 提交归用户；本 US 不合入 `main`、不部署
