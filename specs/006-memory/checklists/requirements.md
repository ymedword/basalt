# Specification Quality Checklist: US-6 Memory 长期记忆（两层作用域 + 两档载体 + 两件记忆工具）

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-10-17
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Notes

- Items marked incomplete require spec updates before `/speckit-clarify` or `/speckit-plan`
- 「No implementation details」按本项目口径解读（与 US-2 / US-3 / US-4 / US-5 规格同口径）：产品契约里的**用户可见字面量**（两件工具名 `save_memory` / `recall_memory`、传入参数名 `scope` 与取值 `CORE` / `ARCHIVAL`、后端取值 `markdown` / `sqlite`、配额读数 4096 / 32768、分区标题字面量、确认文案形态、`(无匹配条目)` 渲染）属 WHAT 面，不算实现细节；**内部类型名 / 包落位 / 测试文件组织一律不进本规格**（归 plan 与颗粒度文档 §3——Key Entities 用中文概念名，不写 Go 标识符）。规格中出现的依赖 / 配置键 / 表结构纪律是**零变更判据面**，与既有规格同口径。
- 「Success criteria are technology-agnostic」的工程纪律条目（SC-008）按本项目惯例保留可 grep 判据（import 面 / 依赖清单 diff / 配置键 diff / 迁移 diff）——宪法 Governance 要求每条原则带**可验证的复核方式**，与 US-2~US-5 同口径，不判为泄漏。
- 本规格从 `docs/us/us6-memory.md` 派生（该文档「待决事项」已清零、设计面冻结：2026-10-17 评审回合 P1 三条 / P2 三条 / D1~D4 全部落案）；事实源与裁决顺位见规格头部 blockquote。
- 「测试类 → 用户故事」的划分（四个故事 ↔ §4.2 的十二个测试文件面）是本规格的**唯一自定结构**：颗粒度文档按包分组、未按故事分组；划分口径 = 交付价值面（段 2 注入 / 两层作用域 / 载体与隔离 / 治理可判卷），每组覆盖的测试文件与用例在 tasks 阶段逐条对号，不改动颗粒度文档任何内容。故事编号 `[US1]`~`[US4]` 是 feature 内部标签，与台账 US 编号不混淆（规格内已声明）。
- 边界如实登记项已进规格正文：归档区无配额 + 每次运行整层读一次（**已知量、带读数**，MUST NOT 写成「性能已验证达标」）、跨进程 / 多实例写入不承诺（单实例由实例锁兜底）、`mem0` 档归扩展阶段（取值域外即拒绝启动）、记忆写入审核 / 敏感信息过滤归扩展阶段（本 feature 写入面只有配额一道）。
- 两处上游一致面在澄清会话就地收敛、未留待决：①检索语义的旧表述「多词短语必不命中」与 `contains` 实现不符 → 澄清为「整串子串匹配、连续短语命中、分散词不自动组合」并同步上游四处；②读取面错误处理分三档（缺失 / 合法空层 · I/O 失败 · 非空损坏），原文「标题破坏 → WARN + 当空层回退」的断言**已废止**。规格 MUST 钉两态（连续短语命中 / 分散词不命中），MUST NOT 钉旧表述。
- 硬依赖 US-4 / US-1 / US-5 已交付面逐条列在 `## Assumptions`；前序改造点九项（含「模板默认工具三件 ↔ 注册池八件」的连带关系）属**软门禁**，开工前报告。
