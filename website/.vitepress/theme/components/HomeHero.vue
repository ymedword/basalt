<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { localePath } from '../../data/navigation.mjs'
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
</script>

<template>
  <section class="home-hero" aria-labelledby="home-title">
    <p class="eyebrow">BASALT / AGENT OS</p>
    <h1 id="home-title">{{ zh ? '看见 Agent 的工作过程。' : 'See your agents at work.' }}</h1>
    <p class="hero-description">{{ zh ? '面向企业团队的自托管 Agent OS。' : 'A self-hosted Agent OS for enterprise teams.' }}<br>{{ zh ? '独立业务 Agent，共享运行底座。' : 'Independent agents. Shared infrastructure.' }}</p>
    <div class="hero-actions">
      <a class="action-primary" :href="withBase(localePath(zh ? 'zh' : 'en', 'quickstart'))">{{ zh ? '开始使用' : 'Get started' }} <span aria-hidden="true">→</span></a>
      <a class="action-secondary" :href="withBase(localePath(zh ? 'zh' : 'en', 'home') + '#demo')">{{ zh ? '观看演示' : 'Watch demo' }}</a>
    </div>
    <img class="hero-rock" :src="withBase('/brand/rock-column.svg')" :alt="zh ? '独立岩柱构成的共享基座线稿' : 'Independent basalt columns on a shared foundation'" width="520" height="590" fetchpriority="high">
  </section>
</template>
