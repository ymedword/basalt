---
title: Basalt — Independent agents, shared infrastructure
description: A self-hosted Agent OS for enterprise teams. Independent business agents share execution, connections, scheduling and auditing.
layout: home
---

<!-- markdownlint-configure-file {"MD033":{"allowed_elements":["a","script","HomeHero","CliDemo","CapabilityGrid","div"]}} -->

<script setup>
import { withBase } from 'vitepress'
</script>

<HomeHero />

## CLI demo {#demo}

<CliDemo />

<CapabilityGrid />

<div class="home-examples">

Basalt hosts multiple independent business agents on a shared Go runtime and management foundation. Identity, configuration and business state are scoped per agent; execution, connections, scheduling, storage and auditing are shared.

**Demo configurations:** daily-report prepares scheduled briefings; file-review checks workspace files. Two purposes, independently managed. <a :href="withBase('/architecture/#capabilities')">Explore agents and the shared foundation →</a>

</div>
