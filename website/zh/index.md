---
title: Basalt｜企业自托管 Agent OS
description: 面向企业团队的自托管 Agent OS，托管独立业务 Agent，复用执行、接入、调度和审计底座，可选 Compose 多 Worker 部署。
layout: home
---

<!-- markdownlint-configure-file {"MD033":{"allowed_elements":["a","script","HomeHero","CliDemo","CapabilityGrid","div"]}} -->

<script setup>
import { withBase } from 'vitepress'
</script>

<HomeHero />

## 命令行演示 {#demo}

<CliDemo />

<CapabilityGrid />

<div class="home-examples">

Basalt 用统一的 Go 运行与管理底座托管多个独立业务 Agent。每个 Agent 的身份、配置和业务状态按作用域管理，共享执行、接入、调度、存储与审计。可选 Compose 多 Worker 部署增加控制面与 Worker，共用一个 PostgreSQL；已发布资产自动分发，并按每次执行固定版本。

**演示配置：** 随二进制提供五个每日演示——业务日报、文档差异、GitHub 雷达、行业简报与代码仓库日报，各自拥有独立的定时、凭据与输出。 <a :href="withBase('/zh/architecture/#capabilities')">了解 Agent 与共享底座 →</a>

<a :href="withBase('/zh/docs/#verification')">查看仓库的自动化检查 →</a>

</div>
