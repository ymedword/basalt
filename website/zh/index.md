---
title: Basalt — 独立 Agent，共享底座
description: 面向企业团队的自托管 Agent OS，托管独立业务 Agent，复用执行、接入、调度和审计底座。
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

Basalt 用统一的 Go 运行与管理底座托管多个独立业务 Agent。每个 Agent 的身份、配置和业务状态按作用域管理，共享执行、接入、调度、存储与审计。

**演示配置：** daily-report 负责定时简报；file-review 负责工作区文件检查。两个用途，分别管理。 <a :href="withBase('/zh/architecture/#capabilities')">了解 Agent 与共享底座 →</a>

</div>
