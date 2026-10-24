# Research: US-13 官网展示主页与 README（对外门面）

**Date**: 2026-10-24 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

本文件只记录本 US 的关键技术决策、替代方案与取舍；事实主源是 [docs/us/us13-website-readme.md](../../docs/us/us13-website-readme.md) R3（R2 为人工 review ① 基线，R3 按用户 2026-10-24 实施后确认同步）与 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md)。

## D-01 四页结构（首页 / 快速开始 / 架构 / 文档索引）

- **Decision**: 四页 × 英中 = 八个地址；英文在根路径，中文在 `/zh/` 下。
- **Rationale**: 用户 D-1 A 裁决；索引页给"我知道要做什么、去哪里找"的读者一个入口，避免把架构页或快速开始页塞成门户。四页都在导航与页脚可达，语言切换保留锚点。
- **Alternatives considered**: 三页（原方案）——被否，文档入口只能塞进架构页顶部或快速开始页尾，任务分组无处安放；完整文档门户——被否，当前没有足够多公开内容，扩成门户会制造空页。

## D-02 站点生成器与版本

- **Decision**: VitePress 1.6.4，Node 24.22.0（`website/.nvmrc`），`base: '/basalt/'`，`npm ci` + 已提交 `package-lock.json`。
- **Rationale**: 技术方案 §1.3 已列、2026-10-24 npm registry 复核 1.6.4 为 latest；静态产物可部署到 GitHub Pages 项目页，构建期依赖不进单二进制。Node 只用于站点构建 / 检查、管理台重建与演示采集。
- **Alternatives considered**: 自写静态生成脚本——被否，导航 / 主题 / 构建成本高于收益；其他静态站点框架——被否，栈表外新增坐标需用户裁决，且 VitePress 已批准。

## D-03 不引入 Vitest

- **Decision**: 站点测试使用 Node 24 内置 `node --test`；`website/` 只依赖 VitePress。
- **Rationale**: VitePress 1.6.4 依赖 Vite `^5.4.14`；Vitest 5.0.5 的 peer 要求 Vite `^6.4.0 || ^7 || ^8`，同包安装会出现双 Vite 或 peer 冲突。控制台的 Vitest 5 留在 `assets/console`，两个包各自安装、互不影响。
- **Alternatives considered**: 升级 VitePress 或降级 Vitest——被否，超出本 US 范围且引入未批准版本；把站点测试并入控制台包——被否，构建链与产物边界会混在一起。

## D-04 语言切换

- **Decision**: 单一纯函数 `switchLocalePath(path, targetLocale, base)`：保留页面、查询串与哈希；英文 → `/zh/<path>`，中文 → 去掉 `/zh` 前缀；根路径与子路径都先剥离 / 还原 base；无法映射时回退目标语言首页。
- **Rationale**: 语言切换是站点最容易被写坏的行为；集中一个函数可由 `node --test` 穷举，页面组件只调用它。GitHub Pages 项目页的 `/basalt/` 前缀必须在函数内统一处理，不能散落在组件里。
- **Alternatives considered**: 每个页面手写切换链接——被否，八个页面重复且易漂移；用 VitePress 内置语言下拉——被否，默认行为不保留锚点且不易断言。

## D-05 命令唯一来源

- **Decision**: `website/.vitepress/data/quickstart.mjs` 定义 `QUICKSTART_COMMANDS`（init / profile create / serve / chat / session list 等）；站点页面、README 标记块与首页演示 transcript 都从它比对，不允许另写一份。
- **Rationale**: 快速开始一旦与实现漂移，读者第一步就失败；单一数据源 + 检查器能在命令改动而文档未同步时立即失败。`content.test.mjs` 逐条比对，`public_docs_test.go` 再扫公开 README。
- **Alternatives considered**: 以 README 为唯一来源、站点引用 README——被否，站点构建无法直接嵌入带标记的 Markdown 命令块且语言页需要独立渲染；人工同步——被否，无判别力。

## D-06 六类图与图源

- **Decision**: 一个 drawio 图源（6 类 × 英中 12 页），导出 12 个 SVG；六类为项目全景、系统架构简图、核心执行流、模块图、Go 包依赖、部署。每个 SVG 必须带 `data-diagram-id`、`data-source-commit`、`<title>`、`<desc>`、`viewBox`；`check-diagrams` 校验 ID 集合、双语成对、源提交是祖先、无外部引用 / 脚本 / `foreignObject`。架构页承载完整全景与全部专题图；两版 README 各只嵌系统架构简图。
- **Rationale**: 用户 D-4 A 要求两套且同源，2026-10-24 实施后确认扩展为六类并把完整全景放架构文档；只数文件会漏掉"中文图其实是英文内容"或"图源缺失"。执行流图还要求 `<desc>` 覆盖四个终态与失败 / 拒绝 / 取消 / 恢复分支。
- **Alternatives considered**: 只做英文图 + 中文说明——被否，D-4 A 明确两套；复用旧 `docs/assets/basalt-architecture.svg`——被否，旧图是单 Run 设计稿，与当前多 Agent 托管语义不同；README 放两张图——被否，用户 2026-10-24 确认每版只留一张简化图，详细图由架构文档承接。

