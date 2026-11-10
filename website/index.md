---
title: Basalt | Self-hosted Agent OS
description: A self-hosted Agent OS for enterprise teams. Independent business agents share execution, connections, scheduling and auditing, with optional Compose multi-Worker deployment.
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

Basalt hosts multiple independent business agents on a shared Go runtime and management foundation. Identity, configuration and business state are scoped per agent; execution, connections, scheduling, storage and auditing are shared. Optional Compose multi-Worker deployment adds a control plane and Workers over one PostgreSQL, with published assets distributed automatically and pinned per execution.

**Demo configurations:** five daily demos ship with the binary — a business daily, a document diff, a GitHub radar, an industry brief and a repository digest — each with its own schedule, credentials and output. <a :href="withBase('/architecture/#capabilities')">Explore agents and the shared foundation →</a>

<a :href="withBase('/docs/#verification')">How the repository is checked →</a>

</div>
