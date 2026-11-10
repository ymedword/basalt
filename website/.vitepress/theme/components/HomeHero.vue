<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { localePath } from '../../data/navigation.mjs'
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
</script>

<template>
  <section class="home-hero" aria-labelledby="home-title">
    <p class="eyebrow">AGENT OS</p>
    <h1 id="home-title">{{ zh ? 'Basalt｜企业自托管 Agent OS' : 'Basalt | Self-hosted Agent OS' }}</h1>
    <p class="hero-description">{{ zh ? '独立业务 Agent，共享运行、调度与审计底座。' : 'Independent business agents share one runtime, scheduler and audit trail.' }}<br>{{ zh ? '一个 Go 二进制，运行在你自己的基础设施上。' : 'One Go binary, on infrastructure you run yourself.' }}</p>
    <div class="hero-actions">
      <a class="action-primary" :href="withBase(localePath(zh ? 'zh' : 'en', 'quickstart'))">{{ zh ? '开始使用' : 'Get started' }} <span aria-hidden="true">→</span></a>
      <a class="action-secondary" :href="withBase(localePath(zh ? 'zh' : 'en', 'home') + '#demo')">{{ zh ? '观看演示' : 'Watch demo' }}</a>
    </div>
    <img class="hero-rock" :src="withBase('/brand/rock-column.svg')" :alt="zh ? '独立岩柱构成的共享基座线稿' : 'Independent basalt columns on a shared foundation'" width="520" height="590" fetchpriority="high">
  </section>
</template>
