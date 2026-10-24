# Quickstart Validation: US-13 官网展示主页与 README

**Date**: 2026-10-24 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

本文件是验证向导，不是产品快速开始的替代品。产品快速开始的正文以模块卡 §3.4 与站点页面为准；本文件只回答"怎么证明 US-13 做完了"。

## 1. 前置条件

- 仓库当前 HEAD 含本 US 的实现；工作区只有本 US 允许的公开文件（见模块卡 §6.5）。
- Go 工具链与 `GOPROXY=https://goproxy.cn` 可用（Go 侧只跑测试，不改产品代码）。
- Node 24.22.0 可用（本机 PATH 可能没有；可用官方便携包，或直接使用 CI `website` job 的日志）。
- 真实厂商模型与 GitHub Pages 线上部署**均未授权**；验证不得调用外部模型、不得触发部署。

## 2. 站点安装、测试、构建与检查

```bash
cd website
npm ci
npm test          # node --test "tests/*.test.mjs"
npm run build     # 产物在 .vitepress/dist，基路径 /basalt/
npm run check     # check-links + check-diagrams
```

**预期**：

- `npm ci` 无 peer 冲突，只安装 `vitepress@1.6.4` 及其依赖；`npm ls vite` 只有一套 Vite 5。
- `npm test` 覆盖路由 / 语言切换、内容同源、演示夹具、对比度与检查器夹具，全部通过。
- `npm run build` 成功；`dist` 内四页英中齐全。
- `npm run check` 零 finding；缺页、缺图、断链、私有路径、命令漂移、未测量时长、演示不一致、README 多嵌图任一出现即失败。
- 六类图在英中两套下均存在；`basalt-diagrams.drawio` 为 12 页。

## 3. Go 回归测试

```bash
GOPROXY=https://goproxy.cn go test ./internal/cli -run TestPublicDocs
```

**预期**：

- `public` 子测试扫描双语 README，`--profile` 零出现、`--agent` 存在。
- `operations-links` 子测试扫描公开 `docs/Operations.md`，四份私有文档链接零出现。
- `requirements-local` 子测试在 clean checkout 缺私有文件时以明确理由 `t.Skip`；本机存在私有文件时必须通过。
- 该命令不访问网络、不调用模型。

## 4. 本地预览与人工检查（review ② 必验项）

```bash
cd website
npm run dev        # 本地预览；或 npm run preview 预览构建产物
```

逐项完成并留证：

| 检查 | 期望 |
| --- | --- |
| 四页英中 | 八个地址全部可达；导航、页脚、按钮、404 文案对应完整 |
| 语言切换 | 在带锚点 / 查询串的页面切换，落在同页同锚点；base `/basalt/` 正确 |
| 首页六卡 | 公开标题 `Platform capabilities / 平台能力`；六张卡 3 / 2 / 1 列；插图与双语 `alt` 正常 |
| 架构页 | 五项核心能力 + 六类图 × 英中；完整项目全景与全部专题图在架构文档；五层为接入 / 应用 / 执行引擎 / 基础服务 / 存储，安全与审计贯穿；执行流图含四个终态与失败 / 拒绝 / 取消 / 恢复分支 |
| README | 两版各只嵌一张本地化系统架构简图；完整全景与专题图由架构文档承接 |
| 文档索引 | Get started / Understand / Operate / Project 四组；只链真实目标；无搜索框，低调说明无站内搜索 |
| 键盘 | 只用 Tab / Enter 可达主导航、语言切换、页脚链接；焦点环可见 |
| 减少动态效果 | 演示直接显示完整内容，不依赖动画 |
| 断网 | 无外部 CDN / 字体 / 脚本请求；页面版式完整 |
| 三档视口 | 1440×900 / 768×1024 / 390×844 无横向溢出；六卡 3 / 2 / 1 列 |

## 5. 快速开始端到端（review ② 必验项）

在干净临时工作区按公开 README 从零执行：

1. 写最小 `basalt.yaml`（单 provider、管理 token 用 `${BASALT_ADMIN_TOKEN}` 占位）。
2. `basalt init`。
3. `basalt profile create demo`。
4. `basalt serve`。
5. 浏览器打开管理台：登录 → Agents → demo → 配置模型连接 → 保存 → 启用 Agent；页面显示已启用。
6. 停止 `serve`。
7. `basalt chat --agent demo --message "Hello"`。
8. `basalt session list`，确认能看到这次会话。

**预期**：全程不需要手改生命周期状态文件，不需要 `--profile`、`profile enable` 或不存在的命令；`registered=true` 的说明与页面一致；Windows 下 token 生成方式可用。

## 6. 演示夹具复跑

```bash
node website/demo/local-model.mjs   # 仅绑定 127.0.0.1
# 按 website/README.md 的采集步骤重新生成 transcript.json
```

**预期**：stub 首轮返回 `read_file` 工具调用读取样例 README，收到结果后返回固定项目摘要；`transcript.json` 的 `command` 与快速开始 chat 命令逐字一致，`lines` 与 stub 最终文案一致，`exitCode` 为 0；页面显示脚本化标注，终端展示全英文且不随站点语言切换。

## 7. 范围与收口

```bash
node website/scripts/check-scope.mjs
git status --porcelain
```

**预期**：

- `check-scope` 只允许模块卡 §3 的公开文件、`specs/013-*`、`harness/stages/us13-website-readme.json` 与开工前已有的 `tmp/`；越界非零退出。
- `git status` 只出现预期公开文件；私有 `docs/Requirements.md`、`docs/DevelopmentLoop.md`、`docs/TechnicalDesign.md` 不出现在 `git status`（被 `.git/info/exclude` 忽略），也不得 `git add -f`。
- `make check` 七目标全绿；`make check` / `scripts/check-acceptance.*` 不调用 Node / npm。

## 8. 明确不在本轮验证范围

| 项 | 原因 |
| --- | --- |
| 真实厂商模型调用 | 未授权；快速开始由读者填入自己的端点与密钥 |
| GitHub Pages 线上部署与线上 URL | 用户 D-3 A：本 US 不部署；线上验证另行记录 |
| 外部 HTTP 探测 / 生产环境访问 | 本 US 只有本地预览与 CI 构建 |
| 完整教程耗时 | 未测量；公开材料不写时长 |

以上项不得作为收口条件，也不得反过来写成"已验证"。反之，§4–§6 的必验项不得以"已登记未验证"收口。