## D-07 首页演示与模型边界

- **Decision**: `website/demo/local-model.mjs` 只绑定 `127.0.0.1` 的 OpenAI 兼容 stub；首轮返回 `read_file` 工具调用，读取样例 README，收到工具结果后返回固定项目摘要；`transcript.json` 由真实 CLI 执行采集，字段与最终文案逐字一致；页面明确标注"脚本化回复，不代表模型效果"。展示层把工具进度标签译为英文，终端内容全英文且不随站点语言切换；原始 transcript 与产品 CLI 输出保持原样。
- **Rationale**: 用户 D-2 A；真实厂商模型未授权、也不需要真实 key。stub 能验证 CLI、工具调用与持久化链路，且无外部数据与费用，因此不登记外部授权 ref。
- **Alternatives considered**: 真实模型录屏——被否，未授权且不可复跑；纯静态假输出——被否，无法验证链路，也不会随 CLI 变化失败。

## D-08 检查器形态

- **Decision**: `check-links.mjs` / `check-diagrams.mjs` 导出纯函数并带 `main()`；`check-scope.mjs` 只给 stage 用；`checks.test.mjs` 用临时夹具验证检查器本身能抓错。所有 Node 测试用 `node --test`。
- **Rationale**: 检查器若只会在正确输入上返回 0，就没有判别力；夹具测试要求它能对缺失目标、私有路径、外部引用、缺源标记与根路径硬编码逐类报错。
- **Alternatives considered**: 只跑构建、不做后置检查——被否，构建成功不代表链接与图语义正确；用第三方链接检查器——被否，新增坐标且无法覆盖私有路径与本仓图源规则。

## D-09 部署工作流

- **Decision**: `.github/workflows/pages.yml` 仅 `workflow_dispatch`；权限 `contents: read` / `pages: write` / `id-token: write`；`configure-pages` → `upload-pages-artifact` → `deploy-pages`；本 US 不执行。
- **Rationale**: 用户 D-3 A；先本地预览，线上验证由用户触发后单独记录。工作流存在不等于站点已上线，README 与站点在验证前不写线上地址。
- **Alternatives considered**: 合并到 `main` 自动部署——被否，未验证内容不应自动公开；不提供工作流——被否，发布时需要可复现入口。

## D-10 公开 `docs/Operations.md` 链接修正

- **Decision**: 删除指向 `Compatibility.md`、`IndustrySurvey.md`、`Requirements.md`、`TechnicalDesign.md` 的链接；改为公开目标（README、contracts）或去链接；不改运维步骤、命令与判据。
- **Rationale**: 用户 D-6 A；官网把 Operations 作为用户运维入口，已知失效链接应一起收口。`public_docs_test.go` 的 `operations-links` 子测试防止回归。
- **Alternatives considered**: 后置 US-14——被否，官网先上线会先把读者引到私有链接；公开私有正文——被否，边界不允许。

## D-11 依赖措辞（预构建 vs 源码构建）

- **Decision**: 运行预构建二进制不需要 Go / Node；从源码构建需要 Go；管理台前端重建、站点构建与演示 transcript 采集需要 Node（仅维护者）。Node **不是**产品运行时依赖。
- **Rationale**: 用户 review ① 收口第 2 条；把"构建期工具"写成"运行前置"会误导部署者，也会污染单二进制纪律。
- **Alternatives considered**: 统一写"需要 Node"——被否，错误且与 Operations §1.1 冲突；统一不写依赖——被否，贡献者无法构建站点。

## D-12 版本事实与已知漂移

- **Decision**: 以 `go.mod`、`website/package-lock.json`、`website/.nvmrc` 与 CI 为版本事实源；私有文档中 Go 1.27.2 的旧读数作为已知漂移保留，本 US 不静默改写版本行。
- **Rationale**: 本 US 的裁决范围是页面集合与交付边界，不包含版本行；改写会超出 review ① 授权。
- **Alternatives considered**: 顺手把私有文档版本行改成 1.27.3——被否，超出已审范围且属无关改动。

## D-13 五层架构与当前 / 未来分离

- **Decision**: 架构统一采用接入层、应用层、执行引擎层、基础服务层、存储层；调度归应用层，安全与审计贯穿各层。当前单节点实现与未来规划分开表述；GitHub Pages 项目页与真实厂商模型仍不在本轮执行范围。
- **Rationale**: 用户 2026-10-24 确认；五层是运行时职责划分，与 Go 包依赖图互为补充，不能把未来分布式能力画成现状。
- **Alternatives considered**: 按 Go 包分层直接画运行时架构——被否，包依赖方向另有专题图，混在一起会让读者把编译依赖当成部署边界。

## 未决与后续

- 站点本地构建 / 测试在本轮规划时尚未执行（PATH 无 Node）；实现后必须在本机或 CI 取得通过日志，`not_run` 不得作为 review ② 收口状态。
- 线上 Pages 地址与真实厂商模型调用均未授权 / 未执行，不作为本 US 收口条件。
- 未来若启用站内搜索，使用 VitePress 本地搜索，只覆盖站内页面，并在界面标注范围；本 US 不放搜索框。
